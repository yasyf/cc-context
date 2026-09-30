package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackAssertCleared(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	if states, _ := filepath.Glob(filepath.Join(f.Dir, ".git", stackRebaseStateDir, "*", stackRebaseState)); len(states) != 0 {
		t.Errorf("run state left behind: %q", states)
	}
}

func stackReaddWorkspace(t *testing.T, f *vcstest.Fixture, ws string) (kept, marker string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "remove", "--force", ws)
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", ws, "HEAD")
	marker = filepath.Join(ws, "untracked.txt")
	if err := os.WriteFile(marker, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "", marker
}

func stackDisplaceWorkspace(t *testing.T, f *vcstest.Fixture, ws string) (kept, marker string) {
	t.Helper()
	kept, marker = ws+"-kept", filepath.Join(ws, "plain.txt")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "move", ws, kept)
	if err := os.Mkdir(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return kept, marker
}

func stackAssertReplacementKept(t *testing.T, f *vcstest.Fixture, ws, marker string, replacement cleanup.Registration) {
	t.Helper()
	assertIntact(t, f, ws)
	if got := readFileStr(t, marker); got != "replacement\n" {
		t.Errorf("%s = %q, want the replacement's untracked file kept", marker, got)
	}
	if got := observedWorkspace(t, ws); got != replacement {
		t.Errorf("registration at %s = %+v, want the replacement %+v", ws, got, replacement)
	}
}

func stackAssertOriginalKept(t *testing.T, f *vcstest.Fixture, kept string, original cleanup.Registration) {
	t.Helper()
	assertIntact(t, f, kept)
	if got := observedWorkspace(t, kept); got != original {
		t.Errorf("registration at %s = %+v, want the moved original %+v", kept, got, original)
	}
}

// TestStackAbortLeavesAReplacedWorkspaceAlone pins the reused-path hazard: an
// abort whose receipt was lost keeps the run, and once another worktree sits
// at the workspace path the retry finishes by the saved registration instead
// of deleting whatever the path holds now.
func TestStackAbortLeavesAReplacedWorkspaceAlone(t *testing.T) {
	requireCleanupDaemon(t)
	errLost := errors.New("read the receipt: connection reset by peer")
	for _, tt := range []struct {
		name         string
		replace      func(t *testing.T, f *vcstest.Fixture, ws string) (kept, marker string)
		sameAdminDir bool
	}{
		{"moved aside", replaceWorkspace, false},
		{"re-added under the same admin dir", stackReaddWorkspace, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, ws := stackResolvedConflict(t)
			stackDeferTo(t, f, cleanup.Receipt{}, errLost)
			registration := stackRunConflict(t, f).Registration
			if _, _, err := runStackCmd(t, f, "abort"); !errors.Is(err, errLost) {
				t.Fatalf("abort = %v, want the lost receipt to keep the run", err)
			}
			if c := stackRunConflict(t, f); c.Workspace != ws || c.Registration != registration {
				t.Fatalf("run conflict = %+v after the uncertain abort, want %s with %+v", c, ws, registration)
			}
			kept, marker := tt.replace(t, f, ws)
			replacement := observedWorkspace(t, ws)
			if replacement == registration || (replacement.AdminDir == registration.AdminDir) != tt.sameAdminDir {
				t.Fatalf("replacement registration = %+v against the original %+v, want a different tree with the same admin dir = %v", replacement, registration, tt.sameAdminDir)
			}
			h := fixtureCleanup(t, f)
			shipResetLog(t, f)

			out, _, err := runStackCmd(t, f, "abort")
			if err != nil {
				t.Fatalf("abort over the replacement: %v", err)
			}
			if want := "aborted · no branch moved" + shipSep + stackMismatchLine(ws); out != want {
				t.Errorf("abort output = %q, want %q", out, want)
			}
			if jobs := journaledJobs(t, h); len(jobs) != 0 {
				t.Errorf("journaled jobs = %v, want none for a replaced path", jobs)
			}
			stackAssertReplacementKept(t, f, ws, marker, replacement)
			if kept != "" {
				stackAssertOriginalKept(t, f, kept, registration)
			}
			if removals := stackRemovals(t, f); len(removals) != 0 {
				t.Errorf("abort removed worktrees itself: %q", removals)
			}
			stackAssertCleared(t, f)
		})
	}
}

type lostReceiptCleanup struct {
	cleanup.Service
	err   error
	first cleanup.Receipt
	lost  bool
}

func (s *lostReceiptCleanup) Defer(ctx context.Context, r cleanup.DeferRequest) (cleanup.Receipt, error) {
	receipt, err := s.Service.Defer(ctx, r)
	if err != nil || s.lost {
		return receipt, err
	}
	s.first, s.lost = receipt, true
	return cleanup.Receipt{}, s.err
}

// TestStackAbortRejoinsTheLandedJobOverAReplacedWorkspace pins the other half
// of a lost receipt: when the daemon did journal the removal, the retry gets
// that job back by the saved registration, and the job itself blocks on
// identity rather than deleting the worktree that replaced the original.
func TestStackAbortRejoinsTheLandedJobOverAReplacedWorkspace(t *testing.T) {
	requireCleanupDaemon(t)
	f, ws := stackResolvedConflict(t)
	h := fixtureCleanup(t, f)
	h.active.Store(true)
	lossy := &lostReceiptCleanup{Service: h.engine, err: errors.New("read the receipt: connection reset by peer")}
	f.Decorate(func(ctx context.Context) context.Context { return withCleanup(ctx, lossy) })
	registration := stackRunConflict(t, f).Registration

	if _, _, err := runStackCmd(t, f, "abort"); !errors.Is(err, lossy.err) {
		t.Fatalf("abort = %v, want the lost receipt to keep the run", err)
	}
	first := lossy.first
	if first.State != cleanup.State(cleanup.PhaseWaiting) {
		t.Fatalf("first receipt = %+v, want a waiting job", first)
	}
	if got := journaledJobs(t, h); !slices.Equal(got, []string{first.JobID}) {
		t.Fatalf("journaled jobs = %v, want only %s", got, first.JobID)
	}
	if c := stackRunConflict(t, f); c.Workspace != ws || c.Registration != registration {
		t.Fatalf("run conflict = %+v after the uncertain abort, want %s with %+v", c, ws, registration)
	}
	kept, marker := replaceWorkspace(t, f, ws)
	replacement := observedWorkspace(t, ws)

	out, _, err := runStackCmd(t, f, "abort")
	if err != nil {
		t.Fatalf("abort over the replacement: %v", err)
	}
	if want := "aborted · no branch moved" + shipSep + "handed " + ws + " to cleanup job " + first.JobID + ", waiting"; !strings.HasPrefix(out, want) {
		t.Errorf("abort output = %q, want it to start with %q", out, want)
	}
	if got := journaledJobs(t, h); !slices.Equal(got, []string{first.JobID}) {
		t.Errorf("journaled jobs = %v, want only the original %s", got, first.JobID)
	}
	stackAssertCleared(t, f)

	h.active.Store(false)
	ctx, cancel := context.WithTimeout(f.Context(), 10*time.Second)
	defer cancel()
	job, err := h.engine.Wait(ctx, first.JobID)
	var blocked *cleanup.BlockedError
	if !errors.As(err, &blocked) || job.Blocked == nil || job.Blocked.Reason != "identity" {
		t.Fatalf("wait = %+v, %v; want the job blocked on identity", job, err)
	}
	if bound := (cleanup.Registration{Tree: job.Tree, AdminDir: job.AdminDir, Admin: job.Admin}); bound != registration {
		t.Errorf("job is bound to %+v, want the original %+v", bound, registration)
	}
	stackAssertReplacementKept(t, f, ws, marker, replacement)
	stackAssertOriginalKept(t, f, kept, registration)
}

type stackWorkspaceSnapshot struct {
	head, index, marker string
	rebasing            bool
}

func stackSnapshotWorkspace(t *testing.T, f *vcstest.Fixture, ws, marker string) stackWorkspaceSnapshot {
	t.Helper()
	index := gitAt(t, f.Env(), ws, "rev-parse", "--path-format=absolute", "--git-path", "index")
	return stackWorkspaceSnapshot{
		head:     gitAt(t, f.Env(), ws, "rev-parse", "HEAD"),
		index:    readFileStr(t, index),
		marker:   readFileStr(t, marker),
		rebasing: stackRebasing(f.Context(), render.Dir(ws)),
	}
}

func stackRefusesTheReplacedWorkspace(t *testing.T, f *vcstest.Fixture, ws, verb, prefix string) {
	t.Helper()
	registration := stackRunConflict(t, f).Registration
	kept, marker := replaceWorkspace(t, f, ws)
	before := stackSnapshotWorkspace(t, f, ws, marker)
	if before.rebasing {
		t.Fatalf("the replacement at %s has a rebase in progress", ws)
	}

	_, _, err := runStackCmd(t, f, verb)
	if want := prefix + ": " + stackMismatchLine(ws) + " — ccx vcs stack abort, then re-run"; err == nil || err.Error() != want {
		t.Fatalf("%s = %v, want %q", verb, err, want)
	}
	if after := stackSnapshotWorkspace(t, f, ws, marker); after != before {
		t.Errorf("%s touched the replacement: %+v, want %+v", verb, after, before)
	}
	if c := stackRunConflict(t, f); c.Workspace != ws || c.Registration != registration {
		t.Errorf("run conflict = %+v, want it kept naming %s with %+v", c, ws, registration)
	}
	stackAssertOriginalKept(t, f, kept, registration)
}

// TestStackContinueAndRegenerateRefuseAReplacedWorkspace pins that the two
// commands that read and mutate a stopped workspace check its registration
// before their first git call there.
func TestStackContinueAndRegenerateRefuseAReplacedWorkspace(t *testing.T) {
	t.Parallel()
	t.Run("continue", func(t *testing.T) {
		t.Parallel()
		f, ws := stackResolvedConflict(t)
		stackRefusesTheReplacedWorkspace(t, f, ws, "continue", stackRebasePrefix)
	})
	t.Run("regenerate", func(t *testing.T) {
		t.Parallel()
		cmd, marker := regenMarked(t, regenCat)
		f := regenRepo(t, cmd)
		regenBranch(t, f, map[string]string{"src/a.txt": "a-feature\n", "src/f.txt": "f\n", "gen/out.txt": "a-feature\nf\n"})
		regenAdvanceTrunk(t, f, [2]string{"src/a.txt", "a-trunk\n"}, [2]string{"gen/out.txt", "a-trunk\n"})
		ws, _ := regenStop(t, f)
		stackRefusesTheReplacedWorkspace(t, f, ws, "regenerate", "stack regenerate")
		if n := regenRuns(t, marker); n != 0 {
			t.Errorf("the generator ran %d time(s) in the replacement, want none", n)
		}
	})
}

// TestStackRepeatedStopsKeepTheFirstRegistration pins that a workspace whose
// rebase stops more than once is registered exactly once, at creation, and
// every later save carries that same registration.
func TestStackRepeatedStopsKeepTheFirstRegistration(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, f, nil)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	for _, name := range []string{"c.txt", "d.txt"} {
		writeShipFile(t, f.Dir, name, "feature\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name)
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature "+name)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	restackAdvanceRemote(t, f, "main", "d.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--ff-only", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "conflicted files:\n  c.txt\n") {
		t.Fatalf("stack rebase = %v, want the first stop on c.txt", err)
	}
	ws := stackWorkspaceOf(t, err)
	first := stackRunConflict(t, f).Registration
	if observed := observedWorkspace(t, ws); first != observed {
		t.Fatalf("saved registration = %+v, want the one observed at %s: %+v", first, ws, observed)
	}

	writeShipFile(t, ws, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	_, _, err = runStackCmd(t, f, "continue")
	if err == nil || !strings.Contains(err.Error(), "conflicted files:\n  d.txt\n") {
		t.Fatalf("continue = %v, want the second stop on d.txt", err)
	}
	if got := stackWorkspaceOf(t, err); got != ws {
		t.Fatalf("second stop in %s, want the same workspace %s", got, ws)
	}
	if got := stackRunConflict(t, f).Registration; got != first || got != observedWorkspace(t, ws) {
		t.Errorf("saved registration after the second stop = %+v, want the first %+v", got, first)
	}

	writeShipFile(t, ws, "d.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), ws, "git", "add", "d.txt")
	out, _, err := runStackCmd(t, f, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !strings.Contains(out, "resolved feature") {
		t.Errorf("continue output = %q, want feature resolved", out)
	}
	if !stackOnto(t, f, "base", "feature") || !stackOnto(t, f, "main", "base") {
		t.Error("the stack did not land on the new trunk")
	}
	for _, name := range []string{"c.txt", "d.txt"} {
		if got := gitAt(t, f.Env(), f.Dir, "show", "feature:"+name); got != "trunk\nfeature" {
			t.Errorf("feature's %s = %q, want the resolution", name, got)
		}
	}
	sparseAssertWorkspace(t, f, ws, false)
	stackAssertCleared(t, f)
}

// TestStackLegacyConflictRecordIsLeftAlone pins what happens to a run an older
// ccx stopped, which saved no registration: nothing reads or removes its
// workspace, since it cannot be told from another worktree at that path, and
// abort goes through once the path is empty.
func TestStackLegacyConflictRecordIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	ws := stackPlantWorkspace(t, f)
	stackPlantConflict(t, f, time.Minute, &stackConflict{Branch: "feature", Workspace: ws}, "base")
	stackPlantBranches(t, f, "base", stackRebaseBranch{Name: "feature"})
	svc := stackDeferTo(t, f, cleanup.Receipt{}, cleanup.ErrUnsupported)

	for _, verb := range []string{"abort", "continue", "regenerate"} {
		_, _, err := runStackCmd(t, f, verb, "--stack", "base")
		if err == nil || !strings.Contains(err.Error(), " — remove it yourself with ccx vcs worktree rm --path "+ws+", then run ccx vcs stack abort") {
			t.Fatalf("%s = %v, want the refusal naming ccx vcs worktree rm --path %s", verb, err, ws)
		}
		assertIntact(t, f, ws)
		if c := stackRunConflict(t, f); c == nil || c.Workspace != ws || c.Registration != (cleanup.Registration{}) {
			t.Errorf("run conflict after %s = %+v, want the legacy record kept", verb, c)
		}
	}
	if asked := svc.asked(); len(asked) != 0 {
		t.Errorf("defer requests = %+v, want none for a record without a registration", asked)
	}

	mustRun(t, f.Env(), f.Dir, "git", "worktree", "remove", "--force", ws)
	out, _, err := runStackCmd(t, f, "abort", "--stack", "base")
	if err != nil {
		t.Fatalf("abort once the workspace is gone: %v", err)
	}
	want := "aborted · no branch moved"
	if cleanupDaemonized {
		want += shipSep + ws + " was already gone"
	}
	if out != want {
		t.Errorf("abort output = %q, want %q", out, want)
	}
	if asked := svc.asked(); len(asked) != 0 {
		t.Errorf("defer requests = %+v, want none for a path already gone", asked)
	}
	stackAssertCleared(t, f)
}

// TestStackAbortLeavesAReplacedWorkspaceAloneInline pins the synchronous path:
// where no daemon runs, abort compares the registration itself and neither
// moves, prunes, nor deletes a worktree that replaced the original.
func TestStackAbortLeavesAReplacedWorkspaceAloneInline(t *testing.T) {
	t.Parallel()
	f, ws := stackResolvedConflict(t)
	registration := stackRunConflict(t, f).Registration
	kept, marker := replaceWorkspace(t, f, ws)
	replacement := observedWorkspace(t, ws)
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "abort")
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if want := "aborted · no branch moved" + shipSep + stackMismatchLine(ws); out != want {
		t.Errorf("abort output = %q, want %q", out, want)
	}
	stackAssertReplacementKept(t, f, ws, marker, replacement)
	stackAssertOriginalKept(t, f, kept, registration)
	if aside, _ := filepath.Glob(filepath.Join(filepath.Dir(ws), "."+filepath.Base(ws)+".discarded-*")); len(aside) != 0 {
		t.Errorf("abort moved a tree aside: %q", aside)
	}
	if removals := stackRemovals(t, f); len(removals) != 0 {
		t.Errorf("abort pruned or removed worktrees: %q", removals)
	}
	stackAssertCleared(t, f)
}

// TestStackAbortKeepsTheRunWhenTheWorkspaceCannotBeInspected pins that only a
// definite mismatch lets abort finish: a path that holds no worktree at all is
// an inspection failure, and the run stays for a retry.
func TestStackAbortKeepsTheRunWhenTheWorkspaceCannotBeInspected(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		f, ws := stackResolvedConflict(t)
		registration := stackRunConflict(t, f).Registration
		kept, marker := stackDisplaceWorkspace(t, f, ws)
		shipResetLog(t, f)

		_, _, err := runStackCmd(t, f, "abort")
		if err == nil || !strings.HasPrefix(err.Error(), "stack rebase: cleanup observe "+ws+": ") || !strings.HasSuffix(err.Error(), "; the run is kept, so abort can run again") {
			t.Fatalf("abort = %v, want the inspection failure to keep the run", err)
		}
		if c := stackRunConflict(t, f); c.Workspace != ws || c.Registration != registration {
			t.Errorf("run conflict = %+v, want it kept naming %s with %+v", c, ws, registration)
		}
		if got := readFileStr(t, marker); got != "plain\n" {
			t.Errorf("%s = %q, want the directory untouched", marker, got)
		}
		if removals := stackRemovals(t, f); len(removals) != 0 {
			t.Errorf("abort pruned or removed worktrees: %q", removals)
		}
		stackAssertOriginalKept(t, f, kept, registration)
	})
	t.Run("daemon", func(t *testing.T) {
		requireCleanupDaemon(t)
		f, ws := stackResolvedConflict(t)
		h := fixtureCleanup(t, f)
		registration := stackRunConflict(t, f).Registration
		kept, marker := stackDisplaceWorkspace(t, f, ws)

		_, _, err := runStackCmd(t, f, "abort")
		var refused *cleanup.RefusedError
		if !errors.As(err, &refused) || refused.Reason != "unregistered" || refused.Worktree != ws {
			t.Fatalf("abort = %v, want the daemon's unregistered refusal", err)
		}
		if want := " — nothing was removed, and " + ws + " is left where it is; the run is kept, so abort can run again"; !strings.HasSuffix(err.Error(), want) {
			t.Errorf("abort = %v, want it to end with %q", err, want)
		}
		if jobs := journaledJobs(t, h); len(jobs) != 0 {
			t.Errorf("journaled jobs = %v, want none", jobs)
		}
		if c := stackRunConflict(t, f); c.Workspace != ws || c.Registration != registration {
			t.Errorf("run conflict = %+v, want it kept naming %s with %+v", c, ws, registration)
		}
		if got := readFileStr(t, marker); got != "plain\n" {
			t.Errorf("%s = %q, want the directory untouched", marker, got)
		}
		stackAssertOriginalKept(t, f, kept, registration)
	})
}
