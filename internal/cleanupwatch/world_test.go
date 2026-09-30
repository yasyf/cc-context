package cleanupwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	daemonCommand = "/opt/homebrew/opt/git/libexec/git-core/git fsmonitor--daemon run --detach --ipc-threads=8"
	daemonStart   = "Wed Sep 30 00:20:59 2026"
	selfCommand   = "/opt/homebrew/bin/watchman --no-spawn --no-pretty debug-status"
)

type fakeSub struct {
	Name   string
	PID    int
	Client int
}

type fakeRoot struct {
	Queries  []map[string]any
	Subs     []fakeSub
	Triggers []map[string]any
	Config   string
	Recrawl  map[string]any
}

type fakeRepo struct {
	Admin      string
	Toplevel   string
	Socket     string
	SocketDir  string
	StatusCode int
	Config     []ConfigValue
}

type world struct {
	t        *testing.T
	base     string
	home     string
	mu       sync.Mutex
	missing  bool
	down     bool
	sock     string
	roots    map[string]*fakeRoot
	clients  []map[string]any
	commands map[int]string
	nextPID  int
	lookups  [][]int
	selfPIDs []int
	daemons  []Process
	repos    map[string]fakeRepo
	stubborn bool
	nack     map[string]bool
	sticky   map[string]bool
	calls    []string
	guarded  []string
	guardErr error
	hook     func(call string)
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	sock := filepath.Join(base, "watchman.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &world{
		t:        t,
		base:     base,
		home:     home,
		sock:     sock,
		roots:    map[string]*fakeRoot{},
		commands: map[int]string{},
		nextPID:  60000,
		repos:    map[string]fakeRepo{},
		nack:     map[string]bool{},
		sticky:   map[string]bool{},
	}
}

func (w *world) deps() Deps {
	return Deps{
		Run:   w,
		Procs: w,
		Guard: func(_ context.Context, path string) error {
			w.record("guard " + path)
			w.mu.Lock()
			w.guarded = append(w.guarded, path)
			err := w.guardErr
			w.mu.Unlock()
			return err
		},
		Protected:      DefaultProtected,
		MaxRoots:       16,
		MaxClients:     16,
		Timeout:        time.Second,
		SettleTries:    3,
		SettleInterval: time.Millisecond,
	}
}

func (w *world) dir(rel string) string {
	w.t.Helper()
	p := filepath.Join(w.base, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *world) linked(repo, name string) string {
	w.t.Helper()
	wt := w.dir(name)
	admin := w.dir(filepath.Join(repo, ".git", "worktrees", filepath.Base(name)))
	w.write(filepath.Join(admin, "gitdir"), filepath.Join(wt, ".git")+"\n")
	w.write(filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	w.repos[wt] = fakeRepo{
		Admin:  admin,
		Socket: filepath.Join(admin, ipcSocket),
		Config: []ConfigValue{
			{Scope: "global", Origin: "file:" + filepath.Join(w.home, ".gitconfig"), Value: "false"},
			{Scope: "worktree", Origin: "file:" + filepath.Join(admin, "config.worktree"), Value: "true"},
		},
	}
	return wt
}

func (w *world) mainRepo(name string) string {
	w.t.Helper()
	wt := w.dir(name)
	admin := w.dir(filepath.Join(name, ".git"))
	w.repos[wt] = fakeRepo{
		Admin:  admin,
		Socket: filepath.Join(admin, ipcSocket),
		Config: []ConfigValue{{Scope: "local", Origin: "file:" + filepath.Join(admin, "config"), Value: "true"}},
	}
	return wt
}

func (w *world) daemon(pid int, socket, cwd string) {
	w.t.Helper()
	w.write(socket, "")
	w.daemons = append(w.daemons, Process{PID: pid, Start: daemonStart, Command: daemonCommand, CWD: cwd, Sockets: []string{socket}})
}

func (w *world) write(path, content string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) watch(path string, r *fakeRoot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roots[path] = r
}

func (w *world) subscribe(root string, sub fakeSub) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.roots[root].Subs = append(w.roots[root].Subs, sub)
}

func (w *world) client(pid int, name, command string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clients = append(w.clients, map[string]any{
		"state": "waiting for request",
		"peer":  map[string]any{"pid": pid, "name": name},
		"since": 1790000000,
	})
	if command != "" {
		w.commands[pid] = command
	}
}

func (w *world) record(call string) {
	w.mu.Lock()
	w.calls = append(w.calls, call)
	hook := w.hook
	w.mu.Unlock()
	if hook != nil {
		hook(call)
	}
}

func (w *world) callsMatching(prefix string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (w *world) Run(_ context.Context, dir, name string, args ...string) (Output, error) {
	if dir != "" {
		w.t.Fatalf("%s runs in %q, want the caller's cwd", name, dir)
	}
	w.record(name + " " + strings.Join(args, " "))
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextPID++
	pid := w.nextPID
	var out []byte
	var err error
	switch name {
	case "watchman":
		out, err = w.watchman(pid, args)
	case "git":
		out, err = w.git(args)
	default:
		w.t.Fatalf("unexpected command %s %q", name, args)
	}
	return Output{Stdout: out, PID: pid}, err
}

func (w *world) FSMonitorDaemons(context.Context) ([]Process, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.daemons), nil
}

func (w *world) Processes(_ context.Context, pids []int) (map[int]Process, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(pids) > 0 {
		w.lookups = append(w.lookups, slices.Clone(pids))
	}
	procs := map[int]Process{}
	for _, pid := range pids {
		if c, ok := w.commands[pid]; ok {
			procs[pid] = Process{PID: pid, Start: daemonStart, Command: c}
		}
	}
	return procs, nil
}

func respond(v map[string]any) ([]byte, error) {
	v["version"] = "2026.07.27.00"
	return json.Marshal(v)
}

func (w *world) watchman(pid int, args []string) ([]byte, error) {
	if w.missing {
		return nil, fmt.Errorf("watchman: %w", exec.ErrNotFound)
	}
	if len(args) < 3 || args[0] != "--no-spawn" || args[1] != "--no-pretty" {
		w.t.Fatalf("watchman %q lacks --no-spawn --no-pretty", args)
	}
	cmd, rest := args[2], args[3:]
	if cmd == "get-sockname" {
		return respond(map[string]any{"sockname": w.sock, "unix_domain": w.sock})
	}
	if w.down {
		return nil, &ExitError{Command: "watchman " + cmd, Code: 1}
	}
	switch cmd {
	case "get-pid":
		return respond(map[string]any{"pid": 4242})
	case "watch-list":
		return respond(map[string]any{"roots": slices.Sorted(maps.Keys(w.roots))})
	case "debug-status":
		var statuses []map[string]any
		for _, p := range slices.Sorted(maps.Keys(w.roots)) {
			statuses = append(statuses, rootStatusJSON(p, w.roots[p]))
		}
		w.commands[pid] = selfCommand
		w.selfPIDs = append(w.selfPIDs, pid)
		self := map[string]any{"state": "dispatching command", "peer": map[string]any{"pid": pid, "name": "/opt/homebrew/bin/watchman"}}
		return respond(map[string]any{"roots": statuses, "clients": append(slices.Clone(w.clients), self)})
	case "watch":
		root := rest[0]
		config, err := os.ReadFile(filepath.Join(root, ".watchmanconfig"))
		if err != nil {
			config = []byte("{}")
		}
		w.roots[root] = &fakeRoot{Config: string(config)}
		return respond(map[string]any{"watch": root, "watcher": "fsevents"})
	}
	root := rest[0]
	r, ok := w.roots[root]
	if !ok {
		return respond(map[string]any{"error": "watchman::RootResolveError: failed to resolve root: unable to resolve root " + root + ": failed to resolve root: directory " + root + " is not watched"})
	}
	switch cmd {
	case "debug-root-status":
		return respond(map[string]any{"root_status": rootStatusJSON(root, r)})
	case "debug-get-subscriptions":
		subscribers := []map[string]any{}
		subscriptions := []map[string]any{}
		for i, s := range r.Subs {
			subscribers = append(subscribers, map[string]any{"serial": i, "info": map[string]any{
				"name": s.Name, "client": strconv.Itoa(s.Client), "stm": "0x1", "is_owner": true, "pid": s.PID,
			}})
			subscriptions = append(subscriptions, map[string]any{"name": s.Name, "client_id": s.Client, "last_responses": []any{}})
		}
		return respond(map[string]any{"items": []any{}, "next_serial": len(r.Subs), "subscribers": subscribers, "subscriptions": subscriptions})
	case "trigger-list":
		triggers := r.Triggers
		if triggers == nil {
			triggers = []map[string]any{}
		}
		return respond(map[string]any{"triggers": triggers})
	case "get-config":
		config := r.Config
		if config == "" {
			config = "{}"
		}
		return respond(map[string]any{"config": json.RawMessage(config)})
	case "watch-del":
		if w.nack[root] {
			return respond(map[string]any{"watch-del": false, "root": root})
		}
		if !w.sticky[root] {
			delete(w.roots, root)
		}
		return respond(map[string]any{"watch-del": true, "root": root})
	}
	w.t.Fatalf("unexpected watchman command %q", args)
	return nil, nil
}

func rootStatusJSON(path string, r *fakeRoot) map[string]any {
	recrawl := r.Recrawl
	if recrawl == nil {
		recrawl = map[string]any{"count": 0, "should-recrawl": false, "warning": nil, "reason": "startup", "started": -3000, "completed": -2500, "stats": nil}
	}
	queries := r.Queries
	if queries == nil {
		queries = []map[string]any{}
	}
	return map[string]any{
		"path": path, "fstype": "apfs", "watcher": "fsevents", "uptime": 60, "case_sensitive": false,
		"done_initial": true, "cancelled": false, "crawl-status": "crawl completed", "enable_parallel_crawl": false,
		"cookie_dir": []string{path}, "cookie_prefix": []string{path + "/.watchman-cookie-"}, "cookie_list": []string{},
		"recrawl_info": recrawl, "queries": queries,
	}
}

func (w *world) git(args []string) ([]byte, error) {
	exit := func(code int) ([]byte, error) {
		return nil, &ExitError{Command: "git " + strings.Join(args, " "), Code: code, Stderr: "fatal: not a git repository"}
	}
	if gitDir, ok := strings.CutPrefix(args[0], "--git-dir="); ok {
		wt, _ := strings.CutPrefix(args[1], "--work-tree=")
		repo, ok := w.repos[wt]
		if !ok || repo.Admin != gitDir || len(args) != 4 || args[2] != "fsmonitor--daemon" {
			w.t.Fatalf("git %q does not name a known worktree's git dir", args)
		}
		serving := func(p Process) bool { return slices.Contains(p.Sockets, repo.Socket) }
		switch args[3] {
		case "stop":
			i := slices.IndexFunc(w.daemons, serving)
			if i < 0 {
				return exit(1)
			}
			if !w.stubborn {
				w.daemons = slices.Delete(w.daemons, i, i+1)
			}
			return nil, nil
		case "status":
			if repo.StatusCode != 0 {
				return exit(repo.StatusCode)
			}
			if slices.ContainsFunc(w.daemons, serving) {
				return []byte("fsmonitor-daemon is watching '" + wt + "'\n"), nil
			}
			return exit(1)
		}
	}
	if len(args) < 3 || args[0] != "-C" {
		w.t.Fatalf("git %q runs without -C", args)
	}
	wt, rest := args[1], strings.Join(args[2:], " ")
	repo, ok := w.repos[wt]
	switch rest {
	case "rev-parse --absolute-git-dir --show-toplevel":
		if !ok {
			return exit(128)
		}
		top := repo.Toplevel
		if top == "" {
			top = wt
		}
		return []byte(repo.Admin + "\n" + top + "\n"), nil
	case "config -z --show-scope --show-origin --get-all core.fsmonitor":
		if len(repo.Config) == 0 {
			return exit(1)
		}
		var b strings.Builder
		for _, c := range repo.Config {
			b.WriteString(c.Scope + "\x00" + c.Origin + "\x00" + c.Value + "\x00")
		}
		return []byte(b.String()), nil
	case "config --get fsmonitor.socketDir":
		if repo.SocketDir == "" {
			return exit(1)
		}
		return []byte(repo.SocketDir + "\n"), nil
	}
	w.t.Fatalf("unexpected git %q", args)
	return nil, nil
}
