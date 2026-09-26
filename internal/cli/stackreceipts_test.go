package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStackSubmitUnchangedStackCompletesItsRun is target-regroup's "receipts are
// incomplete": a submit whose every pull request already shows its head skipped
// the push, and the receipts with it, and stranded the run.
func TestStackSubmitUnchangedStackCompletesItsRun(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	for i := range 3 {
		if i == 1 {
			api.prs["base"], api.prs["feature"] = 100, 101
		}
		if _, _, err := runStackCmd(t, f, "submit"); err != nil {
			t.Fatalf("stack submit #%d: %v", i+1, err)
		}
		if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
			t.Fatalf("stack submit #%d left run state behind: %v", i+1, left)
		}
	}
}
