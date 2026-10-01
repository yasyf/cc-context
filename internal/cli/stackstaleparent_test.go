package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackRebaseStartsAtTheParentHeadOnlyTheRemoteReflogRemembers(t *testing.T) {
	f := stackStaleParentRepo(t)

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "base..feature"); n != "1" {
		t.Errorf("feature holds %s commits over base, want its own 1", n)
	}
	if !stackOnto(t, f, "base", "feature") {
		t.Error("feature is not on base")
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "feature", "--not", "refs/remotes/origin/main"); n != "2" {
		t.Errorf("feature holds %s commits over trunk, want base's 1 and its own 1", n)
	}
}

func TestStackRebaseRefusesABranchNoReflogPlacesOnItsParent(t *testing.T) {
	f := stackStaleParentRepo(t)
	stale := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	published := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/base")
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase replayed feature with no base it could tell from base's commits")
	}
	for _, want := range []string{
		"feature carries no head of base",
		"gt records it on " + shortSHA(stale),
		"base is at " + shortSHA(stale) + " here and at " + shortSHA(published) + " on origin",
		"git rebase --onto base",
		"gt track --parent base feature",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stack rebase = %v, want it to name %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != before {
		t.Errorf("feature moved to %s on a refusal", got)
	}
}

func TestStackRebaseKeepsAChildOffALandedParentWithoutReflogs(t *testing.T) {
	f := shipGTRepo(t)
	stackOffLandedParent(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")

	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--no-push"); err != nil {
		t.Fatalf("stack rebase refused a child already on trunk: %v", err)
	}
}

func TestStackRebaseMovesAChildNamedOntoTrunkWithoutReflogs(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "origin/main", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")

	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--no-push", "--parent", "b=main"); err != nil {
		t.Fatalf("stack rebase refused b named onto trunk: %v", err)
	}
}

func stackStaleParentRepo(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	stale := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	cutFrom := stackPublishRewrite(t, f, stale, "first.txt")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", cutFrom, stale, "feature")
	stackPublishRewrite(t, f, stale, "second.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	return f
}

func stackPublishRewrite(t *testing.T, f *vcstest.Fixture, stale, upstream string) string {
	t.Helper()
	stackAdvanceTrunk(t, f, upstream, upstream+"\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", "refs/remotes/origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", stale)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "HEAD:refs/heads/base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "+refs/heads/base:refs/remotes/origin/base")
	return gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
}
