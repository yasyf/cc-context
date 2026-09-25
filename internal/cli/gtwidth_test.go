package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackSubmitDropsCommitsTrunkAlreadyHolds(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	writeShipFile(t, f.Dir, "own.txt", "the branch's own work\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "own.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "own")
	restackAdvanceRemote(t, f, "main", "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--ff-only", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"base": gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/main")}); err != nil {
		t.Fatalf("record base as restacked: %v", err)
	}
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", "origin/main...base"); got != "own.txt" {
		t.Fatalf("submitted inherited files: %s", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..base"); got != "1" {
		t.Fatalf("submitted %s commits, want only own work", got)
	}
	if local, remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); local != remote {
		t.Fatal("repaired branch was not pushed")
	}
}

// TestShipGTRefusesCommitsTrunkAlreadyHolds pins that the width refusal covers
// the ship lane too, and that it lands before the push: the submit is the last
// point where a pull request proposing work the branch does not own can still
// be stopped.
func TestShipGTRefusesCommitsTrunkAlreadyHolds(t *testing.T) {
	log := setupShipGT(t, true)
	t.Setenv("GIT_BRANCH", "feature")
	setGTState(t, `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
	t.Setenv("GIT_CHERRY", "- 1234567890ab\n+ abcdef012345\n")

	_, err := runShipCmd(context.Background(), t, "-m", "fix: frobnicate", "--no-watch", "--no-pr")
	if err == nil {
		t.Fatal("ship succeeded carrying a commit trunk already holds, want a refusal")
	}
	if !strings.Contains(err.Error(), "feature carries 1 commit(s) refs/remotes/origin/main already holds") {
		t.Errorf("error = %v, want it to name the inherited commit", err)
	}
	for _, inv := range readInvocations(t, log) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "push" {
			t.Errorf("ship pushed %v — the refusal must come first", inv)
		}
	}
}
