package cleanupwatch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTakeTreatsAbsentWatchmanAsAValidState(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, w *world)
		absent string
	}{
		{
			name:   "not installed",
			setup:  func(_ *testing.T, w *world) { w.missing = true },
			absent: "watchman is not installed",
		},
		{
			name: "no socket",
			setup: func(t *testing.T, w *world) {
				w.down = true
				if err := os.Remove(w.sock); err != nil {
					t.Fatal(err)
				}
			},
			absent: "watchman server is not running",
		},
		{
			name: "stale socket",
			setup: func(t *testing.T, w *world) {
				w.down = true
				w.sock = staleSocket(t)
			},
			absent: "watchman server is not running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			wt := w.linked("repo", "wt")
			w.daemon(101, w.repos[wt].Socket, w.home)
			tt.setup(t, w)

			r, err := Take(context.Background(), w.deps())
			if err != nil {
				t.Fatalf("Take: %v", err)
			}
			if r.Watchman.Available || r.Watchman.Absent != tt.absent {
				t.Errorf("Watchman = available %t absent %q, want absent %q", r.Watchman.Available, r.Watchman.Absent, tt.absent)
			}
			if len(r.FSMonitor) != 1 || r.FSMonitor[0].Worktree != wt {
				t.Errorf("FSMonitor = %+v, want the daemon for %s", r.FSMonitor, wt)
			}
		})
	}
}

func TestTakeFailsWhenALiveServerDoesNotAnswer(t *testing.T) {
	w := newWorld(t)
	w.down = true
	w.sock = liveSocket(t)

	_, err := Take(context.Background(), w.deps())
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Take error = %v, want the get-pid failure", err)
	}
}

func staleSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return path
}

func TestTakeReportsEveryRootsConsumersConfigAndCrawl(t *testing.T) {
	w := newWorld(t)
	idle := w.dir("idle")
	busy := w.dir("busy")
	triggered := w.dir("triggered")
	queried := w.dir("queried")
	stale := w.dir("stale")
	gone := filepath.Join(w.base, "gone")
	w.write(filepath.Join(stale, ".watchmanconfig"), `{"ignore_dirs": ["node_modules", ".yarn/cache"], "idle_reap_age_seconds": 172800}`)
	w.write(filepath.Join(idle, ".watchmanconfig"), `{"settle": 200}`)
	w.watch(idle, &fakeRoot{Config: `{"settle":200}`})
	w.watch(triggered, &fakeRoot{Triggers: []map[string]any{{"name": "build", "command": []string{"tilt", "trigger"}}}})
	w.watch(queried, &fakeRoot{Queries: []map[string]any{{"state": "Generating", "client-pid": 502, "elapsed-milliseconds": 40}}})
	w.watch(busy, &fakeRoot{
		Subs: []fakeSub{{Name: "relay", PID: 501, Client: 7}},
		Recrawl: map[string]any{
			"count": 20, "should-recrawl": false, "warning": "Recrawled this watch 19 times", "reason": "MustScanSubDirs UserDropped",
			"started": -10000, "completed": -2460,
		},
	})
	w.watch(stale, &fakeRoot{Config: `{"ignore_dirs": ["node_modules"]}`})
	w.watch(gone, &fakeRoot{Recrawl: map[string]any{"count": 0, "reason": "startup", "started": -500}})
	w.client(501, "relay", "/usr/local/bin/relay --watch")
	w.client(503, "watchman", "")

	r, err := Take(context.Background(), w.deps())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	byPath := map[string]Root{}
	for _, root := range r.Watchman.Roots {
		byPath[root.Path] = root
	}
	tests := []struct {
		path     string
		verdict  string
		stale    bool
		recrawls int
		reason   string
		crawlMS  int64
		crawling bool
		missing  bool
		subs     int
		triggers int
		queries  int
	}{
		{path: idle, verdict: VerdictRetirable, reason: "startup", crawlMS: 500},
		{path: busy, verdict: VerdictBusy, recrawls: 20, reason: "MustScanSubDirs UserDropped", crawlMS: 7540, subs: 1},
		{path: triggered, verdict: VerdictBusy, reason: "startup", crawlMS: 500, triggers: 1},
		{path: queried, verdict: VerdictBusy, reason: "startup", crawlMS: 500, queries: 1},
		{path: stale, verdict: VerdictRetirable, stale: true, reason: "startup", crawlMS: 500},
		{path: gone, verdict: VerdictMissing, reason: "startup", crawling: true, missing: true},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.path), func(t *testing.T) {
			got, ok := byPath[tt.path]
			if !ok {
				t.Fatalf("no root %s in %+v", tt.path, r.Watchman.Roots)
			}
			if got.Verdict != tt.verdict || got.StaleConfig != tt.stale || got.Recrawls != tt.recrawls ||
				got.RecrawlReason != tt.reason || got.CrawlMS != tt.crawlMS || got.Crawling != tt.crawling || got.Missing != tt.missing ||
				len(got.Subscriptions) != tt.subs || len(got.Triggers) != tt.triggers || len(got.Queries) != tt.queries {
				t.Errorf("root = %+v, want %+v", got, tt)
			}
		})
	}
	if got := byPath[busy].Subscriptions[0]; got != (Subscription{Name: "relay", PID: 501, Client: "7"}) {
		t.Errorf("subscription = %+v, want relay pid 501 client 7", got)
	}
	if got := byPath[triggered].Triggers[0]; got.Name != "build" || !slices.Equal(got.Command, []string{"tilt", "trigger"}) {
		t.Errorf("trigger = %+v", got)
	}
	if got := byPath[queried].Queries[0]; got != (Query{PID: 502, State: "Generating", ElapsedMS: 40}) {
		t.Errorf("query = %+v", got)
	}
	want := []Client{
		{PID: 501, Name: "relay", State: "waiting for request", Command: "/usr/local/bin/relay --watch", Protected: true, Roots: []string{busy}},
		{PID: 503, Name: "watchman", State: "waiting for request", Gone: true},
		{PID: w.selfPIDs[len(w.selfPIDs)-1], Name: "/opt/homebrew/bin/watchman", State: "dispatching command", Self: true},
	}
	if !slices.EqualFunc(r.Watchman.Clients, want, func(a, b Client) bool {
		return a.PID == b.PID && a.Name == b.Name && a.Command == b.Command && a.Protected == b.Protected &&
			a.Gone == b.Gone && a.Self == b.Self && slices.Equal(a.Roots, b.Roots)
	}) {
		t.Errorf("clients = %+v, want %+v", r.Watchman.Clients, want)
	}
	if len(w.callsMatching("watchman --no-spawn --no-pretty watch ")) != 0 || len(w.callsMatching("watchman --no-spawn --no-pretty watch-del")) != 0 {
		t.Errorf("census mutated watchman: %q", w.calls)
	}
}

func TestTakeBoundsTheRootsItInspects(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		w.watch(w.dir(name), &fakeRoot{})
	}
	d := w.deps()
	d.MaxRoots = 2

	r, err := Take(context.Background(), d)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	var examined []string
	for _, root := range r.Watchman.Roots {
		examined = append(examined, filepath.Base(root.Path))
	}
	var skipped []string
	for _, p := range r.Watchman.Unexamined {
		skipped = append(skipped, filepath.Base(p))
	}
	if !slices.Equal(examined, []string{"a", "b"}) || !slices.Equal(skipped, []string{"c", "d"}) {
		t.Errorf("examined %q unexamined %q, want [a b] and [c d]", examined, skipped)
	}
	if got := len(w.callsMatching("watchman --no-spawn --no-pretty get-config")); got != 2 {
		t.Errorf("get-config ran %d times, want 2", got)
	}
	if !strings.Contains(r.Render(), "2 roots not examined (census bounded at 2)") {
		t.Errorf("Render omits the bound:\n%s", r.Render())
	}
}

func TestTakeBoundsClientLookups(t *testing.T) {
	w := newWorld(t)
	w.watch(w.dir("a"), &fakeRoot{})
	for _, pid := range []int{701, 701, 702, 703, 704, 705} {
		w.client(pid, "node", "node "+strconv.Itoa(pid)+".js")
	}
	d := w.deps()
	d.MaxRoots = 1
	d.MaxClients = 2

	r, err := Take(context.Background(), d)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !slices.EqualFunc(w.lookups, [][]int{{701, 702}}, slices.Equal) {
		t.Errorf("pid lookups = %v, want one batch of the first 2 distinct pids", w.lookups)
	}
	var examined, unexamined []int
	for _, c := range r.Watchman.Clients {
		switch {
		case c.Self:
		case c.Unexamined:
			unexamined = append(unexamined, c.PID)
		default:
			examined = append(examined, c.PID)
		}
	}
	if !slices.Equal(examined, []int{701, 701, 702}) || !slices.Equal(unexamined, []int{703, 704, 705}) || r.Watchman.ClientBound != 2 {
		t.Errorf("examined %v unexamined %v bound %d, want [701 701 702], [703 704 705], 2", examined, unexamined, r.Watchman.ClientBound)
	}
	if got := strings.Count(r.Render(), "not examined (bounded at 2 pids)"); got != 3 {
		t.Errorf("Render names %d unexamined clients, want 3:\n%s", got, r.Render())
	}
}

func TestTakeParsesLiveWatchmanShapes(t *testing.T) {
	const status = `{"clients":[{"state":"dispatching command","peer":{"pid":84210,"name":"/opt/homebrew/bin/watchman"},"since":1790753060}],"roots":[{"fstype":"apfs","uptime":197905,"case_sensitive":false,"path":"/Users/yasyf/Code/monorepo","enable_parallel_crawl":false,"recrawl_info":{"count":20,"should-recrawl":false,"completed":-197891186,"stats":null,"started":-197905467,"warning":"Recrawled this watch 19 times, most recently because:\nMustScanSubDirs UserDropped","reason":"MustScanSubDirs UserDropped"},"watcher":"fsevents","cookie_dir":["/Users/yasyf/Code/monorepo"],"cookie_prefix":["/Users/yasyf/Code/monorepo/.watchman-cookie-host-8934-"],"done_initial":true,"cookie_list":[],"queries":[],"cancelled":false,"crawl-status":"crawl completed 197891186ms ago, and took 14281ms"}],"version":"2026.07.27.00"}`
	var s watchmanStatus
	if err := json.Unmarshal([]byte(status), &s); err != nil {
		t.Fatal(err)
	}
	root := rootFromStatus(s.Roots[0])
	if root.Recrawls != 20 || root.RecrawlReason != "MustScanSubDirs UserDropped" || root.CrawlMS != 14281 || root.Crawling ||
		!strings.HasPrefix(root.RecrawlWarning, "Recrawled this watch 19 times") {
		t.Errorf("root = %+v, want 20 recrawls, crawl 14281ms", root)
	}
	s.SelfPID = 84210
	w := newWorld(t)
	clients, err := w.deps().inspectClients(context.Background(), s)
	if err != nil || len(clients) != 1 || !clients[0].Self || clients[0].PID != 84210 || len(w.lookups) != 0 {
		t.Errorf("clients = %+v, %v, lookups %v; want the census's own request exempt by pid without a lookup", clients, err, w.lookups)
	}
	const subs = `{"version":"2026.07.27.00","items":[],"subscribers":[{"serial":0,"info":{"name":"relay","client":"12","stm":"0x6000","is_owner":true,"pid":777,"query":{"fields":["name"]}}}],"next_serial":1,"subscriptions":[{"name":"relay","client_id":12,"last_responses":[]}]}`
	var raw rawSubscriptions
	if err := json.Unmarshal([]byte(subs), &raw); err != nil {
		t.Fatal(err)
	}
	if got := mergeSubscriptions(raw); !slices.Equal(got, []Subscription{{Name: "relay", PID: 777, Client: "12"}}) {
		t.Errorf("subscriptions = %+v, want relay pid 777 client 12", got)
	}
}

func TestWatchmanErrorResponsesFailEvenOnExitZero(t *testing.T) {
	w := newWorld(t)
	_, err := w.deps().consumers(context.Background(), filepath.Join(w.base, "unwatched"))
	var werr *WatchmanError
	if !errors.As(err, &werr) || werr.Command != "debug-get-subscriptions" || !strings.Contains(werr.Message, "is not watched") {
		t.Fatalf("consumers error = %v, want a not-watched WatchmanError", err)
	}
}

func TestTakeIdentifiesFSMonitorDaemonsBySocketNotCWD(t *testing.T) {
	w := newWorld(t)
	linked := w.linked("repo", "linked")
	main := w.mainRepo("main")
	orphanAdmin := w.dir("repo/.git/worktrees/deleted")
	w.write(filepath.Join(orphanAdmin, "gitdir"), filepath.Join(w.base, "deleted", ".git")+"\n")
	w.daemon(101, w.repos[linked].Socket, w.home)
	w.daemon(102, w.repos[main].Socket, w.home)
	w.daemon(103, filepath.Join(orphanAdmin, ipcSocket), w.home)
	w.daemons = append(w.daemons,
		Process{PID: 104, Command: daemonCommand, CWD: linked},
		Process{PID: 105, Command: daemonCommand, CWD: w.home, Sockets: []string{filepath.Join(w.home, hashedSocket("/elsewhere"))}},
	)
	w.write(filepath.Join(w.home, hashedSocket("/elsewhere")), "")
	w.missing = true

	r, err := Take(context.Background(), w.deps())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	tests := []struct {
		pid      int
		verdict  string
		worktree string
		config   string
	}{
		{101, VerdictRetirable, linked, "true"},
		{102, VerdictRetirable, main, "true"},
		{103, VerdictOrphan, filepath.Join(w.base, "deleted"), ""},
		{104, VerdictUnresolved, "", ""},
		{105, VerdictUnresolved, "", ""},
	}
	if len(r.FSMonitor) != len(tests) {
		t.Fatalf("FSMonitor = %+v, want %d daemons", r.FSMonitor, len(tests))
	}
	for i, tt := range tests {
		got := r.FSMonitor[i]
		var effective string
		if n := len(got.Config); n > 0 {
			effective = got.Config[n-1].Value
		}
		if got.PID != tt.pid || got.Verdict != tt.verdict || got.Worktree != tt.worktree || effective != tt.config {
			t.Errorf("daemon %d = %+v, want verdict %s worktree %q config %q", tt.pid, got, tt.verdict, tt.worktree, tt.config)
		}
	}
	if origin := r.FSMonitor[0].Config[1]; origin.Scope != "worktree" || origin.Origin != "file:"+filepath.Join(w.repos[linked].Admin, "config.worktree") {
		t.Errorf("effective origin = %+v, want the worktree-scoped config.worktree", origin)
	}
}

func TestCheckQuarantine(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(w *world) string
		watched bool
	}{
		{
			name: "outside every watcher",
			setup: func(w *world) string {
				w.watch(w.dir("root"), &fakeRoot{})
				return w.dir("jobs/1")
			},
		},
		{
			name: "inside a root, even an ignored dir",
			setup: func(w *world) string {
				w.watch(w.dir("root"), &fakeRoot{Config: `{"ignore_dirs":["jobs"]}`})
				return w.dir("root/jobs/1")
			},
			watched: true,
		},
		{
			name: "containing a root",
			setup: func(w *world) string {
				w.watch(w.dir("jobs/1/nested"), &fakeRoot{})
				return w.dir("jobs/1")
			},
			watched: true,
		},
		{
			name: "inside an fsmonitor worktree",
			setup: func(w *world) string {
				wt := w.linked("repo", "wt")
				w.daemon(101, w.repos[wt].Socket, w.home)
				return w.dir("wt/jobs")
			},
			watched: true,
		},
		{
			name: "below a hashed-socket daemon's worktree",
			setup: func(w *world) string {
				socket := filepath.Join(w.home, hashedSocket(filepath.Join(w.base, "remote")))
				w.daemon(102, socket, w.home)
				return w.dir("remote/jobs")
			},
			watched: true,
		},
		{
			name: "any hashed-socket daemon, even one that may sit inside",
			setup: func(w *world) string {
				w.daemon(103, filepath.Join(w.home, hashedSocket(filepath.Join(w.base, "jobs", "linked"))), w.home)
				return w.dir("jobs")
			},
			watched: true,
		},
		{
			name: "daemon with malformed git metadata",
			setup: func(w *world) string {
				admin := w.dir("repo/.git/worktrees/broken")
				w.write(filepath.Join(admin, "gitdir"), "not-a-git-link\n")
				w.daemon(104, filepath.Join(admin, ipcSocket), w.home)
				return w.dir("jobs")
			},
			watched: true,
		},
		{
			name: "main-worktree daemon whose core.worktree points at the quarantine",
			setup: func(w *world) string {
				main := w.mainRepo("a")
				repo := w.repos[main]
				repo.Toplevel = w.dir("b")
				w.repos[main] = repo
				w.daemon(105, repo.Socket, w.home)
				return w.dir("b/jobs")
			},
			watched: true,
		},
		{
			name: "fsmonitor daemon past the process table's bound",
			setup: func(w *world) string {
				w.daemons = append(w.daemons, Process{PID: 106, Start: daemonStart, Command: daemonCommand, Unexamined: true})
				return w.dir("jobs")
			},
			watched: true,
		},
		{
			name: "watchman absent, daemons elsewhere",
			setup: func(w *world) string {
				w.missing = true
				wt := w.linked("repo", "wt")
				w.daemon(101, w.repos[wt].Socket, w.home)
				return w.dir("jobs/1")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			dir := tt.setup(w)
			err := CheckQuarantine(context.Background(), w.deps(), dir)
			if got := errors.Is(err, ErrWatched); got != tt.watched {
				t.Errorf("CheckQuarantine = %v, want watched %t", err, tt.watched)
			}
			if !tt.watched && err != nil {
				t.Errorf("CheckQuarantine = %v, want nil", err)
			}
		})
	}
}

func TestRender(t *testing.T) {
	r := Report{
		Watchman: Watchman{
			Available: true, Version: "v1", PID: 9,
			Roots: []Root{{
				Path: "/r", Watcher: "fsevents", FSType: "apfs", Recrawls: 2, RecrawlReason: "startup", CrawlMS: 1500,
				Subscriptions: []Subscription{{Name: "s"}}, StaleConfig: true, Verdict: VerdictBusy,
			}},
			Clients: []Client{{PID: 5, Name: "relay", State: "waiting for request", Protected: true, Roots: []string{"/r"}}},
		},
		FSMonitor: []Daemon{{PID: 7, Worktree: "/wt", Socket: "/g/fsmonitor--daemon.ipc", Verdict: VerdictRetirable,
			Config: []ConfigValue{{Scope: "worktree", Origin: "file:/g/config.worktree", Value: "true"}}}},
	}
	r.Watchman.Roots[0].DiskConfigError = "parse .watchmanconfig: bad"
	r.Watchman.Unexamined = []string{"/s"}
	r.FSMonitor = append(r.FSMonitor, Daemon{PID: 8, Verdict: VerdictUnresolved, Error: "no fsmonitor IPC socket among its open files"})
	want := "watchman v1 (pid 9): 2 roots, 1 clients\n" +
		"  busy  /r  fsevents/apfs  recrawls 2 (startup)  crawl 1.5s  subs 1 triggers 0 queries 0  config stale (parse .watchmanconfig: bad)\n" +
		"  1 roots not examined (census bounded at 1):\n" +
		"    /s\n" +
		"  client pid 5 relay [waiting for request] protected subscribes /r\n" +
		"fsmonitor: 2 daemons\n" +
		"  retire-on-removal  pid 7  /wt  socket /g/fsmonitor--daemon.ipc  core.fsmonitor=true (worktree file:/g/config.worktree)\n" +
		"  unresolved  pid 8  (no fsmonitor IPC socket among its open files)\n"
	if got := r.Render(); got != want {
		t.Errorf("Render =\n%s\nwant\n%s", got, want)
	}
	if got := (Report{Watchman: Watchman{Absent: "watchman is not installed"}}).Render(); got != "watchman: unavailable (watchman is not installed)\nfsmonitor: 0 daemons\n" {
		t.Errorf("absent Render = %q", got)
	}
}
