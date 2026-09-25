package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
)

// TestStackSubmitRefusesCommitsTrunkAlreadyHolds pins the submit-time half of
// the width rule: a branch carrying a patch the remote trunk already holds
// proposes work it does not own, and ancestry cannot see it, since the copy has
// a sha of its own. gt is told the branch is restacked — the row a replay
// writes — so nothing moves it and the copy reaches the submit, which is the
// state a bad run leaves behind.
func TestStackSubmitRefusesCommitsTrunkAlreadyHolds(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
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

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded carrying a commit trunk already holds, want a refusal")
	}
	if !strings.Contains(err.Error(), "already holds, so its pull request proposes work the branch does not own") {
		t.Errorf("error = %v, want it to name the inherited commit", err)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "base") {
		t.Error("origin holds base — the refusal must come before the push")
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
