package cli

import (
	"strings"
	"testing"
)

// TestStackNewRemovesALaneGTWillNotAdopt is target-regroup's refusal: a gt
// track that refused the new lane left its worktree behind, so the retry
// refused on the existing destination.
func TestStackNewRemovesALaneGTWillNotAdopt(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "loose")
	shipGTIntercept(t, f, "track", "echo 'ERROR: refused' >&2\nexit 1\n")

	for range 2 {
		_, _, err := runStackCmd(t, f, "new", "--full-history", "child", "--parent", "loose")
		if err == nil || !strings.Contains(err.Error(), "since gt could not adopt it onto loose") {
			t.Fatalf("stack new onto loose = %v, want the lane removed and the parent named", err)
		}
		if gitBranchExists(t, f.Env(), f.Dir, "child") {
			t.Fatal("child branch left behind")
		}
		if list := gitAt(t, f.Env(), f.Dir, "worktree", "list"); strings.Contains(list, "child") {
			t.Fatalf("child worktree left behind: %s", list)
		}
	}
}
