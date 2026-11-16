package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/version"
)

const (
	resumeStateEnv = "CCX_MCP_RESUME_STATE"
	pluginRootEnv  = "CLAUDE_PLUGIN_ROOT"
	orphanedMarker = ".orphaned_at"

	resumeIdle  = 5 * time.Minute
	resumePoll  = 30 * time.Second
	resumeCheck = 10 * time.Minute
)

var errResume = errors.New("mcpserver: a newer ccx is installed")

type stdio struct {
	in       *os.File
	reader   *bufio.Reader
	relay    *io.PipeWriter
	inFlight atomic.Int64
	last     atomic.Int64
	out      sync.Mutex
}

func newStdio(in *os.File) (*stdio, io.ReadCloser) {
	requests, relay := io.Pipe()
	s := &stdio{in: in, reader: bufio.NewReader(in), relay: relay}
	s.last.Store(time.Now().UnixNano())
	return s, requests
}

func (s *stdio) forward(superseded func() bool) {
	checked := time.Now()
	for {
		if s.reader.Buffered() == 0 {
			ready, err := pollReadable(s.in, resumePoll)
			if err != nil {
				_ = s.relay.CloseWithError(err)
				return
			}
			if !ready {
				if s.inFlight.Load() == 0 && time.Since(time.Unix(0, s.last.Load())) >= resumeIdle && time.Since(checked) >= resumeCheck {
					checked = time.Now()
					if superseded() {
						_ = s.relay.CloseWithError(errResume)
						return
					}
				}
				continue
			}
		}
		line, err := s.reader.ReadBytes('\n')
		if len(line) > 0 {
			s.last.Store(time.Now().UnixNano())
			s.inFlight.Add(flightDelta(line))
			if _, werr := s.relay.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			_ = s.relay.CloseWithError(err)
			return
		}
	}
}

func (s *stdio) Write(p []byte) (int, error) {
	s.out.Lock()
	defer s.out.Unlock()
	n, err := os.Stdout.Write(p)
	for _, line := range bytes.Split(p[:n], []byte("\n")) {
		s.inFlight.Add(flightDelta(line))
	}
	s.last.Store(time.Now().UnixNano())
	return n, err
}

func flightDelta(line []byte) int64 {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &msg) != nil || len(msg.ID) == 0 || string(msg.ID) == "null" {
		return 0
	}
	if msg.Method != "" {
		return 1
	}
	return -1
}

func pollReadable(f *os.File, within time.Duration) (bool, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var n int
	var polled error
	if err := conn.Control(func(fd uintptr) {
		n, polled = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, int(within.Milliseconds())) //nolint:gosec // G115: a file descriptor fits in int32.
	}); err != nil {
		return false, err
	}
	if errors.Is(polled, unix.EINTR) {
		return false, nil
	}
	return n > 0, polled
}

func newestInstall() (string, string) {
	root := os.Getenv(pluginRootEnv)
	if root == "" {
		return "", ""
	}
	siblings, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		return "", ""
	}
	best, bestDir := "", ""
	for _, entry := range siblings {
		dir := filepath.Join(filepath.Dir(root), entry.Name())
		if !entry.IsDir() || !isVersion(entry.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, orphanedMarker)); err == nil { // #nosec G703 -- a version dir Claude Code made beside CLAUDE_PLUGIN_ROOT
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, "bin", "ccx")); err != nil || info.Mode()&0o111 == 0 { // #nosec G703 -- a version dir Claude Code made beside CLAUDE_PLUGIN_ROOT
			continue
		}
		if best == "" || versionGreater(entry.Name(), best) {
			best, bestDir = entry.Name(), dir
		}
	}
	return best, bestDir
}

func superseded() bool {
	newest, _ := newestInstall()
	own := strings.TrimPrefix(version.String(), "v")
	return newest != "" && isVersion(own) && versionGreater(newest, own)
}

func isVersion(v string) bool {
	for _, part := range strings.Split(v, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func versionGreater(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func resumedState() (*mcp.ServerSessionState, error) {
	raw, ok := os.LookupEnv(resumeStateEnv)
	if !ok {
		return nil, nil
	}
	_ = os.Unsetenv(resumeStateEnv)
	state := new(mcp.ServerSessionState)
	if err := json.Unmarshal([]byte(raw), state); err != nil {
		return nil, fmt.Errorf("mcpserver: resume state: %w", err)
	}
	if state.InitializeParams != nil && state.InitializedParams == nil {
		state.InitializedParams = new(mcp.InitializedParams)
	}
	return state, nil
}

func reexec(ss *mcp.ServerSession) error {
	_, dir := newestInstall()
	if dir == "" {
		return errResume
	}
	state, err := json.Marshal(mcp.ServerSessionState{InitializeParams: ss.InitializeParams(), InitializedParams: new(mcp.InitializedParams)})
	if err != nil {
		return err
	}
	launcher := filepath.Join(dir, "bin", "ccx")
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, resumeStateEnv+"=") || strings.HasPrefix(kv, pluginRootEnv+"=")
	})
	env = append(env, resumeStateEnv+"="+string(state), pluginRootEnv+"="+dir)
	return syscall.Exec(launcher, []string{launcher, "mcp"}, env) //nolint:gosec // G204: the newest cc-context install this plugin cache holds.
}

func serveResumable(ctx context.Context, s *mcp.Server) (*mcp.ServerSession, error) {
	state, err := resumedState()
	if err != nil {
		return nil, err
	}
	carrier, requests := newStdio(os.Stdin)
	go carrier.forward(superseded)
	ss, err := s.Connect(ctx, &mcp.IOTransport{Reader: requests, Writer: nopCloser{carrier}}, &mcp.ServerSessionOptions{State: state})
	if err != nil {
		return nil, err
	}
	return ss, ss.Wait()
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
