package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanupwatch"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	watcherSelfPID   = 4242
	watcherRelayPID  = 7
	watcherServerPID = 99
	watcherOwnerPID  = 101
	watcherOwnerPS   = "Wed Sep 30 00:20:59 2026"
	watcherServerPS  = "Thu Sep 3 09:05:07 2026"
	watcherSockname  = "/nonexistent/watchman.sock"
	watcherServerCmd = "/opt/homebrew/bin/watchman --foreground --logfile=/nonexistent/log --sockname=" + watcherSockname
	watchDelCall     = "watchman --no-spawn --no-pretty watch-del "
	fsmonitorStop    = "fsmonitor--daemon stop"
)

var errLsofDenied = errors.New("lsof: permission denied")

var (
	watcherServer = cleanup.ProcessID{PID: watcherServerPID, Start: time.Date(2026, time.September, 3, 9, 5, 7, 0, time.Local).Unix()}
	watcherOwner  = cleanup.ProcessID{PID: watcherOwnerPID, Start: time.Date(2026, time.September, 30, 0, 20, 59, 0, time.Local).Unix()}
)

type watcherWorld struct {
	mu         sync.Mutex
	wt         string
	roots      []string
	subscribed map[string]bool
	owner      *cleanupwatch.Process
	ownerNow   string
	server     cleanupwatch.Process
	serverGone bool
	answer     int
	socket     string
	lsofErr    error
	hook       func(call string)
	calls      []string
	guarded    []string
}

func newWatcherWorld(t *testing.T, roots ...string) *watcherWorld {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &watcherWorld{
		wt:         filepath.Join(base, "wt"),
		subscribed: map[string]bool{},
		server:     cleanupwatch.Process{PID: watcherServerPID, Start: watcherServerPS, Command: watcherServerCmd},
		answer:     watcherServerPID,
		socket:     watcherSockname,
	}
	if err := os.MkdirAll(filepath.Join(w.wt, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		path := filepath.Join(base, r)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		w.roots = append(w.roots, path)
	}
	return w
}

func (w *watcherWorld) path(rel string) string {
	return filepath.Join(filepath.Dir(w.wt), rel)
}

func (w *watcherWorld) withOwner(t *testing.T) {
	t.Helper()
	socket := filepath.Join(w.wt, ".git", "fsmonitor--daemon.ipc")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.owner = &cleanupwatch.Process{
		PID:     watcherOwnerPID,
		Start:   watcherOwnerPS,
		Command: "/opt/homebrew/opt/git/libexec/git-core/git fsmonitor--daemon run --detach --ipc-threads=8",
		CWD:     filepath.Dir(w.wt),
		Sockets: []string{socket},
	}
}

func (w *watcherWorld) deps(guard cleanupwatch.ActivityGuard) cleanupwatch.Deps {
	d := cleanupwatch.NewDeps(guard)
	d.Run, d.Procs = w, w
	d.SettleInterval = time.Millisecond
	return d
}

func (w *watcherWorld) mutated() (deleted []string, stops int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.calls {
		if root, ok := strings.CutPrefix(c, watchDelCall); ok {
			deleted = append(deleted, root)
		}
		if strings.HasSuffix(c, fsmonitorStop) {
			stops++
		}
	}
	return deleted, stops
}

func (w *watcherWorld) guard(ctx context.Context, path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.guarded = append(w.guarded, path)
	retiring := cleanup.RetiringFrom(ctx)
	var holders []cleanup.Holder
	for _, r := range w.roots {
		if (r == path || strings.HasPrefix(r, path+"/")) && !slices.Contains(retiring, watcherServer) {
			holders = append(holders, cleanup.Holder{PID: watcherServerPID, Name: "watchman", Evidence: cleanup.EvidenceFD, Path: r})
		}
	}
	if w.owner != nil && path == w.wt && !slices.Contains(retiring, watcherOwner) {
		holders = append(holders, cleanup.Holder{PID: watcherOwnerPID, Name: "git", Evidence: cleanup.EvidenceFD, Path: w.wt})
	}
	if len(holders) > 0 {
		return &cleanup.ActiveError{Worktree: path, Holders: holders}
	}
	return nil
}

func (w *watcherWorld) Run(_ context.Context, dir render.Dir, name string, args ...string) (cleanupwatch.Output, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	w.calls = append(w.calls, line)
	if w.hook != nil {
		w.hook(line)
	}
	inWorktree := name == "git" && dir != render.Ambient
	switch {
	case inWorktree && line == "git rev-parse --absolute-git-dir --show-toplevel":
		return cleanupwatch.Output{Stdout: []byte(string(dir) + "/.git\n" + string(dir) + "\n")}, nil
	case inWorktree && line == "git config --get fsmonitor.socketDir":
		return cleanupwatch.Output{}, &cleanupwatch.ExitError{Command: line, Code: 1}
	case inWorktree && line == "git config -z --show-scope --show-origin --get-all core.fsmonitor":
		return cleanupwatch.Output{Stdout: []byte("worktree\x00file:.git/config.worktree\x00true\x00")}, nil
	case dir != render.Ambient:
		return cleanupwatch.Output{}, fmt.Errorf("unscripted in %s: %s", dir, line)
	case name == "watchman" && len(args) > 2 && args[0] == "--no-spawn" && args[1] == "--no-pretty":
		return w.watchman(args[2:])
	case name == "git" && strings.HasSuffix(line, fsmonitorStop):
		w.owner = nil
		return cleanupwatch.Output{}, nil
	case name == "git" && strings.HasSuffix(line, "fsmonitor--daemon status") && w.owner == nil:
		return cleanupwatch.Output{}, &cleanupwatch.ExitError{Command: line, Code: 1}
	case name == "lsof" && line == fmt.Sprintf("lsof -n -P -w -a -p %d -U -F ftn", w.server.PID):
		return w.lsof()
	}
	return cleanupwatch.Output{}, fmt.Errorf("unscripted: %s", line)
}

func (w *watcherWorld) lsof() (cleanupwatch.Output, error) {
	switch {
	case w.lsofErr != nil:
		return cleanupwatch.Output{}, w.lsofErr
	case w.serverGone || w.socket == "":
		return cleanupwatch.Output{}, &cleanupwatch.ExitError{Command: "lsof", Code: 1}
	}
	out := fmt.Sprintf("p%d\nf3\ntunix\nn->0x48d040701cc5092b\nf4\ntunix\nn%s\n", w.server.PID, w.socket)
	return cleanupwatch.Output{Stdout: []byte(out)}, nil
}

func (w *watcherWorld) watchman(args []string) (cleanupwatch.Output, error) {
	var resp any
	pid := 0
	switch args[0] {
	case "get-sockname":
		resp = map[string]string{"sockname": watcherSockname, "version": "2026.09.28.00"}
	case "get-pid":
		resp = map[string]int{"pid": w.answer}
	case "debug-status":
		roots := make([]map[string]any, 0, len(w.roots))
		for _, r := range w.roots {
			roots = append(roots, map[string]any{"path": r, "fstype": "apfs", "watcher": "fsevents", "done_initial": true})
		}
		clients := []map[string]any{{"state": "idle", "peer": map[string]any{"pid": watcherSelfPID, "name": "watchman"}}}
		if len(w.subscribed) > 0 {
			clients = append(clients, map[string]any{"state": "idle", "peer": map[string]any{"pid": watcherRelayPID, "name": "relay"}})
		}
		resp, pid = map[string]any{"roots": roots, "clients": clients}, watcherSelfPID
	case "watch-list":
		resp = map[string]any{"roots": w.roots}
	case "debug-get-subscriptions":
		subs, subscribers := []map[string]any{}, []map[string]any{}
		if w.subscribed[args[1]] {
			subs = append(subs, map[string]any{"name": "relay-sub", "client_id": 1})
			subscribers = append(subscribers, map[string]any{"info": map[string]any{"name": "relay-sub", "client": "1", "pid": watcherRelayPID}})
		}
		resp = map[string]any{"subscriptions": subs, "subscribers": subscribers}
	case "trigger-list":
		resp = map[string]any{"triggers": []any{}}
	case "get-config":
		resp = map[string]any{"config": map[string]any{}}
	case "watch-del":
		w.roots = slices.DeleteFunc(w.roots, func(r string) bool { return r == args[1] })
		resp = map[string]any{"watch-del": true, "root": args[1]}
	default:
		return cleanupwatch.Output{}, fmt.Errorf("unscripted watchman %s", args[0])
	}
	data, err := json.Marshal(resp)
	return cleanupwatch.Output{Stdout: data, PID: pid}, err
}

func (w *watcherWorld) FSMonitorDaemons(context.Context) ([]cleanupwatch.Process, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.owner == nil {
		return nil, nil
	}
	return []cleanupwatch.Process{*w.owner}, nil
}

func (w *watcherWorld) Processes(_ context.Context, pids []int) (map[int]cleanupwatch.Process, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	procs := map[int]cleanupwatch.Process{}
	for _, pid := range pids {
		switch {
		case pid == watcherRelayPID:
			procs[pid] = cleanupwatch.Process{PID: pid, Start: "Wed Sep 30 00:00:07 2026", Command: "relay --watch"}
		case pid == w.server.PID && !w.serverGone:
			procs[pid] = w.server
		case pid == watcherOwnerPID && w.owner != nil:
			owner := *w.owner
			if w.ownerNow != "" {
				owner.Start = w.ownerNow
			}
			procs[pid] = owner
		}
	}
	return procs, nil
}

type guardLog struct {
	mu       sync.Mutex
	verdicts []error
	paths    []string
}

func (g *guardLog) guard(_ context.Context, path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = append(g.paths, path)
	if i := len(g.paths) - 1; i < len(g.verdicts) {
		return g.verdicts[i]
	}
	return nil
}

func TestNewCleanupWatchersWiresTheGuard(t *testing.T) {
	held := errors.New("held")
	var asked []string
	w := newCleanupWatchers(func(_ context.Context, worktree string) error {
		asked = append(asked, worktree)
		return held
	})
	deps := w.deps
	if err := deps.Guard(t.Context(), "/wt"); !errors.Is(err, held) || !slices.Equal(asked, []string{"/wt"}) {
		t.Fatalf("deps.Guard = %v after asking %q, want the injected guard's verdict for /wt", err, asked)
	}
	deps.Guard = nil
	if want := cleanupwatch.NewDeps(nil); !reflect.DeepEqual(deps, want) {
		t.Errorf("deps = %+v, want the production boundaries %+v", deps, want)
	}
}

func TestCleanupWatchersRetiring(t *testing.T) {
	tests := []struct {
		name       string
		roots      []string
		owner      bool
		subscribed string
		serverGone bool
		ownerNow   string
		setup      func(w *watcherWorld)
		want       []cleanup.ProcessID
		wantErr    error
		wantMsg    string
	}{
		{
			name:  "watched roots and the fsmonitor owner",
			roots: []string{"wt/a", "wt/b", "other"},
			owner: true,
			want:  []cleanup.ProcessID{watcherServer, watcherOwner},
		},
		{
			name:  "a watched root without an fsmonitor owner",
			roots: []string{"wt/a"},
			want:  []cleanup.ProcessID{watcherServer},
		},
		{
			name:  "the fsmonitor owner alone",
			roots: []string{"other"},
			owner: true,
			want:  []cleanup.ProcessID{watcherOwner},
		},
		{
			name:  "roots elsewhere on the host name nothing",
			roots: []string{"other", "wt-sibling"},
		},
		{
			name:       "a subscribed root refuses",
			roots:      []string{"wt/a"},
			subscribed: "wt/a",
			wantErr:    cleanupwatch.ErrRefused,
			wantMsg:    `subscription "relay-sub" (pid 7) on <base>/wt/a`,
		},
		{
			name:       "an exited watchman server refuses",
			roots:      []string{"wt/a"},
			serverGone: true,
			wantErr:    cleanupwatch.ErrRefused,
			wantMsg:    "watcher pid 99 exited",
		},
		{
			name:     "an fsmonitor owner restarted since planning refuses",
			owner:    true,
			ownerNow: "Wed Sep 30 09:00:00 2026",
			wantErr:  cleanupwatch.ErrRefused,
			wantMsg:  "fsmonitor owner pid 101 restarted since planning",
		},
		{
			name:  "a watchman pid reused by a protected relay refuses",
			roots: []string{"wt/a"},
			setup: func(w *watcherWorld) {
				w.server = cleanupwatch.Process{PID: watcherServerPID, Start: "Wed Sep 30 12:00:00 2026", Command: "relay --watch"}
			},
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: `pid 99 answering for watchman runs "relay --watch", not a watchman server`,
		},
		{
			name:    "a watchman pid reused by a watchman client refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.server.Command = "/opt/homebrew/bin/watchman --no-spawn --no-pretty get-pid" },
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "not a watchman server",
		},
		{
			name:    "a watchman pid reused by another program refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.server.Command = "/usr/bin/vim --foreground" },
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "not a watchman server",
		},
		{
			name:  "a server command naming a protected consumer refuses",
			roots: []string{"wt/a"},
			setup: func(w *watcherWorld) {
				w.server.Command = "/opt/homebrew/bin/watchman --foreground --logfile=/nonexistent/tilt/log"
			},
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "not a watchman server",
		},
		{
			name:    "a server holding another socket refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.socket = watcherSockname + ".old" },
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "watchman server pid 99 does not hold its socket " + watcherSockname,
		},
		{
			name:    "a server holding only connected sockets refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.socket = "->0x5f9a0c1e2d3b4a55" },
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "does not hold its socket",
		},
		{
			name:    "a server with no unix sockets refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.socket = "" },
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "does not hold its socket",
		},
		{
			name:    "an unreadable socket table refuses",
			roots:   []string{"wt/a"},
			setup:   func(w *watcherWorld) { w.lsofErr = errLsofDenied },
			wantErr: errLsofDenied,
			wantMsg: "list the sockets of pid 99",
		},
		{
			name:  "a server restarted between the two get-pid answers refuses",
			roots: []string{"wt/a"},
			setup: func(w *watcherWorld) {
				w.hook = func(call string) {
					if strings.HasPrefix(call, "lsof ") {
						w.answer = 199
					}
				}
			},
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "watchman server pid 99 started " + watcherServerPS + " changed while it was identified: pid 199 answers",
		},
		{
			name:  "a server restarted under the same pid while it was identified refuses",
			roots: []string{"wt/a"},
			setup: func(w *watcherWorld) {
				w.hook = func(call string) {
					if strings.HasPrefix(call, "lsof ") {
						w.server.Start = "Thu Sep 3 10:00:00 2026"
					}
				}
			},
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "changed while it was identified: pid 99 answers, pid 99 started Thu Sep 3 10:00:00 2026",
		},
		{
			name:  "a server that exec'd another program while it was identified refuses",
			roots: []string{"wt/a"},
			setup: func(w *watcherWorld) {
				w.hook = func(call string) {
					if strings.HasPrefix(call, "lsof ") {
						w.server.Command = "/bin/sh -c relay"
					}
				}
			},
			wantErr: cleanupwatch.ErrRefused,
			wantMsg: "changed while it was identified",
		},
		{
			name:  "watchers are named in pid order",
			roots: []string{"wt/a"},
			owner: true,
			setup: func(w *watcherWorld) { w.server.PID, w.answer = 150, 150 },
			want:  []cleanup.ProcessID{watcherOwner, {PID: 150, Start: watcherServer.Start}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := newWatcherWorld(t, tt.roots...)
			if tt.owner {
				world.withOwner(t)
			}
			if tt.subscribed != "" {
				world.subscribed[world.path(tt.subscribed)] = true
			}
			world.serverGone, world.ownerNow = tt.serverGone, tt.ownerNow
			if tt.setup != nil {
				tt.setup(world)
			}
			roots := slices.Clone(world.roots)
			got, err := cleanupWatchers{deps: world.deps(world.guard)}.Retiring(t.Context(), world.wt)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Retiring = %v, %v; want error %v", got, err, tt.wantErr)
			}
			if want := strings.ReplaceAll(tt.wantMsg, "<base>", filepath.Dir(world.wt)); err != nil && !strings.Contains(err.Error(), want) {
				t.Errorf("Retiring error = %q, want it to contain %q", err, want)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Retiring = %+v, want %+v", got, tt.want)
			}
			deleted, stops := world.mutated()
			if len(world.guarded) != 0 || len(deleted) != 0 || stops != 0 || !slices.Equal(world.roots, roots) {
				t.Errorf("Retiring guarded %q, deleted %q, stopped %d times; want a read-only lookup", world.guarded, deleted, stops)
			}
		})
	}
}

func TestCleanupWatchersRetireUnderTheDiscount(t *testing.T) {
	tests := []struct {
		name        string
		discount    bool
		hook        func(w *watcherWorld) func(string)
		wantErr     error
		wantDeleted []string
		wantStopped bool
		wantAfter   string
	}{
		{
			name:        "the discount reaches every pre-mutation guard",
			discount:    true,
			wantDeleted: []string{"wt/a", "wt/b"},
			wantStopped: true,
		},
		{
			name:      "without the discount nothing is retired",
			wantErr:   cleanupwatch.ErrRefused,
			wantAfter: "watchman (pid 99) holding open <base>/wt/a",
		},
		{
			name:     "a root retained by a partial retirement blocks the undiscounted guard",
			discount: true,
			hook: func(w *watcherWorld) func(string) {
				return func(call string) {
					if strings.HasPrefix(call, watchDelCall) {
						w.subscribed[w.path("wt/b")] = true
					}
				}
			},
			wantErr:     cleanupwatch.ErrRefused,
			wantDeleted: []string{"wt/a"},
			wantAfter:   "watchman (pid 99) holding open <base>/wt/b; git (pid 101) holding open <base>/wt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := newWatcherWorld(t, "wt/a", "wt/b", "other")
			world.withOwner(t)
			adapter := cleanupWatchers{deps: world.deps(world.guard)}
			ctx := t.Context()
			if tt.discount {
				ids, err := adapter.Retiring(ctx, world.wt)
				if err != nil {
					t.Fatalf("Retiring: %v", err)
				}
				ctx = cleanup.WithRetiring(ctx, ids)
			}
			if tt.hook != nil {
				world.hook = tt.hook(world)
			}
			if err := adapter.Retire(ctx, world.wt); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Retire error = %v, want %v", err, tt.wantErr)
			}
			var wantDeleted []string
			for _, r := range tt.wantDeleted {
				wantDeleted = append(wantDeleted, world.path(r))
			}
			deleted, stops := world.mutated()
			if !slices.Equal(deleted, wantDeleted) || (stops == 1) != tt.wantStopped || (world.owner == nil) != tt.wantStopped {
				t.Errorf("Retire deleted %q and stopped %d times, want %q stopped %t", deleted, stops, wantDeleted, tt.wantStopped)
			}
			if !slices.Contains(world.roots, world.path("other")) {
				t.Errorf("roots after Retire = %q, want the host's other root kept", world.roots)
			}
			world.hook = nil
			after := world.guard(t.Context(), world.wt)
			want := strings.ReplaceAll(tt.wantAfter, "<base>", filepath.Dir(world.wt))
			if (want == "") != (after == nil) || after != nil && !strings.Contains(after.Error(), want) {
				t.Errorf("undiscounted guard after Retire = %v, want %q", after, want)
			}
		})
	}
}

func TestCleanupWatchersRetire(t *testing.T) {
	active := &cleanup.ActiveError{Worktree: "wt", Holders: []cleanup.Holder{{PID: 9, Name: "zsh", TTY: true, Evidence: cleanup.EvidenceCwd, Path: "wt"}}}
	tests := []struct {
		name        string
		subscribed  string
		verdicts    []error
		noGuard     bool
		missing     bool
		wantErr     error
		wantActive  bool
		wantMsg     string
		wantDeleted []string
		wantGuards  int
	}{
		{
			name:        "retires every nested root behind the guard",
			wantDeleted: []string{"wt/a", "wt/b"},
			wantGuards:  2,
		},
		{
			name:       "an active tree retires nothing",
			verdicts:   []error{active},
			wantErr:    cleanupwatch.ErrRefused,
			wantActive: true,
			wantMsg:    "retire the watchers of <base>/wt: watcher retirement refused: activity guard for <base>/wt: ",
			wantGuards: 1,
		},
		{
			name:        "a refusal part-way names what it already retired",
			verdicts:    []error{nil, active},
			wantErr:     cleanupwatch.ErrRefused,
			wantActive:  true,
			wantMsg:     "retire the watchers of <base>/wt after retiring watchman roots <base>/wt/a: ",
			wantDeleted: []string{"wt/a"},
			wantGuards:  2,
		},
		{
			name:       "a subscribed root refuses before the guard",
			subscribed: "wt/b",
			wantErr:    cleanupwatch.ErrRefused,
			wantMsg:    `subscription "relay-sub" (pid 7) on <base>/wt/b`,
		},
		{
			name:    "no guard authorizes nothing",
			noGuard: true,
			wantErr: cleanupwatch.ErrNoGuard,
		},
		{
			name:    "a missing worktree fails to plan",
			missing: true,
			wantErr: fs.ErrNotExist,
			wantMsg: "plan the watcher retirement of <base>/wt/gone: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := newWatcherWorld(t, "wt/a", "wt/b")
			if tt.subscribed != "" {
				world.subscribed[world.path(tt.subscribed)] = true
			}
			g := &guardLog{verdicts: tt.verdicts}
			guard := cleanupwatch.ActivityGuard(g.guard)
			if tt.noGuard {
				guard = nil
			}
			target := world.wt
			if tt.missing {
				target = filepath.Join(world.wt, "gone")
			}
			err := cleanupWatchers{deps: world.deps(guard)}.Retire(t.Context(), target)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Retire error = %v, want %v", err, tt.wantErr)
			}
			var gotActive *cleanup.ActiveError
			if errors.As(err, &gotActive) != tt.wantActive {
				t.Errorf("Retire error = %v, want the guard's ActiveError reachable: %t", err, tt.wantActive)
			}
			if want := strings.ReplaceAll(tt.wantMsg, "<base>", filepath.Dir(world.wt)); err != nil && !strings.Contains(err.Error(), want) {
				t.Errorf("Retire error = %q, want it to contain %q", err, want)
			}
			var wantDeleted []string
			for _, r := range tt.wantDeleted {
				wantDeleted = append(wantDeleted, world.path(r))
			}
			if deleted, _ := world.mutated(); !slices.Equal(deleted, wantDeleted) {
				t.Errorf("watch-del roots = %q, want %q", deleted, wantDeleted)
			}
			if len(g.paths) != tt.wantGuards || slices.ContainsFunc(g.paths, func(p string) bool { return p != world.wt }) {
				t.Errorf("guard asked about %q, want %d calls for %s", g.paths, tt.wantGuards, world.wt)
			}
		})
	}
}

func TestCleanupWatchersCheckQuarantine(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		job   string
		want  error
	}{
		{name: "outside every watched tree", roots: []string{"watched"}, job: "jobs/1"},
		{name: "inside a watched root", roots: []string{"watched"}, job: "watched/jobs/1", want: cleanupwatch.ErrWatched},
		{name: "around a watched root", roots: []string{"jobs/1/inner"}, job: "jobs/1", want: cleanupwatch.ErrWatched},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := newWatcherWorld(t, tt.roots...)
			job := world.path(tt.job)
			if err := os.MkdirAll(job, 0o700); err != nil {
				t.Fatal(err)
			}
			err := cleanupWatchers{deps: world.deps(nil)}.CheckQuarantine(t.Context(), job)
			if !errors.Is(err, tt.want) {
				t.Fatalf("CheckQuarantine = %v, want %v", err, tt.want)
			}
			if deleted, stops := world.mutated(); len(deleted) != 0 || stops != 0 {
				t.Errorf("CheckQuarantine deleted %q and stopped %d times, want a read-only check", deleted, stops)
			}
		})
	}
}

func TestCleanupWatchersCmd(t *testing.T) {
	asJSON := func(t *testing.T, r cleanupwatch.Report) string {
		t.Helper()
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	tests := []struct {
		name    string
		args    []string
		want    func(*testing.T, cleanupwatch.Report) string
		trimmed bool
	}{
		{
			name: "human report",
			want: func(_ *testing.T, r cleanupwatch.Report) string { return r.Render() },
		},
		{
			name: "json census",
			args: []string{"--json"},
			want: asJSON,
		},
		{
			name:    "budget caps the human report",
			args:    []string{"--budget", "20"},
			want:    func(_ *testing.T, r cleanupwatch.Report) string { return render.Cap(r.Render(), 20) },
			trimmed: true,
		},
		{
			name: "json ignores the budget",
			args: []string{"--json", "--budget", "20"},
			want: asJSON,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			world := newWatcherWorld(t, "wt/a", "wt/b")
			world.withOwner(t)
			world.subscribed[world.roots[1]] = true
			census, err := cleanupwatch.Take(t.Context(), world.deps(nil))
			if err != nil {
				t.Fatalf("Take: %v", err)
			}
			if !census.Watchman.Available || len(census.Watchman.Roots) != 2 || len(census.Watchman.Clients) != 2 || len(census.FSMonitor) != 1 {
				t.Fatalf("census = %+v, want 2 roots, 2 clients, and 1 fsmonitor daemon", census)
			}
			roots := slices.Clone(world.roots)
			var out bytes.Buffer
			cmd := cleanupWatchersCmd(world.deps(nil))
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tt.args)
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("watchers %q: %v", tt.args, err)
			}
			if want := tt.want(t, census); out.String() != want {
				t.Errorf("watchers %q =\n%s\nwant\n%s", tt.args, out.String(), want)
			}
			if trimmed := strings.Contains(out.String(), "re-run with a larger --budget"); trimmed != tt.trimmed {
				t.Errorf("watchers %q trimmed = %t, want %t", tt.args, trimmed, tt.trimmed)
			}
			deleted, stops := world.mutated()
			if !slices.Equal(world.roots, roots) || len(deleted) != 0 || stops != 0 || world.owner == nil {
				t.Errorf("watchers %q deleted %q and stopped %d times, want %q and the fsmonitor owner untouched", tt.args, deleted, stops, roots)
			}
		})
	}
}
