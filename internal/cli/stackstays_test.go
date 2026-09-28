package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackPublishedBehindTrunk(t *testing.T, file string) (*vcstest.Fixture, map[string]string) {
	t.Helper()
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := map[string]string{}
	for _, branch := range []string{"base", "feature"} {
		published[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	}
	stackAdvanceTrunk(t, f, file, "upstream\n")
	shipResetLog(t, f)
	return f, published
}

func TestStackSubmitLeavesACleanPublishedBranchOnItsTrunk(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantPlan  string
		ontoTrunk bool
	}{
		{"stays", nil, "base" + shipSep + "stays on ", false},
		{"onto trunk", []string{"--onto-trunk"}, "base" + shipSep + "onto main" + shipSep + "from ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, published := stackPublishedBehindTrunk(t, "upstream.txt")
			pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")

			out, errStr, err := runStackCmd(t, f, append([]string{"submit"}, tc.args...)...)
			if err != nil {
				t.Fatalf("stack submit %v = %v (stderr=%q)", tc.args, err, errStr)
			}
			if !strings.Contains(out, tc.wantPlan) {
				t.Errorf("plan = %q, want %q", out, tc.wantPlan)
			}
			if clean := "merges cleanly onto main@" + pin[:12]; strings.Contains(out, clean) == tc.ontoTrunk {
				t.Errorf("plan = %q, want %q named only when base stays", out, clean)
			}
			base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if moved := base != published["base"]; moved != tc.ontoTrunk {
				t.Errorf("origin base moved = %t (%.12s → %.12s), want %t", moved, published["base"], base, tc.ontoTrunk)
			}
			if moved := feature != published["feature"]; moved != tc.ontoTrunk {
				t.Errorf("origin feature moved = %t (%.12s → %.12s), want %t", moved, published["feature"], feature, tc.ontoTrunk)
			}
			if onTrunk := stackOnto(t, f, pin, base); onTrunk != tc.ontoTrunk {
				t.Errorf("origin base on the fetched trunk = %t, want %t", onTrunk, tc.ontoTrunk)
			}
			if !stackOnto(t, f, base, feature) {
				t.Error("origin feature does not sit on origin base")
			}
		})
	}
}

func TestStackSubmitPushesNewWorkAboveABranchLeftOnItsTrunk(t *testing.T) {
	f, published := stackPublishedBehindTrunk(t, "upstream.txt")
	stackCommit(t, f, "more.txt")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published["base"] {
		t.Errorf("origin base moved to %.12s, want it left at %.12s", got, published["base"])
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != local {
		t.Errorf("origin feature = %.12s, want the local head %.12s pushed as is", got, local)
	}
}

func TestStackSubmitRebasesAPublishedBranchThatConflictsWithTrunk(t *testing.T) {
	f, published := stackPublishedBehindTrunk(t, "base.txt")

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit onto a conflicting trunk succeeded, want a conflict stop")
	}
	if want := "base" + shipSep + "onto main" + shipSep + "from "; !strings.Contains(out, want) {
		t.Errorf("plan = %q, want %q", out, want)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Conflict == nil || run.Conflict.Branch != "base" {
		t.Errorf("run conflict = %+v, want a stop on base", run.Conflict)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published["base"] {
		t.Errorf("origin base moved to %.12s before the conflict was resolved", got)
	}
}

func TestStackSubmitMovesTheChildOfALandedParentOntoTrunk(t *testing.T) {
	f, _ := stackPublishedBehindTrunk(t, "upstream.txt")
	restackSquashRemote(t, f, "main", "base (#41)", "base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")

	out, errStr, err := runStackCmd(t, f, "submit", "--landed", "base")
	if err != nil {
		t.Fatalf("stack submit --landed base = %v (stderr=%q)", err, errStr)
	}
	if want := "feature" + shipSep + "onto main (was base)"; !strings.Contains(out, want) {
		t.Errorf("plan = %q, want %q", out, want)
	}
	if !stackOnto(t, f, "origin/main", gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")) {
		t.Error("origin feature is not on the trunk its landed parent left it on")
	}
}

func TestShipLeavesACleanPublishedTipOnItsTrunk(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		ontoTrunk bool
	}{
		{"stays", nil, false},
		{"onto trunk", []string{"--onto-trunk"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			stubGTAPI(t)
			shipGTStack(t, f, "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
			shipGTReady(t, f)

			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tc.args...)
			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship %v = %v (stderr=%q)", tc.args, err, errStr)
			}
			published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if got := gitAt(t, f.Env(), f.Dir, "log", "-1", "--format=%s", published); got != "fix: frobnicate" {
				t.Errorf("origin feature tip = %q, want the shipped commit", got)
			}
			if onTrunk := stackOnto(t, f, "origin/main", published); onTrunk != tc.ontoTrunk {
				t.Errorf("origin feature on the fetched trunk = %t, want %t", onTrunk, tc.ontoTrunk)
			}
		})
	}
}
