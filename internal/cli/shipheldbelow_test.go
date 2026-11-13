package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func shipGTHeldUntracked(t *testing.T) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t, vcstest.GTStack("base"))
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "trunk2.txt", "trunk2\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "trunk2.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "trunk2")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	shipGTUntracked(t, f, "c")
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	shipGTReady(t, f)
	return f, held
}

func TestShipDryRunLeavesAParentAnotherWorktreeHolds(t *testing.T) {
	f, held := shipGTHeldUntracked(t)

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-pr")

	if moved := dryRunBranches(report, "restack"); slices.Contains(moved, "base") {
		t.Errorf("restack = %v, want base, which %s holds, left at its published head\n%s", moved, held, report)
	}
	if pushed := dryRunBranches(report, "pr new"); slices.Contains(pushed, "base") {
		t.Errorf("pr new = %v, want base, which %s holds, not pushed\n%s", pushed, held, report)
	}
	if notes := strings.Join(dryRunValues(report, "note"), "\n"); !strings.Contains(notes, " · kept at its published head; --include base moves it") {
		t.Errorf("notes = %q, want base named as kept for the working copy holding it", notes)
	}
}

func TestShipKeepsAParentAnotherWorktreeHoldsAtItsPublishedHead(t *testing.T) {
	f, held := shipGTHeldUntracked(t)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-pr", "--no-watch")
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"c"}) {
		t.Errorf("pushed %v, want c alone", refs)
	}
	if !strings.Contains(out+errStr, " · kept at its published head; --include base moves it") {
		t.Errorf("output = %q, stderr = %q, want base named as kept for the working copy holding it", out, errStr)
	}
	for where, dir := range map[string]string{"local": f.Dir, "held": held, "origin": f.RemoteDir} {
		if got := gitAt(t, f.Env(), dir, "rev-parse", "base"); got != base {
			t.Errorf("%s base = %.12s, want it left at %.12s", where, got, base)
		}
	}
	if !stackOnto(t, f, "base", "c") {
		t.Error("c does not sit on base")
	}
}
