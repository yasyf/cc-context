package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestShipReparentsOntoTrunkPastADeletedLandedParent is sand-artifacts-speed:
// deleting a parent branch after it landed left the next branch down with no
// parent in gt state, and a ship above it refused "gt state has no parent for …".
func TestShipReparentsOntoTrunkPastADeletedLandedParent(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("a", "b", "c"))
	stubGTAPI(t)
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "a")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if parent := dropGTParent(t, f, "b"); parent != "main" {
		t.Errorf("gt parent of b = %s, want main", parent)
	}
	if n := gitAt(t, f.Env(), f.RemoteDir, "rev-list", "--count", "main..b"); n != "1" {
		t.Errorf("origin b holds %s commits over trunk, want its own 1", n)
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")) {
		t.Error("origin b does not sit on trunk")
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")) {
		t.Error("origin c does not sit on b")
	}
}

func TestShipLeavesAnOrphanWhoseDeletedParentNeverLanded(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("a", "b", "c"))
	stubGTAPI(t)
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "a")
	shipGTReady(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil || !strings.Contains(err.Error(), "gt state has no parent for b") {
		t.Fatalf("ship over an unlanded deleted parent = %v, want the orphan refused", err)
	}
	if parent := pruneParentOf(t, filepath.Join(f.Dir, ".git"), "b"); parent != "a" {
		t.Errorf("gt parent of b = %s, want a left as it was", parent)
	}
}
