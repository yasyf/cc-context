package cli

import (
	"testing"
)

func TestStackSubmitCarriesTheBranchesAboveItThatOtherLanesHold(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p", "x", "m", "y", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	holders := map[string]string{"x": restackSiblingPath(t, "lane-x"), "y": restackSiblingPath(t, "lane-y")}
	for name, holder := range holders {
		mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", holder, name)
	}
	writeShipFile(t, f.Dir, "p.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "p.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	for _, pair := range [][2]string{{"p", "x"}, {"x", "m"}, {"m", "y"}, {"y", "c"}} {
		parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[0])
		child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[1])
		if !stackOnto(t, f, parent, child) {
			t.Errorf("published %s %s is not stacked on published %s %s", pair[1], child, pair[0], parent)
		}
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p"), gitAt(t, f.Env(), f.Dir, "rev-parse", "p"); got != want {
		t.Errorf("origin p = %s, want the amended %s", got, want)
	}
	for name, holder := range holders {
		if got, want := gitAt(t, f.Env(), holder, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != want {
			t.Errorf("the working copy holding %s is at %s, want the carried %s", name, got, want)
		}
	}
}
