package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const shipGTLockOnce = "#!/bin/sh\nif [ ! -e \"$GIT_DIR/once\" ]; then touch \"$GIT_DIR/once\" \"$GIT_DIR/refs/heads/feature.lock\"; else rm -f \"$GIT_DIR/refs/heads/feature.lock\"; fi\n"

func TestShipGTRetriesAPushTheRemoteFailedToApply(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	writeShipGH(t, f)
	shipGTStack(t, f, "feature")
	writeShipExecutable(t, filepath.Join(f.RemoteDir, "hooks"), "pre-receive", shipGTLockOnce)
	shipGTReady(t, f)

	out, errOut, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v; stdout = %s; stderr = %s", err, out, errOut)
	}
	if remote, head := gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "rev-parse", "feature"), shipHead(t, f); remote != head {
		t.Errorf("remote feature = %s, want the shipped head %s", remote, head)
	}
}

func TestShipGTNamesTheRefTheRemoteKeepsFailing(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	writeShipGH(t, f)
	shipGTStack(t, f, "feature")
	writeShipExecutable(t, filepath.Join(f.RemoteDir, "hooks"), "pre-receive", "#!/bin/sh\ntouch \"$GIT_DIR/refs/heads/feature.lock\"\n")
	shipGTReady(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil {
		t.Fatal("ship pushed past a remote that cannot lock the ref")
	}
	want := "the atomic push of feature moved nothing: remote: error: cannot lock ref 'refs/heads/feature'"
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "the remote rejected feature (atomic transaction failed)") {
		t.Errorf("err = %q, want %q and the rejected ref with its reason", err, want)
	}
	pushes := 0
	for _, argv := range shipGTInvocations(t, f) {
		if len(argv) > 1 && argv[0] == "git" && slices.Contains(argv, "--atomic") {
			pushes++
		}
	}
	if pushes != 2 {
		t.Errorf("atomic pushes = %d, want the push and one retry", pushes)
	}
}

func TestGitPushVerdictNamesTheRejectedRefWithoutTransferProgress(t *testing.T) {
	stderr := "remote: Resolving deltas:   0% (0/6)\rremote: Resolving deltas:  50% (3/6)\rremote: Resolving deltas: 100% (6/6), completed with 6 local objects.\nTo https://github.com/Forge-AI/monorepo\n ! [remote rejected]         3cb8e756578ceb54d28b6591ce36084b6084ac83 -> tpr-api-every-cluster (failed)\nerror: failed to push some refs to 'https://github.com/Forge-AI/monorepo'\n"
	if got, want := gitPushVerdict(errors.New(stderr)), "the remote rejected tpr-api-every-cluster (failed)"; got != want {
		t.Errorf("gitPushVerdict = %q, want %q", got, want)
	}
}

// TestShipGTRefusesABranchAtItsParentsHead is the graphite lane's side of
// Forge-AI/monorepo#50896: a stacked branch reset onto its parent's head gives
// its open pull request no commits over that parent, and GitHub closes it.
func TestShipGTRefusesABranchAtItsParentsHead(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	api.openPRs("base", "feature")
	f.Decorate(api.ctx)
	writeShipGH(t, f)
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	published := shipHead(t, f)
	parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "base")
	shipResetLog(t, f)

	_, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch")
	if want := "ship: feature sits at base's head " + shortOID(parent) + ", so pull request #9001"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ship = %v, want %q", err, want)
	}
	if n := pushCount(t, f); n != 0 {
		t.Errorf("ran %d pushes, want none", n)
	}
	if remote := shipRemoteTip(t, f, "origin", "feature"); remote != published {
		t.Errorf("origin feature = %s, want the pull request's %s", remote, published)
	}
}
