package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
)

func TestStackSubmitDropsAClosedPullRequestMidStack(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "a"},
		"c": {Number: 3, Title: "c", State: "OPEN", Base: "b"},
	})
	installDropGH(t, f, map[string]dropSeed{"b": {number: 2, state: "CLOSED", base: "a"}})
	closed := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "b"+shipSep+"drop (#2 closed)"+shipSep+`#2 "b"`) {
		t.Errorf("report = %q, want b dropped naming its closed pull request", out)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "c")
	if err != nil || receipt == nil || receipt.Parent != "a" {
		t.Fatalf("c publication = %+v, %v, want parent a", receipt, err)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "a..c"); n != "1" {
		t.Errorf("origin c holds %s commits over a, want its own 1", n)
	}
	if files := gitAt(t, f.Env(), f.RemoteDir, "ls-tree", "--name-only", "c"); strings.Contains(files, "b.txt") {
		t.Errorf("origin c carries the closed b's b.txt: %q", files)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); slices.Contains(refs, "b") {
		t.Errorf("pushed %v, want the closed b left out", refs)
	}
	if heads := api.submitHeads(); slices.Contains(heads, "b") {
		t.Errorf("submit posts = %v, want the closed b left out", heads)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "b"); got != closed {
		t.Errorf("local b = %s, want it kept at %s", got, closed)
	}
}

func TestStackRebaseDropsAClosedPullRequestThatConflictsWithTrunk(t *testing.T) {
	f := shipGTRepo(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "x")
	stackCommit(t, f, "c.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	shipGTStack(t, f, "y")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	stubStackPRs(t, f, map[string]*stackPR{
		"x": {Number: 5, Title: "x", State: "CLOSED", Base: "main"},
		"y": {Number: 6, Title: "y", State: "OPEN", Base: "x"},
	})
	closed := gitAt(t, f.Env(), f.Dir, "rev-parse", "x")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if !strings.Contains(out, "dropped x (#5 closed)") {
		t.Errorf("report = %q, want x dropped naming its closed pull request", out)
	}
	if parent := dropGTParent(t, f, "y"); parent != "main" {
		t.Errorf("gt parent of y = %s, want main", parent)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..y"); n != "1" {
		t.Errorf("y holds %s commits over trunk, want its own 1", n)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "x"); got != closed {
		t.Errorf("local x = %s, want it kept at %s", got, closed)
	}
}

// TestShipRefusesToDropItsOwnClosedPullRequest is #31933: a --tip-only ship
// of a branch whose pull request the owner closed dropped that branch.
func TestShipRefusesToDropItsOwnClosedPullRequest(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bare", nil},
		{"pr meta", []string{"--pr-title", "Title", "--pr-body-file", body}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			priorSubmits := len(api.submitHeads())
			stubStackPRs(t, f, map[string]*stackPR{"feature": {Number: 7, Title: "feature", State: "CLOSED", Base: "main"}})
			shipResetLog(t, f)

			args := append([]string{"--no-commit", "--no-watch", "--tip-only"}, tc.args...)
			_, errStr, err := runShipCmdFull(f.Context(), t, args...)
			want := "ship: feature's pull request #7 was closed without merging, and ship will not drop the branch it ships — reopen it with gh pr reopen 7. Nothing was committed"
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("ship = %v (stderr=%q), want %q", err, errStr, want)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
				t.Errorf("pushed %v, want nothing", refs)
			}
			if heads := api.submitHeads()[priorSubmits:]; len(heads) != 0 {
				t.Errorf("submit posts = %v, want none", heads)
			}
			if parent := dropGTParent(t, f, "feature"); parent != "main" {
				t.Errorf("gt parent of feature = %q, want main", parent)
			}
		})
	}
}

func TestStackSubmitRefusesToReopenOntoAnUnpublishedParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "b")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "a"},
	})
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "b's pull request #2 closed when its base a was deleted, and its new parent a is not on the remote to reopen it onto — publish a first with ccx vcs stack submit --to a") {
		t.Fatalf("stack submit = %v, want the base-deleted close refused", err)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
		t.Errorf("pushed %v, want nothing", refs)
	}
}

// TestStackSubmitReopensAPullRequestItsLandedParentsDeletionClosed is the loop
// between submit and drop --repair: the parent landed and its branch was
// deleted, closing the child's pull request, and each command pointed at the
// other. Submit from the child reopens it onto trunk before pushing.
func TestStackSubmitReopensAPullRequestItsLandedParentsDeletionClosed(t *testing.T) {
	for _, verb := range []string{"submit", "rebase"} {
		t.Run(verb, func(t *testing.T) {
			stackReopensOnto(t, verb)
		})
	}
}

func stackReopensOnto(t *testing.T, verb string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	restackSquashRemote(t, f, "main", "a (#1)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "a")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "MERGED", Base: "main", Head: landed, Landed: true},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "a"},
	})
	gh := installDropGH(t, f, map[string]dropSeed{"b": {number: 2, state: "CLOSED", base: "a"}})
	gh.markDeleted("a")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, verb)
	if err != nil {
		t.Fatalf("stack %s: %v", verb, err)
	}
	if !strings.Contains(out, "reopen (base a deleted)") {
		t.Errorf("report = %q, want it to name the reopen", out)
	}
	if got := gh.pr(2); got != "OPEN main" {
		t.Errorf("b's PR = %q, want %q", got, "OPEN main")
	}
	invocations := shipGTInvocations(t, f)
	resurrected := dropStep(t, invocations, "git", "push", "origin", "refs/heads/main:refs/heads/a")
	reopened := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "state=open")
	retargeted := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "base=main")
	redeleted := dropStep(t, invocations, "git", "push", "--delete", "a")
	pushed := dropStep(t, invocations, "git", "push", "--atomic")
	if resurrected >= reopened || reopened >= retargeted || retargeted >= redeleted || redeleted >= pushed {
		t.Errorf("steps ran at resurrect=%d reopen=%d retarget=%d delete=%d push=%d, want that order", resurrected, reopened, retargeted, redeleted, pushed)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "a") {
		t.Error("origin still carries the resurrected a")
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..b"); n != "1" {
		t.Errorf("origin b holds %s commits over trunk, want its own 1", n)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"b"}) {
		t.Errorf("submit posts = %v, want b alone", heads)
	}
	if entry := api.submitEntry("b"); entry.Action != gtapi.SubmitUpdate || entry.PRNumber != 2 || entry.Base != "main" {
		t.Errorf("b submitted as %s #%d onto %s, want an update of #2 onto main", entry.Action, entry.PRNumber, entry.Base)
	}
}

// TestStackSubmitReopensAPullRequestClosedWhileGraphiteRecreatedItsBase is
// #30441: the merge queue deleted and recreated the child's graphite-base
// branch as the parent landed, GitHub closed the child for the deletion, and
// submit dropped it as abandoned because its base was back by the time it
// looked. Submit reopens it onto trunk and leaves Graphite's branch alone.
func TestStackSubmitReopensAPullRequestClosedWhileGraphiteRecreatedItsBase(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a:refs/heads/graphite-base/2")
	restackSquashRemote(t, f, "main", "a (#1)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "a")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "MERGED", Base: "main", Head: landed, Landed: true},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "graphite-base/2"},
	})
	gh := installDropGH(t, f, map[string]dropSeed{"b": {number: 2, state: "CLOSED", base: "graphite-base/2"}})
	gh.closedByBaseDeletion(2)
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "reopen (base graphite-base/2 deleted)") {
		t.Errorf("report = %q, want it to name the reopen", out)
	}
	if got := gh.pr(2); got != "OPEN main" {
		t.Errorf("b's PR = %q, want %q", got, "OPEN main")
	}
	invocations := shipGTInvocations(t, f)
	reopened := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "state=open")
	retargeted := dropStep(t, invocations, "gh", "api", "PATCH", "repos/yasyf/cc-context/pulls/2", "base=main")
	pushed := dropStep(t, invocations, "git", "push", "--atomic")
	if reopened >= retargeted || retargeted >= pushed {
		t.Errorf("steps ran at reopen=%d retarget=%d push=%d, want that order", reopened, retargeted, pushed)
	}
	if !gitBranchExists(t, f.Env(), f.RemoteDir, "graphite-base/2") {
		t.Error("origin lost Graphite's graphite-base/2")
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..b"); n != "1" {
		t.Errorf("origin b holds %s commits over trunk, want its own 1", n)
	}
	if entry := api.submitEntry("b"); entry.Action != gtapi.SubmitUpdate || entry.PRNumber != 2 || entry.Base != "main" {
		t.Errorf("b submitted as %s #%d onto %s, want an update of #2 onto main", entry.Action, entry.PRNumber, entry.Base)
	}
}
