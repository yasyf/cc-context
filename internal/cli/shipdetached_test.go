package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func shipFromHeldBranch(t *testing.T) (f *vcstest.Fixture, holder, tip string) {
	t.Helper()
	f = shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	tip = pushCommit(t, f, "feature.txt", "feature\n", "test: 🧪 feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach")
	holder = filepath.Join(t.TempDir(), "feature")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", holder, "feature")
	return f, holder, tip
}

func TestShipFromADetachedHeadPushesHEADAsTheNamedBranch(t *testing.T) {
	f, holder, tip := shipFromHeldBranch(t)
	writeShipFile(t, f.Dir, "fix.txt", "fix\n")

	out, stderr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--branch", "feature", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	head := shipHead(t, f)
	if want := "pushed feature → origin · " + shortOID(tip) + ".." + shortOID(head); !strings.Contains(out, want) {
		t.Errorf("summary = %q, want it to carry %q", out, want)
	}
	if parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD^"); parent != tip {
		t.Errorf("HEAD^ = %s, want the commit cut at the detached HEAD %s", parent, tip)
	}
	if remote := shipRemoteTip(t, f, "origin", "feature"); remote != head {
		t.Errorf("origin feature = %s, want HEAD %s", remote, head)
	}
	if branch := gitAt(t, f.Env(), f.Dir, "branch", "--show-current"); branch != "" {
		t.Errorf("branch = %q, want HEAD left detached", branch)
	}
	if local := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/heads/feature"); local != tip {
		t.Errorf("local feature = %s, want %s left for the working copy holding it", local, tip)
	}
	if held := gitAt(t, f.Env(), holder, "rev-parse", "HEAD"); held != tip {
		t.Errorf("holder HEAD = %s, want it untouched at %s", held, tip)
	}
	if status := gitAt(t, f.Env(), holder, "status", "--porcelain"); status != "" {
		t.Errorf("holder status = %q, want a clean, untouched checkout", status)
	}
	if want := "holds feature at " + shortOID(tip) + ", now stale against the pushed " + shortOID(head); !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want the stale-holder warning %q", stderr, want)
	}
}

func TestShipAmendFromADetachedHeadForcesUnderALease(t *testing.T) {
	f, _, _ := shipFromHeldBranch(t)
	writeShipFile(t, f.Dir, "fix.txt", "fix\n")
	if _, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--branch", "feature", "--no-watch"); err != nil {
		t.Fatalf("first ship error = %v", err)
	}
	first := shipHead(t, f)
	writeShipFile(t, f.Dir, "fix.txt", "amended\n")

	out, _, err := runShipCmdFull(f.Context(), t, "--amend", "--branch", "feature", "--no-watch")
	if err != nil {
		t.Fatalf("amend ship error = %v", err)
	}
	amended := shipHead(t, f)
	if want := "force-pushed feature → origin · replaced " + shortOID(first) + " with " + shortOID(amended); !strings.Contains(out, want) {
		t.Errorf("summary = %q, want it to carry %q", out, want)
	}
	if remote := shipRemoteTip(t, f, "origin", "feature"); remote != amended {
		t.Errorf("origin feature = %s, want the amended HEAD %s", remote, amended)
	}
}

func TestShipFromADetachedHeadRefusesADivergedRemote(t *testing.T) {
	f, holder, _ := shipFromHeldBranch(t)
	writeShipFile(t, holder, "other.txt", "other\n")
	mustRun(t, f.Env(), holder, "git", "add", "-A")
	mustRun(t, f.Env(), holder, "git", "commit", "-qm", "test: 🧪 other lane")
	mustRun(t, f.Env(), holder, "git", "push", "-q", "origin", "feature")
	other := gitAt(t, f.Env(), holder, "rev-parse", "HEAD")
	writeShipFile(t, f.Dir, "fix.txt", "fix\n")

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--branch", "feature", "--no-watch")
	if err == nil || !strings.Contains(err.Error(), "divergence, not a rewrite") || !strings.Contains(err.Error(), "stays committed at the detached HEAD") {
		t.Fatalf("ship error = %v, want a divergence refusal naming the commit left at HEAD", err)
	}
	if remote := shipRemoteTip(t, f, "origin", "feature"); remote != other {
		t.Errorf("origin feature = %s, want the other lane's %s kept", remote, other)
	}
}

func TestShipFromADetachedHeadRefusesBeforeCommitting(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"a flag it cannot carry", []string{"--branch", "feature", "--draft"}, "ship: --draft does not apply to a ship from a detached HEAD"},
		{"a branch with no local ref", []string{"--branch", "nowhere"}, "ship: --branch nowhere names no local branch"},
		{"trunk", []string{"--branch", "main"}, "ship: --branch main names trunk"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, _, tip := shipFromHeldBranch(t)
			writeShipFile(t, f.Dir, "fix.txt", "fix\n")
			shipResetLog(t, f)

			_, _, err := runShipCmdFull(f.Context(), t, append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)...)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("ship error = %v, want it to start %q", err, tt.want)
			}
			assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
			if head := shipHead(t, f); head != tip {
				t.Errorf("HEAD = %s, want it unmoved at %s", head, tip)
			}
		})
	}
}
