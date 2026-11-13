package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestShipCarriesTheBranchesAboveItThatOtherWorkingCopiesHold is
// oncall-card-actions-build's stack the other way round: leaving the branches
// other lanes' worktrees held kept their pull requests on the parent's old
// commit, and a server-side rebase after the parent landed replayed that commit
// over a fix pushed above it.
func TestShipCarriesTheBranchesAboveItThatOtherWorkingCopiesHold(t *testing.T) {
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
	writeShipFile(t, f.Dir, "p.txt", "more\n")
	shipResetLog(t, f)

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "p.txt")
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "resubmitted c, g above p") {
		t.Errorf("ship output = %q, want c and g resubmitted above p", out)
	}
	for holder, pair := range map[string][2]string{c: {"p", "c"}, g: {"c", "g"}} {
		parent, name := pair[0], pair[1]
		if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name), gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != want {
			t.Errorf("origin %s = %s, want the carried %s", name, got, want)
		}
		if !stackOnto(t, f, parent, name) {
			t.Errorf("local %s does not sit on the new %s", name, parent)
		}
		if got, want := gitAt(t, f.Env(), holder, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != want {
			t.Errorf("the working copy holding %s is at %s, want the carried %s", name, got, want)
		}
	}
	if want := "ship: this commit lands on p, and c, stacked on it, is in " + c + "; cd there if the work is c's\n"; !strings.Contains(errStr, want) {
		t.Errorf("ship stderr = %q, want %q", errStr, want)
	}
}

func TestShipRefusesToMoveABranchAboveItOutFromUnderUncommittedWork(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p")
	c := stackLaneHere(t, f, "c")
	api.openPRs("p", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	heads := map[string]string{}
	for _, name := range []string{"p", "c"} {
		heads[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
	}
	writeShipFile(t, c, "scratch.txt", "work in progress\n")
	writeShipFile(t, f.Dir, "p.txt", "more\n")
	want := "ship: c, stacked on p, is checked out in " + c + " with uncommitted work, and this commit would move it"

	if refusals := dryRunValues(dryRunReport(t, f, "-m", "fix: frobnicate", "p.txt"), "refuses"); !slices.ContainsFunc(refusals, func(r string) bool { return strings.HasPrefix(r, want) }) {
		t.Errorf("dry-run refusals = %q, want %q", refusals, want)
	}
	shipResetLog(t, f)
	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "p.txt")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ship = %v, want %q", err, want)
	}
	for _, name := range []string{"p", "c"} {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", name); got != heads[name] {
			t.Errorf("local %s moved from %s to %s", name, heads[name], got)
		}
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != heads[name] {
			t.Errorf("origin %s moved from %s to %s", name, heads[name], got)
		}
	}
	if dirt := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); !strings.Contains(dirt, "p.txt") {
		t.Errorf("status = %q, want p.txt still uncommitted", dirt)
	}
	if body, err := os.ReadFile(filepath.Join(c, "scratch.txt")); err != nil || string(body) != "work in progress\n" {
		t.Errorf("c's scratch.txt = %q (%v), want the uncommitted work kept", body, err)
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
