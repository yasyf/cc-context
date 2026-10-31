package cli

import (
	"testing"
)

// TestStackRebaseDropsALandedGrandparentUnderAPublishedStack is
// release-slack-polish's refusal: p was published twice onto moving trunk, x and
// y on it, p landed as a squash, and a rebase from y refused x as replaying
// commits trunk already held.
func TestStackRebaseDropsALandedGrandparentUnderAPublishedStack(t *testing.T) {
	f := stackRebaseRepo(t, "p", "x", "y")
	stackAdvanceTrunk(t, f, "one.txt", "one\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "two.txt", "two\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stubOpenPRs(t, f, map[string]*stackPR{"p": {Number: 41, Title: "p", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p")}}, "x", "y")
	restackSquashRemote(t, f, "main", "p (#41)", "p")
	stackAdvanceTrunk(t, f, "three.txt", "three\n")

	if _, _, err := runStackCmd(t, f, "rebase"); err != nil {
		t.Fatalf("stack rebase after p landed: %v", err)
	}
	for branch, own := range map[string]string{"x": "1", "y": "2"} {
		if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main.."+branch); n != own {
			t.Errorf("origin %s holds %s commits over main, want %s", branch, n, own)
		}
	}
}

// TestStackSubmitReplaysOnlyAChildsOwnCommitsPastTwoSquashLandedParents is a
// lane's replay after a and b squash-landed under c and their branches went:
// c held a commit past its publication, and submit replayed it from trunk's old
// merge base, re-applying a's and b's commits over their squashes.
func TestStackSubmitReplaysOnlyAChildsOwnCommitsPastTwoSquashLandedParents(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	stackAdvanceTrunk(t, f, "one.txt", "one\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	writeShipFile(t, f.Dir, "c2.txt", "c2\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c2.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "c2")
	stubOpenPRs(t, f, map[string]*stackPR{
		"a": {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a")},
		"b": {Number: 42, Title: "b", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")},
	}, "c")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	restackSquashRemote(t, f, "main", "b (#42)", "b")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "--prune", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "a", "b")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit after a and b squash-landed: %v", err)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..c"); n != "2" {
		t.Errorf("origin c holds %s commits over main, want its own 2", n)
	}
}
