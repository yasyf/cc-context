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
	stubOpenPRs(t, f, nil, "p", "c")
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
	f.Decorate(api.ctx)
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.queued["base"] = true
	stubPRState(t, prPoll(`"p0":`+prNode(100, "OPEN", prComments())))
	stubStackPRs(t, f, map[string]*stackPR{
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

// TestStackSubmitMovesAPullRequestTheQueueEvicted is run-retry-fixes' #31086:
// Graphite still flagged it in the queue after the queue ejected it for failed
// CI, so stack submit kept it at its ejected head and --parent refused to move
// it, though ccx vcs pr status read it out of the queue.
func TestStackSubmitMovesAPullRequestTheQueueEvicted(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.queued["base"] = true
	stubPRState(t, prPoll(`"p0":`+prNode(100, "OPEN", prComments(prActivityEvicted))))
	stubStackPRs(t, f, map[string]*stackPR{
		"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main"},
		"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base"},
	})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	ejected := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")

	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--parent", "base=main"); err != nil {
		t.Fatalf("stack rebase --dry-run --parent base=main of an evicted base = %v, want no refusal", err)
	}
	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if strings.Contains(out, "base · kept") || strings.Contains(out, "base · stays") {
		t.Errorf("report = %q, want the evicted base moved onto main", out)
	}
	moved := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if moved == ejected {
		t.Fatalf("origin base still at its ejected head %s", ejected)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"), moved) {
		t.Error("origin base does not sit on the advanced main")
	}
}

// TestShipNewBranchOnAnOpenParentReadsTheQueue is hot-deploy's #25973: ship
// --new-branch stacked on a parent with an open pull request refused, because
// the merge queue read sent a null prNumbers and graphite answered 400.
func TestShipNewBranchOnAnOpenParentReadsTheQueue(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["p"] = 100
	stubStackPRs(t, f, map[string]*stackPR{"p": {Number: 100, Title: "p", State: "OPEN", Base: "main"}})
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
