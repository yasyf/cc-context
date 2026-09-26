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
	stubOpenPRs(t, map[string]*stackPR{"p": {Number: 41, Title: "p", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p")}}, "x", "y")
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
