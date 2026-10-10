package cli

import (
	"strings"
	"testing"
)

// TestShipLeavesTheBranchesAboveItThatOtherWorkingCopiesHold is
// oncall-card-actions-build's report: a plain ship from the bottom of a stack
// cut with stack new replayed the branches other lanes' worktrees held.
func TestShipLeavesTheBranchesAboveItThatOtherWorkingCopiesHold(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p")
	c := stackLaneHere(t, f, "c")
	out, _, err := runStackCmdIn(t, f, c, "new", "--full-history", "g")
	if err != nil {
		t.Fatalf("stack new g: %v", err)
	}
	g := out[strings.LastIndex(out, shipSep)+len(shipSep):]
	writeShipFile(t, g, "g.txt", "g\n")
	mustRun(t, f.Env(), g, "git", "add", "g.txt")
	mustRun(t, f.Env(), g, "git", "commit", "-qm", "g")
	api.openPRs("p", "c", "g")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	heads := map[string]string{}
	for _, name := range []string{"c", "g"} {
		heads[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
		if local := gitAt(t, f.Env(), f.Dir, "rev-parse", name); local != heads[name] {
			t.Fatalf("fixture: local %s = %s, want its published head %s", name, local, heads[name])
		}
	}
	writeShipFile(t, f.Dir, "p.txt", "more\n")
	shipResetLog(t, f)

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "p.txt")
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	for _, name := range []string{"c", "g"} {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != heads[name] {
			t.Errorf("origin %s moved from %s to %s", name, heads[name], got)
		}
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != heads[name] {
			t.Errorf("local %s moved from %s to %s", name, heads[name], got)
		}
	}
	for holder, name := range map[string]string{c: "c", g: "g"} {
		if got := gitAt(t, f.Env(), holder, "rev-parse", "HEAD"); got != heads[name] {
			t.Errorf("the working copy holding %s moved to %s", name, got)
		}
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "p"), gitAt(t, f.Env(), f.Dir, "rev-parse", "p"); got != want {
		t.Errorf("origin p = %s, want the shipped head %s", got, want)
	}
	if want := "left c (checked out in " + c + ") where it is, with the branches above it, for that working copy to restack; --all-lanes carries it"; !strings.Contains(out, want) {
		t.Errorf("ship output = %q, want %q", out, want)
	}
	if strings.Contains(out, "resubmitted") {
		t.Errorf("ship output = %q, want no branch above p resubmitted", out)
	}
}
