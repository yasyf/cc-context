package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
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

func TestStackListJSON(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "restacked"
		if stale {
			name = "needs-restack"
		}
		t.Run(name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "base", "middle")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
			lane := filepath.Join(t.TempDir(), "feature lane")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-qb", "feature", lane, "middle")
			lane = gitAt(t, f.Env(), lane, "rev-parse", "--show-toplevel")
			mustRun(t, f.Env(), lane, "gt", "track", "--parent", "middle", "--no-interactive")
			mustRun(t, f.Env(), lane, "gt", "freeze", "--no-interactive")
			if stale {
				mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "advance base")
			}

			out, errOut, err := runStackCmd(t, f, "list", "--json")
			if err != nil {
				t.Fatalf("stack list --json: %v\n%s", err, errOut)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("decode listing %q: %v", out, err)
			}
			want := map[string]any{
				"root": f.Dir,
				"branches": []any{
					map[string]any{"branch": "base", "path": f.Dir, "current": true, "needs_restack": false, "state": "frozen"},
					map[string]any{"branch": "middle", "path": "", "current": false, "needs_restack": stale, "state": "frozen"},
					map[string]any{"branch": "feature", "path": lane, "current": false, "needs_restack": false, "state": "frozen"},
				},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("listing = %#v, want %#v", got, want)
			}
		})
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

func TestStackSubmitFrozenBranches(t *testing.T) {
	tests := []struct {
		name     string
		frozen   bool
		unfrozen bool
		stale    bool
	}{
		{name: "frozen-stale", frozen: true, stale: true},
		{name: "frozen-current", frozen: true},
		{name: "restack-and-push", stale: true},
		{name: "unfrozen-restack-and-push", unfrozen: true, stale: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			branches := []string{"base", "feature", "tip"}
			shipGTStack(t, f, branches...)
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature", "tip")
			remote := map[string]string{}
			for _, branch := range branches {
				remote[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
				if tt.frozen || tt.unfrozen {
					mustRun(t, f.Env(), f.Dir, "gt", "freeze", branch, "--no-interactive")
				}
			}
			if tt.unfrozen {
				for _, branch := range branches {
					mustRun(t, f.Env(), f.Dir, "gt", "unfreeze", branch, "--no-interactive")
				}
			}
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
			for _, branch := range branches[1:] {
				mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", filepath.Join(t.TempDir(), branch), branch)
			}
			if tt.stale {
				mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "advance base")
			}
			before := map[string]string{}
			for _, branch := range branches {
				before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
			}

			out, _, err := runStackCmd(t, f, "submit")
			if tt.frozen && tt.stale {
				if err == nil || !strings.Contains(err.Error(), "feature is frozen") {
					t.Fatalf("stack submit = %q, %v; want a frozen feature refusal", out, err)
				}
				if heads := api.submitHeads(); len(heads) != 0 {
					t.Errorf("submitted %v despite a frozen stale branch", heads)
				}
				for _, branch := range branches {
					if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != before[branch] {
						t.Errorf("local %s = %s, want unchanged %s", branch, got, before[branch])
					}
					if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != remote[branch] {
						t.Errorf("remote %s = %s, want unchanged %s", branch, got, remote[branch])
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			if !strings.Contains(out, "submitted 3 branches") {
				t.Errorf("report = %q, want all three branches submitted", out)
			}
			if heads := api.submitHeads(); !slices.Equal(heads, branches) {
				t.Errorf("submitted %v, want %v", heads, branches)
			}
			parent := "main"
			for _, branch := range branches {
				head := gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
				if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != head {
					t.Errorf("remote %s = %s, want local %s", branch, got, head)
				}
				mustRun(t, f.Env(), f.Dir, "git", "merge-base", "--is-ancestor", parent, branch)
				parent = branch
			}
		})
	}
}

func TestStackSubmitFrozenSiblingStopsReplay(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "a-movable")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	shipGTStack(t, f, "z-frozen")
	mustRun(t, f.Env(), f.Dir, "gt", "freeze", "z-frozen", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	branches := []string{"base", "a-movable", "z-frozen"}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "a-movable", "z-frozen")
	remote := map[string]string{}
	for _, branch := range branches {
		remote[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	}
	mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "advance base")
	before := map[string]string{}
	for _, branch := range branches {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "z-frozen is frozen") {
		t.Fatalf("stack submit = %q, %v; want a frozen sibling refusal", out, err)
	}
	if n := api.routeCount("/graphite/cli/submit/pre-submit-pull-requests"); n != 0 {
		t.Errorf("presubmit calls = %d, want none", n)
	}
	if heads := api.submitHeads(); len(heads) != 0 {
		t.Errorf("submitted %v despite a frozen stale sibling", heads)
	}
	for _, branch := range branches {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != before[branch] {
			t.Errorf("local %s = %s, want unchanged %s", branch, got, before[branch])
		}
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != remote[branch] {
			t.Errorf("remote %s = %s, want unchanged %s", branch, got, remote[branch])
		}
	}
}

func TestStackSubmitRefusesIncorrectRestackMetadata(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	branches := []string{"base", "feature"}
	shipGTStack(t, f, branches...)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	remote := map[string]string{}
	for _, branch := range branches {
		remote[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "advance base")
	before := map[string]string{}
	for _, branch := range branches {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := gtmeta.RecordRestacked(f.Context(), commonDir, map[string]string{"feature": before["base"]}); err != nil {
		t.Fatalf("record feature as restacked: %v", err)
	}
	state, err := gtStateAt(f.Context(), commonDir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if state["feature"].NeedsRestack {
		t.Fatal("feature metadata still requests a restack")
	}
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "restack left feature off its parent") {
		t.Fatalf("stack submit = %q, %v; want an off-parent feature refusal", out, err)
	}
	if n := api.routeCount("/graphite/cli/submit/pre-submit-pull-requests"); n != 0 {
		t.Errorf("presubmit calls = %d, want none", n)
	}
	if heads := api.submitHeads(); len(heads) != 0 {
		t.Errorf("submitted %v despite incorrect restack metadata", heads)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "push" {
			t.Errorf("pushed %v despite incorrect restack metadata", inv)
		}
	}
	for _, branch := range branches {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != before[branch] {
			t.Errorf("local %s = %s, want unchanged %s", branch, got, before[branch])
		}
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != remote[branch] {
			t.Errorf("remote %s = %s, want unchanged %s", branch, got, remote[branch])
		}
	}
}

// TestStackSubmitSubmitsTheCleanPrefixOfAConflict pins what a conflict above a
// clean branch costs: that branch and what sits on it, nothing below. base
// restacks onto the new trunk and is submitted; feature is left where it was,
// its ref, its remote and gt's record of its base alike, and the refusal names
// it with the stack rebase that resolves it. The replay still publishes only
// after every branch it keeps has replayed, so native gt never reads a
// half-moved stack.
func TestStackSubmitSubmitsTheCleanPrefixOfAConflict(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackConflicting(t, f)
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	before, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(t.TempDir(), "state.json")
	realGit := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\n"+
		"if [ -z \"$CCX_SHIM_DEPTH\" ]; then\n"+
		"  case \"$*\" in\n"+
		"    'replay --onto '*|'-c replay.refAction=print replay --onto '*)\n"+
		"      "+shellSingleQuote(realGit)+" \"$@\" || exit \"$?\"\n"+
		"      CCX_SHIM_DEPTH=1 gt state --no-interactive > "+shellSingleQuote(observed)+"\n"+
		"      exit \"$?\" ;;\n"+
		"  esac\n"+
		"fi\n"+
		"exec "+shellSingleQuote(realGit)+" \"$@\"\n")

	out, _, err := runStackCmd(t, f, "submit", "--pr-title", "feature=feature work")
	if err == nil {
		t.Fatal("stack submit succeeded, want feature's conflict reported")
	}
	for _, want := range []string{"left feature unsubmitted", "feature does not rebase onto base cleanly", "ccx vcs stack rebase --no-push"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q", err, want)
		}
	}
	if !strings.Contains(out, "submitted 1 branches") {
		t.Errorf("report = %q, want base submitted", out)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
		t.Errorf("submit posts = %v, want base alone", heads)
	}
	if !stackOnto(t, f, "origin/main", "base") {
		t.Error("base was not restacked onto the new trunk")
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"), gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != want {
		t.Errorf("origin base = %s, want the restacked %s", got, want)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature = %s, want it left at %s", got, feature)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "feature") {
		t.Error("origin has feature — the submit pushed the branch that conflicted")
	}
	after, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := after["feature"], before["feature"]; got.Head != want.Head || !reflect.DeepEqual(got.Parents, want.Parents) {
		t.Errorf("feature's record = %#v, want its head and recorded base unchanged from %#v", got, want)
	}
	if !after["feature"].NeedsRestack {
		t.Error("gt reads feature as restacked, want it named off its moved parent")
	}
	state, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]struct {
		NeedsRestack bool `json:"needs_restack"`
	}
	if err := json.Unmarshal(state, &native); err != nil {
		t.Fatal(err)
	}
	if !native["base"].NeedsRestack {
		t.Errorf("native gt observed a temporary replayed base: %s", state)
	}
}

// TestRestackRefusalsRouteThroughStackRebase pins where every branch a replay
// cannot move is sent: ccx vcs stack rebase, which rebases with rerere off and
// resumes through ccx vcs stack continue. gt restack replays whatever rerere
// recorded, and it has lost its own operation mid-conflict, leaving raw git
// rebase --continue as the only way out.
func TestRestackRefusalsRouteThroughStackRebase(t *testing.T) {
	for _, msg := range []string{
		(&errRestackConflict{Branch: "feature", Onto: "base"}).Error(),
		gtOffParent("feature", ""),
		(&errSubmitInherited{Branch: "feature", Trunk: "main", Commits: []string{"abc1234"}}).Error(),
		(&errRestackBehind{Trunk: "main", Branches: []string{"feature"}, Summary: "conflict"}).Error(),
	} {
		if strings.Contains(msg, "gt restack") || !strings.Contains(msg, "ccx vcs stack rebase") {
			t.Errorf("refusal = %q, want ccx vcs stack rebase rather than gt restack", msg)
		}
	}
}

func TestStackSubmitRestackPublishesAtomically(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	concurrent := gitAt(t, f.Env(), f.Dir, "commit-tree", feature+"^{tree}", "-p", feature, "-m", "concurrent change")
	restackAdvanceRemote(t, f, "main", "advanced.txt", "advanced\n")
	realGit := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\n"+
		"if [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$*\" = 'update-ref --stdin' ]; then\n"+
		"  CCX_SHIM_DEPTH=1 "+shellSingleQuote(realGit)+" update-ref refs/heads/feature "+concurrent+" "+feature+" || exit \"$?\"\n"+
		"  CCX_SHIM_DEPTH=1 gt freeze base --no-interactive >&2 || exit \"$?\"\n"+
		"fi\n"+
		"exec "+shellSingleQuote(realGit)+" \"$@\"\n")

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "publish restacked branches") {
		t.Fatalf("stack submit = %v, want a failed ref transaction", err)
	}
	for branch, want := range map[string]string{"base": base, "feature": concurrent} {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != want {
			t.Errorf("%s = %s, want %s", branch, got, want)
		}
	}
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	state, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if state["base"].State != "frozen" {
		t.Errorf("base state = %q, want the concurrent freeze preserved", state["base"].State)
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

// TestStackSubmitReRecordsABranchRebasedOutsideGT is the postpath refusal: a
// branch rebased onto trunk with raw git leaves gt's recorded base behind, so
// the span gt describes reaches back over trunk commits. The branch already sits
// on the pinned trunk, so its record moves and its shas stay.
func TestStackSubmitReRecordsABranchRebasedOutsideGT(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.HasPrefix(out, "re-recorded the base of base") {
		t.Errorf("report = %q, want it to name the re-recorded base", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — a branch already on trunk is not replayed", got, base)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	if s := state["base"]; s.NeedsRestack {
		t.Errorf("gt still reads base as off its parent: %+v", s)
	}
}

// TestStackSubmitLeavesTrunkOutOfAStaleSpan is the same stale record on a
// branch that trunk has since moved past again: the span reaches over trunk
// commits the pin holds, and the replay carries the branch's own commit alone
// rather than copying those onto it.
func TestStackSubmitLeavesTrunkOutOfAStaleSpan(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	restackAdvanceRemote(t, f, "main", "later.txt", "later\n")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "base..refs/remotes/origin/main"); behind != "0" {
		t.Errorf("origin/main holds %s commit(s) base does not", behind)
	}
	if files := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", "refs/remotes/origin/main...base"); files != "base.txt" {
		t.Errorf("base proposes %q, want base.txt alone", files)
	}
}

// TestStackSubmitRefusesCopiesOfTheParent keeps the refusal where no exclusion
// by ancestry can help: a child rebased onto trunk with raw git carries its
// parent's commit under a new sha, which a replay onto the parent would apply a
// second time.
func TestStackSubmitRefusesCopiesOfTheParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	// A rebase in the same second as the submit's replay of base would mint
	// base's replacement at the copy's sha, and feature would sit on it.
	mustRun(t, append(f.Env(), "GIT_COMMITTER_DATE=2001-09-09T01:46:40Z"), f.Dir, "git", "rebase", "-q", "origin/main")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded, want the copy of base's commit refused")
	}
	if !strings.Contains(err.Error(), "feature carries 1 commit(s) base already holds under other shas") {
		t.Errorf("error = %v, want it to name the copy", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — the refusal rolls the chain back", got, base)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature = %s, want %s", got, feature)
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

func TestStackSubmitRestacksARejectedParentRevision(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	oldBase := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "--amend", "-qm", "rewritten base")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "base", oldBase, "feature")
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	db, err := sql.Open("sqlite", filepath.Join(commonDir, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE branch_metadata SET validation_result = ? WHERE branch_name = ?`, "BAD_PARENT_REVISION", "feature"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	_, _, err = runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	state, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
		t.Fatal(err)
	}
	for branch, parent := range map[string]string{"base": "main", "feature": "base"} {
		if state[branch].NeedsRestack {
			t.Errorf("%s still needs restack", branch)
		}
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch+"^"); got != state[parent].Head {
			t.Errorf("%s parent = %s, want %s", branch, got, state[parent].Head)
		}
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != state[branch].Head {
			t.Errorf("remote %s = %s, want %s", branch, got, state[branch].Head)
		}
	}
}

// stackBesideBase cuts base and name from trunk while both sit on it, adopts
// name onto base the way gt track does for two branches at one commit, and only
// then gives base a commit: gt records name as base's child though it carries
// none of base's work. With own set, name takes a commit of its own as well.
func stackBesideBase(t *testing.T, f *vcstest.Fixture, name string, own bool) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "base")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", name)
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "base", "--no-interactive")
	if own {
		writeShipFile(t, f.Dir, name+".txt", name+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name+".txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	writeShipFile(t, f.Dir, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "base.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	if parent := dropGTParent(t, f, name); parent != "base" {
		t.Fatalf("gt parent of %s = %s, want base", name, parent)
	}
}

// TestStackSubmitLeavesAStrayBranchAlone is #25109: a branch gt parents on this
// stack whose pull request targets trunk and whose history carries none of its
// parent's work belongs to another lane, and a submit run from the stack below
// it must neither replay it onto that parent nor push it.
func TestStackSubmitLeavesAStrayBranchAlone(t *testing.T) {
	for _, tt := range []struct {
		name string
		pr   bool
	}{
		{name: "its pull request targets trunk", pr: true},
		{name: "it has no pull request"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			stackBesideBase(t, f, "stray", true)
			if tt.pr {
				api.prs["stray"], api.bases["stray"] = 41, "main"
			}
			stray := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray")
			shipResetLog(t, f)

			_, errOut, err := runStackCmd(t, f, "submit")
			if err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray"); got != stray {
				t.Errorf("stray moved to %s, want it left at %s", got, stray)
			}
			if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
				t.Errorf("submit posts = %v, want base alone", heads)
			}
			if !strings.Contains(errOut, "stray") {
				t.Errorf("stderr = %q, want it to name the branch it left alone", errOut)
			}
		})
	}
}

// TestStackSubmitLeavesAStrayBesideItsGrandparent is a stray cut from its gt
// parent's own parent: it shares that grandparent's commits with the parent, and
// still carries none of the parent's own.
func TestStackSubmitLeavesAStrayBesideItsGrandparent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "p")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "stray")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "p", "--no-interactive")
	writeShipFile(t, f.Dir, "stray.txt", "stray\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "stray.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "stray")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "p\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	stray := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray")
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray"); got != stray {
		t.Errorf("stray moved to %s, want it left at %s", got, stray)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"a", "p"}) {
		t.Errorf("submit posts = %v, want a and p", heads)
	}
	if !strings.Contains(errOut, "gt track --force --parent a stray") {
		t.Errorf("stderr = %q, want it to re-record stray onto a", errOut)
	}
}

// TestStackSubmitLeavesAStrayAboveALandedBranch runs the submit from a branch
// that landed: dropping it must not take the stray check with it.
func TestStackSubmitLeavesAStrayAboveALandedBranch(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b", "stray")
	api.prs["stray"], api.bases["stray"] = 42, "main"
	api.landed["a"] = gtStubLanded{number: 41, state: gtapi.PRMerged, head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")}
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	stray := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "stray"); got != stray {
		t.Errorf("stray moved to %s, want it left at %s", got, stray)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"b"}) {
		t.Errorf("submit posts = %v, want b alone", heads)
	}
}

// TestStackSubmitRefusesPRFieldsForABranchLeftOut names a stray in
// --pr-title: the submit leaves that branch alone, so it must not restate its
// pull request either.
func TestStackSubmitRefusesPRFieldsForABranchLeftOut(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	writeShipGH(t, f)
	stackBesideBase(t, f, "stray", true)
	api.prs["base"], api.prs["stray"], api.bases["stray"] = 5, 41, "main"
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit", "--pr-title", "stray=Stray title")
	if err == nil || !strings.Contains(err.Error(), "leaves out") {
		t.Fatalf("stack submit error = %v, want a refusal naming stray", err)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if isPREdit(inv) {
			t.Errorf("restated %v", inv)
		}
	}
}

// TestStackSubmitSkipsAnEmptyChild pins an in-progress lane: a tracked child
// with no commit past trunk is a lane nobody has committed to yet, not a merged
// one, and it must not abort the submit of the stack below it.
func TestStackSubmitSkipsAnEmptyChild(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	stackBesideBase(t, f, "lane", false)
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	lane := gitAt(t, f.Env(), f.Dir, "rev-parse", "lane")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "lane"); got != lane {
		t.Errorf("lane moved to %s, want it left at %s", got, lane)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
		t.Errorf("submit posts = %v, want base alone", heads)
	}
	if !strings.Contains(out, "skipped empty lane") {
		t.Errorf("report = %q, want it to name the empty lane", out)
	}
}

// TestStackSubmitSkipsAnEmptyChildOfABranch is an empty lane on an unpublished
// branch: nothing trunk holds, and no commit to open a pull request from.
func TestStackSubmitSkipsAnEmptyChildOfABranch(t *testing.T) {
	for _, tt := range []struct {
		name    string
		advance bool
	}{
		{name: "its parent moved on", advance: true},
		{name: "its parent stayed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			shipGTStack(t, f, "base")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "lane")
			mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "base", "--no-interactive")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
			if tt.advance {
				writeShipFile(t, f.Dir, "more.txt", "more\n")
				mustRun(t, f.Env(), f.Dir, "git", "add", "more.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "more")
			}
			shipResetLog(t, f)

			out, _, err := runStackCmd(t, f, "submit")
			if err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
				t.Errorf("submit posts = %v, want base alone", heads)
			}
			if !strings.Contains(out, "skipped empty lane") {
				t.Errorf("report = %q, want it to name the empty lane", out)
			}
		})
	}
}

// stackHeldFeature stacks feature on base, hands feature to a sibling working
// copy, and moves trunk so the submit has to restack the held branch.
func stackHeldFeature(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	shipResetLog(t, f)
	return held
}

// TestStackSubmitSnapshotsOnlyADirtyWorkingCopy covers a lane whose tree is
// clean but whose index is stat-dirty, as a touched file leaves it: git stash
// create sees a change, refreshes, finds none, and exits 1 with no message,
// which failed a submit over a clean lane. A clean tree has nothing to set
// aside, so no snapshot runs; when a dirty lane's snapshot does fail, the
// refusal names the command.
func TestStackSubmitSnapshotsOnlyADirtyWorkingCopy(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		f := shipGTRepo(t)
		stubGTAPI(t)
		held := stackHeldFeature(t, f)
		touched := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(held, "feature.txt"), touched, touched); err != nil {
			t.Fatalf("touch feature.txt: %v", err)
		}

		if _, _, err := runStackCmd(t, f, "submit"); err != nil {
			t.Fatalf("stack submit: %v", err)
		}
		for _, inv := range shipGTInvocations(t, f) {
			if len(inv) > 2 && inv[0] == "git" && inv[1] == "stash" && inv[2] == "create" {
				t.Errorf("ran %v over a clean working copy", inv)
			}
		}
	})
	t.Run("staged, with the tree put back", func(t *testing.T) {
		f := shipGTRepo(t)
		stubGTAPI(t)
		held := stackHeldFeature(t, f)
		writeShipFile(t, held, "feature.txt", "staged\n")
		mustRun(t, f.Env(), held, "git", "add", "feature.txt")
		writeShipFile(t, held, "feature.txt", "feature\n")

		if _, _, err := runStackCmd(t, f, "submit"); err != nil {
			t.Fatalf("stack submit: %v", err)
		}
		if staged := gitAt(t, f.Env(), held, "show", ":feature.txt"); staged != "staged" {
			t.Errorf("index holds %q for feature.txt, want the staged edit kept", staged)
		}
	})
	t.Run("dirty with its index locked", func(t *testing.T) {
		f := shipGTRepo(t)
		stubGTAPI(t)
		held := stackHeldFeature(t, f)
		writeShipFile(t, held, "feature.txt", "edited\n")
		gitDir := gitAt(t, f.Env(), held, "rev-parse", "--absolute-git-dir")
		writeShipFile(t, gitDir, "index.lock", "")

		_, _, err := runStackCmd(t, f, "submit")
		if err == nil {
			t.Fatal("stack submit succeeded over a locked index")
		}
		if !strings.Contains(err.Error(), "git stash create") {
			t.Errorf("error = %v, want it to name the command that failed", err)
		}
	})
}

// TestStackSubmitWritesPRTitlesAndBodies gives stack submit the branch-scoped
// title and body flags ship carries: a bare value applies to the branch checked
// out here, a <branch>= value to that branch.
func TestStackSubmitWritesPRTitlesAndBodies(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	writeShipGH(t, f)
	shipGTStack(t, f, "base", "feature")
	api.prs["base"], api.prs["feature"] = 5, 6
	body := writePRBody(t, "base.md", "base body\n")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--pr-title", "Feature title", "--pr-body-file", "base="+body); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	var edits []string
	for _, inv := range shipGTInvocations(t, f) {
		if isPREdit(inv) {
			edits = append(edits, strings.Join(inv[4:], " "))
		}
	}
	slices.Sort(edits)
	want := []string{
		"repos/" + fakePRRepo + "/pulls/5 --silent -F body=@" + body,
		"repos/" + fakePRRepo + "/pulls/6 --silent -f title=Feature title",
	}
	if !slices.Equal(edits, want) {
		t.Errorf("restates = %q, want %q", edits, want)
	}
}

// TestStackNewTakesASlashedBranchName cuts a branch whose name carries the
// owner/topic convention: the branch keeps the slash, and the working copy's one
// path element folds it into a dash.
func TestStackNewTakesASlashedBranchName(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")

	out, _, err := runStackCmd(t, f, "new", "yasyf/feature")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	path := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	if base := filepath.Base(path); base != "yasyf-feature" {
		t.Errorf("minted %q, want a pool entry named yasyf-feature", path)
	}
	if there := gitAt(t, f.Env(), path, "branch", "--show-current"); there != "yasyf/feature" {
		t.Errorf("the new working copy is on %q, want yasyf/feature", there)
	}
	if parent := dropGTParent(t, f, "yasyf/feature"); parent != "base" {
		t.Errorf("gt parent = %s, want base", parent)
	}
}

// TestStackNewOnTrunkCutsFromTheFetchedTrunk cuts a lane straight off trunk
// while the local trunk lags the remote: the lane starts where a pull request
// is measured, the remote trunk, not on the stale local branch.
func TestStackNewOnTrunkCutsFromTheFetchedTrunk(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main")

	out, _, err := runStackCmd(t, f, "new", "lane", "--parent", "main")
	if err != nil {
		t.Fatalf("stack new: %v", err)
	}
	path := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	if head := gitAt(t, f.Env(), path, "rev-parse", "HEAD"); head != remote {
		t.Errorf("lane cut at %s, want the remote trunk %s", head, remote)
	}
	if s, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test"); err != nil {
		t.Fatalf("gt state: %v", err)
	} else if s["lane"].NeedsRestack {
		t.Errorf("lane reads as needing a restack straight after its cut")
	}
}
