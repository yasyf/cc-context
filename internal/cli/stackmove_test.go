package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func strayInANativeStack(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.openPRs("b")
	shipGTStack(t, f, "a", "b", "stray")
	stubStackPRs(t, f, map[string]*stackPR{"stray": {Number: 42, Title: "stray", State: "OPEN", Base: "other", Stack: 7}})
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	return f
}

func TestStackSubmitNamesTheMoveForAStrayWhoseNativeStackHoldsItsBase(t *testing.T) {
	f := strayInANativeStack(t)
	_, errOut, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if !strings.Contains(errOut, "native GitHub stack #7 holds it") || !strings.Contains(errOut, "ccx vcs stack submit --move stray") {
		t.Errorf("stderr = %q, want the stray refusal to name ccx vcs stack submit --move stray", errOut)
	}
}

func TestStackPlanKeepsAStrayOnlyWhenMoveNamesIt(t *testing.T) {
	f := strayInANativeStack(t)
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		moves []string
		kept  bool
	}{{nil, false}, {[]string{"stray"}, true}} {
		run, err := stackPlan(f.Context(), l, filepath.Join(f.Dir, ".git"), stackRebaseOpts{members: []string{"a", "b", "stray"}, moves: tc.moves, submit: true, dryRun: true})
		if err != nil {
			t.Fatalf("stackPlan(moves=%v): %v", tc.moves, err)
		}
		if kept := run.branch("stray") != nil; kept != tc.kept {
			t.Errorf("stackPlan(moves=%v) kept stray = %t, want %t", tc.moves, kept, tc.kept)
		}
	}
}

func TestStackPlanRefusesAMoveOfABranchOutsideTheRun(t *testing.T) {
	f := strayInANativeStack(t)
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
	f := strayInANativeStack(t)
	_, _, err := runStackCmd(t, f, "submit", "--move", "stray")
	if err == nil || !strings.Contains(err.Error(), "--move moves a pull request out of a native GitHub stack") {
		t.Fatalf("stack submit --move on a Graphite lane = %v, want the native-only refusal", err)
	}
}
