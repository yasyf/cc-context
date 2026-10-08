package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

func TestShipAmendOfAMidStackBranchCarriesItsChildren(t *testing.T) {
	for _, elsewhere := range []bool{false, true} {
		name := "child_here"
		if elsewhere {
			name = "child_elsewhere"
		}
		t.Run(name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "p", "c", "g")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
			if elsewhere {
				mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", f.WorktreePath("held"), "c")
			}
			writeShipFile(t, f.Dir, "p.txt", "amended\n")
			shipResetLog(t, f)

			out, _, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "p.txt")
			if err != nil {
				t.Fatalf("ship --amend = %v", err)
			}
			if !strings.Contains(out, "resubmitted c, g above p") {
				t.Errorf("ship output = %q, want it to name the resubmitted children", out)
			}
			for _, pair := range [][2]string{{"p", "c"}, {"c", "g"}} {
				if !stackOnto(t, f, pair[0], pair[1]) {
					t.Errorf("local %s does not sit on the amended %s", pair[1], pair[0])
				}
				parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[0])
				child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[1])
				if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", child+".."+parent); behind != "0" {
					t.Errorf("published %s holds %s commit(s) published %s does not", pair[0], behind, pair[1])
				}
			}
		})
	}
}

// TestShipLeavesTheDraftStateOfResubmittedChildrenAlone pins that a ship
// carrying its children converts only its own pull request: an explicit draft
// false on a child's update published a held draft above it.
func TestShipLeavesTheDraftStateOfResubmittedChildrenAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.mu.Lock()
	api.prs["p"], api.prs["c"] = 100, 101
	api.mu.Unlock()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	shipResetLog(t, f)

	out, _, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "p.txt")
	if err != nil {
		t.Fatalf("ship --amend = %v", err)
	}
	if !strings.Contains(out, "resubmitted c above p") {
		t.Fatalf("ship output = %q, want it to name the resubmitted child", out)
	}
	api.mu.Lock()
	tip, _ := api.lastEntry("p")
	child, _ := api.lastEntry("c")
	api.mu.Unlock()
	if tip.Action != gtapi.SubmitUpdate || tip.Draft == nil || *tip.Draft {
		t.Errorf("p submitted as %s with draft %v, want an update publishing it", tip.Action, tip.Draft)
	}
	if child.Action != gtapi.SubmitUpdate || child.Draft != nil {
		t.Errorf("c submitted as %s with draft %v, want an update omitting draft", child.Action, child.Draft)
	}
}
