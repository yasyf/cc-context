package cli

import (
	"strings"
	"testing"
)

// TestStackNewRemovesALaneGTWillNotAdopt is target-regroup's refusal: a parent
// that dropped out of Graphite tracking left the new worktree behind, so the
// retry refused on the existing destination.
func TestStackNewRemovesALaneGTWillNotAdopt(t *testing.T) {
	f := shipGTRepo(t)
	shipGTUntracked(t, f, "loose")

	for range 2 {
		_, _, err := runStackCmd(t, f, "new", "child", "--parent", "loose")
		if err == nil || !strings.Contains(err.Error(), "track loose first with gt track --parent <its parent> loose") {
			t.Fatalf("stack new onto untracked loose = %v, want the lane removed and the parent named", err)
		}
		if gitBranchExists(t, f.Env(), f.Dir, "child") {
			t.Fatal("child branch left behind")
		}
		if list := gitAt(t, f.Env(), f.Dir, "worktree", "list"); strings.Contains(list, "child") {
			t.Fatalf("child worktree left behind: %s", list)
		}
	}
}
