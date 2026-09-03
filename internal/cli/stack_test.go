package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runStackCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newStackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return strings.TrimSpace(out.String()), errOut.String(), err
}

// TestStackNewCutsTheBranchInItsOwnWorkingCopy pins what makes a stack workable
// by several agents at once: the branch is created in the new working copy, so
// the one the command ran from never moves. gt create cannot do this — it cuts
// in the working copy it runs from, which is exactly the branch-switch a lane
// per agent cannot afford.
func TestStackNewCutsTheBranchInItsOwnWorkingCopy(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")

	out, _, err := runStackCmd(t, "new", "feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	path := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	if fi, statErr := os.Stat(path); statErr != nil || !fi.IsDir() {
		t.Fatalf("stack new reported %q, which is no working copy: %v", path, statErr)
	}
	if base := filepath.Base(path); base != "feature" {
		t.Errorf("minted %q, want a pool entry named feature", path)
	}
	if here := gitAt(t, f.Dir, "branch", "--show-current"); here != "base" {
		t.Errorf("the calling working copy is on %q, want base — stack new must not switch it", here)
	}
	if there := gitAt(t, path, "branch", "--show-current"); there != "feature" {
		t.Errorf("the new working copy is on %q, want feature", there)
	}
}

// TestStackNewTracksTheParent pins the Graphite half: a lane whose branch gt does
// not know is a lane no restack or submit reaches.
func TestStackNewTracksTheParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")

	if _, _, err := runStackCmd(t, "new", "feature"); err != nil {
		t.Fatalf("stack new: %v", err)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	s, tracked := state["feature"]
	if !tracked {
		t.Fatalf("gt state has no entry for feature: %v", state)
	}
	if len(s.Parents) == 0 || s.Parents[0].Ref != "base" {
		t.Errorf("feature's parents = %v, want base", s.Parents)
	}
}

// TestStackListNamesTheWorkingCopyHoldingEachBranch pins the answer the listing
// exists for: which lane a branch has to be worked from. It runs from the bottom
// of the stack on purpose — the branch above is held by another working copy,
// which this one cannot check out to ask about, so a listing that walked only
// the downstack would show a stack of one and hide every lane above it.
func TestStackListNamesTheWorkingCopyHoldingEachBranch(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	out, _, err := runStackCmd(t, "new", "feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	lane := out[strings.LastIndex(out, shipSep)+len(shipSep):]

	out, _, err = runStackCmd(t, "list")
	if err != nil {
		t.Fatalf("stack list: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("listing = %q, want one line per stack branch", out)
	}
	if want := "base" + shipSep + "here"; lines[0] != want {
		t.Errorf("first line = %q, want %q — the listing reads bottom-up from the branch here", lines[0], want)
	}
	if want := "feature" + shipSep + lane; lines[1] != want {
		t.Errorf("second line = %q, want %q — the lane above must be named, not hidden", lines[1], want)
	}
}

// TestStackNewColocatesJJInTheLane pins the jj half of a lane. jj refuses
// --colocate inside a git worktree and takes the same path for a bare
// jj git init, so the lane is cut with --git-repo . instead — the one spelling
// that works, and the one that leaves jj treating the workspace as colocated.
// jj then detaches git's HEAD at the working-copy commit's parent, which would
// leave gt with no branch to read, so the lane re-attaches by name.
func TestStackNewColocatesJJInTheLane(t *testing.T) {
	f := shipGTRepo(t, vcstest.JJ())
	shipGTStack(t, f, "base")

	out, _, err := runStackCmd(t, "new", "feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	lane := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	for _, dir := range []string{".git", ".jj"} {
		if _, statErr := os.Stat(filepath.Join(lane, dir)); statErr != nil {
			t.Errorf("lane %s has no %s: %v — it must answer to git, gt and jj alike", lane, dir, statErr)
		}
	}
	if there := gitAt(t, lane, "branch", "--show-current"); there != "feature" {
		t.Errorf("lane HEAD = %q, want feature — jj detaches it at @- and the lane must re-attach", there)
	}
}

// TestGtBottomUpOrdersTheRestack pins the order the whole restack rests on: a
// parent rebased after its child leaves the child off its parent again, and
// nothing revisits it.
func TestGtBottomUpOrdersTheRestack(t *testing.T) {
	bottomUp := []string{"low", "mid", "high"}
	if reversed := gtBottomUp([]string{"high", "mid", "low"}); !slices.Equal(reversed, bottomUp) {
		t.Errorf("gtBottomUp = %v, want %v", reversed, bottomUp)
	}
}

// TestGtRestackPlanFollowsParentsNotOrder pins the plan against the shape a
// whole-stack walk produces: a tree, where one subtree sitting off its parent
// says nothing about a sibling in another. Reading the chain positionally —
// everything after the first stale branch moves — rewrites branches that were
// already restacked, and their pull requests with them.
func TestGtRestackPlanFollowsParentsNotOrder(t *testing.T) {
	state := gtState{
		"main":  {Trunk: true, Head: "trunk"},
		"base":  {Head: "base", Parents: []gtRef{{Ref: "main", SHA: "trunk"}}},
		"stale": {Head: "stale", NeedsRestack: true, Parents: []gtRef{{Ref: "base", SHA: "old-base"}}},
		"above": {Head: "above", Parents: []gtRef{{Ref: "stale", SHA: "stale"}}},
		"clean": {Head: "clean", Parents: []gtRef{{Ref: "base", SHA: "base"}}},
	}
	chain := []string{"base", "stale", "clean", "above"}

	got, held := gtRestackPlan(state, chain)
	want := []string{"stale", "above"}
	if !slices.Equal(got, want) {
		t.Errorf("gtRestackPlan(%v) = %v, want %v — clean sits on base and nothing moved under it", chain, got, want)
	}
	if len(held) != 0 {
		t.Errorf("gtRestackPlan held %v, want nothing — gt is holding no branch here", held)
	}
}

// TestGtRestackPlanLeavesAFrozenBranchAlone pins that gt's own hold is honoured.
// gt freeze marks a branch not to be rebased, and gt restack declines it; a
// restack that moves it anyway rebases work the user asked to be left alone,
// and drags its children onto the result.
func TestGtRestackPlanLeavesAFrozenBranchAlone(t *testing.T) {
	state := gtState{
		"main":   {Trunk: true, Head: "trunk"},
		"frozen": {Head: "frozen", NeedsRestack: true, State: "frozen", Parents: []gtRef{{Ref: "main", SHA: "old-trunk"}}},
		"above":  {Head: "above", Parents: []gtRef{{Ref: "frozen", SHA: "frozen"}}},
	}
	chain := []string{"frozen", "above"}

	got, held := gtRestackPlan(state, chain)
	if len(got) != 0 {
		t.Errorf("gtRestackPlan moved %v, want nothing — frozen stays put and above still sits on it", got)
	}
	if held["frozen"] != "frozen" {
		t.Errorf("gtRestackPlan held = %v, want frozen named with its hold", held)
	}
}

// TestStackAllRefusesTrunk pins the scope. Every stack in the repository sits on
// trunk, so a walk rooted there is not "the stack" — it is all of them, which a
// submit would push and a listing would misreport as one.
func TestStackAllRefusesTrunk(t *testing.T) {
	f := shipGTRepo(t)
	_ = f

	_, _, err := runStackCmd(t, "list")
	if err == nil {
		t.Fatal("stack list succeeded on trunk, want a refusal naming trunk")
	}
	if !strings.Contains(err.Error(), "is trunk") {
		t.Errorf("error = %v, want it to name trunk", err)
	}
}

// TestStackSubmitGoesThroughTheGraphiteAPI pins the one submit implementation:
// stack submit force-pushes each branch and posts the stack to Graphite the way
// ship does, rather than shelling out to a gt submit nothing else runs anymore.
func TestStackSubmitGoesThroughTheGraphiteAPI(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "submitted 2 branches") {
		t.Errorf("report = %q, want it to name both branches", out)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base", "feature"}) {
		t.Errorf("submit posts = %v, want one per branch, base first", heads)
	}
	for _, branch := range []string{"base", "feature"} {
		if !gitBranchExists(t, f.RemoteDir, branch) {
			t.Errorf("origin lacks %s — the submit never pushed it", branch)
		}
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "gt" && inv[1] == "submit" {
			t.Errorf("stack submit shelled out to %v, want the API path", inv)
		}
	}
}
