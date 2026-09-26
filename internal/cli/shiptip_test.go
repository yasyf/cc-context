package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackSubmitTakesLanded(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit", "--landed", "a")
	if err != nil {
		t.Fatalf("stack submit --landed a: %v", err)
	}
	if !strings.Contains(out, "a · drop (declared landed)") {
		t.Errorf("report = %q, want a dropped as declared landed", out)
	}
	assertLandedPublished(t, f, api, "1")
}

func TestShipTakesLanded(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--landed", "a")
	if err != nil {
		t.Fatalf("ship --landed a = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(got, "a · drop (declared landed)") {
		t.Errorf("summary = %q, want a dropped as declared landed", got)
	}
	assertLandedPublished(t, f, api, "2")
}

// stackResetOverPublished is a lane's hard reset: feature's published commit is
// gone from the local head, replaced by a commit of another branch's.
func stackResetOverPublished(t *testing.T) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "main")
	stackCommit(t, f, "other.txt")
	shipResetLog(t, f)
	return f, published
}

func TestStackSubmitRefusesALocalHeadDroppingPublishedCommits(t *testing.T) {
	f, published := stackResetOverPublished(t)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), `drops 1 commit(s) its published head origin/feature`) || !strings.Contains(err.Error(), `"feature"`) {
		t.Fatalf("submit after a hard reset = %v, want a refusal naming the dropped commit", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != published {
		t.Errorf("origin feature moved to %s, want %s left in place", got, published)
	}

	if _, _, err := runStackCmd(t, f, "submit", "--drop-commits"); err != nil {
		t.Fatalf("submit --drop-commits: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%s", "feature"); got != "other.txt" {
		t.Errorf("origin feature tip = %q, want the local head's other.txt", got)
	}
}

// stackMergeableBase is a submitted base, feature stack whose base pull request
// is open and mergeable, with trunk moved on since.
func stackMergeableBase(t *testing.T, mergeable string) (*vcstest.Fixture, *gtAPIStub, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	stubStackPRs(t, map[string]*stackPR{
		"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main", Mergeable: mergeable},
		"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
	})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipGTReady(t, f)
	return f, api, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
}

func stackAssertBaseKept(t *testing.T, f *vcstest.Fixture, base string) {
	t.Helper()
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("origin base moved to %s, want its approved head %s kept", got, base)
	}
	if !stackOnto(t, f, base, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")) {
		t.Error("origin feature does not sit on base's published head")
	}
}

func TestShipKeepsAMergeableAncestorOffNewerTrunk(t *testing.T) {
	f, _, base := stackMergeableBase(t, "MERGEABLE")

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	stackAssertBaseKept(t, f, base)
}

func TestShipTipOnlyPushesNoAncestor(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
	}
	stackAssertBaseKept(t, f, base)
}
