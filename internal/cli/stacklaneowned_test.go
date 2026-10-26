package cli

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackAssertUntouched(t *testing.T, f *vcstest.Fixture, before map[string]string) {
	t.Helper()
	for name, head := range before {
		for where, dir := range map[string]string{"local": f.Dir, "origin": f.RemoteDir} {
			if got := gitAt(t, f.Env(), dir, "rev-parse", name); got != head {
				t.Errorf("%s %s = %.12s, want it left at %.12s", where, name, got, head)
			}
		}
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); slices.ContainsFunc(refs, func(ref string) bool { _, theirs := before[ref]; return theirs }) {
		t.Errorf("pushed refs = %v, want none of %v", refs, slices.Sorted(maps.Keys(before)))
	}
}

func stackHeads(t *testing.T, f *vcstest.Fixture, names ...string) map[string]string {
	t.Helper()
	heads := make(map[string]string, len(names))
	for _, name := range names {
		heads[name] = gitAt(t, f.Env(), f.Dir, "rev-parse", name)
	}
	return heads
}

func TestStackRebaseKeepsEveryBranchBelowAHeldOneAtItsPublishedHead(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "low", "mid", "top")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "low", "mid", "top")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "mid")
	before := stackHeads(t, f, "low", "mid")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	out, errOut, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase from top: %v\n%s", err, errOut)
	}
	for _, want := range []string{
		"low sits under mid, held by " + held + ", not pushed; its lane runs ccx vcs stack submit · kept at its published head; --include low moves it",
		"mid held by " + held + ", not pushed; its lane runs ccx vcs stack submit · kept at its published head; --include mid moves it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	stackAssertUntouched(t, f, before)
	if got := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); got != before["mid"] {
		t.Errorf("%s HEAD = %.12s, want it left at %.12s", held, got, before["mid"])
	}
	if !stackOnto(t, f, "mid", "top") {
		t.Error("top does not sit on mid")
	}
}

func TestStackRebaseLeavesABranchAboveThatAnotherWorktreeHoldsUnpushed(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "mid", "top")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid", "top")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "mid")
	before := stackHeads(t, f, "mid", "top")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	out, errOut, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase from base: %v\n%s", err, errOut)
	}
	for _, want := range []string{
		"mid held by " + held + ", not pushed; its lane runs ccx vcs stack submit",
		"top sits on mid, held by " + held + ", not pushed; its lane runs ccx vcs stack submit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	stackAssertUntouched(t, f, before)
	if got := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); got != before["mid"] {
		t.Errorf("%s HEAD = %.12s, want it left at %.12s", held, got, before["mid"])
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")) {
		t.Error("origin base is not on the new trunk")
	}
}

func TestStackContinuePushesOnlyTheLanesOwnBranches(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t, vcstest.GTStack("low", "mid"))
	stubOpenPRs(t, f, nil, "low", "mid", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "c.txt", "feature\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "low", "mid", "feature")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "mid")
	writeShipFile(t, held, "c.txt", "mid lane\n")
	mustRun(t, f.Env(), held, "git", "add", "c.txt")
	mustRun(t, f.Env(), held, "git", "commit", "-qm", "mid lane")
	mustRun(t, f.Env(), held, "git", "push", "-q", "origin", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	before := stackHeads(t, f, "low", "mid")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil {
		t.Fatal("stack rebase succeeded, want feature's conflict with mid's new head to stop it")
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "c.txt", "resolved\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")

	if _, errOut, err := runStackCmdIn(t, f, ws, "continue"); err != nil {
		t.Fatalf("continue: %v\n%s", err, errOut)
	}
	stackAssertUntouched(t, f, before)
	feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", feature+"^"); got != before["mid"] {
		t.Errorf("published feature sits on %.12s, want mid's published head %.12s", got, before["mid"])
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "show", feature+":c.txt"); got != "resolved" {
		t.Errorf("published feature's c.txt = %q, want the resolution", got)
	}
}

func TestStackRebaseIncludeTakesOnlyTheNamedHeldBranch(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "low", "mid", "top")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "low", "mid", "top")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "mid")
	before := stackHeads(t, f, "low")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	out, errOut, err := runStackCmd(t, f, "rebase", "--include", "mid")
	if err != nil {
		t.Fatalf("stack rebase --include mid: %v\n%s", err, errOut)
	}
	if want := "low sits under mid, held by " + held + ", not pushed"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want %q", out, want)
	}
	stackAssertUntouched(t, f, before)
}

func TestStackSubmitKeepsABranchNoWorkingCopyHoldsUnderAnotherLanesAtItsPublishedHead(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "tree", "fixtures", "step5", "claims")
	api.prs["tree"], api.prs["fixtures"], api.prs["step5"], api.prs["claims"] = 7, 8, 9, 10
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish the stack: %v", err)
	}
	tree := restackSiblingPath(t, "lane-tree")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", tree, "tree")
	step5 := restackSiblingPath(t, "lane-step5")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", step5, "step5")
	writeShipFile(t, tree, "tree-fix.txt", "tree fix\n")
	mustRun(t, f.Env(), tree, "git", "add", "tree-fix.txt")
	mustRun(t, f.Env(), tree, "git", "commit", "-qm", "tree fix")
	mustRun(t, f.Env(), tree, "git", "push", "-q", "origin", "tree")
	before := stackHeads(t, f, "fixtures", "step5")
	stackCommit(t, f, "claims-more.txt")
	posted := len(api.submitHeads())
	shipResetLog(t, f)

	out, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit from claims: %v\n%s", err, errOut)
	}
	stackAssertUntouched(t, f, before)
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"claims"}) {
		t.Errorf("submit posts = %v, want claims alone", heads)
	}
	if claims := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "claims"); !stackOnto(t, f, before["step5"], claims) {
		t.Errorf("published claims %.12s is not stacked on step5's published head %.12s", claims, before["step5"])
	}
	if want := "stack submit: keeping fixtures at its published head — it sits under step5, checked out in " + step5 + "; 0 commit(s) not on origin; pass --include fixtures to submit it\n"; !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestStackSubmitIncludeKeepsTheBranchesBelowEveryNamedOneAtTheirPublishedHeads(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "other", "mine", "tip")
	api.prs["other"], api.prs["mine"], api.prs["tip"] = 7, 8, 9
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish the stack: %v", err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "other")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "other")
	stackCommit(t, f, "other-rework.txt")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "other")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "tip")
	stackCommit(t, f, "tip-more.txt")
	posted := len(api.submitHeads())
	shipResetLog(t, f)

	out, errOut, err := runStackCmd(t, f, "submit", "--include", "mine")
	if err != nil {
		t.Fatalf("stack submit --include mine: %v\n%s", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "other"); got != published {
		t.Errorf("origin other = %.12s, want its published head %.12s left alone", got, published)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "other"); got != local {
		t.Errorf("local other = %.12s, want it left at %.12s", got, local)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"tip"}) {
		t.Errorf("submit posts = %v, want tip alone", heads)
	}
	if want := "stack submit: keeping other at its published head — it sits under mine, the lowest branch this run names; 1 commit(s) not on origin; pass --include other to submit it\n"; !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}
