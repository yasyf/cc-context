package cli

import (
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
)

// TestStackSubmitIncludeTakesBranchesOtherWorktreesHold is the 2026-10-13
// build-tools stack: --include named two branches other working copies held,
// whose local heads origin had since rewritten, and the run kept both at their
// published heads and refused, asking for the very --include flags it was given.
func TestStackSubmitIncludeTakesBranchesOtherWorktreesHold(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid", "top")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	for _, name := range []string{"mid", "top"} {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", "refs/remotes/origin/"+name)
		writeShipFile(t, f.Dir, name+".txt", name+" rewritten\n")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-am", name+" rewritten")
		mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "HEAD:refs/heads/"+name)
		mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "+refs/heads/"+name+":refs/remotes/origin/"+name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane-mid"), "mid")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane-top"), "top")
	writeShipFile(t, f.Dir, "base.txt", "amended\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-am", "base")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--include", "mid", "--include", "top"); err != nil {
		t.Fatalf("stack submit --include mid --include top: %v", err)
	}
	for _, pair := range [][2]string{{"base", "mid"}, {"mid", "top"}} {
		parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[0])
		child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[1])
		if !stackOnto(t, f, parent, child) {
			t.Errorf("published %s %s is not stacked on published %s %s", pair[1], child, pair[0], parent)
		}
	}
	for _, name := range []string{"mid", "top"} {
		if got := gitAt(t, f.Env(), f.RemoteDir, "show", name+":"+name+".txt"); got != name+" rewritten" {
			t.Errorf("published %s.txt = %q, want origin's rewrite kept", name, got)
		}
	}
}

// TestStackSubmitToDropsALandedParentAnotherWorktreeHolds is the same stack's
// second refusal: its bottom landed while another working copy still held it,
// and --to refused because the next branch was last published onto it, naming
// the landed branch as one above the stop.
func TestStackSubmitToDropsALandedParentAnotherWorktreeHolds(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "own", "low", "high")
	api.prs["own"], api.prs["low"], api.prs["high"] = 41, 42, 43
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stubStackPRs(t, f, map[string]*stackPR{"own": {Number: 41, Title: "own", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "own")}})
	restackSquashRemote(t, f, "main", "own (#41)", "own")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "low")
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := gtmeta.Reparent(f.Context(), commonDir, map[string]string{"low": "main"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane-own"), "own")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--to", "high"); err != nil {
		t.Fatalf("stack submit --to high: %v", err)
	}
	trunk := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main")
	for _, pair := range [][2]string{{trunk, "low"}, {gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "low"), "high"}} {
		child := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", pair[1])
		if !stackOnto(t, f, pair[0], child) {
			t.Errorf("published %s %s is not stacked on %s", pair[1], child, pair[0])
		}
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", trunk+".."+gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "high")); n != "2" {
		t.Errorf("published high holds %s commits over trunk, want low's and its own", n)
	}
}
