package cli

import (
	"strings"
	"testing"
)

// TestStackRebaseParentPushesOnlyTheBranchItMoves is release-picture's #25937:
// stacking #25907 onto it with --parent also restacked and pushed #25937 onto
// newer trunk while it sat in the merge queue, and --dry-run never said so.
func TestStackRebaseParentPushesOnlyTheBranchItMoves(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "p", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p")

	out, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--parent", "c=p")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --parent c=p: %v", err)
	}
	if !strings.Contains(out, "pushes c\n") && !strings.HasSuffix(out, "pushes c") {
		t.Errorf("plan = %q, want it to name c as the one branch pushed", out)
	}
	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "c=p"); err != nil {
		t.Fatalf("stack rebase --parent c=p: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p"); got != published {
		t.Errorf("origin p moved to %s, want its published %s left alone", got, published)
	}
	if !stackOnto(t, f, published, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")) {
		t.Error("origin c does not sit on p's published head")
	}
}

func TestStackSubmitNeverPushesAQueuedBranch(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	api := stubGTAPI(t)
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.queued["base"] = true
	stubStackPRs(t, map[string]*stackPR{
		"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main"},
		"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base"},
	})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	queued := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "base · kept at its published head") {
		t.Errorf("report = %q, want the queued base kept", out)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != queued {
		t.Errorf("origin base moved to %s while queued at %s", got, queued)
	}

	_, _, err = runStackCmd(t, f, "rebase", "--parent", "base=main")
	if err == nil || !strings.Contains(err.Error(), "base is in the merge queue") {
		t.Fatalf("stack rebase --parent base=main of a queued base = %v, want a refusal", err)
	}
}

// TestShipNewBranchOnAnOpenParentReadsTheQueue is hot-deploy's #25973: ship
// --new-branch stacked on a parent with an open pull request refused, because
// the merge queue read sent a null prNumbers and graphite answered 400.
func TestShipNewBranchOnAnOpenParentReadsTheQueue(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "p")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["p"] = 100
	stubStackPRs(t, map[string]*stackPR{"p": {Number: 100, Title: "p", State: "OPEN", Base: "main"}})
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--new-branch=bench", "f.txt"); err != nil {
		t.Fatalf("ship --new-branch=bench = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p"); got != published {
		t.Errorf("origin p moved to %s, want its published %s left alone", got, published)
	}
	if !stackOnto(t, f, published, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "bench")) {
		t.Error("origin bench does not sit on p's published head")
	}
}
