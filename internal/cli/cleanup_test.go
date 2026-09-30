package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/daemon"
	"github.com/yasyf/cc-context/internal/cleanup/relocate"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const testHolderPID = 4242

var queuedJobPattern = regexp.MustCompile(`deletion queued ([0-9a-f]{16}-[0-9a-f]{6})`)

type cleanupHarness struct {
	layout cleanup.Layout
	engine *daemon.Engine
	active atomic.Bool

	mu        sync.Mutex
	retireErr error
}

func (h *cleanupHarness) guard(_ context.Context, worktree string) error {
	if !h.active.Load() {
		return nil
	}
	return &cleanup.ActiveError{Worktree: worktree, Holders: []cleanup.Holder{
		{PID: testHolderPID, Name: "claude", TTY: true, Evidence: cleanup.EvidenceCwd, Path: worktree},
	}}
}

func (h *cleanupHarness) Retiring(context.Context, string) ([]cleanup.ProcessID, error) {
	return nil, nil
}

func (h *cleanupHarness) Retire(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retireErr
}

func (h *cleanupHarness) CheckQuarantine(context.Context, string) error { return nil }

func (h *cleanupHarness) refuseRetire(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retireErr = err
}

type idleCPU struct{}

func (idleCPU) Sample(context.Context) (time.Duration, error) { return 0, nil }

var testCleanups sync.Map

func fixtureCleanup(t *testing.T, f *vcstest.Fixture) *cleanupHarness {
	t.Helper()
	if h, ok := testCleanups.Load(f); ok {
		return h.(*cleanupHarness)
	}
	h := &cleanupHarness{layout: cleanup.Layout{Root: filepath.Join(filepath.Dir(f.Dir), "cleanup")}}
	journal, err := cleanup.OpenJournal(h.layout)
	if err != nil {
		t.Fatalf("open cleanup journal: %v", err)
	}
	config := relocate.Config{Journal: journal, Guard: h.guard, Watchers: h, GitEnv: f.Env(), Now: time.Now}
	tuning := daemon.DefaultTuning()
	tuning.Rate = 1_000_000
	tuning.SampleEvery = 5 * time.Millisecond
	tuning.Recheck = 20 * time.Millisecond
	engine, err := daemon.New(daemon.Config{
		Journal:   journal,
		Relocator: relocate.New(config),
		Deleter:   rmtree.Deleter{},
		CPU:       idleCPU{},
		Version:   "test",
		Tuning:    tuning,
	})
	if err != nil {
		t.Fatalf("new cleanup engine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	t.Cleanup(func() {
		testCleanups.Delete(f)
		if err := engine.Stop(context.Background()); err != nil {
			t.Errorf("stop cleanup engine: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("cleanup engine run: %v", err)
		}
		cancel()
	})
	h.engine = engine
	preview := relocate.New(config).Preview
	f.Decorate(func(ctx context.Context) context.Context {
		return withCleanupPreview(withCleanup(ctx, engine), preview)
	})
	testCleanups.Store(f, h)
	return h
}

func runCleanupCmd(t *testing.T, f *vcstest.Fixture, args ...string) (string, error) {
	t.Helper()
	fixtureCleanup(t, f)
	cmd := newCleanupCmd() //nolint:contextcheck // ExecuteContext(ctx) below is what sets cmd's context; contextcheck cannot see through cobra's two-step wiring
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(f.Context())
	return out.String(), err
}

func requireCleanupDaemon(t *testing.T) {
	t.Helper()
	if !cleanupDaemonized {
		t.Skip("worktree rm removes inline where the cleanup daemon does not run")
	}
}

func queuedJobID(t *testing.T, out string) string {
	t.Helper()
	m := queuedJobPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output %q carries no deletion receipt", out)
	}
	return m[1]
}

func cleanupJob(t *testing.T, f *vcstest.Fixture, h *cleanupHarness, id string) cleanup.Job {
	t.Helper()
	report, err := h.engine.Status(f.Context(), cleanup.Query{JobID: id})
	if err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	if len(report.Jobs) != 1 || report.Jobs[0].ID != id {
		t.Fatalf("status %s jobs = %+v, want exactly that job", id, report.Jobs)
	}
	return report.Jobs[0]
}

func journaledJobs(t *testing.T, h *cleanupHarness) []string {
	t.Helper()
	entries, err := os.ReadDir(h.layout.JobsDir())
	if err != nil {
		t.Fatalf("read jobs dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lstat %s = %v, want it gone", path, err)
	}
}

func assertIntact(t *testing.T, f *vcstest.Fixture, path string) {
	t.Helper()
	if got := readFileStr(t, filepath.Join(path, "f.txt")); got == "" {
		t.Errorf("%s/f.txt is empty, want the checkout untouched", path)
	}
	if !worktreeRegistered(t, f.Env(), f.Dir, path) {
		t.Errorf("git no longer registers %s, want it untouched", path)
	}
}

func recoveryRefs(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, f.Env(), f.Dir, "git", "for-each-ref", "--format=%(refname) %(objectname)", cleanup.RecoveryRefPrefix))
}

func TestCleanupWorktreeRmQueuesDeletion(t *testing.T) {
	requireCleanupDaemon(t)
	f := vcstest.Repo(t, vcstest.Remote())
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	path := addPoolWorktree(t, f, "feat")
	head := strings.TrimSpace(mustRun(t, f.Env(), path, "git", "rev-parse", "HEAD"))

	out, err := runWorktreeCmd(t, f, "rm", "feat")
	if err != nil {
		t.Fatalf("rm error = %v", err)
	}
	id := queuedJobID(t, out)
	if want := "removed feat · git worktree · " + path + " · deletion queued " + id + "\n"; out != want {
		t.Errorf("rm output = %q, want %q", out, want)
	}
	assertGone(t, path)
	if worktreeRegistered(t, f.Env(), f.Dir, path) {
		t.Errorf("git still registers %s, want it unregistered", path)
	}
	job := cleanupJob(t, f, h, id)
	if job.Original != path || job.Head != head || job.RecoveryRef != cleanup.RecoveryRefFor(id) || job.Branch != "feat" {
		t.Errorf("job = original %q head %q ref %q branch %q, want %q %q %q %q",
			job.Original, job.Head, job.RecoveryRef, job.Branch, path, head, cleanup.RecoveryRefFor(id), "feat")
	}
	if job.Phase.Logical() {
		t.Errorf("job phase = %s, want the logical removal complete", job.Phase)
	}
	if got, want := recoveryRefs(t, f), cleanup.RecoveryRefFor(id)+" "+head; got != want {
		t.Errorf("recovery refs = %q, want %q", got, want)
	}
	mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "--verify", "refs/heads/feat")
}

func TestCleanupWorktreeRmWait(t *testing.T) {
	requireCleanupDaemon(t)
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	path := addPoolWorktree(t, f, "feat")

	out, err := runWorktreeCmd(t, f, "rm", "feat", "--wait")
	if err != nil {
		t.Fatalf("rm --wait error = %v", err)
	}
	id := queuedJobID(t, out)
	if want := "removed feat · git worktree · " + path + " · deletion queued " + id + " · deleted\n"; out != want {
		t.Errorf("rm --wait output = %q, want %q", out, want)
	}
	job := cleanupJob(t, f, h, id)
	if job.Phase != cleanup.PhaseDone || job.Removed == 0 {
		t.Errorf("job phase %s removed %d, want done with its entries counted", job.Phase, job.Removed)
	}
	assertGone(t, path)
	assertGone(t, h.layout.Payload(id))
	assertGone(t, h.layout.Registered(id))
}

func TestCleanupWorktreeRmPathTargets(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	root := filepath.Dir(f.Dir)
	checkouts := filepath.Join(root, "checkouts")
	alias := filepath.Join(root, "alias")
	stray := filepath.Join(root, "stray")
	for _, dir := range []string{checkouts, stray} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.Symlink(checkouts, alias); err != nil {
		t.Fatalf("symlink %s: %v", alias, err)
	}
	other := vcstest.Repo(t)
	foreign := filepath.Join(root, "foreign")
	addLinkedWorktree(t, other.Env(), other.Dir, foreign, "")
	otherCommon, err := filepath.EvalSymlinks(filepath.Join(other.Dir, ".git"))
	if err != nil {
		t.Fatalf("resolve %s: %v", other.Dir, err)
	}
	mainRefusal := `worktree rm: "` + f.Dir + `" is the repository's own working copy, not a linked worktree`

	tests := []struct {
		name     string
		tree     string
		args     func(tree string) []string
		want     func(tree string) string
		notFound bool
	}{
		{
			name: "registered path",
			tree: "plain",
			args: func(tree string) []string { return []string{"rm", "--path", tree} },
		},
		{
			name: "symlinked spelling",
			tree: "sym",
			args: func(string) []string { return []string{"rm", "--path", filepath.Join(alias, "sym")} },
		},
		{
			name: "trailing slash",
			tree: "slash",
			args: func(tree string) []string { return []string{"rm", "--path", tree + "/"} },
		},
		{
			name: "subdirectory",
			tree: "nested",
			args: func(tree string) []string { return []string{"rm", "--path", filepath.Join(tree, "sub")} },
			want: func(tree string) string {
				return "worktree rm: " + filepath.Join(tree, "sub") + " is inside the working copy " + tree + ", not its root"
			},
		},
		{
			name: "relative",
			tree: "rel",
			args: func(string) []string { return []string{"rm", "--path", "checkouts/rel"} },
			want: func(string) string { return `worktree rm: --path "checkouts/rel" is not an absolute path` },
		},
		{
			name: "main working copy",
			args: func(string) []string { return []string{"rm", "--path", f.Dir} },
			want: func(string) string { return mainRefusal },
		},
		{
			name: "main working copy forced",
			args: func(string) []string { return []string{"rm", "--path", f.Dir, "--force"} },
			want: func(string) string { return mainRefusal },
		},
		{
			name: "another repository",
			args: func(string) []string { return []string{"rm", "--path", foreign, "--force"} },
			want: func(string) string {
				return "worktree rm: " + foreign + " is a working copy of " + otherCommon + ", not of this repository"
			},
		},
		{
			name: "unregistered",
			args: func(string) []string { return []string{"rm", "--path", stray} },
			want: func(string) string {
				return "worktree rm: this repository registers no worktree at " + stray + ": not found"
			},
			notFound: true,
		},
		{
			name: "missing",
			args: func(string) []string { return []string{"rm", "--path", filepath.Join(root, "missing")} },
			want: func(string) string {
				return "worktree rm: nothing exists at " + filepath.Join(root, "missing") + ": not found"
			},
			notFound: true,
		},
		{
			name: "name and path",
			tree: "both",
			args: func(tree string) []string { return []string{"rm", "both", "--path", tree} },
			want: func(string) string { return "worktree rm: name a working copy or pass --path, not both" },
		},
		{
			name: "name and empty path",
			tree: "empty",
			args: func(string) []string { return []string{"rm", "empty", "--path", ""} },
			want: func(string) string { return "worktree rm: name a working copy or pass --path, not both" },
		},
		{
			name: "empty path",
			args: func(string) []string { return []string{"rm", "--path", ""} },
			want: func(string) string { return `worktree rm: --path "" is not an absolute path` },
		},
		{
			name: "neither",
			args: func(string) []string { return []string{"rm", "--force"} },
			want: func(string) string { return "worktree rm: name a working copy, or pass --path <absolute-path>" },
		},
	}
	removed := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := ""
			if tt.tree != "" {
				tree = filepath.Join(checkouts, tt.tree)
				addLinkedWorktree(t, f.Env(), f.Dir, tree, "")
				if err := os.Mkdir(filepath.Join(tree, "sub"), 0o750); err != nil {
					t.Fatalf("mkdir sub: %v", err)
				}
			}

			out, err := runWorktreeCmd(t, f, tt.args(tree)...)
			if tt.want != nil {
				if err == nil || err.Error() != tt.want(tree) {
					t.Fatalf("%v error = %v, want %q", tt.args(tree), err, tt.want(tree))
				}
				if got := errors.Is(err, ErrNotFound); got != tt.notFound {
					t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v", got, tt.notFound)
				}
				if out != "" {
					t.Errorf("%v output = %q, want none", tt.args(tree), out)
				}
				if tree != "" {
					assertIntact(t, f, tree)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v error = %v", tt.args(tree), err)
			}
			removed++
			want := "removed " + tt.tree + " · git worktree · " + tree
			if cleanupDaemonized {
				want += " · deletion queued " + queuedJobID(t, out)
			}
			if out != want+"\n" {
				t.Errorf("%v output = %q, want %q", tt.args(tree), out, want+"\n")
			}
			assertGone(t, tree)
			if worktreeRegistered(t, f.Env(), f.Dir, tree) {
				t.Errorf("git still registers %s, want it unregistered", tree)
			}
		})
	}
	assertIntact(t, f, f.Dir)
	if !worktreeRegistered(t, other.Env(), other.Dir, foreign) {
		t.Errorf("the other repository no longer registers %s, want it untouched", foreign)
	}
	want := 0
	if cleanupDaemonized {
		want = removed
	}
	if got := journaledJobs(t, h); len(got) != want {
		t.Errorf("journaled jobs = %v, want %d, one per removal", got, want)
	}
}

func TestCleanupWorktreeRmPathForce(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	fixtureCleanup(t, f)
	checkouts := filepath.Join(filepath.Dir(f.Dir), "checkouts")
	dirty := filepath.Join(checkouts, "dirty")
	locked := filepath.Join(checkouts, "locked")
	addLinkedWorktree(t, f.Env(), f.Dir, dirty, "")
	addLinkedWorktree(t, f.Env(), f.Dir, locked, "")
	if err := os.WriteFile(filepath.Join(dirty, "scratch.txt"), []byte("discard me\n"), 0o600); err != nil {
		t.Fatalf("dirty worktree: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "lock", "--reason", "agent busy", locked)
	refusedFor := func(t *testing.T, err error, reason, path string) {
		t.Helper()
		if err == nil {
			t.Fatalf("rm --path %s removed a tree it must refuse", path)
		}
		var refused *cleanup.RefusedError
		if cleanupDaemonized && (!errors.As(err, &refused) || refused.Reason != reason || refused.Worktree != path) {
			t.Errorf("rm --path %s error = %v, want a %q *cleanup.RefusedError", path, err, reason)
		}
		assertIntact(t, f, path)
	}

	_, err := runWorktreeCmd(t, f, "rm", "--path", dirty)
	refusedFor(t, err, "dirty", dirty)
	_, err = runWorktreeCmd(t, f, "rm", "--path", locked, "--force")
	refusedFor(t, err, "locked", locked)
	if _, err := runWorktreeCmd(t, f, "rm", "--path", dirty, "--force"); err != nil {
		t.Fatalf("rm --path --force on a dirty tree error = %v", err)
	}
	assertGone(t, dirty)
	if worktreeRegistered(t, f.Env(), f.Dir, dirty) {
		t.Errorf("git still registers %s, want it unregistered", dirty)
	}
}

func TestCleanupWorktreeRmDryRun(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	path := addPoolWorktree(t, f, "feat")

	for _, args := range [][]string{{"rm", "feat", "--dry-run"}, {"rm", "--path", path, "--dry-run", "--wait"}} {
		out, err := runWorktreeCmd(t, f, args...)
		if err != nil {
			t.Fatalf("%v error = %v", args, err)
		}
		if want := "would remove feat · git worktree · " + path + "\n"; out != want {
			t.Errorf("%v output = %q, want %q", args, out, want)
		}
	}
	assertIntact(t, f, path)
	if got := journaledJobs(t, h); len(got) != 0 {
		t.Errorf("journaled jobs = %v, want none", got)
	}
	if got := recoveryRefs(t, f); got != "" {
		t.Errorf("recovery refs = %q, want none", got)
	}
}

func TestCleanupWorktreeRmRefusals(t *testing.T) {
	requireCleanupDaemon(t)
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)

	dirty := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, "scratch.txt"), []byte("unsaved\n"), 0o600); err != nil {
			t.Fatalf("dirty %s: %v", path, err)
		}
	}
	locked := func(t *testing.T, path string) {
		t.Helper()
		mustRun(t, f.Env(), f.Dir, "git", "worktree", "lock", "--reason", "agent busy", path)
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T, path string)
		active bool
		dryRun bool
		reason string
	}{
		{name: "dirty", setup: dirty, reason: "dirty"},
		{name: "dirty-preview", setup: dirty, dryRun: true, reason: "dirty"},
		{name: "locked", setup: locked, reason: "locked"},
		{name: "active", active: true},
		{name: "active-preview", active: true, dryRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := addPoolWorktree(t, f, tt.name)
			if tt.setup != nil {
				tt.setup(t, path)
			}
			h.active.Store(tt.active)
			defer h.active.Store(false)
			args := []string{"rm", tt.name}
			if tt.dryRun {
				args = append(args, "--dry-run")
			}

			out, err := runWorktreeCmd(t, f, args...)
			if err == nil {
				t.Fatalf("%v removed a tree it must refuse: %q", args, out)
			}
			if tt.active {
				var active *cleanup.ActiveError
				if !errors.As(err, &active) || active.Worktree != path || len(active.Holders) != 1 || active.Holders[0].PID != testHolderPID {
					t.Fatalf("%v error = %v, want the guard's *cleanup.ActiveError for %s", args, err, path)
				}
			} else {
				var refused *cleanup.RefusedError
				if !errors.As(err, &refused) || refused.Reason != tt.reason || refused.Worktree != path {
					t.Fatalf("%v error = %v, want a %q *cleanup.RefusedError for %s", args, err, tt.reason, path)
				}
			}
			if out != "" {
				t.Errorf("%v output = %q, want none", args, out)
			}
			assertIntact(t, f, path)
		})
	}
	if got := journaledJobs(t, h); len(got) != 0 {
		t.Errorf("journaled jobs = %v, want none", got)
	}
	if got := recoveryRefs(t, f); got != "" {
		t.Errorf("recovery refs = %q, want none", got)
	}
}

func TestCleanupWorktreeRmForceDirty(t *testing.T) {
	requireCleanupDaemon(t)
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	path := addPoolWorktree(t, f, "feat")
	if err := os.WriteFile(filepath.Join(path, "scratch.txt"), []byte("discard me\n"), 0o600); err != nil {
		t.Fatalf("dirty worktree: %v", err)
	}

	out, err := runWorktreeCmd(t, f, "rm", "feat", "--force")
	if err != nil {
		t.Fatalf("rm --force error = %v", err)
	}
	id := queuedJobID(t, out)
	if want := "removed feat · git worktree · " + path + " · deletion queued " + id + "\n"; out != want {
		t.Errorf("rm --force output = %q, want %q", out, want)
	}
	if job := cleanupJob(t, f, h, id); !job.Force {
		t.Errorf("job force = false, want the --force the removal carried")
	}
	assertGone(t, path)
	if worktreeRegistered(t, f.Env(), f.Dir, path) {
		t.Errorf("git still registers %s, want it unregistered", path)
	}
}

type waitFailure struct {
	cleanup.Service
	err error
}

func (s waitFailure) Wait(context.Context, string) (cleanup.Job, error) { return cleanup.Job{}, s.err }

func TestCleanupWorktreeRmWaitFailure(t *testing.T) {
	requireCleanupDaemon(t)
	tests := []struct {
		name     string
		err      error
		cause    func(id string) string
		notFound bool
	}{
		{
			name:  "cancelled",
			err:   context.Canceled,
			cause: func(string) string { return "context canceled" },
		},
		{
			name:     "unknown job",
			err:      cleanup.ErrUnknownJob,
			cause:    func(id string) string { return "job " + id + " not found: " + cleanup.ErrUnknownJob.Error() },
			notFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t)
			f.Isolate(t)
			h := fixtureCleanup(t, f)
			f.Decorate(func(ctx context.Context) context.Context {
				return withCleanup(ctx, waitFailure{Service: h.engine, err: tt.err})
			})
			path := addPoolWorktree(t, f, "feat")

			out, err := runWorktreeCmd(t, f, "rm", "feat", "--wait")
			if err == nil {
				t.Fatalf("rm --wait succeeded, want the wait failure")
			}
			id := queuedJobID(t, err.Error())
			if want := "worktree rm: removed " + path + ", deletion queued " + id + "; wait: " + tt.cause(id); err.Error() != want {
				t.Errorf("rm --wait error = %q, want %q", err.Error(), want)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("errors.Is(err, %v) = false, want the wait failure wrapped", tt.err)
			}
			if got := errors.Is(err, ErrNotFound); got != tt.notFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v", got, tt.notFound)
			}
			if out != "" {
				t.Errorf("rm --wait output = %q, want none", out)
			}
			assertGone(t, path)
			if worktreeRegistered(t, f.Env(), f.Dir, path) {
				t.Errorf("git still registers %s, want it unregistered", path)
			}
		})
	}
}

func TestCleanupWorktreeRmDeferredJob(t *testing.T) {
	requireCleanupDaemon(t)
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	ws := filepath.Join(filepath.Dir(f.Dir), "checkouts", "conflict-feat")
	addLinkedWorktree(t, f.Env(), f.Dir, ws, "")
	h.active.Store(true)
	receipt, err := cleanupDeferWorkspace(f.Context(), filepath.Join(f.Dir, ".git"), ws, "stack replay", false)
	if err != nil {
		t.Fatalf("defer error = %v", err)
	}

	for _, args := range [][]string{{"rm", "--path", ws}, {"rm", "--path", ws, "--wait"}} {
		out, err := runWorktreeCmd(t, f, args...)
		want := &cleanup.RefusedError{Worktree: ws, Reason: "waiting", Detail: "deferred removal " + receipt.JobID + " still waits for the tree: " + receipt.Detail}
		var refused *cleanup.RefusedError
		if !errors.As(err, &refused) || *refused != *want {
			t.Fatalf("%v error = %v, want %v", args, err, want)
		}
		if out != "" {
			t.Errorf("%v output = %q, want none", args, out)
		}
	}
	assertIntact(t, f, ws)
	if got := journaledJobs(t, h); len(got) != 1 || got[0] != receipt.JobID {
		t.Errorf("journaled jobs = %v, want only the deferred %s", got, receipt.JobID)
	}
}

func TestCleanupWorktreeRmPathStaleRegistration(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	fixtureCleanup(t, f)
	tree := filepath.Join(filepath.Dir(f.Dir), "checkouts", "gone")
	addLinkedWorktree(t, f.Env(), f.Dir, tree, "")
	if err := os.RemoveAll(tree); err != nil {
		t.Fatalf("remove %s: %v", tree, err)
	}

	out, err := runWorktreeCmd(t, f, "rm", "--path", tree+"/")
	want := "worktree rm: this repository still registers " + tree + `, but nothing exists there — "git worktree prune" drops the stale registration`
	if err == nil || err.Error() != want {
		t.Fatalf("rm --path error = %v, want %q", err, want)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = true, want a registered path outside the not-found class")
	}
	if out != "" {
		t.Errorf("rm --path output = %q, want none", out)
	}
	if !worktreeRegistered(t, f.Env(), f.Dir, tree) {
		t.Errorf("git no longer registers %s, want the registration untouched", tree)
	}
}

func TestCleanupGitRelativePATH(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in the lookup must find
		t.Fatalf("write git: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(cwd, bin)
	if err != nil {
		t.Fatalf("relative %s: %v", bin, err)
	}
	ctx := render.WithEnv(context.Background(), "PATH="+rel)
	tree := filepath.Join(bin, "tree")
	resolves := func(prefix string) string {
		return prefix + `: git resolves to "` + filepath.Join(rel, "git") + `", not an absolute path`
	}

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "rm",
			call: func() error { _, err := queueGitWorktreeRemoval(ctx, tree, worktreeRmOptions{}); return err },
			want: resolves("worktree rm"),
		},
		{
			name: "dry-run",
			call: func() error { return previewGitWorktreeRemoval(ctx, tree, false) },
			want: resolves("worktree rm"),
		},
		{
			name: "defer",
			call: func() error {
				_, err := cleanupDeferWorkspace(ctx, filepath.Join(bin, ".git"), tree, "stack replay", false)
				return err
			},
			want: resolves("cleanup defer"),
		},
		{
			name: "adopt",
			call: func() error {
				cmd := newCleanupAdoptCmd() //nolint:contextcheck // ExecuteContext(ctx) below is what sets cmd's context
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetArgs([]string{
					"--source", tree, "--dev", "1", "--ino", "2", "--common-dir", filepath.Join(bin, ".git"),
					"--head", "0123456789abcdef0123456789abcdef01234567", "--recovery-ref", "refs/cleanup-worktrees/20260928/0123456789abcdef0123", "--original", tree,
				})
				return cmd.ExecuteContext(ctx)
			},
			want: resolves("cleanup adopt"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCleanupQueueCommands(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	ctx := f.Context()
	git := render.LookPath(ctx, "git")
	first := filepath.Join(filepath.Dir(f.Dir), "checkouts", "first")
	second := filepath.Join(filepath.Dir(f.Dir), "checkouts", "second")
	addLinkedWorktree(t, f.Env(), f.Dir, first, "")
	addLinkedWorktree(t, f.Env(), f.Dir, second, "")
	header := "cleanup daemon test · pid " + strconv.Itoa(os.Getpid()) + " · governor "

	if out, err := runCleanupCmd(t, f, "pause"); err != nil || out != "deletion paused\n" {
		t.Fatalf("pause = %q, %v; want %q", out, err, "deletion paused\n")
	}
	receipt, err := h.engine.Remove(ctx, cleanup.Request{Worktree: first, Git: git})
	if err != nil {
		t.Fatalf("remove %s: %v", first, err)
	}
	if receipt.State != cleanup.State(cleanup.PhaseUnregistered) {
		t.Fatalf("receipt state = %s, want %s", receipt.State, cleanup.PhaseUnregistered)
	}

	out, err := runCleanupCmd(t, f, "status")
	if err != nil {
		t.Fatalf("status error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], header) || !strings.HasSuffix(lines[0], " · deletion paused") {
		t.Fatalf("status = %q, want a paused header and one job", out)
	}
	if want := receipt.JobID + " · paused · " + first + " · 0 entries"; lines[1] != want {
		t.Errorf("status job line = %q, want %q", lines[1], want)
	}
	out, err = runCleanupCmd(t, f, "status", "--json")
	if err != nil {
		t.Fatalf("status --json error = %v", err)
	}
	var report cleanup.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if !report.Paused || report.Version != "test" || len(report.Jobs) != 1 || report.Jobs[0].ID != receipt.JobID || report.Jobs[0].Phase != cleanup.PhaseUnregistered {
		t.Errorf("report = %+v, want paused with job %s unregistered", report, receipt.JobID)
	}

	h.refuseRetire(errors.New("watcher still attached"))
	_, err = h.engine.Remove(ctx, cleanup.Request{Worktree: second, Git: git})
	var blocked *cleanup.BlockedError
	if !errors.As(err, &blocked) || blocked.Job.Blocked.Reason != "watchers" {
		t.Fatalf("remove %s error = %v, want a watchers *cleanup.BlockedError", second, err)
	}
	blockedID := blocked.Job.ID
	assertIntact(t, f, second)
	out, err = runCleanupCmd(t, f, "status", blockedID)
	if err != nil {
		t.Fatalf("status %s error = %v", blockedID, err)
	}
	lines = strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if want := blockedID + " · blocked · " + second + " · 0 entries · watchers: "; len(lines) != 2 || !strings.HasPrefix(lines[1], want) || !strings.Contains(lines[1], "watcher still attached") {
		t.Errorf("status %s = %q, want one line starting %q naming the watcher", blockedID, out, want)
	}
	out, err = runCleanupCmd(t, f, "status", "--limit", "1")
	if err != nil {
		t.Fatalf("status --limit 1 error = %v", err)
	}
	lines = strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 || lines[1] != receipt.JobID+" · paused · "+first+" · 0 entries" || lines[2] != "omitted 1 more job" {
		t.Errorf("status --limit 1 = %q, want the first job and one omitted", out)
	}

	h.refuseRetire(nil)
	out, err = runCleanupCmd(t, f, "retry", blockedID)
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if want := "retried " + blockedID + " · prepared · " + second + " · 0 entries\n"; out != want {
		t.Errorf("retry = %q, want %q", out, want)
	}
	if out, err := runCleanupCmd(t, f, "resume"); err != nil || out != "deletion resumed\n" {
		t.Fatalf("resume = %q, %v; want %q", out, err, "deletion resumed\n")
	}
	for _, job := range []struct{ id, path string }{{receipt.JobID, first}, {blockedID, second}} {
		out, err := runCleanupCmd(t, f, "wait", job.id)
		if err != nil {
			t.Fatalf("wait %s error = %v", job.id, err)
		}
		done := cleanupJob(t, f, h, job.id)
		if want := job.id + " · done · " + job.path + " · " + strconv.FormatUint(done.Removed, 10) + " entries\n"; out != want || done.Removed == 0 {
			t.Errorf("wait %s = %q (removed %d), want %q with entries counted", job.id, out, done.Removed, want)
		}
		assertGone(t, job.path)
		assertGone(t, h.layout.Payload(job.id))
	}

	const unknown = "0000000000000000-000000"
	for _, args := range [][]string{{"wait", unknown}, {"retry", unknown}, {"status", unknown}} {
		_, err := runCleanupCmd(t, f, args...)
		if !errors.Is(err, ErrNotFound) || !errors.Is(err, cleanup.ErrUnknownJob) || ExitCode(err) != 3 {
			t.Errorf("%v error = %v (exit %d), want not found at exit 3", args, err, ExitCode(err))
		}
	}
}

func TestCleanupDeferWorkspace(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	ws := filepath.Join(filepath.Dir(f.Dir), "checkouts", "conflict-feat")
	addLinkedWorktree(t, f.Env(), f.Dir, ws, "")
	head := strings.TrimSpace(mustRun(t, f.Env(), ws, "git", "rev-parse", "HEAD"))
	h.active.Store(true)

	receipt, err := cleanupDeferWorkspace(f.Context(), filepath.Join(f.Dir, ".git"), ws, "stack replay", false)
	if err != nil {
		t.Fatalf("defer error = %v", err)
	}
	want := cleanup.Receipt{
		JobID:       receipt.JobID,
		State:       cleanup.State(cleanup.PhaseWaiting),
		Original:    ws,
		RecoveryRef: cleanup.RecoveryRefFor(receipt.JobID),
		Detail:      ws + " is in use: claude (pid 4242 on a terminal) working in " + ws,
	}
	if receipt != want {
		t.Errorf("receipt = %+v, want %+v", receipt, want)
	}
	if job := cleanupJob(t, f, h, receipt.JobID); job.Phase != cleanup.PhaseWaiting || job.Owner != "stack replay" || !job.Deferred {
		t.Errorf("job phase %s owner %q deferred %v, want waiting for %q", job.Phase, job.Owner, job.Deferred, "stack replay")
	}
	assertIntact(t, f, ws)
	if got := recoveryRefs(t, f); got != "" {
		t.Errorf("recovery refs = %q, want none while the workspace waits", got)
	}

	h.active.Store(false)
	out, err := runCleanupCmd(t, f, "wait", receipt.JobID)
	if err != nil {
		t.Fatalf("wait error = %v", err)
	}
	if !strings.HasPrefix(out, receipt.JobID+" · done · "+ws+" · ") {
		t.Errorf("wait = %q, want the job done", out)
	}
	assertGone(t, ws)
	if worktreeRegistered(t, f.Env(), f.Dir, ws) {
		t.Errorf("git still registers %s, want it unregistered", ws)
	}
	if got, want := recoveryRefs(t, f), cleanup.RecoveryRefFor(receipt.JobID)+" "+head; got != want {
		t.Errorf("recovery refs = %q, want %q", got, want)
	}
}

func TestCleanupAdopt(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	h := fixtureCleanup(t, f)
	head := strings.TrimSpace(mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "HEAD"))
	const legacyID = "0123456789abcdef0123"
	source := filepath.Join(filepath.Dir(f.Dir), "cache", cleanup.LegacyQuarantinePrefix+"20260928", legacyID+"-feat")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("parked\n"), 0o600); err != nil {
		t.Fatalf("write quarantined file: %v", err)
	}
	ref := cleanup.LegacyRecoveryPrefix + "20260928/" + legacyID
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", ref, head)
	tree, _, err := cleanup.LstatID(source)
	if err != nil {
		t.Fatalf("lstat quarantine: %v", err)
	}
	adopt := func(oid string) []string {
		return []string{
			"adopt", "--source", source,
			"--dev", strconv.FormatUint(tree.Dev, 10), "--ino", strconv.FormatUint(tree.Ino, 10),
			"--common-dir", filepath.Join(f.Dir, ".git"), "--head", oid, "--recovery-ref", ref,
			"--original", filepath.Join(filepath.Dir(f.Dir), "old", "feat"),
		}
	}

	if _, err := runCleanupCmd(t, f, "adopt", "--source", source); err == nil || !strings.Contains(err.Error(), "required flag(s)") {
		t.Errorf("adopt with one flag error = %v, want the required flags named", err)
	}
	_, err = runCleanupCmd(t, f, adopt(strings.Repeat("0", len(head)))...)
	var refused *cleanup.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "recovery" || refused.Worktree != source {
		t.Fatalf("adopt with a foreign head error = %v, want a recovery *cleanup.RefusedError", err)
	}
	if got := readFileStr(t, filepath.Join(source, "f.txt")); got != "parked\n" {
		t.Errorf("quarantined f.txt = %q after the refusal, want it untouched", got)
	}
	if got := journaledJobs(t, h); len(got) != 0 {
		t.Errorf("journaled jobs = %v, want none", got)
	}

	out, err := runCleanupCmd(t, f, adopt(head)...)
	if err != nil {
		t.Fatalf("adopt error = %v", err)
	}
	id := queuedJobID(t, out)
	if want := "adopted " + legacyID + "-feat · legacy quarantine · " + source + " · deletion queued " + id + "\n"; out != want {
		t.Errorf("adopt output = %q, want %q", out, want)
	}
	assertGone(t, source)
	if _, err := runCleanupCmd(t, f, "wait", id); err != nil {
		t.Fatalf("wait %s error = %v", id, err)
	}
	job := cleanupJob(t, f, h, id)
	if !job.Adopted || job.Owner != legacyQuarantineOwner || job.Source != source || job.Phase != cleanup.PhaseDone {
		t.Errorf("job adopted %v owner %q source %q phase %s, want an adopted %q job done from %s",
			job.Adopted, job.Owner, job.Source, job.Phase, legacyQuarantineOwner, source)
	}
	if got := strings.TrimSpace(mustRun(t, f.Env(), f.Dir, "git", "rev-parse", ref)); got != head {
		t.Errorf("%s = %q after adoption, want it untouched at %q", ref, got, head)
	}
}

func TestCleanupRenderReport(t *testing.T) {
	at := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		report cleanup.Report
		want   string
	}{
		{
			name:   "empty",
			report: cleanup.Report{Version: "v1.2.3", PID: 77, Governor: cleanup.Governor{State: "idle"}},
			want:   "cleanup daemon v1.2.3 · pid 77 · governor idle · deletion running\nno cleanup jobs\n",
		},
		{
			name: "paused queue",
			report: cleanup.Report{
				Version:  "v1.2.3",
				PID:      77,
				Paused:   true,
				Governor: cleanup.Governor{State: "throttled", CPUPercent: 61.4},
				Jobs: []cleanup.Job{
					{ID: "a", Phase: cleanup.PhaseDeleting, Original: "/w/one", Removed: 1200},
					{ID: "b", Phase: cleanup.PhaseUnregistered, Original: "/w/two"},
					{ID: "c", Phase: cleanup.PhaseMoved, Original: "/w/three", Blocked: &cleanup.Blockage{Reason: "activity", Detail: "vim holds it", At: at}},
					{ID: "d", Phase: cleanup.PhaseWaiting, Original: "/w/four", Errors: []cleanup.JobError{{At: at, Phase: cleanup.PhaseQueued, Message: "old"}, {At: at, Phase: cleanup.PhaseWaiting, Message: "/w/four is in use"}}},
					{ID: "e", Phase: cleanup.PhaseDone, Original: "/w/five", Removed: 9},
				},
				Omitted: 3,
				Damaged: []cleanup.Damaged{{ID: "f", Error: "record truncated"}},
			},
			want: "cleanup daemon v1.2.3 · pid 77 · governor throttled at 61% cpu · deletion paused\n" +
				"a · paused · /w/one · 1200 entries\n" +
				"b · paused · /w/two · 0 entries\n" +
				"c · blocked · /w/three · 0 entries · activity: vim holds it\n" +
				"d · waiting · /w/four · 0 entries · /w/four is in use\n" +
				"e · done · /w/five · 9 entries\n" +
				"omitted 3 more jobs\n" +
				"damaged f · record truncated\n",
		},
		{
			name: "running with a blocked deletion",
			report: cleanup.Report{
				Version:  "dev",
				PID:      9,
				Governor: cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"},
				Jobs: []cleanup.Job{
					{ID: "a", Phase: cleanup.PhaseDeleting, Original: "/w/one", Removed: 5},
					{ID: "b", Phase: cleanup.PhaseDeleting, Original: "/w/two", Removed: 1, Blocked: &cleanup.Blockage{Reason: "delete", Detail: "nested mount", At: at}},
				},
				Omitted: 1,
			},
			want: "cleanup daemon dev · pid 9 · governor unavailable: fseventsd not found · deletion running\n" +
				"a · deleting · /w/one · 5 entries\n" +
				"b · blocked · /w/two · 1 entries · delete: nested mount\n" +
				"omitted 1 more job\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderCleanupReport(tt.report); got != tt.want {
				t.Errorf("render =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestCleanupEntryPointsUndecorated(t *testing.T) {
	tests := []struct {
		name string
		call func()
		want string
	}{
		{
			name: "service",
			call: func() { _, _ = cleanupService(context.Background()) },
			want: "cli: the cleanup daemon was reached on a context no test decorated",
		},
		{
			name: "preview",
			call: func() { _, _ = cleanupPreview(context.Background())(context.Background(), cleanup.Request{}) },
			want: "cli: the cleanup preflight was reached on a context no test decorated",
		},
		{
			name: "serve",
			call: func() {
				cmd := newCleanupServeCmd() //nolint:contextcheck // ExecuteContext below is what sets cmd's context
				cmd.SetArgs([]string{})
				_ = cmd.ExecuteContext(context.Background())
			},
			want: "cli: the cleanup daemon was served from a test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if got := recover(); got != tt.want {
					t.Errorf("recover() = %v, want %q", got, tt.want)
				}
			}()
			tt.call()
			t.Errorf("the %s entry point returned in a test", tt.name)
		})
	}
}
