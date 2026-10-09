package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// gtLandedStack stacks b on a and lands a on origin's trunk as the merge queue
// does — one squash, so a's head is no ancestor of trunk — with GitHub
// reporting a's pull request as landed.
func gtLandedStack(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	shipGTStack(t, f, "a", "b")
	stubStackPRs(t, f, map[string]*stackPR{"a": {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")}})
	restackSquashRemote(t, f, "main", "a (#41)", "a")
}

func assertLandedPublished(t *testing.T, f *vcstest.Fixture, api *gtAPIStub, own string) {
	t.Helper()
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "b")
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.Parent != "main" {
		t.Fatalf("b publication = %+v, want parent main", receipt)
	}
	if source := gitAt(t, f.Env(), f.Dir, "rev-parse", "b"); source != receipt.Source {
		t.Errorf("b source = %s, want unchanged %s", source, receipt.Source)
	}
	if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); remote != receipt.Head {
		t.Errorf("b remote = %s, want published %s", remote, receipt.Head)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "refs/remotes/origin/main.."+receipt.Head); got != own {
		t.Errorf("b published %s commit(s) above trunk, want %s", got, own)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"b"}) {
		t.Errorf("submit posts = %v, want b alone", heads)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"b"}) {
		t.Errorf("pushed %v, want b alone", refs)
	}
	entry := api.submitEntry("b")
	if remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main"); entry.Base != "main" || entry.BaseSha != remote {
		t.Errorf("b submitted onto %s at %s, want main at %s", entry.Base, entry.BaseSha, remote)
	}
}

// TestStackSubmitDropsALandedParent is the ten minutes a stack sat after its
// bottom landed through the queue, until deleting the landed branch closed the
// child's pull request: one stack submit retargets the child.
func TestStackSubmitDropsALandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	gtLandedStack(t, f)
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "a · drop (#41 landed)") {
		t.Errorf("report = %q, want it to name the landed branch", out)
	}
	assertLandedPublished(t, f, api, "1")
}

// TestShipGTDropsALandedParent is the same landing met by a plain ship from the
// child's lane.
func TestShipGTDropsALandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	gtLandedStack(t, f)
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(got, "a · drop (#41 landed)") {
		t.Errorf("summary = %q, want it to name the landed branch", got)
	}
	assertLandedPublished(t, f, api, "2")
}

// TestShipGTLeavesAnUnchangedDownstackBranchAlone is the lane C incident: a
// parent lane's Graphite error — "Pull request not found" on a pull request
// that existed — failed the child's ship, though the child changed nothing of
// the parent's. A branch below the tip that its last submit already carries is
// neither pushed nor submitted again.
func TestShipGTLeavesAnUnchangedDownstackBranchAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.submitErrors["base"] = "GitHub returned an error: Pull request not found"
	posted := len(api.submitHeads())
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "not resubmitting 1 branch, unchanged since its last submit: base"; !strings.Contains(errStr, want) {
		t.Errorf("stderr = %q, want it to carry %q", errStr, want)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"feature"}) {
		t.Errorf("submit posts = %v, want feature alone", heads)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"base", "feature"}) {
		t.Errorf("pushed %v, want base kept in the atomic push under its lease", refs)
	}
	if want := "submitted feature → PR #101 " + gtStubPRURL(101); !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
}

// TestShipGTReportsFromGraphiteAlone pins the report to what the submit already
// knows: the pull request it opened, from Graphite's own answer, with no GitHub
// round trip to spend a rate limit another lane has exhausted.
func TestShipGTReportsFromGraphiteAlone(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "submitted feature → PR #101 " + gtStubPRURL(101); !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 2 && inv[0] == "gh" && inv[1] == "api" && inv[2] == "graphql" {
			t.Errorf("ship ran %v — the submit's report is Graphite's", inv)
		}
	}
}

// TestShipGTResubmitsAnUnchangedBranchToChangeItsDraftState keeps --draft
// meaning what it says on a branch whose pull request is otherwise unchanged.
func TestShipGTResubmitsAnUnchangedBranchToChangeItsDraftState(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	posted := len(api.submitHeads())
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--draft"); err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"base", "feature"}) {
		t.Errorf("submit posts = %v, want base resubmitted as a draft too", heads)
	}
}

func TestShipGTAmendRefusalNamesTheRecovery(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("a", "b"))
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	pre := shipHead(t, f)
	shipGTReady(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch")
	if err == nil {
		t.Fatal("ship --amend published a branch replaying trunk's commit")
	}
	amended := shipHead(t, f)
	if amended == pre {
		t.Fatalf("fixture refused before the amend formed: %v", err)
	}
	for _, want := range []string{"would replay 3 commits but owns 2", "ccx vcs stack rebase --parent b=<branch>", shortOID(amended), "git reset --soft " + pre} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
}

// TestStackSubmitRetargetsAParkedPullRequest is the stack whose submit left a
// pull request on graphite-base/N, a different few on each run: Graphite moves
// a parked pull request back onto its parent only when graphite-base/N already
// sits at the parent's head, so the submit's own push must move it there.
func TestStackSubmitRetargetsAParkedPullRequest(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "p", "c", "g")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["p"], api.prs["c"], api.prs["g"] = 100, 101, 102
	api.mu.Lock()
	api.parkChildren([]gtapi.PreSubmitBranch{{HeadRefName: "p", PRNumber: 100}})
	api.mu.Unlock()
	if parked := api.parkedPRs(); !slices.Equal(parked, []string{"c"}) {
		t.Fatalf("parked %v, want c parked by a submit of p alone", parked)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if parked := api.parkedPRs(); len(parked) != 0 {
		t.Errorf("parked %v after the submit, want every pull request on its parent", parked)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Contains(refs, "graphite-base/101") {
		t.Errorf("pushed %v, want graphite-base/101 moved in the atomic push", refs)
	}
}

func TestStackSubmitLeavesAGraphiteBaseTheRemoteDeleted(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "p", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["p"], api.prs["c"] = 100, 101
	api.mu.Lock()
	api.parkChildren([]gtapi.PreSubmitBranch{{HeadRefName: "p", PRNumber: 100}})
	api.mu.Unlock()
	mustRun(t, f.Env(), f.Dir, "git", "--git-dir="+f.RemoteDir, "update-ref", "-d", "refs/heads/graphite-base/101")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, f)

	if _, errOut, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v (stderr=%q)", err, errOut)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); slices.Contains(refs, "graphite-base/101") {
		t.Errorf("pushed %v, want the deleted graphite-base/101 left deleted", refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "for-each-ref", "refs/heads/graphite-base/"); got != "" {
		t.Errorf("origin graphite-base refs = %q, want none", got)
	}
}

func TestStackSubmitReplaysAChildGitHubShowsParked(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "p", "c", "g")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["p"], api.prs["c"], api.prs["g"] = 100, 101, 102
	api.mu.Lock()
	api.parkChildren([]gtapi.PreSubmitBranch{{HeadRefName: "p", PRNumber: 100}})
	api.mu.Unlock()
	stubOpenPRs(t, f, map[string]*stackPR{"c": {Number: 101, Title: "c", State: "OPEN", Base: "graphite-base/101"}}, "p", "g")
	writeShipGH(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "--force", "origin", "p")
	posted := len(api.submitHeads())
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v (stderr=%q)", err, errOut)
	}
	if strings.Contains(errOut, "graphite-base/101") {
		t.Errorf("stack submit read graphite-base/101 as c's parent:\n%s", errOut)
	}
	if !stackOnto(t, f, "p", "c") || !stackOnto(t, f, "c", "g") {
		t.Errorf("c and g were left off the amended p:\n%s", errOut)
	}
	if heads := api.submitHeads()[posted:]; !slices.Contains(heads, "c") || !slices.Contains(heads, "g") {
		t.Errorf("submit posts = %v, want c and g resubmitted on the amended p", heads)
	}
	if entry := api.submitEntry("c"); entry.Base != "p" {
		t.Errorf("c submitted on %q, want its gt parent p", entry.Base)
	}
	if parked := api.parkedPRs(); len(parked) != 0 {
		t.Errorf("parked %v after the submit, want every pull request on its parent", parked)
	}
}

// TestShipGTResubmitsTheChildOfAResubmittedBranch is the ship that left its
// tip's parent parked: trunk moved, so the bottom branch was resubmitted on
// the new trunk while the unchanged middle one was left out, and Graphite's
// pre-submit parks every open child a submit leaves out.
func TestShipGTResubmitsTheChildOfAResubmittedBranch(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "a", "b", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["a"], api.prs["b"], api.prs["c"] = 100, 101, 102
	stubOpenPRs(t, f, nil, "a", "b", "c")
	posted := len(api.submitHeads())
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if parked := api.parkedPRs(); len(parked) != 0 {
		t.Errorf("parked %v after the ship, want every pull request on its parent", parked)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"a", "b", "c"}) {
		t.Errorf("submit posts = %v, want b resubmitted beneath its resubmitted parent", heads)
	}
}

func TestStackSubmitStopsAtTo(t *testing.T) {
	f := stackStoppedFixture(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "dns")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--to", "valkey"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	refs := gtPushedRefs(shipGTInvocations(t, f))
	slices.Sort(refs)
	if !slices.Equal(refs, []string{"dns", "valkey"}) {
		t.Errorf("pushed %v, want dns and valkey alone", refs)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"dns", "valkey"}) {
		t.Errorf("submit posts = %v, want dns then valkey", heads)
	}
}
