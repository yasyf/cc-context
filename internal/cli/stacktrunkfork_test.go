package cli

import (
	"testing"
)

// TestShipRecordsTheForkPointOfABranchRebasedOntoNewerTrunk is
// deploy-auto-rollback: the branch was rebased onto newer trunk outside gt,
// with local main stale, and ship refused "would replay 4 commits but owns 2 —
// re-record its base with gt track --parent main".
func TestShipRecordsTheForkPointOfABranchRebasedOntoNewerTrunk(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	stackCommit(t, f, "feature.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-p", "main", "--no-interactive")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	stackAdvanceTrunk(t, f, "second.txt", "second\n")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "main", "origin/main~1")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--no-push"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..feature"); n != "2" {
		t.Errorf("feature holds %s commits over trunk, want its own 2", n)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature left trunk")
	}
}

// TestShipReparentsPastARecordedParentTrunkAlreadyHolds is the gt track -f
// case: gt recorded the branch on a branch whose head trunk already holds.
func TestShipReparentsPastARecordedParentTrunkAlreadyHolds(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "spent")
	stackCommit(t, f, "spent.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-p", "main", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	stackCommit(t, f, "feature.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-p", "spent", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "spent:main")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--no-push"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if parent := dropGTParent(t, f, "feature"); parent != "main" {
		t.Errorf("gt parent of feature = %s, want main", parent)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..feature"); n != "2" {
		t.Errorf("feature holds %s commits over trunk, want its own 2", n)
	}
}
