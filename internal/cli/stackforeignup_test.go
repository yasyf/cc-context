package cli

import (
	"strings"
	"testing"
)

// TestStackRebaseLeavesARefusedBranchAboveItAlone is release-picture's refusal:
// gt recorded another lane's release-notes-fallback on #25907's branch, its
// pull request #25944 had landed with commits since, and restacking #25907
// refused on that branch instead of leaving it to its lane.
func TestStackRebaseLeavesARefusedBranchAboveItAlone(t *testing.T) {
	f := stackRebaseRepo(t, "llm", "fallback")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "fallback")
	stackCommit(t, f, "later.txt")
	past := gitAt(t, f.Env(), f.Dir, "rev-parse", "fallback")
	stubOpenPRs(t, f, map[string]*stackPR{"fallback": {Number: 44, Title: "fallback", State: "MERGED", Landed: true, Head: landedAt}}, "llm")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "llm")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")

	_, errOut, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase from llm: %v", err)
	}
	if !strings.Contains(errOut, "left fallback alone") || !strings.Contains(errOut, "#44 landed at") {
		t.Errorf("stderr = %q, want fallback left alone with its reason", errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "fallback"); got != past {
		t.Errorf("fallback moved to %s, want %s", got, past)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "fallback") {
		t.Error("origin has fallback, another lane's branch")
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "llm")) {
		t.Error("origin llm is not on the new trunk")
	}
}

func TestStackRebaseCarriesAPullRequestOnTrunkOntoABranchInsertedBelowIt(t *testing.T) {
	f := stackRebaseRepo(t, "pr")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "pr")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "insert", "main")
	stackCommit(t, f, "insert.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "main", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "insert", "main", "pr")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "insert", "--no-interactive")
	stubStackPRs(t, f, map[string]*stackPR{"pr": {Number: 42, Title: "pr", State: "OPEN", Base: "main"}})
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "insert")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")

	out, errOut, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase from insert: %v", err)
	}
	if strings.Contains(errOut, "left pr alone") {
		t.Errorf("stderr = %q, want pr carried onto insert, not left alone", errOut)
	}
	if !stackOnto(t, f, "insert", "pr") || !stackOnto(t, f, "origin/main", "insert") {
		t.Errorf("pr is not on insert on the new trunk:\n%s%s", out, errOut)
	}
}
