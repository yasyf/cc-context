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
	f := shipGTRepo(t, vcstest.GTStack("base"))

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
	f := shipGTRepo(t, vcstest.GTStack("base"))

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
	f := shipGTRepo(t, vcstest.GTStack("base"))
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
			f := shipGTRepo(t, vcstest.GTStack("base", "middle"))
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
	f := shipGTRepo(t, vcstest.JJ(), vcstest.GTStack("base"))

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestStackSubmitGoesThroughTheGraphiteAPI(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	api := stubGTAPI(t)
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(out, "published 2 branches · source checkouts unchanged") {
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

func TestStackSubmitReportsWhatItProposes(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
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
			branches := []string{"base", "feature", "tip"}
			f := shipGTRepo(t, vcstest.GTStack(branches...))
			api := stubGTAPI(t)
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
			if tt.stale {
				mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "advance base")
			}
			before := map[string]string{}
			for _, branch := range branches {
				before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
			}

			sources := stackRebaseSourceSnapshot(t, f, branches...)
			out, _, err := runStackCmd(t, f, "submit", "--include", "feature", "--include", "tip")
			if tt.frozen {
				if err == nil || !strings.Contains(err.Error(), "is frozen") {
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
			if !strings.Contains(out, "published 3 branches · source checkouts unchanged") {
				t.Errorf("report = %q, want all three branches published without moving sources", out)
			}
			if heads := api.submitHeads(); !slices.Equal(heads, branches) {
				t.Errorf("submitted %v, want %v", heads, branches)
			}
			for _, branch := range branches {
				stackAssertRebasePublication(t, f, sources[branch])
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
	if err == nil || !strings.Contains(err.Error(), "is frozen") {
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

func TestStackSubmitRepairsIncorrectRestackMetadata(t *testing.T) {
	branches := []string{"base", "feature"}
	f := shipGTRepo(t, vcstest.GTStack(branches...))
	api := stubGTAPI(t)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
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
	sources := stackRebaseSourceSnapshot(t, f, branches...)
	shipResetLog(t, f)

	_, _, err = runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("repair metadata: %v", err)
	}
	base := stackAssertRebasePublication(t, f, sources["base"])
	feature := stackAssertRebasePublication(t, f, sources["feature"])
	if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", base+".."+feature); got != "1" {
		t.Errorf("feature owns %s commits, want 1", got)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, branches) {
		t.Errorf("submitted %v, want %v", heads, branches)
	}
}

func TestStackSubmitRestackConflictMovesNothing(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stackConflicting(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
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

	_, _, err = runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded, want the conflict on feature")
	}
	if !strings.Contains(err.Error(), "nothing has moved") {
		t.Errorf("error = %v, want an unchanged-stack refusal", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base = %s, want %s — it was restacked onto the new trunk while feature stayed on the old one", got, base)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature = %s, want %s", got, feature)
	}
	after, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("metadata after concurrent native validation = %#v, want %#v", after, before)
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
	if _, _, err := runStackCmd(t, f, "abort"); err != nil {
		t.Fatalf("abort: %v", err)
	}
}

func TestStackSubmitRestackPublishesAtomically(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
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
	if err == nil || !strings.Contains(err.Error(), "a source branch moved") {
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

func TestStackSubmitIgnoresDivergedLocalTrunk(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "foreign.txt", "another lane's work\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "foreign.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "foreign")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("submit with unrelated local trunk: %v", err)
	}
	if strings.Contains(errOut, "holds 1 commit") {
		t.Errorf("unrelated trunk warning: %q", errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); got != trunk {
		t.Errorf("local trunk moved to %s", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("already-current base changed to %s", got)
	}
	if files := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", "origin/main...base"); strings.Contains(files, "foreign.txt") {
		t.Errorf("base inherited foreign work: %s", files)
	}
}

func TestStackSubmitDoesNotDuplicateAnAlreadyRebasedSpan(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	shipResetLog(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	_, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("submit rebased branch with stale metadata: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("already rebased branch rewritten: %s", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..base"); got != "1" {
		t.Errorf("submitted branch owns %s commits, want 1", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", "origin/main...base"); got != "base.txt" {
		t.Errorf("submitted changes = %q, want base.txt", got)
	}
}

func TestStackListKeepsTheStackWholeAcrossARejectedRevision(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("a", "b", "c"))
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
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
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
	sources := stackRebaseSourceSnapshot(t, f, "base", "feature")
	before, err := gtmeta.Read(f.Context(), commonDir)
	if err != nil {
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
	for _, branch := range []string{"base", "feature"} {
		if !reflect.DeepEqual(state[branch], before[branch]) {
			t.Errorf("source %s metadata = %+v, want unchanged %+v", branch, state[branch], before[branch])
		}
		published := stackAssertRebasePublication(t, f, sources[branch])
		if !stackOnto(t, f, "origin/main", published) {
			t.Errorf("published %s is not on the new trunk", branch)
		}
	}
}

func stackLane(t *testing.T, f *vcstest.Fixture, name string) string {
	t.Helper()
	out, _, err := runStackCmd(t, f, "new", name)
	if err != nil {
		t.Fatalf("stack new %s: %v", name, err)
	}
	lane := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	writeShipFile(t, lane, name+".txt", name+"\n")
	mustRun(t, f.Env(), lane, "git", "add", name+".txt")
	mustRun(t, f.Env(), lane, "git", "commit", "-qm", name)
	return lane
}

func TestStackSubmitSkipsABranchAnotherWorkingCopyHolds(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base")
	lane := stackLane(t, f, "feature")
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
		t.Errorf("submit posts = %v, want base alone — feature is another lane's", heads)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "feature") {
		t.Error("origin carries feature — the submit pushed another lane's branch")
	}
	if want := "skipping feature (checked out in " + lane + ")"; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}

func TestStackSubmitIncludesANamedLane(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base")
	stackLane(t, f, "feature")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--include", "feature"); err != nil {
		t.Fatalf("stack submit --include feature: %v", err)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base", "feature"}) {
		t.Errorf("submit posts = %v, want base then feature", heads)
	}
}

func stackCommit(t *testing.T, f *vcstest.Fixture, file string) {
	t.Helper()
	writeShipFile(t, f.Dir, file, file+"\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", file)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", file)
}

func stackAssertSubmitRefusesChangedPublicationRemote(t *testing.T, f *vcstest.Fixture, branch, want string) {
	t.Helper()
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	receiptRef := stackPublicationRef(branch, "receipt")
	receipt := gitAt(t, f.Env(), f.Dir, "rev-parse", receiptRef)
	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("stack submit = %v, want the changed publication remote refused", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != source {
		t.Errorf("source %s = %s, want unchanged %s", branch, got, source)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != remote {
		t.Errorf("remote %s = %s, want unchanged %s", branch, got, remote)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", receiptRef); got != receipt {
		t.Errorf("publication receipt = %s, want unchanged %s", got, receipt)
	}
	if pushes := stackPublicationPushes(t, f); pushes != 0 {
		t.Errorf("refused submit ran %d pushes, want none", pushes)
	}
}

func TestStackSubmitRefusesADirectPushAfterPublication(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stackCommit(t, f, "pushed.txt")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	stackCommit(t, f, "local.txt")
	shipResetLog(t, f)

	stackAssertSubmitRefusesChangedPublicationRemote(t, f, "base", "base remote changed after its isolated publication; not adopting the new remote head")
}

func TestShipAmendPushesOverTheHeadItLastSubmitted(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	writeShipFile(t, f.Dir, "base.txt", "base amended\n")
	shipResetLog(t, f)

	if _, _, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "base.txt"); err != nil {
		t.Fatalf("ship --amend = %v, want the amend pushed over the head this repository submitted", err)
	}
	source := stackRebaseSourceSnapshot(t, f, "base")["base"]
	published := stackAssertRebasePublication(t, f, source)
	if !stackOnto(t, f, "origin/main", published) {
		t.Error("published amend is not on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "show", "base:base.txt"); got != "base amended" {
		t.Errorf("remote base.txt = %q, want the amend", got)
	}
}

func TestShipOverAFrozenParentRestacksOnlyTheChild(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "freeze", "--no-interactive")
	frozen := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	shipGTStack(t, f, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	writeShipFile(t, f.Dir, "more.txt", "more\n")
	shipResetLog(t, f)

	if _, _, err := runShipCmdFull(f.Context(), t, "-m", "more", "--no-watch", "more.txt"); err != nil {
		t.Fatalf("ship = %v, want the child shipped over its frozen parent", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != frozen {
		t.Errorf("local base = %s, want the frozen head %s left alone", got, frozen)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != frozen {
		t.Errorf("remote base = %s, want the frozen head %s left alone", got, frozen)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != want {
		t.Errorf("remote feature = %s, want the local head %s", got, want)
	}
	if !stackOnto(t, f, frozen, "feature") {
		t.Error("feature left its frozen parent")
	}
	if stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature moved onto the new trunk past its frozen parent")
	}
}

func TestStackSubmitAdoptsARemoteReplayAfterPublication(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	shipGTStack(t, f, "base")
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	clone := filepath.Join(t.TempDir(), "graphite-app")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "base", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "-c", "user.email=app@graphite.dev", "-c", "user.name=graphite-app", "rebase", "-q", "origin/main")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "--force", "origin", "base")
	replayed := gitAt(t, f.Env(), clone, "rev-parse", "HEAD")
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit over a replay of its own publication = %v, want the replay adopted", err)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if !stackOnto(t, f, "origin/main", remote) || gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main.."+remote) != "1" || gitAt(t, f.Env(), f.Dir, "diff", replayed, remote, "--", "base.txt") != "" {
		t.Fatalf("published base %s is not the adopted %s replayed onto the new trunk", remote, replayed)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != source {
		t.Errorf("source base = %s, want unchanged %s", got, source)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "base")
	if err != nil || receipt == nil || receipt.Source != source || receipt.Head != remote {
		t.Fatalf("receipt = %+v, %v, want source %s published as %s", receipt, err, source, remote)
	}
}

func TestStackSubmitRefusesARemoteRewriteAfterPublication(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	clone := filepath.Join(t.TempDir(), "graphite-app")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "base", f.RemoteDir, clone)
	writeShipFile(t, clone, "rewrite.txt", "rewritten by graphite-app\n")
	mustRun(t, f.Env(), clone, "git", "add", "rewrite.txt")
	mustRun(t, f.Env(), clone, "git", "-c", "user.email=app@graphite.dev", "-c", "user.name=graphite-app", "commit", "-q", "--amend", "--no-edit")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "--force", "origin", "base")
	shipResetLog(t, f)

	stackAssertSubmitRefusesChangedPublicationRemote(t, f, "base", "base has diverged from origin/base")
}

func TestStackSubmitRefusesAForeignMergeCarryingItsOwnChange(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("local restack: %v", err)
	}
	clone := filepath.Join(t.TempDir(), "other")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "base", f.RemoteDir, clone)
	id := []string{"-c", "user.email=t@t.t", "-c", "user.name=t"}
	mustRun(t, f.Env(), clone, "git", append(slices.Clone(id), "merge", "-q", "--no-ff", "--no-commit", "origin/main")...)
	writeShipFile(t, clone, "foreign.txt", "foreign\n")
	mustRun(t, f.Env(), clone, "git", "add", "foreign.txt")
	mustRun(t, f.Env(), clone, "git", append(slices.Clone(id), "commit", "-qm", "merge main")...)
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "base")
	foreign := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "base has diverged from origin/base") {
		t.Fatalf("stack submit = %v, want the foreign merge refused", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != foreign {
		t.Errorf("remote base = %s, want the foreign merge %s kept", got, foreign)
	}
}

func TestStackSubmitRefusesAPushFromElsewhere(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	restackAdvanceRemote(t, f, "base", "foreign.txt", "foreign\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "base")
	foreign := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stackCommit(t, f, "local.txt")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "base has diverged from origin/base") {
		t.Fatalf("stack submit = %v, want the foreign push refused", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != foreign {
		t.Errorf("remote base = %s, want the foreign head %s kept", got, foreign)
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
				stubStackPRs(t, map[string]*stackPR{"stray": {Number: 41, Title: "stray", State: "OPEN", Base: "main"}})
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
			if !strings.Contains(errOut, "left stray alone") {
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
	stubStackPRs(t, map[string]*stackPR{
		"a":     {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")},
		"stray": {Number: 42, Title: "stray", State: "OPEN", Base: "main"},
	})
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
	stubGTAPI(t)
	writeShipGH(t, f)
	stackBesideBase(t, f, "stray", true)
	stubStackPRs(t, map[string]*stackPR{"stray": {Number: 41, Title: "stray", State: "OPEN", Base: "main"}})
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit", "--pr-title", "stray=Stray title")
	if err == nil || !strings.Contains(err.Error(), "leaves out") {
		t.Fatalf("stack submit error = %v, want a refusal naming stray", err)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if isPREdit(inv) || gtPushedRefs([][]string{inv}) != nil {
			t.Errorf("ran %v before refusing", inv)
		}
	}
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
	seedPRViews(t, map[string]string{
		"base":    `{"number":5,"url":"https://github.com/x/pull/5","body":""}`,
		"feature": `{"number":6,"url":"https://github.com/x/pull/6","body":""}`,
	})
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
	if len(edits) != 2 ||
		!strings.HasPrefix(edits[0], "repos/"+fakePRRepo+"/pulls/5 --silent -F body=@") ||
		edits[1] != "repos/"+fakePRRepo+"/pulls/6 --silent -f title=Feature title" {
		t.Errorf("restates = %q, want base's body on #5 and feature's title on #6", edits)
	}
}

// TestStackSubmitSkipsAnEmptyChild pins an in-progress lane: a tracked child
// with no commit past its recorded parent is a lane nobody has committed to yet,
// not a landed one, and it must neither be dropped from gt nor abort the submit
// of the stack below it.
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
	if parent := dropGTParent(t, f, "lane"); parent != "base" {
		t.Errorf("gt parent of lane = %q, want base still tracked", parent)
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

// TestStackSubmitShipsTheParentPastAConflictingChildLane is the lane shape
// behind #69's clean-prefix report: the parent's lane runs stack submit while
// the child, checked out in another lane's working copy, would conflict on the
// new trunk. The parent restacks and is submitted; the child is not touched.
func TestStackSubmitShipsTheParentPastAConflictingChildLane(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	api := stubGTAPI(t)
	stackConflicting(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	child := restackSiblingPath(t, "child")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", child, "feature")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base"}) {
		t.Errorf("submit posts = %v, want base alone", heads)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "base")
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || !stackOnto(t, f, "origin/main", receipt.Head) {
		t.Errorf("base publication = %+v, want a head on the new trunk", receipt)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved to %s, want it left at %s", got, feature)
	}
	if !strings.Contains(errOut, "feature (checked out in "+child+")") {
		t.Errorf("stderr = %q, want it to name the child lane it skipped", errOut)
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
// is measured, the remote trunk, not on the stale local branch, and the local
// trunk is left where it is.
func TestStackNewOnTrunkCutsFromTheFetchedTrunk(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
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
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); got != local {
		t.Errorf("local main moved to %s, want it left at %s", got, local)
	}
	if parent := dropGTParent(t, f, "lane"); parent != "main" {
		t.Errorf("gt parent = %s, want main", parent)
	}
}

// TestStackSubmitRestacksAChildOfAnAmendedParent is the restack stack submit
// exists for: amending the parent's own commit leaves its child carrying the
// old copy under another patch, and the child is still the parent's to move.
func TestStackSubmitRestacksAChildOfAnAmendedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "p", "c")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if strings.Contains(errOut, "left c alone") {
		t.Errorf("stderr = %q, want c restacked, not left as a stray", errOut)
	}
	parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p")
	child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "c")
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.Parent != "p" || receipt.Base != parent || receipt.Head != child {
		t.Fatalf("child publication = %+v, want head %s on p at %s", receipt, child, parent)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", child+".."+parent); behind != "0" {
		t.Errorf("published p holds %s commit(s) published c does not", behind)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"p", "c"}) {
		t.Errorf("submit posts = %v, want p then c", heads)
	}
}

// TestStackRebaseRefusesAParentOverrideTheRunLeavesOut refuses a --parent the
// run cannot honor, before anything moves: the branch it names is one the run
// leaves where it is.
func TestStackRebaseRefusesAParentOverrideTheRunLeavesOut(t *testing.T) {
	f := shipGTRepo(t)
	stackBesideBase(t, f, "lane", false)
	lane := gitAt(t, f.Env(), f.Dir, "rev-parse", "lane")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push", "--parent", "lane=main")
	if err == nil || !strings.Contains(err.Error(), "names lane, which this run leaves where it is") {
		t.Fatalf("error = %v, want a refusal naming the empty lane", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "lane"); got != lane {
		t.Errorf("lane moved to %s on a refusal", got)
	}
}
