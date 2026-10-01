package cleanupwatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/render"
)

type scripted map[string]struct {
	out []byte
	err error
}

func (s scripted) Run(_ context.Context, dir render.Dir, name string, args ...string) (Output, error) {
	if dir != render.Ambient {
		return Output{}, errors.New(name + " runs in " + string(dir) + ", want the caller's cwd")
	}
	r, ok := s[name+" "+strings.Join(args, " ")]
	if !ok {
		return Output{}, errors.New("unscripted: " + name + " " + strings.Join(args, " "))
	}
	return Output{Stdout: r.out}, r.err
}

const psOut = `    1 Sun Sep 27 13:23:31 2026     /sbin/launchd
  101 Wed Sep 30 00:20:59 2026     /opt/homebrew/opt/git/libexec/git-core/git fsmonitor--daemon run --detach --ipc-threads=8
  102 Wed Sep 30 00:21:00 2026     /opt/homebrew/bin/git status
  103 Wed Sep 30 00:21:01 2026     git fsmonitor--daemon run
  104 Wed Sep 30 00:21:02 2026     /bin/zsh -c git fsmonitor--daemon run
  105 Wed Sep 30 00:21:03 2026     /opt/homebrew/bin/git fsmonitor--daemon status
  106 Wed Sep  3 09:05:07 2026     /opt/homebrew/bin/git -C /Users/me/wt fsmonitor--daemon run
  107 Wed Sep 30 00:21:05 2026     /opt/homebrew/bin/git fsmonitor--daemon run --detach
  108 Wed Sep 30 00:21:06 2026     /opt/homebrew/bin/git fsmonitor--daemon run --detach
  109 Wed Sep 30 00:21:07 2026     /opt/homebrew/bin/git fsmonitor--daemon run --detach
`

const lsofOut = `p101
fcwd
tDIR
n/Users/me
ftxt
tREG
n/opt/homebrew/Cellar/git/2.56.0/bin/git
f3
tunix
n->0x48d040701cc5092b
f4
tunix
n/Users/me/Code/repo/.git/worktrees/a/fsmonitor--daemon.ipc
p106
fcwd
tDIR
n/Users/me/wt
f5
tunix
n.git/fsmonitor--daemon.ipc
`

func TestProcTableFSMonitorDaemons(t *testing.T) {
	run := scripted{
		"ps -axo pid=,lstart=,command=":           {out: []byte(psOut)},
		"lsof -n -P -w -F ftn -p 101,103,106,107": {out: []byte(lsofOut), err: &ExitError{Command: "lsof", Code: 1}},
		"ps -o pid=,lstart=,command= -p 103,107":  {out: []byte("  107 Wed Sep 30 00:21:05 2026     /opt/homebrew/bin/git fsmonitor--daemon run --detach\n")},
	}
	want := []Process{{
		PID:     101,
		Start:   "Wed Sep 30 00:20:59 2026",
		Command: "/opt/homebrew/opt/git/libexec/git-core/git fsmonitor--daemon run --detach --ipc-threads=8",
		CWD:     "/Users/me",
		Sockets: []string{"/Users/me/Code/repo/.git/worktrees/a/fsmonitor--daemon.ipc"},
	}, {
		PID:     106,
		Start:   "Wed Sep 3 09:05:07 2026",
		Command: "/opt/homebrew/bin/git -C /Users/me/wt fsmonitor--daemon run",
		CWD:     "/Users/me/wt",
		Sockets: []string{".git/fsmonitor--daemon.ipc"},
	}, {
		PID:     107,
		Start:   "Wed Sep 30 00:21:05 2026",
		Command: "/opt/homebrew/bin/git fsmonitor--daemon run --detach",
	}, {
		PID:        108,
		Start:      "Wed Sep 30 00:21:06 2026",
		Command:    "/opt/homebrew/bin/git fsmonitor--daemon run --detach",
		Unexamined: true,
	}, {
		PID:        109,
		Start:      "Wed Sep 30 00:21:07 2026",
		Command:    "/opt/homebrew/bin/git fsmonitor--daemon run --detach",
		Unexamined: true,
	}}
	got, err := ProcTable{Run: run, Timeout: time.Second, MaxDaemons: 4}.FSMonitorDaemons(context.Background())
	if err != nil {
		t.Fatalf("FSMonitorDaemons: %v", err)
	}
	if !slices.EqualFunc(got, want, func(a, b Process) bool {
		return a.PID == b.PID && a.Start == b.Start && a.Command == b.Command && a.CWD == b.CWD &&
			a.Unexamined == b.Unexamined && slices.Equal(a.Sockets, b.Sockets)
	}) {
		t.Errorf("FSMonitorDaemons =\n%+v\nwant\n%+v", got, want)
	}
}

func TestProcTableFailures(t *testing.T) {
	tests := []struct {
		name string
		run  scripted
		want []int
		err  string
	}{
		{
			name: "lsof failed outright",
			run: scripted{
				"ps -axo pid=,lstart=,command=": {out: []byte("  103 Wed Sep 30 00:21:01 2026     git fsmonitor--daemon run\n")},
				"lsof -n -P -w -F ftn -p 103":   {err: &ExitError{Command: "lsof", Code: 2}},
			},
			err: "list fsmonitor sockets",
		},
		{
			name: "a pid reused between ps and lsof is dropped",
			run: scripted{
				"ps -axo pid=,lstart=,command=":      {out: []byte("  103 Wed Sep 30 00:21:01 2026     git fsmonitor--daemon run\n")},
				"lsof -n -P -w -F ftn -p 103":        {err: &ExitError{Command: "lsof", Code: 1}},
				"ps -o pid=,lstart=,command= -p 103": {out: []byte("  103 Wed Sep 30 09:00:00 2026     /usr/bin/vim\n")},
			},
		},
		{
			name: "a live daemon lsof could not read stays, socketless",
			run: scripted{
				"ps -axo pid=,lstart=,command=":      {out: []byte("  103 Wed Sep 30 00:21:01 2026     git fsmonitor--daemon run\n")},
				"lsof -n -P -w -F ftn -p 103":        {err: &ExitError{Command: "lsof", Code: 1}},
				"ps -o pid=,lstart=,command= -p 103": {out: []byte("  103 Wed Sep 30 00:21:01 2026     git fsmonitor--daemon run\n")},
			},
			want: []int{103},
		},
		{
			name: "no daemons skips lsof",
			run:  scripted{"ps -axo pid=,lstart=,command=": {out: []byte("    1 Sun Sep 27 13:23:31 2026     /sbin/launchd\n")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProcTable{Run: tt.run, Timeout: time.Second, MaxDaemons: 4}.FSMonitorDaemons(context.Background())
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("FSMonitorDaemons error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FSMonitorDaemons: %v", err)
			}
			var pids []int
			for _, p := range got {
				if len(p.Sockets) != 0 {
					t.Errorf("daemon %d has sockets %q, want none read", p.PID, p.Sockets)
				}
				pids = append(pids, p.PID)
			}
			if !slices.Equal(pids, tt.want) {
				t.Errorf("daemons = %v, want %v", pids, tt.want)
			}
		})
	}
}

func TestProcTableProcesses(t *testing.T) {
	tests := []struct {
		name string
		run  scripted
		pids []int
		want map[int]Process
	}{
		{
			name: "some alive",
			run:  scripted{"ps -o pid=,lstart=,command= -p 7,8": {out: []byte("    7 Wed Sep 30 00:00:07 2026     relay --watch\n")}},
			pids: []int{7, 8},
			want: map[int]Process{7: {PID: 7, Start: "Wed Sep 30 00:00:07 2026", Command: "relay --watch"}},
		},
		{
			name: "none alive",
			run:  scripted{"ps -o pid=,lstart=,command= -p 8": {err: &ExitError{Command: "ps", Code: 1}}},
			pids: []int{8},
			want: map[int]Process{},
		},
		{
			name: "no pids runs nothing",
			run:  scripted{},
			want: map[int]Process{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProcTable{Run: tt.run, Timeout: time.Second}.Processes(context.Background(), tt.pids)
			if err != nil || len(got) != len(tt.want) {
				t.Fatalf("Processes = %+v, %v; want %+v", got, err, tt.want)
			}
			for pid, p := range tt.want {
				if g := got[pid]; g.PID != p.PID || g.Start != p.Start || g.Command != p.Command {
					t.Errorf("Processes[%d] = %+v, want %+v", pid, g, p)
				}
			}
		})
	}
}

type stalled struct{}

func (stalled) Run(ctx context.Context, _ render.Dir, name string, _ ...string) (Output, error) {
	<-ctx.Done()
	return Output{}, fmt.Errorf("%s: %w", name, ctx.Err())
}

func TestProcTableMarksATimedOutListing(t *testing.T) {
	table := ProcTable{Run: stalled{}, Timeout: time.Millisecond, MaxDaemons: 4}
	if _, err := table.FSMonitorDaemons(context.Background()); !errors.Is(err, cleanup.ErrUnprobed) {
		t.Errorf("FSMonitorDaemons error = %v, want cleanup.ErrUnprobed", err)
	}
	if _, err := table.Processes(context.Background(), []int{7}); !errors.Is(err, cleanup.ErrUnprobed) {
		t.Errorf("Processes error = %v, want cleanup.ErrUnprobed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := table.FSMonitorDaemons(ctx); errors.Is(err, cleanup.ErrUnprobed) || !errors.Is(err, context.Canceled) {
		t.Errorf("FSMonitorDaemons under a cancelled caller = %v, want context.Canceled alone", err)
	}
}

func TestWatchmanMarksATimedOutProbe(t *testing.T) {
	d := Deps{Run: stalled{}, Timeout: time.Millisecond}
	if _, err := d.probe(context.Background()); !errors.Is(err, cleanup.ErrUnprobed) {
		t.Errorf("probe error = %v, want cleanup.ErrUnprobed", err)
	}
}

func TestExecRunnerReportsTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ExecRunner{}.Run(ctx, render.Ambient, "sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want context.DeadlineExceeded", err)
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Errorf("Run error = %v, want no ExitError a caller could read as a verdict", err)
	}
}

func TestExecRunnerExitErrorAndPID(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), render.Ambient, "sh", "-c", "echo $$; echo err >&2; exit 3")
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 || exitErr.Stderr != "err\n" {
		t.Fatalf("Run error = %#v, want exit 3 with captured stderr", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); out.PID == 0 || got != strconv.Itoa(out.PID) {
		t.Errorf("Run = stdout %q pid %d, want the child's own pid", got, out.PID)
	}
}

func TestExecRunnerRunsInDir(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := ExecRunner{}.Run(context.Background(), render.Dir(dir), "sh", "-c", "pwd -P")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != dir {
		t.Errorf("child ran in %q, want %q", got, dir)
	}
}
