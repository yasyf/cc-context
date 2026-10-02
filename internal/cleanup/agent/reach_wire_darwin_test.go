package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/daemon"
)

const wireReport = `{"report":{"version":"v0.0.1","pid":7,"paused":true,"governor":{"state":"idle","cpu_percent":0},"jobs":[]}}`

var programBytes = []byte("the program the running daemon started from")

type serveLock int

const (
	unlocked serveLock = iota
	released
	held
)

type wireServer struct {
	mu    sync.Mutex
	ops   []string
	conns []net.Conn
}

func (s *wireServer) record(conn net.Conn) (string, bool) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return "", false
	}
	var req struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, req.Op)
	return req.Op, true
}

func (s *wireServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

func serveWire(t *testing.T, socket string, handle func(s *wireServer, conn net.Conn)) *wireServer {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &wireServer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			handle(s, conn)
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, conn := range s.conns {
			_ = conn.Close()
		}
	})
	return s
}

func answering(protocol int) func(*testing.T, string) *wireServer {
	hello := fmt.Sprintf(`{"info":{"version":"v0.0.1","protocol":%d,"pid":7}}`, protocol)
	return func(t *testing.T, socket string) *wireServer {
		return serveWire(t, socket, func(s *wireServer, conn net.Conn) {
			defer func() { _ = conn.Close() }()
			op, ok := s.record(conn)
			if !ok {
				return
			}
			reply := map[string]string{"hello": hello, "status": wireReport}[op]
			_, _ = conn.Write([]byte(reply + "\n"))
		})
	}
}

func snapshot(t *testing.T, root string) map[string]cleanup.FileID {
	t.Helper()
	ids := map[string]cleanup.FileID{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		id, _, err := cleanup.LstatID(path)
		if err != nil {
			return err
		}
		ids[path] = id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestReachOverTheWire(t *testing.T) {
	missing := func(*testing.T, string) *wireServer { return nil }
	stale := func(t *testing.T, socket string) *wireServer {
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		l.SetUnlinkOnClose(false)
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	tests := []struct {
		name     string
		setup    func(t *testing.T, socket string) *wireServer
		lock     serveLock
		wantErr  error
		wantText string
		wantOps  []string
	}{
		{
			name:     "missing socket",
			setup:    missing,
			wantErr:  errNoDaemon,
			wantText: "no such file or directory",
		},
		{
			name:     "socket left behind by an exited daemon",
			setup:    stale,
			wantErr:  errNoDaemon,
			wantText: "connection refused",
		},
		{
			name:     "socket left behind beside a released serve lock",
			setup:    stale,
			lock:     released,
			wantErr:  errNoDaemon,
			wantText: "connection refused",
		},
		{
			name:     "refusing socket while a process holds the serve lock",
			setup:    stale,
			lock:     held,
			wantErr:  errUnresponsive,
			wantText: "but is not accepting connections on",
		},
		{
			name:     "missing socket while a process holds the serve lock",
			setup:    missing,
			lock:     held,
			wantErr:  errUnresponsive,
			wantText: "but is not accepting connections on",
		},
		{
			name: "daemon accepts and never answers",
			setup: func(t *testing.T, socket string) *wireServer {
				return serveWire(t, socket, func(s *wireServer, conn net.Conn) { s.record(conn) })
			},
			wantErr:  errUnresponsive,
			wantText: "sent no hello within 100ms",
			wantOps:  []string{"hello"},
		},
		{
			name: "daemon accepts and closes while shutting down",
			setup: func(t *testing.T, socket string) *wireServer {
				return serveWire(t, socket, func(_ *wireServer, conn net.Conn) { _ = conn.Close() })
			},
			wantErr:  errUnresponsive,
			wantText: "closed the connection without a hello",
		},
		{name: "older release on this protocol answers status", setup: answering(cleanup.Protocol), wantOps: []string{"hello", "status"}},
		{
			name:     "newer protocol",
			setup:    answering(cleanup.Protocol + 1),
			wantErr:  errNewerProtocol,
			wantText: fmt.Sprintf("daemon v0.0.1 (pid 7) speaks protocol %d, this ccx %d", cleanup.Protocol+1, cleanup.Protocol),
			wantOps:  []string{"hello"},
		},
		{
			name:     "older protocol",
			setup:    answering(cleanup.Protocol - 1),
			wantErr:  errOlderProtocol,
			wantText: fmt.Sprintf("daemon v0.0.1 (pid 7) speaks protocol %d, this ccx %d", cleanup.Protocol-1, cleanup.Protocol),
			wantOps:  []string{"hello"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "ccxa")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			layout := cleanup.Layout{Root: root}
			socket, err := layout.Socket()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(layout.ProgramPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(layout.ProgramPath(), programBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.lock != unlocked {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				lock, err := durable.AcquireLock(ctx, layout.ServeLockPath())
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				if tt.lock == released {
					if err := lock.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Cleanup(func() { _ = lock.Close() })
				}
			}
			server := tt.setup(t, socket)
			before := snapshot(t, root)

			start := time.Now()
			ctl, err := Reach(context.Background(), layout, func(socket string) cleanup.Control { return daemon.Dial(socket) }, 100*time.Millisecond)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("Reach() took %s, want it bounded by its 100ms hello", elapsed)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reach() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				report, err := ctl.Status(context.Background(), cleanup.Query{})
				if err != nil {
					t.Fatalf("Status() error = %v", err)
				}
				if report.Version != "v0.0.1" || report.PID != 7 || !report.Paused {
					t.Errorf("Status() = %+v, want the v0.0.1 daemon's paused report", report)
				}
			} else {
				if ctl != nil {
					t.Errorf("Reach() control = %v, want nil beside an error", ctl)
				}
				if !strings.Contains(err.Error(), tt.wantText) {
					t.Errorf("Reach() error = %q, want it to contain %q", err, tt.wantText)
				}
			}
			if lock := layout.ServeLockPath(); tt.lock == held && !strings.Contains(err.Error(), "a daemon holds the serve lock "+lock) {
				t.Errorf("Reach() error = %q, want it to name the held serve lock %s", err, lock)
			}
			if server != nil {
				if got := server.recorded(); !slices.Equal(got, tt.wantOps) {
					t.Errorf("daemon saw ops %q, want %q", got, tt.wantOps)
				}
			}
			if after := snapshot(t, root); !reflect.DeepEqual(after, before) {
				t.Errorf("layout changed from %v to %v, want nothing installed, locked, or created", before, after)
			}
			if got, err := os.ReadFile(layout.ProgramPath()); err != nil || !bytes.Equal(got, programBytes) {
				t.Errorf("program copy = %q, %v; want the running daemon's bytes untouched", got, err)
			}
		})
	}
}
