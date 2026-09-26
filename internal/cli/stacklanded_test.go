package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// stackOffLandedParent is #25813: a squash landed a, b was already moved onto
// the newer trunk and pushed there, and gt still records b on a.
func stackOffLandedParent(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	gtLandedStack(t, f)
	restackAdvanceRemote(t, f, "main", "later.txt", "later\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "origin/main", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "-f", "origin", "b")
	stubStackPRs(t, map[string]*stackPR{
		"a": {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")},
		"b": {Number: 42, Title: "b", State: "OPEN", Base: "main"},
	})
	if parent := dropGTParent(t, f, "b"); parent != "a" {
		t.Fatalf("gt parent of b = %s, want the landed a", parent)
	}
}

func stackAssertOnTrunk(t *testing.T, f *vcstest.Fixture, api *gtAPIStub, branch string) {
	t.Helper()
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main.."+branch); n != "1" {
		t.Errorf("origin %s holds %s commits over main, want its own 1", branch, n)
	}
	if receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), branch); err != nil || receipt == nil || receipt.Parent != "main" {
		t.Errorf("%s publication = %+v, %v, want parent main", branch, receipt, err)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{branch}) {
		t.Errorf("pushed %v, want %s alone", refs, branch)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{branch}) {
		t.Errorf("submit posts = %v, want %s alone", heads, branch)
	}
}

func TestStackSubmitMovesABranchOffALandedParentItAlreadyLeft(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackOffLandedParent(t, f)
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "a · drop (#41 landed)") {
		t.Errorf("report = %q, want the landed a dropped", out)
	}
	stackAssertOnTrunk(t, f, api, "b")
}

func TestStackRebaseTakesTrunkAsTheParentOfABranchOffALandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackOffLandedParent(t, f)
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "b=main"); err != nil {
		t.Fatalf("stack rebase --parent b=main: %v", err)
	}
	stackAssertOnTrunk(t, f, api, "b")
}

// stackOnForeignLane is #25816: gt track --force --parent main took the most
// recent tracked ancestor instead: an empty ladder another lane had just cut on
// feature's own trunk commit, newer than the stale local trunk. ladder then
// gained a commit of its own.
func stackOnForeignLane(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	restackAdvanceRemote(t, f, "main", "earlier.txt", "earlier\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "reader", "origin/main")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "main", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "ladder")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "reader", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "reader")
	stackCommit(t, f, "reader.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature", "origin/main")
	stackCommit(t, f, "feature.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--force", "--parent", "main", "--no-interactive")
	if parent := dropGTParent(t, f, "feature"); parent != "ladder" {
		t.Fatalf("gt track --force --parent main recorded %s, want the ladder gt picks over --parent", parent)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "ladder")
	stackCommit(t, f, "ladder.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 42, Title: "feature", State: "OPEN", Base: "main"}})
}

func TestStackSubmitRefusesAForeignLaneBelowTheBranch(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackOnForeignLane(t, f)
	heads := map[string]string{}
	for _, b := range []string{"reader", "ladder", "feature"} {
		heads[b] = gitAt(t, f.Env(), f.Dir, "rev-parse", b)
	}
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit restacked and pushed another lane's ladder and reader")
	}
	for _, want := range []string{"feature carries none of ladder's commits", "another lane's ladder, reader", "gt track --force", "--parent feature=<branch>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want %q", err, want)
		}
	}
	for b, head := range heads {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", b); got != head {
			t.Errorf("%s moved to %s, want it left at %s", b, got, head)
		}
		if b != "feature" && gitBranchExists(t, f.Env(), f.RemoteDir, b) {
			t.Errorf("origin has %s, another lane's local-only branch", b)
		}
	}
	if posts := api.submitHeads(); len(posts) != 0 {
		t.Errorf("submit posts = %v, want none", posts)
	}
}

func TestStackRebaseParentLeavesTheForeignLaneBehind(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackOnForeignLane(t, f)
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "feature=main"); err != nil {
		t.Fatalf("stack rebase --parent feature=main: %v", err)
	}
	stackAssertOnTrunk(t, f, api, "feature")
	for _, b := range []string{"reader", "ladder"} {
		if gitBranchExists(t, f.Env(), f.RemoteDir, b) {
			t.Errorf("origin has %s, another lane's local-only branch", b)
		}
	}
}

func TestStackRebaseParentKeepsAPublishedParentsCommitsBehind(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/b")
	stackCommit(t, f, "fix.txt")

	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "b=main"); err != nil {
		t.Fatalf("stack rebase --parent b=main: %v", err)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..b"); n != "2" {
		t.Errorf("origin b holds %s commits over main, want b and fix without a's", n)
	}
}

func TestStackRebaseParentKeepsAChildOnItsPublishedParent(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "c=a"); err != nil {
		t.Fatalf("stack rebase --parent c=a: %v", err)
	}

	out, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--parent", "b=main")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --parent b=main: %v", err)
	}
	if !strings.Contains(out, "c · onto a") {
		t.Errorf("plan = %q, want c kept on a, where it was published", out)
	}
}

func TestStackRebaseJudgesEachParentByTheBranchOnIt(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	old := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	writeShipFile(t, f.Dir, "a.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qa", "--amend", "-m", "a amended")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "a", old, "b")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase from c above a restacked b: %v", err)
	}
	if !stackOnto(t, f, "b", "c") {
		t.Error("c does not sit on b")
	}
}

func TestStackRebaseKeepsABranchCutFromAnEmptyParent(t *testing.T) {
	f := shipGTRepo(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "a")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "main", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "b")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "a", "--no-interactive")
	stackCommit(t, f, "b.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	stackCommit(t, f, "a.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase of b cut from a before a's first commit: %v", err)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "a..b"); n != "1" || !stackOnto(t, f, "a", "b") {
		t.Errorf("b holds %s commits over a, want its own 1 on a", n)
	}
}

func TestStackRebaseRefusesTrunkAsAChild(t *testing.T) {
	f := stackRebaseRepo(t, "a")

	_, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--parent", "main=a")
	if err == nil || !strings.Contains(err.Error(), "main is trunk") {
		t.Fatalf("stack rebase --parent main=a = %v, want trunk refused as a child", err)
	}
}
