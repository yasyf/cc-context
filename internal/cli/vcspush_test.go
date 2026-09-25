package cli

import (
	"bytes"
	"context"
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
