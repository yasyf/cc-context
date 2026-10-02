package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/daemon"
)

const standInSocketEnv = "CCX_AGENT_STAND_IN_DAEMON"

type standIn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	once  sync.Once
	ops   []string
}

func TestStandInDaemonProcess(t *testing.T) {
	socket := os.Getenv(standInSocketEnv)
	if socket == "" {
		t.Skip("runs only as the stand-in daemon process another test starts")
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	hello := fmt.Sprintf(`{"info":{"version":"v1.2.2","protocol":%d,"pid":%d}}`, cleanup.Protocol, os.Getpid())
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			var req struct {
				Op string `json:"op"`
			}
			if line, err := bufio.NewReader(conn).ReadBytes('\n'); err == nil && json.Unmarshal(line, &req) == nil {
				fmt.Printf("op %s\n", req.Op)
				if req.Op == "hello" {
					_, _ = conn.Write([]byte(hello + "\n"))
				}
			}
			_ = conn.Close()
		}
	}()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = l.Close()
}

func startStandIn(t *testing.T, socket string) *standIn {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStandInDaemonProcess$", "-test.count=1") //nolint:gosec // re-executes this test binary as a stand-in daemon
	cmd.Env = append(os.Environ(), standInSocketEnv+"="+socket)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &standIn{cmd: cmd, stdin: stdin, lines: make(chan string, 64)}
	go func() {
		defer close(d.lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			d.lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() { d.stop(t) })
	for line := range d.lines {
		if line == "ready" {
			return d
		}
	}
	t.Fatal("the stand-in daemon exited before it listened")
	return nil
}

func (d *standIn) stop(t *testing.T) []string {
	t.Helper()
	d.once.Do(func() {
		_ = d.stdin.Close()
		for line := range d.lines {
			if op, ok := strings.CutPrefix(line, "op "); ok {
				d.ops = append(d.ops, op)
			}
		}
		if err := d.cmd.Wait(); err != nil {
			t.Errorf("stand-in daemon pid %d: %v", d.cmd.Process.Pid, err)
		}
	})
	return d.ops
}

func TestConnectNeverStopsADaemonThatReplacedTheObservedOne(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "ccxs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	layout := cleanup.Layout{Root: filepath.Join(root, "state")}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	socket, err := layout.Socket()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "ccx")
	if err := os.WriteFile(source, sourceBytes, 0o755); err != nil { //nolint:gosec // the fixture stands in for an installed executable
		t.Fatal(err)
	}
	observed := startStandIn(t, socket)
	var (
		observedOps []string
		successor   *standIn
		applied     int
	)
	s := starter{
		Options: Options{
			Layout:  layout,
			Version: "v1.2.3",
			Source:  source,
			Dial:    func(socket string) cleanup.Control { return daemon.Dial(socket) },
			Timeout: 5 * time.Second,
		},
		apply: func(context.Context) error {
			applied++
			return nil
		},
		alive: processAlive,
		held:  serveLockHeld,
	}
	s.born = func(pid int) (time.Time, error) {
		start, err := processStart(pid)
		if successor == nil {
			if pid != observed.cmd.Process.Pid {
				t.Errorf("recorded the start of pid %d, want the observed daemon's pid %d", pid, observed.cmd.Process.Pid)
			}
			observedOps = observed.stop(t)
			successor = startStandIn(t, socket)
		}
		return start, err
	}

	ctl, err := s.connect(t.Context())
	if ctl != nil || !errors.Is(err, errStartedElsewhere) || !errors.Is(err, errOutdatedAfterStart) {
		t.Fatalf("connect() = %v, %v; want nil and an error wrapping %v and %v", ctl, err, errStartedElsewhere, errOutdatedAfterStart)
	}
	if want := fmt.Sprintf("pid %d (uid %d) serves the socket, not the outdated daemon (pid %d, uid %d); it is left running", successor.cmd.Process.Pid, os.Getuid(), observed.cmd.Process.Pid, os.Getuid()); !strings.Contains(err.Error(), want) {
		t.Errorf("connect() error = %q, want it to contain %q", err, want)
	}
	if !processAlive(successor.cmd.Process.Pid) {
		t.Errorf("the daemon that replaced the observed one (pid %d) is gone, want it left running", successor.cmd.Process.Pid)
	}
	if got, want := observedOps, []string{"hello", "hello"}; !slices.Equal(got, want) {
		t.Errorf("observed daemon received %q, want %q: the probe and the probe under the start lock", got, want)
	}
	ops := successor.stop(t)
	if len(ops) == 0 {
		t.Errorf("the replacing daemon received nothing, want readiness hellos")
	}
	for _, op := range ops {
		if op != "hello" {
			t.Errorf("the replacing daemon received %q, want only readiness hellos and never a shutdown", op)
		}
	}
	if applied != 0 {
		t.Errorf("apply ran %d times, want 0", applied)
	}
	if _, err := os.Lstat(layout.ProgramPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("program copy stat error = %v, want nothing installed", err)
	}
}
