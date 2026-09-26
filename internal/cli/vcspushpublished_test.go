package cli

import "testing"

// TestVcsPushTakesOverItsOwnIsolatedPublication is rv2-riza-cache-derive:
// ccx vcs stack submit replayed the lane's branch onto newer trunk on the
// remote only, and the next ccx vcs push refused that replay as "a divergence,
// not a rewrite".
func TestVcsPushTakesOverItsOwnIsolatedPublication(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stubGTAPI(t)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if gitAt(t, f.Env(), f.Dir, "rev-parse", "feature") == gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature") {
		t.Fatal("fixture: stack submit moved the local branch, so the remote is not an isolated publication")
	}
	pushCommit(t, f, "next.txt", "next\n", "next")

	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push over its own publication = %v", err)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != want {
		t.Errorf("origin feature = %s, want the pushed local head %s", got, want)
	}
}

func TestVcsPushTakesOverAServerRestackOfItsOwnCommits(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stubGTAPI(t)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	server := f.WorktreePath("server")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", server, "main")
	mustRun(t, f.Env(), server, "git", "cherry-pick", "base")
	mustRun(t, f.Env(), server, "git", "push", "-q", "origin", "HEAD:main")
	mustRun(t, f.Env(), server, "git", "cherry-pick", "feature")
	mustRun(t, f.Env(), server, "git", "push", "-qf", "origin", "HEAD:feature")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	pushCommit(t, f, "next.txt", "next\n", "next")

	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push over a server restack of its own commits = %v", err)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != want {
		t.Errorf("origin feature = %s, want the pushed local head %s", got, want)
	}
}
