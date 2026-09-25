package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// dryRunFixture is a three-branch graphite stack whose bottom branch grew a
// commit under the two above it, standing on the tip with an edit waiting —
// the shape every restack question in the field is asked from.
func dryRunFixture(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	writeShipFile(t, f.Dir, "a.txt", "moved on\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "a moves on")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	shipGTReady(t, f)
	return f
}

// dryRunRefs snapshots every ref in the repository, the record a read-only
// claim is graded against.
func dryRunRefs(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	return gitAt(t, f.Env(), f.Dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

func dryRunReport(t *testing.T, f *vcstest.Fixture, args ...string) string {
	t.Helper()
	out, errOut, err := runShipCmdFull(f.Context(), t, append([]string{"--dry-run"}, args...)...)
	if err != nil {
		t.Fatalf("ship --dry-run error = %v\n%s", err, errOut)
	}
	return out
}

// dryRunValues pulls the value of every line carrying label, so an assertion
// names the fact rather than the column the renderer pads it to.
func dryRunValues(report, label string) []string {
	var values []string
	for _, line := range strings.Split(report, "\n") {
		if len(line) < infoLabelWidth || strings.TrimSpace(line[:infoLabelWidth]) != label {
			continue
		}
		values = append(values, strings.TrimSpace(line[infoLabelWidth:]))
	}
	return values
}

func dryRunBranches(report, label string) []string {
	values := dryRunValues(report, label)
	names := make([]string, 0, len(values))
	for _, value := range values {
		names = append(names, strings.SplitN(value, shipSep, 2)[0])
	}
	return names
}

// TestShipDryRunMovesNoRef is the whole promise: the report costs the caller
// nothing. Every ref and the working copy are compared byte for byte across the
// run, and the invocation log is checked for a mutating verb as well, so a call
// that moved something back again would still fail.
func TestShipDryRunMovesNoRef(t *testing.T) {
	f := dryRunFixture(t)
	refs := dryRunRefs(t, f)
	tree := gitAt(t, f.Env(), f.Dir, "status", "--porcelain")

	report := dryRunReport(t, f, "-m", "fix: frobnicate")

	if got := dryRunRefs(t, f); got != refs {
		t.Errorf("refs moved across a dry run:\n got: %s\nwant: %s", got, refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); got != tree {
		t.Errorf("working copy moved across a dry run:\n got: %s\nwant: %s", got, tree)
	}
	invocations := shipGTInvocations(t, f)
	assertNoShipMutation(t, invocations)
	for _, inv := range invocations {
		if inv[0] == "gt" || (len(inv) > 1 && inv[1] == "fetch") {
			t.Errorf("dry run reached a mutating tool: %v", inv)
		}
	}
	if !strings.HasPrefix(report, "dry run") {
		t.Errorf("report = %q, want it to lead by saying it did nothing", report)
	}
}

// TestShipDryRunNamesTheTrackParent proves the report answers the question gt
// track answers silently: an untracked branch is adopted onto the nearest
// tracked ancestor, not onto trunk, and every branch above that ancestor rides
// out with the submit built on it.
func TestShipDryRunNamesTheTrackParent(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b")
	shipGTUntracked(t, f, "c")
	shipGTReady(t, f)

	report := dryRunReport(t, f, "-m", "fix: frobnicate")

	parent := dryRunValues(report, "parent")
	if len(parent) != 1 {
		t.Fatalf("parent lines = %v, want exactly one", parent)
	}
	if !strings.HasPrefix(parent[0], "b"+shipSep) {
		t.Errorf("parent = %q, want the nearest tracked ancestor b", parent[0])
	}
	if !strings.Contains(parent[0], "nearest tracked ancestor") {
		t.Errorf("parent = %q, want it to say why b was resolved", parent[0])
	}
}

// TestShipDryRunOrdersOnlyContainedTrackedBranches pins the cost of that answer:
// one for-each-ref names the tracked branches the untracked one contains, and
// only those are ordered, so a repository with thousands of tracked siblings
// costs no ancestry check per sibling.
func TestShipDryRunOrdersOnlyContainedTrackedBranches(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "x", "y")
	mustRun(t, f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "a", "b")
	shipGTUntracked(t, f, "c")
	shipGTReady(t, f)

	report := dryRunReport(t, "-m", "fix: frobnicate")

	if parent := dryRunValues(report, "parent"); len(parent) != 1 || !strings.HasPrefix(parent[0], "b"+shipSep) {
		t.Fatalf("parent = %v, want the nearest tracked ancestor b", parent)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if !slices.Contains(inv, "--is-ancestor") {
			continue
		}
		for _, sibling := range []string{"refs/heads/x", "refs/heads/y"} {
			if slices.Contains(inv, sibling) {
				t.Errorf("ancestry check %v ran on %s, which c does not contain", inv, sibling)
			}
		}
	}
}

// TestShipDryRunSeparatesTheStagedIndex proves the two sets are never one
// count. The graphite commit carries no pathspec, so a path-scoped ship takes
// the whole index with it, and the report says which files that is.
func TestShipDryRunSeparatesTheStagedIndex(t *testing.T) {
	f := dryRunFixture(t)
	writeShipFile(t, f.Dir, "named.txt", "named\n")
	writeShipFile(t, f.Dir, "foreign.txt", "another lane\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "foreign.txt")

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "named.txt")

	commit := dryRunValues(report, "commit")
	if len(commit) != 1 {
		t.Fatalf("commit lines = %v, want exactly one", commit)
	}
	for _, want := range []string{"1 file named", "named.txt", "1 staged elsewhere", "swept in", "foreign.txt"} {
		if !strings.Contains(commit[0], want) {
			t.Errorf("commit = %q, want it to carry %q", commit[0], want)
		}
	}
}

// TestShipDryRunPredictsTheRealShip is the only assertion that makes the report
// worth printing: the branches it names as moving are exactly the branches the
// real ship then moves, and the ones it leaves out do not move.
func TestShipDryRunPredictsTheRealShip(t *testing.T) {
	f := dryRunFixture(t)
	before := map[string]string{}
	for _, branch := range []string{"main", "a", "b", "c"} {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")
	predicted := map[string]bool{}
	for _, name := range dryRunBranches(report, "restack") {
		predicted[name] = true
	}
	if !predicted["b"] {
		t.Fatalf("restack lines = %v, want b, which sits off the amended a", dryRunValues(report, "restack"))
	}

	shipResetLog(t, f)
	if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}

	// c carries the commit, so it moves whether or not it is restacked.
	predicted["c"] = true
	for branch, was := range before {
		moved := gitAt(t, f.Env(), f.Dir, "rev-parse", branch) != was
		if moved != predicted[branch] {
			t.Errorf("%s moved = %v, dry run predicted %v", branch, moved, predicted[branch])
		}
	}
}

// TestShipDryRunNamesTheMovingPRHeads proves the report answers whose work a
// force-push lands on, and separates a head that moves from one pushed where it
// stands. Ownership is the working copy holding the branch: one shared account
// authors every lane, so the path is the only field telling them apart.
func TestShipDryRunNamesTheMovingPRHeads(t *testing.T) {
	f := dryRunFixture(t)
	stub := stubGTAPI(t)
	stub.prs["a"] = 20001
	stub.prs["b"] = 22285
	stub.prs["c"] = 23277

	report := dryRunReport(t, f, "-m", "fix: frobnicate")

	heads := dryRunValues(report, "pr head")
	if len(heads) != 3 {
		t.Fatalf("pr head lines = %v, want one per open pull request the submit pushes", heads)
	}
	for i, want := range []string{
		"#20001" + shipSep + "a" + shipSep,
		"#22285" + shipSep + "b" + shipSep,
		"#23277" + shipSep + "c" + shipSep,
	} {
		if !strings.HasPrefix(heads[i], want) {
			t.Errorf("pr head %d = %q, want it to lead with %q", i, heads[i], want)
		}
	}
	if !strings.Contains(heads[0], "force-pushed unchanged") {
		t.Errorf("a = %q, want it named as pushed where it stands — no restack moves it", heads[0])
	}
	if !strings.Contains(heads[1], "moves") || !strings.HasSuffix(heads[1], "no working copy") {
		t.Errorf("b = %q, want a moving head held by no working copy", heads[1])
	}
	if !strings.Contains(heads[2], "moves") || !strings.HasSuffix(heads[2], "here") {
		t.Errorf("c = %q, want a moving head held here", heads[2])
	}
}

// TestShipDryRunNamesTheDerivedTitles proves the report answers what a submit
// would open, not only what it would move. The title comes from the first
// commit above the base, not the latest one: a carries two commits and the
// title is the first, which is how a pull request has gone out under a title
// nobody recognized.
func TestShipDryRunNamesTheDerivedTitles(t *testing.T) {
	f := dryRunFixture(t)
	stub := stubGTAPI(t)
	stub.prs["b"] = 22285

	report := dryRunReport(t, f, "-m", "fix: frobnicate")

	creates := dryRunValues(report, "pr new")
	want := []string{
		"a" + shipSep + `opens a pull request titled "a"`,
		"c" + shipSep + `opens a pull request titled "c"`,
	}
	if len(creates) != len(want) {
		t.Fatalf("pr new lines = %v, want %v", creates, want)
	}
	for i, line := range creates {
		if line != want[i] {
			t.Errorf("pr new line %d = %q, want %q", i, line, want[i])
		}
	}
}

// TestShipDryRunNamesTheRewrittenPaths proves the report names the file whose
// content the replay decides rather than the caller: b dropped the rendered
// file, a has re-rendered it since, and which copy survives the restack is not
// visible in either tree until the restack has already run.
func TestShipDryRunNamesTheRewrittenPaths(t *testing.T) {
	f := shipGTRepo(t)
	writeShipFile(t, f.Dir, "gen.txt", "rendered\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "gen.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "render")
	shipGTStack(t, f, "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "b")
	mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "gen.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "b drops the rendered file")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	writeShipFile(t, f.Dir, "gen.txt", "re-rendered\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "a re-renders")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
	shipGTReady(t, f)

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")

	if got := dryRunValues(report, "rewrites"); len(got) != 1 || got[0] != "gen.txt" {
		t.Errorf("rewrites = %v, want exactly gen.txt", got)
	}
}

// TestShipDryRunReportsTheRefusalTheRealRunMakes is the bar the report has to
// clear to be worth printing: it must not read as permission for a ship that
// would refuse. The dry run is compared against the real run word for word,
// because a refusal the report paraphrases is one a reader cannot match to the
// failure they get.
func TestShipDryRunReportsTheRefusalTheRealRunMakes(t *testing.T) {
	f := dryRunFixture(t)
	writeShipFile(t, f.Dir, "untracked.txt", "another lane\n")

	report := dryRunReport(t, f, "--no-commit")
	refusals := dryRunValues(report, "refuses")
	if len(refusals) != 1 {
		t.Fatalf("refuses lines = %v, want the one --no-commit earns over a dirty working copy", refusals)
	}

	_, _, err := runShipCmdFull(f.Context(), t, "--no-commit")
	if err == nil {
		t.Fatal("the real ship accepted a dirty working copy under --no-commit")
	}
	if refusals[0] != err.Error() {
		t.Errorf("dry run refusal = %q\nreal refusal    = %q", refusals[0], err.Error())
	}
}

// TestShipDryRunReportsAnEmptyCommit covers a clean tree with nothing named on
// a branch level with trunk: there is nothing to cut and nothing to submit, so
// the run refuses and the report says so rather than printing a plan for a run
// that never starts.
func TestShipDryRunReportsAnEmptyCommit(t *testing.T) {
	f := shipGTRepo(t)
	shipGTLevel(t, f, "a")
	shipResetLog(t, f)

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")

	refusals := dryRunValues(report, "refuses")
	if len(refusals) != 1 || !strings.Contains(refusals[0], "nothing to commit") {
		t.Fatalf("refuses lines = %v, want one naming an empty commit", refusals)
	}
}

// TestShipDryRunSaysAnEmptyCommitShipsWhenTheBranchIsAhead is the other half,
// and the one that keeps the refusal honest: a branch already carrying commits
// trunk does not is shipped as --no-commit, not refused. Calling that a
// refusal would be the same false certainty in the opposite direction.
func TestShipDryRunSaysAnEmptyCommitShipsWhenTheBranchIsAhead(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a")
	shipResetLog(t, f)

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")

	if got := dryRunValues(report, "refuses"); len(got) != 0 {
		t.Errorf("refuses lines = %v, want none — the branch is ahead of trunk, so the run ships it", got)
	}
	notes := strings.Join(dryRunValues(report, "note"), " | ")
	if !strings.Contains(notes, "ships it as --no-commit") {
		t.Errorf("notes = %q, want one saying the run ships the branch as --no-commit", notes)
	}
}

// TestShipDryRunReportsNoRefusalWhenTheShipWouldRun is the negative control:
// the refusal lines must be absent when there is nothing to refuse, or they
// would read as noise and be ignored when they matter.
func TestShipDryRunReportsNoRefusalWhenTheShipWouldRun(t *testing.T) {
	f := dryRunFixture(t)

	report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")

	if got := dryRunValues(report, "refuses"); len(got) != 0 {
		t.Errorf("refuses lines = %v, want none on a ship that would run", got)
	}
}
