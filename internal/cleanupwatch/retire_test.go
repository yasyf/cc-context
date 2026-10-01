package cleanupwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fixture struct {
	w      *world
	wt     string
	nested string
	other  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	w := newWorld(t)
	f := fixture{w: w, wt: w.linked("repo", "wt")}
	f.nested = w.dir("wt/packages/app")
	f.other = w.dir("other")
	w.watch(f.wt, &fakeRoot{})
	w.watch(f.nested, &fakeRoot{})
	w.watch(f.other, &fakeRoot{})
	w.watch(w.base, &fakeRoot{Config: `{"ignore_dirs":["wt"]}`})
	w.daemon(101, w.repos[f.wt].Socket, w.home)
	return f
}

func (f fixture) expand(s string) string {
	return strings.NewReplacer("NESTED", f.nested, "WT", f.wt, "OTHER", f.other, "BASE", f.w.base).Replace(s)
}

func (f fixture) mutations() []string {
	var out []string
	for _, c := range f.w.callsMatching("") {
		switch {
		case strings.HasPrefix(c, "guard "),
			strings.HasPrefix(c, "watchman --no-spawn --no-pretty watch-del "),
			strings.HasPrefix(c, "watchman --no-spawn --no-pretty watch "),
			strings.HasSuffix(c, "fsmonitor--daemon stop"):
			out = append(out, c)
		}
	}
	return out
}

func (f fixture) locked(edit func()) {
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	edit()
}

func onNth(prefix string, n int, do func()) func(string) {
	seen := 0
	return func(call string) {
		if strings.HasPrefix(call, prefix) {
			if seen++; seen == n {
				do()
			}
		}
	}
}

func TestRetireRetiresExactlyTheRootsAtOrBelowAndTheOwnedDaemon(t *testing.T) {
	f := newFixture(t)
	d := f.w.deps()
	admin := f.w.repos[f.wt].Admin

	p, err := Plan(context.Background(), d, f.wt)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Refused() || !slices.Equal(rootPaths(p.Roots), []string{f.nested, f.wt}) ||
		!slices.Equal(p.Ancestors, []Coverage{{Root: f.w.base, Ignored: true}}) ||
		p.FSMonitor == nil || p.FSMonitor.PID != 101 || p.GitDir != admin {
		t.Fatalf("Plan = %+v, want nested then wt, ignored ancestor, daemon 101, git dir %s", p, admin)
	}
	if got := f.mutations(); len(got) != 0 {
		t.Fatalf("Plan mutated: %q", got)
	}
	if last := p.FSMonitor.Config[len(p.FSMonitor.Config)-1]; last.Scope != "worktree" || last.Value != "true" {
		t.Errorf("fsmonitor config origin = %+v, want worktree-scoped true", last)
	}

	out, err := Retire(context.Background(), d, p)
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if !slices.Equal(out.Retired, []string{f.nested, f.wt}) || out.Stopped == nil || out.Stopped.PID != 101 {
		t.Errorf("Outcome = %+v, want nested and wt retired and daemon 101 stopped", out)
	}
	want := []string{
		"guard " + f.wt,
		"watchman --no-spawn --no-pretty watch-del " + f.nested,
		"guard " + f.wt,
		"watchman --no-spawn --no-pretty watch-del " + f.wt,
		"guard " + f.wt,
		"git --git-dir=" + admin + " --work-tree=" + f.wt + " fsmonitor--daemon stop",
	}
	if got := f.mutations(); !slices.Equal(got, want) {
		t.Errorf("mutations =\n%q\nwant\n%q", got, want)
	}
	remaining, err := d.watchList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(remaining, []string{f.w.base, f.other}) {
		t.Errorf("remaining roots = %q, want the ancestor and the unrelated root", remaining)
	}
	if len(f.w.daemons) != 0 {
		t.Errorf("daemons = %+v, want none", f.w.daemons)
	}
}

func TestPlanRefusesEveryUnclearConsumer(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f fixture)
		maxRoot int
		maxConn int
		want    []string
	}{
		{
			name: "subscription from a protected consumer",
			setup: func(f fixture) {
				f.w.subscribe(f.nested, fakeSub{Name: "watchman-make", PID: 601, Client: 3})
				f.w.client(601, "python3.12", "python3 /opt/homebrew/bin/watchman-make -p '**/*.proto'")
			},
			want: []string{`subscription "watchman-make" (pid 601) on NESTED`, "protected consumer pid 601"},
		},
		{
			name: "protected trigger callback",
			setup: func(f fixture) {
				f.w.roots[f.wt].Triggers = []map[string]any{{"name": "gen", "command": []string{"yarn", "relay-compiler"}}}
			},
			want: []string{`protected trigger "gen" on WT`},
		},
		{
			name: "plain trigger",
			setup: func(f fixture) {
				f.w.roots[f.wt].Triggers = []map[string]any{{"name": "lint", "command": []string{"make", "lint"}}}
			},
			want: []string{`trigger "lint" on WT`},
		},
		{
			name: "query in flight",
			setup: func(f fixture) {
				f.w.roots[f.wt].Queries = []map[string]any{{"state": "Generating", "client-pid": 602, "elapsed-milliseconds": 3}}
			},
			want: []string{"query in flight on WT (pid 602, Generating)"},
		},
		{
			name: "consumer on an ancestor that does not ignore the target",
			setup: func(f fixture) {
				f.w.roots[f.w.base].Config = `{}`
				f.w.subscribe(f.w.base, fakeSub{Name: "relay", PID: 610, Client: 11})
			},
			want: []string{`ancestor root does not ignore WT: subscription "relay" (pid 610) on BASE`},
		},
		{
			name:  "consumer on an ancestor that ignores the target",
			setup: func(f fixture) { f.w.subscribe(f.w.base, fakeSub{Name: "relay", PID: 610, Client: 11}) },
		},
		{
			name:  "client no subscription places",
			setup: func(f fixture) { f.w.client(603, "node", "node /repo/node_modules/.bin/jest --watch") },
			want:  []string{"unaccounted watchman client pid 603"},
		},
		{
			name: "the watchman server connected to itself",
			setup: func(f fixture) {
				f.w.client(4242, "/opt/homebrew/bin/watchman", "/opt/homebrew/bin/watchman --foreground --logfile=/var/log/watchman.log")
			},
		},
		{
			name:  "protected client with no subscription",
			setup: func(f fixture) { f.w.client(604, "relay", "relay --validate") },
			want:  []string{"protected consumer pid 604 (relay --validate) connected with no subscription"},
		},
		{
			name: "client placed only outside the target",
			setup: func(f fixture) {
				f.w.subscribe(f.other, fakeSub{Name: "relay", PID: 605, Client: 4})
				f.w.client(605, "relay", "relay --watch")
			},
		},
		{
			name: "second connection from a placed client",
			setup: func(f fixture) {
				f.w.subscribe(f.other, fakeSub{Name: "sub", PID: 611, Client: 12})
				f.w.client(611, "node", "node server.js")
				f.w.client(611, "node", "node server.js")
			},
			want: []string{"unaccounted watchman client pid 611 (node server.js) holds 2 connections but subscriptions place only 1"},
		},
		{
			name: "client beyond the census bound",
			setup: func(f fixture) {
				f.w.subscribe(f.other, fakeSub{Name: "sub", PID: 606, Client: 5})
				f.w.client(606, "node", "node server.js")
			},
			maxRoot: 1,
			want:    []string{"census bounded at 1 roots cannot place it"},
		},
		{
			name: "client with no peer",
			setup: func(f fixture) {
				f.w.clients = append(f.w.clients, map[string]any{"state": "waiting for request"})
			},
			want: []string{"watchman client with no peer pid (waiting for request)"},
		},
		{
			name:  "another watchman CLI connection",
			setup: func(f fixture) { f.w.client(607, "/opt/homebrew/bin/watchman", "watchman watch-list") },
			want:  []string{"unaccounted watchman client pid 607 (watchman watch-list) connected with no subscription"},
		},
		{
			name:  "persistent watchman CLI",
			setup: func(f fixture) { f.w.client(608, "watchman", "watchman -p -j") },
			want:  []string{"unaccounted watchman client pid 608 (watchman -p -j) connected with no subscription"},
		},
		{
			name:  "protected command behind a watchman peer name",
			setup: func(f fixture) { f.w.client(901, "/opt/homebrew/bin/watchman", "relay --watch") },
			want:  []string{"protected consumer pid 901 (relay --watch) connected with no subscription"},
		},
		{
			name:    "clients past the inspection bound",
			setup:   func(f fixture) { f.w.client(910, "node", "node a.js"); f.w.client(911, "node", "node b.js") },
			maxConn: 1,
			want:    []string{"1 watchman connections past the bound of 1 client pids were not inspected", "unaccounted watchman client pid 910"},
		},
		{
			name: "fsmonitor daemon past the process table's bound",
			setup: func(f fixture) {
				f.w.daemons = append(f.w.daemons, Process{PID: 912, Start: daemonStart, Command: daemonCommand, Unexamined: true})
			},
			want: []string{"fsmonitor daemon 912 has an unresolved worktree: past the process table's bound"},
		},
		{
			name: "fsmonitor daemon for a worktree nested in the target",
			setup: func(f fixture) {
				inner := f.w.linked("repo", "wt/inner")
				f.w.daemon(701, f.w.repos[inner].Socket, f.w.home)
			},
			want: []string{"fsmonitor daemon 701 watches WT/inner inside WT without an exact socket match"},
		},
		{
			name: "fsmonitor daemon running inside the target without owning it",
			setup: func(f fixture) {
				elsewhere := f.w.linked("repo", "elsewhere")
				f.w.daemon(702, f.w.repos[elsewhere].Socket, f.wt)
			},
			want: []string{"fsmonitor daemon 702 runs inside WT without owning its socket"},
		},
		{
			name: "two daemons claim the socket",
			setup: func(f fixture) {
				f.w.daemons = append(f.w.daemons, Process{PID: 703, Command: daemonCommand, CWD: f.w.home, Sockets: []string{f.w.repos[f.wt].Socket}})
			},
			want: []string{"fsmonitor daemons 101 and 703 both own"},
		},
		{
			name: "daemon on a relative socket",
			setup: func(f fixture) {
				f.w.daemons = append(f.w.daemons, Process{PID: 704, Command: daemonCommand, CWD: f.w.home, Sockets: []string{".git/" + ipcSocket}})
			},
			want: []string{"fsmonitor daemon 704 has an unresolved worktree: relative IPC socket .git/fsmonitor--daemon.ipc"},
		},
		{
			name: "daemon on another worktree's hashed socket",
			setup: func(f fixture) {
				f.w.daemon(705, filepath.Join(f.w.home, hashedSocket("/elsewhere")), f.w.home)
			},
			want: []string{"fsmonitor daemon 705 has an unresolved worktree: hashed IPC socket names no worktree"},
		},
		{
			name: "daemon with no socket",
			setup: func(f fixture) {
				f.w.daemons = append(f.w.daemons, Process{PID: 706, Command: daemonCommand, CWD: f.w.home})
			},
			want: []string{"fsmonitor daemon 706 has an unresolved worktree: no fsmonitor IPC socket"},
		},
		{
			name: "daemon whose socket is gone",
			setup: func(f fixture) {
				f.w.daemons = append(f.w.daemons, Process{PID: 707, Command: daemonCommand, CWD: f.w.home, Sockets: []string{filepath.Join(f.w.base, "gone", ".git", ipcSocket)}})
			},
			want: []string{"fsmonitor daemon 707 lost its socket"},
		},
		{
			name: "owner whose socket was unlinked",
			setup: func(f fixture) {
				if err := os.Remove(f.w.repos[f.wt].Socket); err != nil {
					f.w.t.Fatal(err)
				}
			},
			want: []string{"fsmonitor daemon 101 serves WT through socket"},
		},
		{
			name: "git metadata links elsewhere",
			setup: func(f fixture) {
				f.w.write(filepath.Join(f.w.repos[f.wt].Admin, "gitdir"), filepath.Join(f.other, ".git")+"\n")
			},
			want: []string{"links back to OTHER, not WT"},
		},
		{
			name: "core.worktree points elsewhere",
			setup: func(f fixture) {
				repo := f.w.repos[f.wt]
				repo.Toplevel = f.other
				f.w.repos[f.wt] = repo
			},
			want: []string{"git resolves WT to worktree OTHER"},
		},
		{
			name:  "git cannot resolve the worktree",
			setup: func(f fixture) { delete(f.w.repos, f.wt) },
			want:  []string{"git cannot resolve WT: fatal: not a git repository"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			d := f.w.deps()
			if tt.maxRoot != 0 {
				d.MaxRoots = tt.maxRoot
			}
			if tt.maxConn != 0 {
				d.MaxClients = tt.maxConn
			}
			p, err := Plan(context.Background(), d, f.wt)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			blockers := strings.Join(p.Blockers, "\n")
			for _, w := range tt.want {
				if w = f.expand(w); !strings.Contains(blockers, w) {
					t.Errorf("blockers =\n%s\nwant one containing %q", blockers, w)
				}
			}
			if len(tt.want) == 0 && p.Refused() {
				t.Fatalf("blockers = %q, want none", p.Blockers)
			}
			_, err = Retire(context.Background(), d, p)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Retire: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Retire error = %v, want ErrRefused", err)
			}
			if got := f.mutations(); len(got) != 0 {
				t.Errorf("refused Retire mutated: %q", got)
			}
		})
	}
}

func TestRetireRefusesDrift(t *testing.T) {
	tests := []struct {
		name    string
		before  func(f fixture)
		between func(t *testing.T, f fixture)
		during  func(t *testing.T, f fixture) func(call string)
		want    string
		retired []string
		stops   int
		stopped bool
	}{
		{
			name:    "late subscriber",
			between: func(_ *testing.T, f fixture) { f.w.subscribe(f.wt, fakeSub{Name: "late", PID: 801, Client: 9}) },
			want:    `subscription "late" (pid 801)`,
		},
		{
			name:    "late unaccounted client",
			between: func(_ *testing.T, f fixture) { f.w.client(803, "node", "node metro") },
			want:    "unaccounted watchman client pid 803",
		},
		{
			name: "subscriber arriving during the first root's retirement",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 1, func() {
					f.w.subscribe(f.wt, fakeSub{Name: "late", PID: 802, Client: 10})
				})
			},
			want:    `subscription "late" (pid 802)`,
			retired: []string{"NESTED"},
		},
		{
			name: "protected client connecting during the first root's retirement",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 1, func() { f.w.client(804, "relay", "relay --watch") })
			},
			want:    "protected consumer pid 804 (relay --watch) connected with no subscription",
			retired: []string{"NESTED"},
		},
		{
			name: "ancestor consumer arriving during the first root's retirement",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 1, func() {
					f.locked(func() { f.w.roots[f.w.base].Config = `{}` })
					f.w.subscribe(f.w.base, fakeSub{Name: "relay", PID: 805, Client: 13})
				})
			},
			want:    `ancestor root does not ignore WT: subscription "relay" (pid 805) on BASE`,
			retired: []string{"NESTED"},
		},
		{
			name:   "fsmonitor daemon starting mid-retirement",
			before: func(f fixture) { f.w.daemons = nil },
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 1, func() {
					f.locked(func() {
						f.w.daemons = append(f.w.daemons, Process{PID: 806, Command: daemonCommand, CWD: f.w.home, Sockets: []string{f.w.repos[f.wt].Socket}})
					})
				})
			},
			want:    "fsmonitor owner of WT changed since planning",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "subscriber arriving during the first guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() {
					f.w.subscribe(f.nested, fakeSub{Name: "relay", PID: 821, Client: 30})
					f.w.client(821, "relay", "relay --watch")
				})
			},
			want: `subscription "relay" (pid 821) on NESTED`,
		},
		{
			name: "query arriving during the first guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() {
					f.locked(func() {
						f.w.roots[f.nested].Queries = []map[string]any{{"state": "Generating", "client-pid": 822, "elapsed-milliseconds": 1}}
					})
				})
			},
			want: "query in flight on NESTED (pid 822, Generating)",
		},
		{
			name: "trigger arriving during the first guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() {
					f.locked(func() {
						f.w.roots[f.nested].Triggers = []map[string]any{{"name": "gen", "command": []string{"make"}}}
					})
				})
			},
			want: `trigger "gen" on NESTED`,
		},
		{
			name: "unaccounted client arriving during the first guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() { f.w.client(823, "node", "node metro") })
			},
			want: "unaccounted watchman client pid 823 (node metro) connected with no subscription",
		},
		{
			name:    "nested root replaced by a directory since planning",
			between: func(t *testing.T, f fixture) { replace(t, f.nested) },
			want:    "root NESTED was replaced since planning",
		},
		{
			name: "nested root replaced by a directory at the first guard",
			during: func(t *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() { replace(t, f.nested) })
			},
			want: "root NESTED was replaced mid-retirement (now NESTED",
		},
		{
			name:    "fsmonitor owner restarted under the same pid since planning",
			between: func(_ *testing.T, f fixture) { f.w.daemons[0].Start = "Wed Sep 30 09:00:00 2026" },
			want:    "fsmonitor owner of WT changed from pid 101 started " + daemonStart,
		},
		{
			name: "fsmonitor owner replaced at the final guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 3, func() { f.locked(func() { f.w.daemons[0].PID = 202 }) })
			},
			want:    "fsmonitor owner changed during the activity check: planned pid 101",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "fsmonitor owner restarted under the same pid at the final guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 3, func() { f.locked(func() { f.w.daemons[0].Start = "Wed Sep 30 09:00:00 2026" }) })
			},
			want:    "found pid 101 started Wed Sep 30 09:00:00 2026",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "fsmonitor owner command changed at the final guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 3, func() {
					f.locked(func() { f.w.daemons[0].Command = "/opt/homebrew/bin/git fsmonitor--daemon run --detach" })
				})
			},
			want:    "fsmonitor owner changed during the activity check",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "second daemon on the socket at the final guard",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("guard ", 3, func() {
					f.locked(func() {
						f.w.daemons = append(f.w.daemons, Process{PID: 203, Start: daemonStart, Command: daemonCommand, CWD: f.w.home, Sockets: []string{f.w.repos[f.wt].Socket}})
					})
				})
			},
			want:    "fsmonitor owner changed during the activity check",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "worktree path replaced",
			between: func(t *testing.T, f fixture) {
				replace(t, f.wt)
			},
			want: "was replaced since planning",
		},
		{
			name: "worktree path swapped for a symlink",
			between: func(t *testing.T, f fixture) {
				if err := os.Rename(f.wt, f.wt+".moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.other, f.wt); err != nil {
					t.Fatal(err)
				}
			},
			want: "was replaced since planning",
		},
		{
			name: "worktree replaced at the first guard",
			during: func(t *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() { replace(t, f.wt) })
			},
			want: "WT was replaced mid-retirement",
		},
		{
			name: "nested root swapped for a symlink at the first guard",
			during: func(t *testing.T, f fixture) func(string) {
				return onNth("guard ", 1, func() {
					if err := os.Rename(f.nested, f.nested+".moved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(f.other, f.nested); err != nil {
						t.Fatal(err)
					}
				})
			},
			want: "root NESTED was replaced mid-retirement (now OTHER",
		},
		{
			name:    "new root under the worktree",
			between: func(_ *testing.T, f fixture) { f.w.watch(f.w.dir("wt/late"), &fakeRoot{}) },
			want:    "root WT/late appeared under WT since planning",
		},
		{
			name:    "fsmonitor owner changed",
			between: func(_ *testing.T, f fixture) { f.w.daemons[0].PID = 202 },
			want:    "fsmonitor owner of WT changed from pid 101 started " + daemonStart,
		},
		{
			name: "fsmonitor owner swapped before the stop",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 2, func() { f.locked(func() { f.w.daemons[0].PID = 202 }) })
			},
			want:    "fsmonitor owner of WT changed from pid 101 started " + daemonStart,
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "git dir redirected before the stop",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("watchman --no-spawn --no-pretty watch-del", 2, func() {
					moved := f.w.dir("repo/.git/worktrees/wt-moved")
					f.w.write(filepath.Join(moved, "gitdir"), filepath.Join(f.wt, ".git")+"\n")
					f.locked(func() {
						repo := f.w.repos[f.wt]
						repo.Admin = moved
						f.w.repos[f.wt] = repo
					})
				})
			},
			want:    "git dir of WT changed to",
			retired: []string{"NESTED", "WT"},
		},
		{
			name: "root re-watched by a late consumer",
			during: func(_ *testing.T, f fixture) func(string) {
				return onNth("git --git-dir=", 1, func() { f.w.watch(f.wt, &fakeRoot{}) })
			},
			want:    "a late consumer re-watched WT after retirement",
			retired: []string{"NESTED", "WT"},
			stops:   1,
			stopped: true,
		},
		{
			name: "fsmonitor respawned after stop",
			during: func(_ *testing.T, f fixture) func(string) {
				return func(call string) {
					if strings.HasSuffix(call, "fsmonitor--daemon status") {
						f.locked(func() {
							f.w.daemons = append(f.w.daemons, Process{PID: 909, Command: daemonCommand, CWD: f.w.home, Sockets: []string{f.w.repos[f.wt].Socket}})
						})
					}
				}
			},
			want:    "an fsmonitor daemon is watching WT again after stop",
			retired: []string{"NESTED", "WT"},
			stops:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.before != nil {
				tt.before(f)
			}
			d := f.w.deps()
			p, err := Plan(context.Background(), d, f.wt)
			if err != nil || p.Refused() {
				t.Fatalf("Plan = %+v, %v", p, err)
			}
			if tt.between != nil {
				tt.between(t, f)
			}
			if tt.during != nil {
				f.w.hook = tt.during(t, f)
			}
			out, err := Retire(context.Background(), d, p)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("Retire error = %v, want ErrRefused", err)
			}
			if want := f.expand(tt.want); !strings.Contains(err.Error(), want) {
				t.Errorf("Retire error = %v, want it to name %q", err, want)
			}
			var retired []string
			for _, r := range tt.retired {
				retired = append(retired, f.expand(r))
			}
			if !slices.Equal(out.Retired, retired) || (out.Stopped != nil) != tt.stopped {
				t.Errorf("Outcome = %+v, want retired %q stopped %t", out, retired, tt.stopped)
			}
			for _, c := range f.w.callsMatching("watchman --no-spawn --no-pretty watch-del ") {
				if root := strings.TrimPrefix(c, "watchman --no-spawn --no-pretty watch-del "); !slices.Contains(retired, root) {
					t.Errorf("watch-del %s ran, want only %q", root, retired)
				}
			}
			var stops int
			for _, c := range f.mutations() {
				if strings.HasSuffix(c, "fsmonitor--daemon stop") {
					stops++
				}
			}
			if stops != tt.stops {
				t.Errorf("fsmonitor stop ran %d times, want %d", stops, tt.stops)
			}
		})
	}
}

func TestRetireRefusesADaemonPastTheBoundAtTheFinalGuard(t *testing.T) {
	f := newFixture(t)
	d := f.w.deps()
	p, err := Plan(context.Background(), d, f.wt)
	if err != nil || p.Refused() {
		t.Fatalf("Plan = %+v, %v", p, err)
	}
	f.w.hook = onNth("guard ", 3, func() {
		f.locked(func() {
			f.w.daemons = append(f.w.daemons, Process{PID: 203, Start: daemonStart, Command: daemonCommand, Unexamined: true})
		})
	})
	out, err := Retire(context.Background(), d, p)
	if want := "daemon 203 (past the process table's bound on fsmonitor daemons"; !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Retire error = %v, want ErrRefused naming %q", err, want)
	}
	if want := []string{f.nested, f.wt}; !slices.Equal(out.Retired, want) || out.Stopped != nil {
		t.Errorf("Outcome = %+v, want retired %q and no daemon stopped", out, want)
	}
	for _, c := range f.mutations() {
		if strings.HasSuffix(c, "fsmonitor--daemon stop") {
			t.Errorf("fsmonitor stop ran (%s), want a refusal before any stop", c)
		}
	}
	var pids []int
	f.locked(func() {
		for _, dm := range f.w.daemons {
			pids = append(pids, dm.PID)
		}
	})
	if !slices.Equal(pids, []int{101, 203}) {
		t.Errorf("daemons after Retire = %v, want 101 and 203 both left running", pids)
	}
}

func replace(t *testing.T, dir string) {
	t.Helper()
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "packages", "app"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestRetireStopsAtAnUnverifiedStep(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f fixture)
		want  string
	}{
		{
			name:  "watch-del not acknowledged",
			setup: func(f fixture) { f.w.nack[f.nested] = true },
			want:  "server answered watch-del=false",
		},
		{
			name:  "root still listed after watch-del",
			setup: func(f fixture) { f.w.sticky[f.nested] = true },
			want:  "watchman still lists NESTED after watch-del",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			d := f.w.deps()
			p, err := Plan(context.Background(), d, f.wt)
			if err != nil || p.Refused() {
				t.Fatalf("Plan = %+v, %v", p, err)
			}
			out, err := Retire(context.Background(), d, p)
			if err == nil || !strings.Contains(err.Error(), f.expand(tt.want)) {
				t.Fatalf("Retire error = %v, want %q", err, f.expand(tt.want))
			}
			if len(out.Retired) != 0 || out.Stopped != nil {
				t.Errorf("Outcome = %+v, want nothing verified", out)
			}
			want := []string{"guard " + f.wt, "watchman --no-spawn --no-pretty watch-del " + f.nested}
			if got := f.mutations(); !slices.Equal(got, want) {
				t.Errorf("mutations = %q, want only the first root's attempt %q", got, want)
			}
		})
	}
}

func TestRetireReportsAnUnverifiedFSMonitorStop(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f fixture)
		want  string
	}{
		{
			name:  "daemon ignores stop",
			setup: func(f fixture) { f.w.stubborn = true },
			want:  "fsmonitor daemon 101 still serves WT after stop",
		},
		{
			name: "status fails outright",
			setup: func(f fixture) {
				repo := f.w.repos[f.wt]
				repo.StatusCode = 128
				f.w.repos[f.wt] = repo
			},
			want: "fsmonitor status in WT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			d := f.w.deps()
			p, err := Plan(context.Background(), d, f.wt)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Retire(context.Background(), d, p)
			if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), f.expand(tt.want)) {
				t.Fatalf("Retire error = %v, want an unverified stop naming %q", err, f.expand(tt.want))
			}
			if out.Stopped != nil || !slices.Equal(out.Retired, []string{f.nested, f.wt}) {
				t.Errorf("Outcome = %+v, want both roots retired and no verified stop", out)
			}
		})
	}
}

func TestRetireNeverMutatesWithoutAPassingGuard(t *testing.T) {
	tests := []struct {
		name string
		edit func(d *Deps, w *world)
		want error
	}{
		{"nil guard", func(d *Deps, _ *world) { d.Guard = nil }, ErrNoGuard},
		{"active worktree", func(_ *Deps, w *world) { w.guardErr = errors.New("pid 700 has its cwd inside") }, ErrRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			d := f.w.deps()
			p, err := Plan(context.Background(), d, f.wt)
			if err != nil || p.Refused() {
				t.Fatalf("Plan = %+v, %v", p, err)
			}
			tt.edit(&d, f.w)
			out, err := Retire(context.Background(), d, p)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Retire error = %v, want %v", err, tt.want)
			}
			if len(out.Retired) != 0 || out.Stopped != nil {
				t.Errorf("Outcome = %+v, want nothing retired", out)
			}
			for _, c := range f.mutations() {
				if !strings.HasPrefix(c, "guard ") {
					t.Errorf("mutation %q ran without a passing guard", c)
				}
			}
		})
	}
}

func TestRetireWithoutWatchmanStillRetiresTheOwnedDaemon(t *testing.T) {
	f := newFixture(t)
	f.w.missing = true
	d := f.w.deps()
	p, err := Plan(context.Background(), d, f.wt)
	if err != nil || p.Refused() || len(p.Roots) != 0 || p.FSMonitor == nil {
		t.Fatalf("Plan = %+v, %v; want no roots and daemon 101", p, err)
	}
	out, err := Retire(context.Background(), d, p)
	if err != nil || len(out.Retired) != 0 || out.Stopped == nil || out.Stopped.PID != 101 {
		t.Fatalf("Retire = %+v, %v; want only daemon 101 stopped", out, err)
	}
}

func TestHashedSocketMatchesGitsNaming(t *testing.T) {
	if got, want := hashedSocket("/Users/me/remote/wt"), ".git-fsmonitor-bf2ea1e22afbaf3c81a215b29efe9bb7b73866bf"; got != want {
		t.Errorf("hashedSocket = %s, want %s", got, want)
	}
}

func TestRetireMatchesAHashedSocketDaemon(t *testing.T) {
	w := newWorld(t)
	wt := w.linked("repo", "wt")
	sockDir := w.dir("sockets")
	repo := w.repos[wt]
	repo.SocketDir = sockDir
	repo.Socket = filepath.Join(sockDir, hashedSocket(wt))
	w.repos[wt] = repo
	w.daemon(301, repo.Socket, w.home)
	w.missing = true
	d := w.deps()

	p, err := Plan(context.Background(), d, wt)
	if err != nil || p.Refused() || p.FSMonitor == nil || p.FSMonitor.PID != 301 {
		t.Fatalf("Plan = %+v, %v; want hashed-socket daemon 301", p, err)
	}
	if _, err := Retire(context.Background(), d, p); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if len(w.daemons) != 0 {
		t.Errorf("daemons = %+v, want none", w.daemons)
	}
}

func TestRetireLeavesOtherWorktreesDaemonsAlone(t *testing.T) {
	w := newWorld(t)
	main := w.mainRepo("repo")
	wt := w.linked("repo", "wt")
	sibling := w.linked("repo", "sibling")
	w.daemon(401, w.repos[main].Socket, w.home)
	w.daemon(402, w.repos[sibling].Socket, w.home)
	w.missing = true
	d := w.deps()

	p, err := Plan(context.Background(), d, wt)
	if err != nil || p.Refused() || p.FSMonitor != nil {
		t.Fatalf("Plan = %+v, %v; want no owner and no blockers", p, err)
	}
	out, err := Retire(context.Background(), d, p)
	if err != nil || out.Stopped != nil {
		t.Fatalf("Retire = %+v, %v; want nothing stopped", out, err)
	}
	if stops := len(w.callsMatching("git --git-dir=")); stops != 0 || len(w.daemons) != 2 {
		t.Errorf("Retire touched another worktree's daemon: calls %q daemons %+v", w.calls, w.daemons)
	}
}

func TestContainmentFollowsIdentityNotSpelling(t *testing.T) {
	f := newFixture(t)
	alias := filepath.Join(filepath.Dir(f.wt), strings.ToUpper(filepath.Base(f.wt)))
	if _, err := os.Stat(alias); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	f.w.repos[alias] = f.w.repos[f.wt]
	d := f.w.deps()

	p, err := Plan(context.Background(), d, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rootPaths(p.Roots), []string{f.nested, f.wt}) || p.FSMonitor == nil {
		t.Errorf("Plan(%s) = %+v, want the true-case roots and daemon 101", alias, p)
	}
	job := filepath.Join(alias, "jobs")
	if err := os.Mkdir(job, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckQuarantine(context.Background(), d, job); !errors.Is(err, ErrWatched) {
		t.Errorf("CheckQuarantine(%s) = %v, want ErrWatched", job, err)
	}
}

func TestRecreate(t *testing.T) {
	const loaded = `{"settle":200}`
	const disk = `{"settle":200,"idle_reap_age_seconds":172800,"fsevents_latency":0.1}`
	tests := []struct {
		name      string
		setup     func(w *world, root string) string
		want      error
		outcome   func(root string) Outcome
		mutations []string
	}{
		{
			name:      "stale idle root",
			setup:     func(_ *world, root string) string { return root },
			outcome:   func(root string) Outcome { return Outcome{Recreated: root} },
			mutations: []string{"guard ROOT", "watchman --no-spawn --no-pretty watch-del ROOT", "guard ROOT", "watchman --no-spawn --no-pretty watch ROOT"},
		},
		{
			name: "activity after the watch-del",
			setup: func(w *world, root string) string {
				w.hook = onNth("watchman --no-spawn --no-pretty watch-del", 1, func() {
					w.mu.Lock()
					w.guardErr = errors.New("tty s004 opened inside")
					w.mu.Unlock()
				})
				return root
			},
			want:      ErrRefused,
			outcome:   func(root string) Outcome { return Outcome{Retired: []string{root}} },
			mutations: []string{"guard ROOT", "watchman --no-spawn --no-pretty watch-del ROOT", "guard ROOT"},
		},
		{
			name: "config changed at the first guard",
			setup: func(w *world, root string) string {
				w.hook = onNth("guard ", 1, func() { w.write(filepath.Join(root, ".watchmanconfig"), `{"ignore_dirs":["src"]}`) })
				return root
			},
			want:      ErrRefused,
			mutations: []string{"guard ROOT"},
		},
		{
			name: "config replaced by identical bytes at the first guard",
			setup: func(w *world, root string) string {
				w.hook = onNth("guard ", 1, func() {
					swap := filepath.Join(root, ".watchmanconfig.new")
					w.write(swap, disk)
					if err := os.Rename(swap, filepath.Join(root, ".watchmanconfig")); err != nil {
						w.t.Fatal(err)
					}
				})
				return root
			},
			want:      ErrRefused,
			mutations: []string{"guard ROOT"},
		},
		{
			name: "config changed at the second guard",
			setup: func(w *world, root string) string {
				w.hook = onNth("guard ", 2, func() { w.write(filepath.Join(root, ".watchmanconfig"), `{"ignore_dirs":["src"]}`) })
				return root
			},
			want:      ErrRefused,
			outcome:   func(root string) Outcome { return Outcome{Retired: []string{root}} },
			mutations: []string{"guard ROOT", "watchman --no-spawn --no-pretty watch-del ROOT", "guard ROOT"},
		},
		{
			name: "subscriber arriving at the first guard",
			setup: func(w *world, root string) string {
				w.hook = onNth("guard ", 1, func() { w.subscribe(root, fakeSub{Name: "relay", PID: 830, Client: 40}) })
				return root
			},
			want:      ErrRefused,
			mutations: []string{"guard ROOT"},
		},
		{
			name: "config already current",
			setup: func(w *world, root string) string {
				w.roots[root].Config = disk
				return root
			},
		},
		{
			name: "subscriber",
			setup: func(w *world, root string) string {
				w.subscribe(root, fakeSub{Name: "relay", PID: 501, Client: 1})
				return root
			},
			want: ErrRefused,
		},
		{
			name: "unaccounted client",
			setup: func(w *world, root string) string {
				w.client(502, "node", "node metro")
				return root
			},
			want: ErrRefused,
		},
		{
			name: "active root",
			setup: func(w *world, root string) string {
				w.guardErr = errors.New("tty s003 inside")
				return root
			},
			want:      ErrRefused,
			mutations: []string{"guard ROOT"},
		},
		{
			name: "not watched",
			setup: func(w *world, _ string) string {
				return w.dir("unwatched")
			},
			want: ErrRefused,
		},
		{
			name: "root path now a symlink",
			setup: func(w *world, root string) string {
				link := filepath.Join(w.base, "link")
				if err := os.Symlink(root, link); err != nil {
					w.t.Fatal(err)
				}
				w.watch(link, &fakeRoot{Config: loaded})
				return link
			},
			want: ErrRefused,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			root := w.dir("root")
			w.write(filepath.Join(root, ".watchmanconfig"), disk)
			w.watch(root, &fakeRoot{Config: loaded})
			target := tt.setup(w, root)

			out, err := Recreate(context.Background(), w.deps(), target)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("Recreate error = %v, want %v", err, tt.want)
			}
			var want Outcome
			if tt.outcome != nil {
				want = tt.outcome(target)
			}
			if out.Recreated != want.Recreated || !slices.Equal(out.Retired, want.Retired) {
				t.Errorf("Outcome = %+v, want %+v", out, want)
			}
			var mutations []string
			for _, m := range tt.mutations {
				mutations = append(mutations, strings.ReplaceAll(m, "ROOT", target))
			}
			if got := (fixture{w: w}).mutations(); !slices.Equal(got, mutations) {
				t.Errorf("mutations = %q, want %q", got, mutations)
			}
			if want.Recreated != "" {
				config, err := w.deps().loadedConfig(context.Background(), root)
				if err != nil || !sameConfig(config, []byte(disk)) {
					t.Errorf("reloaded config = %s, %v; want %s", config, err, disk)
				}
			}
		})
	}
}

func TestRecreateWithoutGuardNeverMutates(t *testing.T) {
	w := newWorld(t)
	root := w.dir("root")
	w.write(filepath.Join(root, ".watchmanconfig"), `{"settle":100}`)
	w.watch(root, &fakeRoot{})
	d := w.deps()
	d.Guard = nil
	if _, err := Recreate(context.Background(), d, root); !errors.Is(err, ErrNoGuard) {
		t.Fatalf("Recreate error = %v, want ErrNoGuard", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("Recreate without a guard ran %q", w.calls)
	}
}

func TestClientBlockersRefuseAnyClientPlacedOnATarget(t *testing.T) {
	f := newFixture(t)
	f.w.subscribe(f.other, fakeSub{Name: "a", PID: 620, Client: 21})
	f.w.subscribe(f.wt, fakeSub{Name: "b", PID: 620, Client: 22})
	f.w.client(620, "node", "node dev-server.js")
	f.w.client(620, "node", "node dev-server.js")
	d := f.w.deps()
	status, err := d.debugStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := d.clientBlockers(context.Background(), status, []string{f.wt})
	if err != nil {
		t.Fatal(err)
	}
	want := "unaccounted watchman client pid 620 (node dev-server.js) subscribes to " + f.other + ", " + f.wt
	if !slices.Equal(blockers, []string{want}) {
		t.Errorf("blockers = %q, want %q", blockers, want)
	}
}
