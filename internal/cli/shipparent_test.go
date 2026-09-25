package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// shipParentRewritten cuts p off trunk and c off p, each with a commit of its
// own, then rewrites p under a new sha carrying the same patch — the parent
// restacked after its child was cut, which leaves p out of c's history. onto
// names the parent gt records for c, and empty leaves c untracked.
func shipParentRewritten(t *testing.T, f *vcstest.Fixture, onto string) {
	t.Helper()
	shipGTStack(t, f, "p")
	shipGTUntracked(t, f, "c")
	if onto != "" {
		mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", onto, "--no-interactive")
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit", "--date=2001-01-01T00:00:00")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	shipResetLog(t, f)
}

// shipParentOf reads the parent gt records for branch.
func shipParentOf(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	s, tracked := state[branch]
	if !tracked {
		t.Fatalf("gt does not track %s", branch)
	}
	return s.Parents[0].Ref
}

// assertOnlyOwnCommit holds c to its own commit and the one ship made above p:
// the old copy of p's commit it was cut on must not ride along.
func assertOnlyOwnCommit(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "c..p"); behind != "0" {
		t.Errorf("p holds %s commit(s) c does not, want c replayed onto p's head", behind)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "p..c"); subjects != "fix: frobnicate\nc" {
		t.Errorf("c carries %q above p, want its own commits alone", subjects)
	}
}

// TestShipGTParentReparentsATrackedBranch is #24579's shape: --parent on a
// branch gt already tracked onto trunk used to be ignored without a word, so the
// submit proposed the parent's commits as the branch's own. The branch now
// moves onto the parent, carrying only its own commit.
func TestShipGTParentReparentsATrackedBranch(t *testing.T) {
	f := shipGTRepo(t)
	shipParentRewritten(t, f, "main")

	shipGTReady(t, f)
	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := "reparented c from main onto p (replayed its 1 own commit(s))"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "c"); parent != "p" {
		t.Errorf("c's parent = %s, want p", parent)
	}
	assertOnlyOwnCommit(t, f)
}

// TestShipGTParentTracksAcrossARewrittenParent is the untracked half: gt track
// --parent refuses a parent no longer in the branch's history, and the advice
// that followed steered to a bare gt track, which adopts onto trunk.
func TestShipGTParentTracksAcrossARewrittenParent(t *testing.T) {
	f := shipGTRepo(t)
	shipParentRewritten(t, f, "")

	shipGTReady(t, f)
	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := "tracked c onto p (replayed its 1 own commit(s))"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "c"); parent != "p" {
		t.Errorf("c's parent = %s, want p", parent)
	}
	assertOnlyOwnCommit(t, f)
}

// shipParentInterleaved leaves c carrying a copy of a commit p gained after c
// was cut, above c's own work: no single replay range takes c's commit and
// leaves that copy behind.
func shipParentInterleaved(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	shipParentRewritten(t, f, "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "q.txt", "q\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "q.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "q")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", "p")
	shipResetLog(t, f)
}

func TestShipGTParentRefusesInterleavedCopies(t *testing.T) {
	f := shipGTRepo(t)
	shipParentInterleaved(t, f)
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")

	shipGTReady(t, f)
	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p")
	if err == nil {
		t.Fatal("ship succeeded, want a refusal naming the interleaved copy")
	}
	if !strings.Contains(err.Error(), "interleaves copies of p's commits") {
		t.Errorf("error = %v, want it to name the copies", err)
	}
	if strings.Contains(err.Error(), "run gt track") {
		t.Errorf("error = %v — a bare gt track adopts onto trunk", err)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); after != before {
		t.Errorf("c moved from %s to %s on a refusal", before, after)
	}
	if parent := shipParentOf(t, f, "c"); parent != "main" {
		t.Errorf("c's parent = %s, want it left on main", parent)
	}
}

// TestShipDryRunMirrorsTheReparent pins the dry run to the decision the real
// run makes: it used to report the tracked parent and derive the pull request's
// title from the parent's commit, which is the title #24579 opened with.
func TestShipDryRunMirrorsTheReparent(t *testing.T) {
	tests := []struct {
		name    string
		onto    string
		because string
	}{
		{name: "tracked onto trunk", onto: "main", because: "graphite tracks c on main, so ship re-parents it, replaying its 1 own commit(s) onto p, which is no longer in its history"},
		{name: "untracked", because: "untracked, so gt track --parent records it (ship drops -f, which would outrank it), replaying its 1 own commit(s) onto p, which is no longer in its history"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipParentRewritten(t, f, tt.onto)
			refs := dryRunRefs(t, f)

			report := dryRunReport(t, f, "--no-commit", "--parent", "p")
			if got := dryRunValues(report, "parent"); len(got) != 1 || got[0] != "p"+shipSep+tt.because {
				t.Errorf("parent = %q, want %q", got, "p"+shipSep+tt.because)
			}
			if got := dryRunValues(report, "pr new"); !strings.Contains(strings.Join(got, "\n"), "c"+shipSep+`opens a pull request titled "c"`) {
				t.Errorf("pr new = %q, want c titled from its own commit", got)
			}
			if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
				t.Errorf("refuses = %q, want none", refusals)
			}
			if after := dryRunRefs(t, f); after != refs {
				t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
			}
		})
	}
}

func TestShipDryRunReportsTheReparentRefusal(t *testing.T) {
	f := shipGTRepo(t)
	shipParentInterleaved(t, f)

	report := dryRunReport(t, f, "--no-commit", "--parent", "p")
	refusals := dryRunValues(report, "refuses")
	if len(refusals) != 1 || !strings.Contains(refusals[0], "interleaves copies of p's commits") {
		t.Errorf("refuses = %q, want the interleaved copy the real run refuses", refusals)
	}
}

func TestShipGTParentRefusesABranchAboveIt(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")

	shipGTReady(t, f)
	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "b")
	if err == nil || !strings.Contains(err.Error(), "names a or a branch stacked above it") {
		t.Fatalf("error = %v, want a refusal naming b as stacked above a", err)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "a"); after != before {
		t.Errorf("a moved from %s to %s on a refusal", before, after)
	}
	if parent := shipParentOf(t, f, "a"); parent != "main" {
		t.Errorf("a's parent = %s, want it left on main", parent)
	}
}
