package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestStackSubmitCorrectsAStaleGraphiteParentRecord is #30363's: Graphite
// recorded dev as its parent while the stack put it on #30342, and stack submit
// left the branch alone as unchanged, so the landing desk's enqueue refused
// #30342 as no member of the stack.
func TestStackSubmitCorrectsAStaleGraphiteParentRecord(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.recorded["feature"] = "main"
	posted := len(api.submitHeads())

	_, errStr, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	if strings.Contains(errStr, "unchanged since its last submit: feature") || strings.Contains(errStr, "unchanged since its last submit: base, feature") {
		t.Errorf("stderr = %q, want feature resubmitted to correct its Graphite parent", errStr)
	}
	heads := api.submitHeads()[posted:]
	if len(heads) == 0 || heads[len(heads)-1] != "feature" {
		t.Fatalf("submit posts = %v, want feature resubmitted", heads)
	}
	if entry := api.submitEntry("feature"); entry.Base != "base" {
		t.Errorf("feature resubmitted onto %q, want base", entry.Base)
	}
}

func TestStackSubmitKeepsAParentGraphiteRecordsWithNoLocalBranch(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid")
	api.prs["base"], api.prs["mid"], api.prs["inserted"] = 7, 8, 9
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish the stack: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	restackRun(t, f, filepath.Dir(other), "git", "clone", "-q", "--branch", "base", f.RemoteDir, other)
	restackRun(t, f, other, "git", "config", "user.email", "o@o.o")
	restackRun(t, f, other, "git", "config", "user.name", "o")
	restackRun(t, f, other, "git", "switch", "-qc", "inserted")
	restackWrite(t, filepath.Join(other, "inserted.txt"), "inserted\n")
	restackRun(t, f, other, "git", "add", "inserted.txt")
	restackRun(t, f, other, "git", "commit", "-qm", "inserted")
	restackRun(t, f, other, "git", "switch", "-qc", "mid", "origin/mid")
	restackRun(t, f, other, "git", "rebase", "-q", "--onto", "inserted", "base", "mid")
	restackRun(t, f, other, "git", "push", "-q", "origin", "inserted")
	restackRun(t, f, other, "git", "push", "-qf", "origin", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/mid")
	api.recorded["mid"] = "inserted"
	stubStackPRs(t, f, map[string]*stackPR{
		"base":     {Number: 7, Title: "base", State: "OPEN", Base: "main"},
		"inserted": {Number: 9, Title: "inserted", State: "OPEN", Base: "base"},
		"mid":      {Number: 8, Title: "mid", State: "OPEN", Base: "inserted"},
	})
	mid := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid")
	posted := len(api.submitHeads())
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errOut)
	}
	if got := thinGTParent(t, f, f.Dir, "mid"); got != "inserted" {
		t.Errorf("mid's gt parent = %q, want inserted, the base Graphite records for #8", got)
	}
	if got := thinGTParent(t, f, f.Dir, "inserted"); got != "base" {
		t.Errorf("inserted's gt parent = %q, want base, the base of its own #9", got)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if slices.Contains(inv, "PATCH") {
			t.Errorf("retargeted a pull request through GitHub: %v", inv)
		}
	}
	api.mu.Lock()
	for _, submit := range api.submits[posted:] {
		if submit.entry.Head == "mid" && submit.entry.Base != "inserted" {
			t.Errorf("mid submitted onto %q, want inserted", submit.entry.Base)
		}
	}
	api.mu.Unlock()
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"); got != mid {
		t.Errorf("origin mid = %s, want %s, already on inserted, left alone", got, mid)
	}
	if want := "fetched inserted and kept it as mid's parent — Graphite stacks #8 on it"; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}

// TestShipTipOnlyLeavesAnAncestorsUntrackedGraphiteParentAlone is DA4's Oct 19
// refusal: a --tip-only ship read the base Graphite records for an ancestor it
// keeps at its published head, and refused on that base's stale local ref.
func TestShipTipOnlyLeavesAnAncestorsUntrackedGraphiteParentAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid", "top")
	api.prs["base"], api.prs["mid"], api.prs["top"], api.prs["inserted"] = 7, 8, 10, 9
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish the stack: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	restackRun(t, f, filepath.Dir(other), "git", "clone", "-q", "--branch", "base", f.RemoteDir, other)
	restackRun(t, f, other, "git", "config", "user.email", "o@o.o")
	restackRun(t, f, other, "git", "config", "user.name", "o")
	restackRun(t, f, other, "git", "switch", "-qc", "inserted")
	restackWrite(t, filepath.Join(other, "inserted.txt"), "inserted\n")
	restackRun(t, f, other, "git", "add", "inserted.txt")
	restackRun(t, f, other, "git", "commit", "-qm", "inserted")
	restackRun(t, f, other, "git", "switch", "-qc", "mid", "origin/mid")
	restackRun(t, f, other, "git", "rebase", "-q", "--onto", "inserted", "base", "mid")
	restackRun(t, f, other, "git", "push", "-q", "origin", "inserted")
	restackRun(t, f, other, "git", "push", "-qf", "origin", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "inserted", "base")
	stale := gitAt(t, f.Env(), f.Dir, "rev-parse", "inserted")
	stubStackPRs(t, f, map[string]*stackPR{
		"base":     {Number: 7, Title: "base", State: "OPEN", Base: "main"},
		"inserted": {Number: 9, Title: "inserted", State: "OPEN", Base: "base"},
		"mid":      {Number: 8, Title: "mid", State: "OPEN", Base: "inserted"},
		"top":      {Number: 10, Title: "top", State: "OPEN", Base: "mid"},
	})
	stackCommit(t, f, "tip.txt")
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q), want top shipped onto mid's published head", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"top"}) {
		t.Errorf("pushed refs = %v, want only top", refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "inserted"); got != stale {
		t.Errorf("local inserted moved from %.12s to %.12s", stale, got)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "top")) {
		t.Error("origin top is not on mid's published head")
	}
}
