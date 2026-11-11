package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipGTAdoptsAnUntrackedParentFromGraphitesRecord(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base")
	shipGTUntracked(t, f, "parent")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "parent")
	api.prs["parent"] = 50
	api.recorded["parent"] = "base"
	shipGTUntracked(t, f, "feature")
	shipGTReady(t, f)

	refs := dryRunRefs(t, f)
	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
		t.Errorf("dry run refuses = %q, want none", refusals)
	}
	if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "parent"+shipSep) || !strings.HasSuffix(got[0], ", after ship tracks parent onto base, the base of its open #50") {
		t.Errorf("dry run parent = %q, want parent, after ship tracks it onto base", got)
	}
	if after := dryRunRefs(t, f); after != refs {
		t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
	}

	out, errOut, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	if err != nil {
		t.Fatalf("ship error = %v; stdout = %s; stderr = %s", err, out, errOut)
	}
	if got := thinGTParent(t, f, f.Dir, "parent"); got != "base" {
		t.Errorf("parent's gt parent = %q, want base, the parent Graphite records for #50", got)
	}
	if got := thinGTParent(t, f, f.Dir, "feature"); got != "parent" {
		t.Errorf("feature's gt parent = %q, want parent", got)
	}
	if !strings.Contains(out, "tracked parent onto base, the base of its open #50") {
		t.Errorf("stdout = %q, want it to name the adoption", out)
	}
}

func TestShipGTDryRunPlansTheUntrackedBranchBelowAnUntrackedBranch(t *testing.T) {
	f := shipGTRepo(t)
	shipGTUntracked(t, f, "theirs")
	shipGTUntracked(t, f, "mine")
	shipGTReady(t, f)

	refs := dryRunRefs(t, f)
	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")
	if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
		t.Errorf("dry run refuses = %q, want none", refusals)
	}
	want := "theirs" + shipSep + "untracked, on untracked theirs, which ship tracks bottom-up onto main first"
	if got := dryRunValues(report, "parent"); len(got) != 1 || got[0] != want {
		t.Errorf("dry run parent = %q, want %q", got, want)
	}
	if after := dryRunRefs(t, f); after != refs {
		t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
	}
}

func TestShipGTRefusesAnUntrackedParentGraphiteHasNoRecordOf(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTUntracked(t, f, "parent")
	shipGTUntracked(t, f, "feature")
	shipGTReady(t, f)
	head := shipHead(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	want := "ship: parent is a branch graphite does not track here, and Graphite records no open pull request for it — track it with gt track parent --parent <its parent>, or pass --no-gt"
	if err == nil || err.Error() != want {
		t.Fatalf("ship error = %v, want %q", err, want)
	}
	assertShipRefusedClean(t, f, head)
}

// TestShipGTReparentsOntoAParentAnotherLanePushedWithPlainGit is
// owner-ledger-1: a tracked branch shipped --parent onto a branch another lane
// pushed with plain git and opened #2000 for refused it as one graphite does
// not track, and the lane fell back to a raw push that recorded no stack.
func TestShipGTReparentsOntoAParentAnotherLanePushedWithPlainGit(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTPushedElsewhere(t, f, "parent")
	api.prs["parent"] = 2000
	api.recorded["parent"] = "main"
	shipGTStack(t, f, "feature")

	unfetched := dryRunUnfetchedBase("parent", "feature").Error()
	if refusals := dryRunValues(dryRunReport(t, f, "--no-commit", "--parent", "parent"), "refuses"); len(refusals) != 1 || refusals[0] != unfetched {
		t.Errorf("dry run refuses = %q, want %q", refusals, unfetched)
	}
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "parent:parent")
	report := dryRunReport(t, f, "--no-commit", "--parent", "parent")
	if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
		t.Errorf("fetched dry run refuses = %q, want none", refusals)
	}
	if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "parent"+shipSep+"graphite tracks feature on main, so ship re-parents it") || !strings.HasSuffix(got[0], ", after ship tracks parent onto main, the base of its open #2000") {
		t.Errorf("fetched dry run parent = %q, want feature re-parented onto parent, after ship tracks parent onto main", got)
	}
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "parent")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "-d", "refs/remotes/origin/parent")
	shipGTReady(t, f)

	out, errOut, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	if err != nil {
		t.Fatalf("ship error = %v; stdout = %s; stderr = %s", err, out, errOut)
	}
	if got := thinGTParent(t, f, f.Dir, "parent"); got != "main" {
		t.Errorf("parent's gt parent = %q, want main, the base of #2000", got)
	}
	if got := thinGTParent(t, f, f.Dir, "feature"); got != "parent" {
		t.Errorf("feature's gt parent = %q, want parent", got)
	}
	for _, want := range []string{"tracked parent onto main, the base of its open #2000", "reparented feature from main onto parent"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to carry %q", out, want)
		}
	}
}

func TestShipGTRefusesAParentNeitherThisCheckoutNorOriginHolds(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	shipGTReady(t, f)
	head := shipHead(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "gone")
	want := "ship: --parent gone names a branch neither this checkout nor origin holds, so feature cannot be recorded on it"
	if err == nil || err.Error() != want {
		t.Fatalf("ship error = %v, want %q", err, want)
	}
	assertShipRefusedClean(t, f, head)
}

// shipGTPushedElsewhere cuts name off main with a commit of its own and pushes
// it with plain git, then drops every ref to it here, leaving it as a branch
// another clone pushed looks from this one.
func shipGTPushedElsewhere(t *testing.T, f *vcstest.Fixture, name string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTUntracked(t, f, name)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", name)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-q", "-D", name)
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "-d", "refs/remotes/origin/"+name)
}
