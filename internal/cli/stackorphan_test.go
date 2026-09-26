package cli

import (
	"strings"
	"testing"
)

// TestStackRebaseTakesGTsParentOverAPublishedOneOutsideTheRun is
// ci-legacy-orphans' panic: c was published onto b, b's pull request closed
// without landing and b left gt, and c was re-recorded onto a.
func TestStackRebaseTakesGTsParentOverAPublishedOneOutsideTheRun(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "gt", "untrack", "b", "--force", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	if parent := dropGTParent(t, f, "c"); parent != "a" {
		t.Fatalf("gt parent of c = %s, want a", parent)
	}

	out, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("stack rebase --dry-run: %v", err)
	}
	if !strings.Contains(out, "c · onto a") {
		t.Errorf("plan = %q, want c onto a, where gt now records it", out)
	}
}

func TestStackRebaseStopsAtAConflictBelowAPublishedParentOutsideTheRun(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "gt", "untrack", "b", "--force", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	stackAdvanceTrunk(t, f, "c.txt", "trunk\n")

	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil {
		t.Fatal("stack rebase replayed c over trunk's c.txt without a conflict")
	}
	if ws := stackWorkspaceOf(t, err); !strings.Contains(ws, "conflict-c") {
		t.Errorf("conflict workspace = %q, want c's", ws)
	}
}
