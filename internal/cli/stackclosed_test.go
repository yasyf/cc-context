package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestStackSubmitDropsAClosedPullRequestMidStack(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	stubStackPRs(t, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "a"},
		"c": {Number: 3, Title: "c", State: "OPEN", Base: "b"},
	})
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
	stubStackPRs(t, map[string]*stackPR{
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

func TestStackSubmitRefusesAPullRequestClosedByItsBaseDeletion(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "b")
	stubStackPRs(t, map[string]*stackPR{
		"a": {Number: 1, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 2, Title: "b", State: "CLOSED", Base: "a"},
	})
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "b's pull request #2 closed when its base a was deleted — reopen and retarget it with ccx vcs stack drop --repair") {
		t.Fatalf("stack submit = %v, want the base-deleted close refused", err)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
		t.Errorf("pushed %v, want nothing", refs)
	}
}
