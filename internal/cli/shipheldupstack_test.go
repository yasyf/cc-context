package cli

import (
	"slices"
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
	if want := "ship: this commit lands on p, and c, stacked on it, is in " + c + "; cd there if the work is c's\n"; !strings.Contains(errStr, want) {
		t.Errorf("ship stderr = %q, want %q", errStr, want)
	}
}

func TestShipWarnsOnlyWhenACommitLandsUnderAHeldChild(t *testing.T) {
	const warning = "this commit lands on p, and c, stacked on it, is in "
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"a commit on the parent warns", []string{"-m", "fix: frobnicate", "--no-push", "p.txt"}, true},
		{"a commit on a new branch lands elsewhere", []string{"-m", "fix: frobnicate", "--no-push", "--new-branch=n", "p.txt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			f.Decorate(stubGTAPI(t).ctx)
			shipGTStack(t, f, "p")
			stackLaneHere(t, f, "c")
			writeShipFile(t, f.Dir, "p.txt", "more\n")

			_, errStr, err := runShipCmdFull(f.Context(), t, tt.args...)
			if err != nil {
				t.Fatalf("ship = %v (stderr=%q)", err, errStr)
			}
			if got := strings.Contains(errStr, warning); got != tt.want {
				t.Errorf("stderr = %q, want the warning %v", errStr, tt.want)
			}
		})
	}
}

func TestShipDryRunNotesAHeldChildOnlyForACommit(t *testing.T) {
	f := shipGTRepo(t)
	f.Decorate(stubGTAPI(t).ctx)
	shipGTStack(t, f, "p")
	c := stackLaneHere(t, f, "c")
	want := "this commit lands on p, and c, stacked on it, is in " + c + "; cd there if the work is c's"

	if notes := dryRunValues(dryRunReport(t, f, "--no-commit"), "note"); slices.Contains(notes, want) {
		t.Errorf("--no-commit notes = %q, want no commit warning", notes)
	}
	writeShipFile(t, f.Dir, "p.txt", "more\n")
	if notes := dryRunValues(dryRunReport(t, f, "-m", "fix: frobnicate", "p.txt"), "note"); !slices.Contains(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}
