package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

var strayOnAnotherBase = map[string]*stackPR{"stray": {Number: 42, Title: "stray", State: "OPEN", Base: "other"}}

func strayOnAnotherBaseRepo(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.openPRs("b")
	shipGTStack(t, f, "a", "b", "stray")
	stubStackPRs(t, f, strayOnAnotherBase)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	return f
}

func TestStackKeptNamesTheMoveOnANativeLaneAndKeepsTheStrayItNames(t *testing.T) {
	f := strayOnAnotherBaseRepo(t)
	ctx, dir := f.Context(), render.Dir(f.Dir)
	_, state, err := gtStackAll(ctx, dir, stackRebasePrefix)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := gtTrunkRef(ctx, dir, stackRebasePrefix, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		moves  []string
		native bool
		kept   bool
		why    string
	}{
		{name: "graphite lane", why: "re-record it with gt track --parent other stray"},
		{name: "native lane", native: true, why: "if b is, move the pull request onto it with ccx vcs stack submit --move stray"},
		{name: "native lane moving it", moves: []string{"stray"}, native: true, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, left, _, err := stackKept(ctx, dir, state, tr, "a", []string{"a", "b", "stray"}, strayOnAnotherBase, nil, nil, tc.moves, tc.native)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(kept, "stray") != tc.kept {
				t.Fatalf("kept = %v, left = %+v, want stray kept %t", kept, left, tc.kept)
			}
			if tc.kept {
				return
			}
			i := slices.IndexFunc(left, func(l stackLeft) bool { return l.branch == "stray" })
			if i < 0 || !strings.Contains(left[i].why, tc.why) || tc.native != strings.Contains(left[i].why, "--move") {
				t.Fatalf("left = %+v, want stray left naming %q", left, tc.why)
			}
		})
	}
}

func TestStackPlanRefusesAMoveOfABranchOutsideTheRun(t *testing.T) {
	f := strayOnAnotherBaseRepo(t)
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stackPlan(f.Context(), l, filepath.Join(f.Dir, ".git"), stackRebaseOpts{members: []string{"a", "b"}, moves: []string{"stray"}, submit: true, dryRun: true})
	if err == nil || !strings.Contains(err.Error(), "--move named stray, which is not a branch of this run") {
		t.Fatalf("stackPlan(--move stray outside the run) = %v, want the refusal naming stray", err)
	}
}

func TestStackSubmitRefusesMoveOnAGraphiteLane(t *testing.T) {
	f := strayOnAnotherBaseRepo(t)
	_, _, err := runStackCmd(t, f, "submit", "--move", "stray")
	if err == nil || !strings.Contains(err.Error(), "--move moves a pull request out of a native GitHub stack") {
		t.Fatalf("stack submit --move on a Graphite lane = %v, want the native-only refusal", err)
	}
}

func TestStackMoveRefusesAGraphiteLane(t *testing.T) {
	f := strayOnAnotherBaseRepo(t)
	_, _, err := runStackCmd(t, f, "move", "--onto", "b")
	if err == nil || !strings.Contains(err.Error(), "does not submit native GitHub stacks") || !strings.Contains(err.Error(), "stack submit --parent") {
		t.Fatalf("stack move on a Graphite lane = %v, want the refusal naming stack submit --parent", err)
	}
}

func TestStackPlanNamesTheInPlaceRetargetOfAMovedPullRequest(t *testing.T) {
	branch := stackRebaseBranch{Name: "tip", Parent: "fix", WasParent: "lower", Replays: 2, PR: &stackPR{Number: 5, Title: "Tip", Base: "lower"}}
	for _, tc := range []struct {
		name    string
		inPlace bool
		moves   []string
		want    bool
	}{
		{name: "moved in place", inPlace: true, moves: []string{"tip"}, want: true},
		{name: "moved by a new pull request", moves: []string{"tip"}},
		{name: "not moved", inPlace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &stackRebaseRun{Trunk: "main", Pin: "abc", Moves: tc.moves, MoveInPlace: tc.inPlace, Branches: []stackRebaseBranch{branch}}
			got := strings.Join(stackPlanLines(run), "\n")
			if has := strings.Contains(got, "retargets #5 from lower, relinking its native stack"); has != tc.want {
				t.Fatalf("plan = %q, names the in-place retarget = %t, want %t", got, has, tc.want)
			}
		})
	}
}
