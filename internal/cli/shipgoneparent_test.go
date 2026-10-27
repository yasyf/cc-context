package cli

import (
	"slices"
	"strings"
	"testing"
)

// TestShipTipOnlyNamesAParentTheQueueJustDeleted is breakglass-stack-ops's
// refusal: the merge queue deleted the parent's branch before it closed the
// parent's pull request, and a --tip-only ship of the child failed on
// "git rev-parse ~1" instead of naming the parent.
func TestShipTipOnlyNamesAParentTheQueueJustDeleted(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	restackSquashRemote(t, f, "main", "base (#41)", "base")
	mustRun(t, f.Env(), f.RemoteDir, "git", "branch", "-D", "base")
	stackCommit(t, f, "tip.txt")
	shipResetLog(t, f)

	args := []string{"--no-commit", "--no-watch", "--tip-only"}
	_, errStr, err := runShipCmdFull(f.Context(), t, args...)
	if err == nil || !strings.Contains(err.Error(), "base's published head, and the remote has none") || !strings.Contains(err.Error(), "--landed base") {
		t.Fatalf("ship --tip-only = %v (stderr=%q), want a refusal naming base and --landed", err, errStr)
	}

	if _, errStr, err := runShipCmdFull(f.Context(), t, append(args, "--landed", "base")...); err != nil {
		t.Fatalf("ship --tip-only --landed base = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..feature"); n != "2" {
		t.Errorf("origin feature holds %s commits over main, want its own 2", n)
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")) {
		t.Error("origin feature is not on the trunk base landed on")
	}
}

// TestShipTipOnlyShipsOverAnUnpublishedGrandparent is dar-sikan-symptom-tests's
// refusal: gt parented the tip on a published branch whose own parent another
// lane never pushed, and --tip-only refused naming that grandparent, though the
// tip ships onto its parent's published head alone.
func TestShipTipOnlyShipsOverAnUnpublishedGrandparent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid", "top")
	api.prs["mid"], api.prs["top"] = 8, 10
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "mid", "top")
	stubStackPRs(t, f, map[string]*stackPR{
		"mid": {Number: 8, Title: "mid", State: "OPEN", Base: "main"},
		"top": {Number: 10, Title: "top", State: "OPEN", Base: "mid"},
	})
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	stackCommit(t, f, "tip.txt")
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q), want top shipped onto mid's published head", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"top"}) {
		t.Errorf("pushed refs = %v, want only top", refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("local base moved from %.12s to %.12s", base, got)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "top")) {
		t.Error("origin top is not on mid's published head")
	}
}
