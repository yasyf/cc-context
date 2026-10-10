package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestShipAmendWithAllLanesRestacksAChildCheckedOutElsewhere is
// breakglass-land: amending the parent refused "feat/break-glass-extend is
// checked out in …" because the child sat in a worktree of its own.
func TestShipAmendWithAllLanesRestacksAChildCheckedOutElsewhere(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	writeShipFile(t, f.Dir, "base.txt", "amended\n")

	if _, err := runShipCmd(f.Context(), t, "--amend", "--all-lanes", "--no-push", "base.txt"); err != nil {
		t.Fatalf("ship --amend --all-lanes = %v", err)
	}
	if !stackOnto(t, f, "base", "feature") {
		t.Error("feature does not sit on the amended base")
	}
	if head, want := gitAt(t, f.Env(), held, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); head != want {
		t.Errorf("held HEAD = %s, want the restacked feature %s", head, want)
	}
	if dirt := gitAt(t, f.Env(), held, "status", "--porcelain"); dirt != "" {
		t.Errorf("held worktree left dirty: %s", dirt)
	}
	if body, err := os.ReadFile(filepath.Join(held, "base.txt")); err != nil || string(body) != "amended\n" {
		t.Errorf("held base.txt = %q (%v), want the amended content", body, err)
	}
}

func TestShipAmendWithAllLanesKeepsADirtyChildCheckoutElsewhere(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	writeShipFile(t, held, "scratch.txt", "work in progress\n")
	before := gitAt(t, f.Env(), held, "rev-parse", "HEAD")
	writeShipFile(t, f.Dir, "base.txt", "amended\n")

	if _, err := runShipCmd(f.Context(), t, "--amend", "--all-lanes", "--no-push", "base.txt"); err == nil {
		t.Fatal("ship --amend --all-lanes moved a child under uncommitted work")
	}
	if head := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); head != before {
		t.Errorf("held HEAD moved to %s under uncommitted work", head)
	}
}

func TestShipAmendLeavesAChildCheckedOutElsewhere(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	writeShipFile(t, f.Dir, "base.txt", "amended\n")

	out, err := runShipCmd(f.Context(), t, "--amend", "--no-push", "base.txt")
	if err != nil {
		t.Fatalf("ship --amend = %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved to %s, want it left at %s for the working copy holding it", got, feature)
	}
	if head := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); head != feature {
		t.Errorf("held HEAD = %s, want it untouched at %s", head, feature)
	}
	if !strings.Contains(out, "left feature ("+gtHeldElsewhere) {
		t.Errorf("ship output = %q, want it to name feature as left", out)
	}
}

func TestShipAmendMovesAnEmptyChildOntoTheAmendedParent(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	shipGTLevel(t, f, "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	writeShipFile(t, f.Dir, "base.txt", "amended\n")

	if _, err := runShipCmd(f.Context(), t, "--amend", "--no-push", "base.txt"); err != nil {
		t.Fatalf("ship --amend = %v", err)
	}
	if base, feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); feature != base {
		t.Errorf("feature = %s, want it level with the amended base %s", feature, base)
	}
}
