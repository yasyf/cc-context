package cli

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runStackCmd(t *testing.T, f *vcstest.Fixture, args ...string) (string, string, error) {
	t.Helper()
	cmd := newStackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(f.Context())
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

	out, _, err := runStackCmd(t, f, "new", "feature")
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
	if here := gitAt(t, f.Env(), f.Dir, "branch", "--show-current"); here != "base" {
		t.Errorf("the calling working copy is on %q, want base — stack new must not switch it", here)
	}
	if there := gitAt(t, f.Env(), path, "branch", "--show-current"); there != "feature" {
		t.Errorf("the new working copy is on %q, want feature", there)
	}
}

// TestStackNewTracksTheParent pins the Graphite half: a lane whose branch gt does
// not know is a lane no restack or submit reaches.
func TestStackNewTracksTheParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")

	if _, _, err := runStackCmd(t, f, "new", "feature"); err != nil {
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
	out, _, err := runStackCmd(t, f, "new", "feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	lane := out[strings.LastIndex(out, shipSep)+len(shipSep):]

	out, _, err = runStackCmd(t, f, "list")
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

	out, _, err := runStackCmd(t, f, "new", "feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	lane := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	for _, dir := range []string{".git", ".jj"} {
		if _, statErr := os.Stat(filepath.Join(lane, dir)); statErr != nil {
			t.Errorf("lane %s has no %s: %v — it must answer to git, gt and jj alike", lane, dir, statErr)
		}
	}
	if there := gitAt(t, f.Env(), lane, "branch", "--show-current"); there != "feature" {
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

	_, _, err := runStackCmd(t, f, "list")
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

	out, _, err := runStackCmd(t, f, "submit")
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
		if !gitBranchExists(t, f.Env(), f.RemoteDir, branch) {
			t.Errorf("origin lacks %s — the submit never pushed it", branch)
		}
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "gt" && inv[1] == "submit" {
			t.Errorf("stack submit shelled out to %v, want the API path", inv)
		}
	}
}

// TestStackSubmitReportsWhatItProposes pins the tell a hundred-file pull
// request over a one-file change shows up as: the width the submit proposes
// against the remote trunk, named in the report rather than discovered on
// GitHub afterwards.
func TestStackSubmitReportsWhatItProposes(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if want := "proposing 2 commit(s), 2 file(s)"; !strings.Contains(out, want) {
		t.Errorf("report = %q, want %q — one commit and one file per branch", out, want)
	}
}

// stackConflicting stacks feature on base and then puts a commit on trunk that
// feature's own commit will not replay over, so a restack of the chain moves
// base and stops on feature.
func stackConflicting(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	shipGTStack(t, f, "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "c.txt", "feature\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--ff-only", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	shipResetLog(t, f)
}

// TestStackSubmitRestackConflictMovesNothing pins the atomicity a stack spread
// across lanes rests on: a chain that stops partway leaves every branch where
// it was. Keeping the moves it had already made left the bottom on the trunk it
// had just reached with everything above it on the old one — two bases in one
// stack, which only commit archaeology names and only a hand rebuild undoes.
func TestStackSubmitRestackConflictMovesNothing(t *testing.T) {
	f := shipGTRepo(t)
	stackConflicting(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded, want the conflict on feature")
	}
	if !strings.Contains(err.Error(), "the restack rolled back, so nothing moved — put back: base") {
		t.Errorf("error = %v, want it to name what it rolled back", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — it was restacked onto the new trunk while feature stayed on the old one", got, base)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature = %s, want %s", got, feature)
	}
}

// TestStackSubmitRefusesADivergedTrunk pins the refusal that keeps another
// lane's unlanded work out of the stack. A local trunk holding commits the
// remote does not is indistinguishable from trunk's own here, and a restack
// onto it lands them in every branch, where the pull requests then propose to
// merge them.
func TestStackSubmitRefusesADivergedTrunk(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "foreign.txt", "another lane's work\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "foreign.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "foreign")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded on a diverged trunk, want a refusal")
	}
	if !strings.Contains(err.Error(), "main holds 1 commit(s) refs/remotes/origin/main does not") {
		t.Errorf("error = %v, want it to name the drift", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — nothing may move onto a diverged trunk", got, base)
	}
	if files := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", "main...base"); strings.Contains(files, "foreign.txt") {
		t.Errorf("base carries %q — the foreign commit reached the branch", files)
	}
}

// TestStackSubmitRefusesADuplicatingSpan pins the width check. gt records the
// revision a branch was stacked on, and a branch rebased outside gt leaves that
// record behind its real base — so the span gt's metadata describes reaches back
// over trunk commits the branch never owned, and replaying it copies every one
// of them onto the branch.
func TestStackSubmitRefusesADuplicatingSpan(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded on a span carrying trunk's own commits, want a refusal")
	}
	if !strings.Contains(err.Error(), "would replay 2 commits but owns 1") {
		t.Errorf("error = %v, want it to name the commits it would copy", err)
	}
	if !strings.Contains(err.Error(), "propose 2 files rather than the 1 it changed") {
		t.Errorf("error = %v, want the file count that is the tell", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — the refusal comes before any ref moves", got, base)
	}
}

func TestStackListKeepsTheStackWholeAcrossARejectedRevision(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c")
	db, err := sql.Open("sqlite", filepath.Join(f.Dir, ".git", ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	for branch, result := range map[string]string{"b": "BAD_PARENT_REVISION", "c": "INVALID_PARENT"} {
		if _, err := db.Exec(`UPDATE branch_metadata SET validation_result = ? WHERE branch_name = ?`, result, branch); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")

	out, _, err := runStackCmd(t, f, "list")
	if err != nil {
		t.Fatalf("stack list: %v", err)
	}
	for _, branch := range []string{"a", "b", "c"} {
		if !strings.Contains(out, branch+shipSep) {
			t.Errorf("stack list = %q, want %s in the one stack", out, branch)
		}
	}
}
