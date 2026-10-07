package cli

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func runVcsPushCmd(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsPushCmd() //nolint:contextcheck // ExecuteContext(ctx) below is what sets cmd's context; contextcheck cannot see through cobra's two-step wiring
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return strings.TrimSpace(out.String()), err
}

func pushCommit(t *testing.T, f *vcstest.Fixture, name, content, message string) string {
	t.Helper()
	writeShipFile(t, f.Dir, name, content)
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", message)
	return shipHead(t, f)
}

// pushCount counts the pushes ccx itself ran, the measure of whether a refusal
// retried.
func pushCount(t *testing.T, f *vcstest.Fixture) int {
	t.Helper()
	n := 0
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "push" {
			n++
		}
	}
	return n
}

func TestVcsPushFastForward(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	tip := shipHead(t, f)
	head := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "pushed main → origin · " + shortOID(tip) + ".." + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "main"); remote != head {
		t.Errorf("origin main = %s, want %s", remote, head)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		for _, arg := range inv {
			if strings.HasPrefix(arg, "--force") {
				t.Fatalf("a fast-forward push carried %q", arg)
			}
		}
	}
}

// TestVcsPushIgnoresConfiguredRefspec pins the push to the commit it graded and
// the branch it graded it against: a configured remote.<name>.push would
// otherwise redirect a bare branch argument at another ref, with a force this
// run never decided on.
func TestVcsPushIgnoresConfiguredRefspec(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main:victim")
	mustRun(t, f.Env(), f.Dir, "git", "config", "remote.origin.push", "+refs/heads/main:refs/heads/victim")
	victim := pushCommit(t, f, "v.txt", "v\n", "test: 🧪 victim")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "--force", "origin", "HEAD:victim")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "HEAD~1")
	head := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	shipResetLog(t, f)

	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("push error = %v", err)
	}
	if remote := shipRemoteTip(t, f, "origin", "main"); remote != head {
		t.Errorf("origin main = %s, want %s", remote, head)
	}
	if remote := shipRemoteTip(t, f, "origin", "victim"); remote != victim {
		t.Errorf("origin victim = %s, want the untouched %s", remote, victim)
	}
}

func TestVcsPushCreatesRemoteBranch(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	head := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "pushed feat → origin · created origin/feat at " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
}

// TestVcsPushFetchesOnlyTheBranchItMoves configures origin to fetch a branch the
// remote has since deleted, which fails a bare git fetch origin with "couldn't
// find remote ref" before push grades anything.
func TestVcsPushFetchesOnlyTheBranchItMoves(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main", "main:gone")
	mustRun(t, f.Env(), f.Dir, "git", "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "config", "--add", "remote.origin.fetch", "+refs/heads/gone:refs/remotes/origin/gone")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "gone")
	tip := shipHead(t, f)
	head := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "pushed main → origin · " + shortOID(tip) + ".." + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "main"); remote != head {
		t.Errorf("origin main = %s, want %s", remote, head)
	}
}

// TestVcsPushServerRestackWithNoRemoteHead restacks feat onto a newer main on
// the server, in a clone with no refs/remotes/origin/HEAD: the remote names its
// trunk, so main's commit is not counted as work feat never held.
func TestVcsPushServerRestackWithNoRemoteHead(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feat")
	clone := filepath.Join(filepath.Dir(f.Dir), "upstream")
	mustRun(t, f.Env(), filepath.Dir(f.Dir), "git", "clone", "-q", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "config", "user.email", "t@t.t")
	mustRun(t, f.Env(), clone, "git", "config", "user.name", "t")
	writeShipFile(t, clone, "c.txt", "c\n")
	mustRun(t, f.Env(), clone, "git", "add", "-A")
	mustRun(t, f.Env(), clone, "git", "commit", "-qm", "test: 🧪 c")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), clone, "git", "switch", "-q", "feat")
	mustRun(t, f.Env(), clone, "git", "rebase", "-q", "main")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "--force", "origin", "feat")
	tip := gitAt(t, f.Env(), clone, "rev-parse", "HEAD")
	head := pushCommit(t, f, "b.txt", "b\n", "test: 🧪 b")
	mustRun(t, f.Env(), f.Dir, "git", "remote", "set-head", "origin", "-d")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "force-pushed feat → origin · replaced " + shortOID(tip) + " with " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
}

// vcsPushQueueReplay is #31266: parent squash-landed as "test: 🧪 parent
// (#7)", the merge queue then replayed parent's own commit onto the new trunk
// beneath feat's, and feat itself already sits on that trunk without it. It
// returns the queue's head and feat's.
func vcsPushQueueReplay(t *testing.T, f *vcstest.Fixture, landed string) (string, string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "parent")
	pushCommit(t, f, "q.txt", "queue\n", "test: 🧪 parent")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	pushCommit(t, f, "b.txt", "b\n", "test: 🧪 feat")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "parent", "feat")
	clone := filepath.Join(filepath.Dir(f.Dir), "upstream")
	mustRun(t, f.Env(), filepath.Dir(f.Dir), "git", "clone", "-q", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "config", "user.email", "t@t.t")
	mustRun(t, f.Env(), clone, "git", "config", "user.name", "t")
	writeShipFile(t, clone, "q.txt", "landed\nqueue\n")
	mustRun(t, f.Env(), clone, "git", "add", "-A")
	mustRun(t, f.Env(), clone, "git", "commit", "-qm", landed)
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "main")
	writeShipFile(t, clone, "q.txt", "landed\nqueue\nqueue\n")
	mustRun(t, f.Env(), clone, "git", "add", "-A")
	mustRun(t, f.Env(), clone, "git", "commit", "-q", "-C", "origin/parent")
	mustRun(t, f.Env(), clone, "git", "cherry-pick", "origin/feat")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "--force", "origin", "HEAD:feat")
	tip := gitAt(t, f.Env(), clone, "rev-parse", "HEAD")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "origin/main", "parent", "feat")
	return tip, shipHead(t, f)
}

func TestVcsPushDropsTheQueuesReplayOfALandedParent(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	tip, head := vcsPushQueueReplay(t, f, "test: 🧪 parent (#7)")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "force-pushed feat → origin · replaced " + shortOID(tip) + " with " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
}

// TestVcsPushRefusesAReplayOfWorkTrunkNeverLanded keeps the refusal when no
// squash on trunk names the replayed commit: trunk landed other work.
func TestVcsPushRefusesAReplayOfWorkTrunkNeverLanded(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	tip, _ := vcsPushQueueReplay(t, f, "test: 🧪 other (#7)")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err == nil || !strings.Contains(err.Error(), "divergence, not a rewrite") {
		t.Fatalf("push = %q, %v, want the divergence refused", got, err)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != tip {
		t.Errorf("origin feat = %s, want the queue's %s left in place", remote, tip)
	}
}

// TestVcsPushRecreatesABranchTheRemoteDeleted leaves origin/feat behind after
// origin deletes feat: the stale ref is no lease to force against, so push
// creates the branch again.
func TestVcsPushRecreatesABranchTheRemoteDeleted(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feat")
	mustRun(t, f.Env(), f.Dir, "git", "--git-dir="+f.RemoteDir, "update-ref", "-d", "refs/heads/feat")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "test: 🧪 a, rewritten")
	head := shipHead(t, f)
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "pushed feat → origin · created origin/feat at " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
}

func TestVcsPushUpToDate(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	head := shipHead(t, f)
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "origin/main already at " + shortOID(head) + " — nothing to push"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := pushCount(t, f); n != 0 {
		t.Errorf("pushes = %d, want 0", n)
	}
}

// TestVcsPushRewriteLeasesTheHeadItGraded rewrites the branch the way a rebase
// does — a new base, new shas, the same work — and asserts the remote lands on
// the rewritten head under a lease pinned to the head this run graded.
func TestVcsPushRewriteLeasesTheHeadItGraded(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	base := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	tip := pushCommit(t, f, "b.txt", "b\n", "test: 🧪 b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feat")

	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	newBase := pushCommit(t, f, "c.txt", "c\n", "test: 🧪 c")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feat")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", newBase, base, "feat")
	head := shipHead(t, f)
	if head == tip {
		t.Fatal("the rebase left feat where the remote already holds it")
	}
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "force-pushed feat → origin · replaced " + shortOID(tip) + " with " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
	lease := "--force-with-lease=refs/heads/feat:" + tip
	leased := false
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		for _, arg := range inv {
			if arg == lease {
				leased = true
			}
		}
	}
	if !leased {
		t.Errorf("no push carried %q; invocations: %v", lease, vcstest.Invocations(t, f.ArgvLog))
	}
}

// TestVcsPushRestackSendsASelfContainedPack restacks a branch whose file the
// new base also edited, so the rebased blob deltas against blobs the remote
// holds. A thin pack sends that delta without its base, and a remote missing
// the base refuses the push.
func TestVcsPushRestackSendsASelfContainedPack(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	var lines strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	pushCommit(t, f, "big.txt", lines.String(), "test: 🧪 big")
	base := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feat")
	tip := pushCommit(t, f, "big.txt", lines.String()+"feat\n", "test: 🧪 feat")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feat")

	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	newBase := pushCommit(t, f, "big.txt", "trunk\n"+lines.String(), "test: 🧪 trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feat")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", newBase, base, "feat")
	head := shipHead(t, f)
	shipThinRejectingRemote(t, f)
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := "force-pushed feat → origin · replaced " + shortOID(tip) + " with " + shortOID(head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feat"); remote != head {
		t.Errorf("origin feat = %s, want %s", remote, head)
	}
}

// TestVcsPushDivergenceRefuses gives the remote a commit the branch has never
// held, which no reflog entry of the branch reaches: a divergence, and a force
// would drop it.
func TestVcsPushDivergenceRefuses(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
	head := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err == nil {
		t.Fatalf("expected a refusal, got summary %q", got)
	}
	for _, want := range []string{"origin/main carries 1 commit(s) main has never held", "divergence, not a rewrite", "upstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if remote := shipRemoteTip(t, f, "origin", "main"); remote == head {
		t.Error("origin main carries the refused head")
	}
	if n := pushCount(t, f); n != 0 {
		t.Errorf("pushes = %d, want 0", n)
	}
}

// TestVcsPushStaleLeaseNeverRetries plays the concurrent session the lease exists
// for: origin advances between the fetch that graded the rewrite and the push,
// so the lease is refused and the refusal is reported, not replayed.
func TestVcsPushStaleLeaseNeverRetries(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	tip := pushCommit(t, f, "a.txt", "a\n", "test: 🧪 a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "test: 🧪 a, rewritten")
	head := shipHead(t, f)
	shipRaceRemote(t, f, "git", "push*", "r.txt", 1)
	shipResetLog(t, f)

	got, err := runVcsPushCmd(f.Context(), t)
	if err == nil {
		t.Fatalf("expected a refusal, got summary %q", got)
	}
	for _, want := range []string{"origin/main moved off " + shortOID(tip), "fetch and reconcile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if remote := shipRemoteTip(t, f, "origin", "main"); remote == head {
		t.Error("origin main carries the head the lease refused")
	}
	if n := pushCount(t, f); n != 1 {
		t.Errorf("pushes = %d, want 1 — a refused lease is reported, never retried", n)
	}
}

// TestVcsPushRetracksABranchRebasedOffItsParent is iam-check's repair by hand:
// the branch was rebased onto trunk outside gt, dropping the parent gt recorded,
// and after push gt called it diverged and refused to track a branch onto it.
func TestVcsPushRetracksABranchRebasedOffItsParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "c")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "main", "a", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	head := pushCommit(t, f, "c2.txt", "c2\n", "test: 🧪 c2")
	fork := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")

	got, err := runVcsPushCmd(f.Context(), t)
	if err != nil {
		t.Fatalf("push error = %v", err)
	}
	if want := " · re-tracked c onto main at " + shortOID(fork); !strings.HasSuffix(got, want) {
		t.Errorf("summary = %q, want it to end %q", got, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "c"); remote != head {
		t.Errorf("origin c = %s, want %s", remote, head)
	}
	if parent := stackParent(t, f, "c"); parent != "main" {
		t.Errorf("gt parent of c = %s, want main", parent)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "d")
	pushCommit(t, f, "d.txt", "d\n", "test: 🧪 d")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "d", "--parent", "c", "--no-interactive")
}
