package cli

import "testing"

// TestStackSubmitAdoptsTheLanesOwnRawPushes is ci-go's #25742 on v0.65.22: the
// lane raw-rebased its published branch onto trunk and pushed it, then
// restacked it again locally, and each run refused the remote as someone
// else's push.
func TestStackSubmitAdoptsTheLanesOwnRawPushes(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "feature")
	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run"); err != nil {
		t.Fatalf("stack rebase --dry-run after the lane's own raw push: %v", err)
	}
	stackAdvanceTrunk(t, f, "latest.txt", "latest\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit over the lane's own earlier raw push: %v", err)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..feature"); n != "1" {
		t.Errorf("origin feature holds %s commits over main, want its own 1", n)
	}
}
