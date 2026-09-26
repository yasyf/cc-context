package cli

import "testing"

// TestVcsPushLeavesTheNextShipAPublicationItAgreesWith is release-slack-backlog's
// #25890: a ccx vcs push of a rewrite rewrote the publication receipt but not
// Graphite's last submitted version, and the next ship refused the two as
// changed.
func TestVcsPushLeavesTheNextShipAPublicationItAgreesWith(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/feature")
	writeShipFile(t, f.Dir, "feature.txt", "rewritten\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qa", "--amend", "-m", "feature")
	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push: %v", err)
	}
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship after vcs push = %v (stderr=%q)", err, errStr)
	}
}
