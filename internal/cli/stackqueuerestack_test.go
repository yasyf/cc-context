package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// stackQueueRestack replays branch's one commit onto origin/main the way the
// merge queue does when it resolves a conflict: the patch changes, and the
// commit keeps its author, author date and message.
func stackQueueRestack(t *testing.T, f *vcstest.Fixture, branch, file string) string {
	t.Helper()
	server := filepath.Join(t.TempDir(), "queue")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", server, "origin/main")
	writeShipFile(t, server, file, "resolved by the queue\n")
	mustRun(t, f.Env(), server, "git", "add", file)
	mustRun(t, f.Env(), server, "git", "commit", "-q", "-C", branch)
	mustRun(t, f.Env(), server, "git", "push", "-qf", "origin", "HEAD:"+branch)
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	return gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/"+branch)
}

// TestStackRebaseTakesTheQueuesRestackOfItsLastSubmission is #30411's refusal:
// the merge queue restacked the parent's last submission onto a newer trunk
// and resolved a conflict, so the patch differs while the commit keeps its
// author, date and message. Local still holds exactly what was submitted, so
// the rebase builds on the queue's restack instead of refusing.
func TestStackRebaseTakesTheQueuesRestackOfItsLastSubmission(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	stackRecordSubmitted(t, f, "base", "feature")
	stackAdvanceTrunk(t, f, "b1.txt", "trunk\n")
	restacked := stackQueueRestack(t, f, "base", "b1.txt")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase over the queue's restack: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != restacked {
		t.Errorf("local base = %s, want the queue's restack %s", got, restacked)
	}
	if !stackOnto(t, f, restacked, "feature") {
		t.Error("feature is not on the queue's restack of base")
	}
}

func TestStackRebaseRefusesTheQueuesRestackOverALocalAmend(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	stackRecordSubmitted(t, f, "base")
	stackAdvanceTrunk(t, f, "b1.txt", "trunk\n")
	stackQueueRestack(t, f, "base", "b1.txt")
	writeShipFile(t, f.Dir, "b1.txt", "amended locally\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "base has diverged from origin/base") {
		t.Fatalf("err = %v, want the divergence refused over the local amend", err)
	}
}

// TestStackSubmitTakesTheQueuesRestackOfItsPublication is the same restack
// reached through submit, where the publication receipt holds the head the
// queue replaced.
func TestStackSubmitTakesTheQueuesRestackOfItsPublication(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, f, nil)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	stackAdvanceTrunk(t, f, "b1.txt", "trunk\n")
	restacked := stackQueueRestack(t, f, "base", "b1.txt")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit over the queue's restack: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != restacked {
		t.Errorf("origin base = %s, want the queue's restack %s left in place", got, restacked)
	}
	if !stackOnto(t, f, restacked, "origin/feature") {
		t.Error("origin feature is not on the queue's restack of base")
	}
}
