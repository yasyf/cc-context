package cli

import (
	"os"
	"path/filepath"
	"slices"
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

func TestShipGTParentReplaysNoTrunkCommitTheParentLacks(t *testing.T) {
	tests := []struct {
		name string
		onto string
		want string
	}{
		{name: "untracked", want: "tracked c onto p (replayed its 1 own commit(s))"},
		{name: "tracked onto trunk", onto: "main", want: "reparented c from main onto p (replayed its 1 own commit(s))"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "p")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
			writeShipFile(t, f.Dir, "trunk.txt", "trunk\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "trunk.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "trunk")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
			shipGTUntracked(t, f, "c")
			if tt.onto != "" {
				mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", tt.onto, "--no-interactive")
			}
			shipGTReady(t, f)

			got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("summary = %q, want it to carry %q", got, tt.want)
			}
			if parent := shipParentOf(t, f, "c"); parent != "p" {
				t.Errorf("c's parent = %s, want p", parent)
			}
			if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "p..c"); subjects != "fix: frobnicate\nc" {
				t.Errorf("c carries %q above p, want its own commits alone", subjects)
			}
		})
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

func TestShipGTParentNamesARebaseInProgressBeforeTheReplay(t *testing.T) {
	f := shipGTRepo(t)
	shipParentRewritten(t, f, "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, f.Dir, "c.txt", "p\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "p writes c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	shipGTReady(t, f)

	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "REBASE_HEAD", before)
	if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p"); err == nil || !strings.Contains(err.Error(), "c's own commits do not replay onto p cleanly") {
		t.Fatalf("ship with only a stale REBASE_HEAD = %v, want the replay refusal", err)
	}
	if err := os.Mkdir(filepath.Join(f.Dir, ".git", "rebase-merge"), 0o750); err != nil {
		t.Fatal(err)
	}

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "p")
	if err == nil || !strings.Contains(err.Error(), "rebase is in progress in "+f.Dir) {
		t.Fatalf("ship with a rebase-merge dir = %v, want the rebase in progress named", err)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); after != before {
		t.Errorf("c moved from %s to %s on a refusal", before, after)
	}
	if parent := shipParentOf(t, f, "c"); parent != "main" {
		t.Errorf("c's parent = %s, want it left on main", parent)
	}
}

// TestShipGTParentMovesNothingWhenALaterCheckRefuses holds the move to the
// same rule as every other write: a ship refused for a dirty --no-commit
// checkout leaves the branch where it was, on the parent gt recorded.
func TestShipGTParentMovesNothingWhenALaterCheckRefuses(t *testing.T) {
	f := shipGTRepo(t)
	shipParentRewritten(t, f, "main")
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	writeShipFile(t, f.Dir, "dirty.txt", "dirty\n")

	if _, err := runShipCmd(f.Context(), t, "--no-commit", "--no-push", "--parent", "p"); err == nil {
		t.Fatal("ship succeeded over a dirty --no-commit checkout")
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); after != before {
		t.Errorf("c moved from %s to %s on a refusal", before, after)
	}
	if parent := shipParentOf(t, f, "c"); parent != "main" {
		t.Errorf("c's parent = %s, want it left on main", parent)
	}
}

// shipBehindTrunk cuts an untracked b with no commit of its own, then moves
// main on past it, the shape a branch cut from a stale origin/dev takes once
// another lane fast-forwards local dev.
func shipBehindTrunk(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "branch", "b")
	writeShipFile(t, f.Dir, "trunk.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "trunk.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
}

// TestShipGTCommitsABranchWithNoCommitOfItsOwn is the standup-skill ship: a
// branch holding nothing above trunk was refused before ship committed the
// edits that would have become its own commit.
func TestShipGTCommitsABranchWithNoCommitOfItsOwn(t *testing.T) {
	f := shipGTRepo(t)
	shipBehindTrunk(t, f)

	shipGTReady(t, f)
	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := "tracked b onto main (moved up to main, holding no commit of its own until this one)"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "b"); parent != "main" {
		t.Errorf("b's parent = %s, want main", parent)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "b..main"); behind != "0" {
		t.Errorf("main holds %s commit(s) b does not, want b on main's head", behind)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "main..b"); subjects != "fix: frobnicate" {
		t.Errorf("b carries %q above main, want the commit ship made alone", subjects)
	}
}

func TestShipGTRefusesABranchWithNoCommitOfItsOwnAndNothingToCommit(t *testing.T) {
	f := shipGTRepo(t)
	shipBehindTrunk(t, f)
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	shipResetLog(t, f)

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "b holds no commit of its own above main") {
		t.Fatalf("error = %v, want the refusal naming b's missing commits", err)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "b"); after != before {
		t.Errorf("b moved from %s to %s on a refusal", before, after)
	}
}

func shipBehindTrunkUnder(t *testing.T, f *vcstest.Fixture, names ...string) {
	t.Helper()
	for _, name := range names {
		writeShipFile(t, f.Dir, name, "one\ntwo\nthree\nfour\nfive\n")
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", ".")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "b")
	for _, name := range names {
		writeShipFile(t, f.Dir, name, "ONE\ntwo\nthree\nfour\nfive\n")
	}
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
	for _, name := range names {
		writeShipFile(t, f.Dir, name, "one\ntwo\nthree\nfour\nFIVE\n")
	}
	shipResetLog(t, f)
}

func TestShipGTCommitsBeforeReplayingOverItsOwnEdits(t *testing.T) {
	f := shipGTRepo(t)
	shipBehindTrunkUnder(t, f, "shared.txt")

	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "shared.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := "tracked b onto main (replayed its 1 own commit(s))"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "b"); parent != "main" {
		t.Errorf("b's parent = %s, want main", parent)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "b..main"); behind != "0" {
		t.Errorf("main holds %s commit(s) b does not, want b on main's head", behind)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "main..b"); subjects != "fix: frobnicate" {
		t.Errorf("b carries %q above main, want the commit ship made alone", subjects)
	}
	if shared := gitAt(t, f.Env(), f.Dir, "show", "b:shared.txt"); shared != "ONE\ntwo\nthree\nfour\nFIVE" {
		t.Errorf("b's shared.txt = %q, want trunk's edit and the ship's together", shared)
	}
	if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "" {
		t.Errorf("status = %q, want a clean working copy", status)
	}
}

func TestShipGTRefusesAReplayOverEditsItLeavesOut(t *testing.T) {
	f := shipGTRepo(t)
	shipBehindTrunkUnder(t, f, "shared.txt", "other.txt")
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "shared.txt")
	if err == nil || !strings.Contains(err.Error(), "replaying b onto main would overwrite uncommitted changes") {
		t.Fatalf("error = %v, want the refusal naming the uncommitted changes", err)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "b"); after != before {
		t.Errorf("b moved from %s to %s on a refusal", before, after)
	}
	if other := gitAt(t, f.Env(), f.Dir, "show", ":other.txt"); other != "one\ntwo\nthree\nfour\nfive" {
		t.Errorf("index other.txt = %q, want b's base left in place", other)
	}
	if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "M other.txt\n M shared.txt" {
		t.Errorf("status = %q, want both edits left uncommitted", status)
	}
}

func TestShipGTAdoptsOntoTheRemoteTrunkNotALocalTrunkLeftBehindIt(t *testing.T) {
	f := shipGTRepo(t)
	writeShipFile(t, f.Dir, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", ".")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "b")
	writeShipFile(t, f.Dir, "trunk.txt", "remote\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", ".")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "remote trunk")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	writeShipFile(t, f.Dir, "local.txt", "local only\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", ".")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "local trunk only")
	stale := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
	writeShipFile(t, f.Dir, "own.txt", "own\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", ".")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "own work")
	writeShipFile(t, f.Dir, "more.txt", "more\n")
	shipResetLog(t, f)

	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "more.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if base := gitAt(t, f.Env(), f.Dir, "merge-base", remote, "b"); base != remote {
		t.Errorf("b forks from main at %s, want it on the remote trunk %s; summary %q", base, remote, got)
	}
	if base := gitAt(t, f.Env(), f.Dir, "merge-base", stale, "b"); base == stale {
		t.Errorf("b carries %s, a commit only the local trunk holds; summary %q", stale, got)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", remote+"..b"); subjects != "fix: frobnicate\nown work" {
		t.Errorf("b carries %q above the remote trunk, want its own commits alone", subjects)
	}
}

// TestShipGTParentPutsTheBranchBackWhenTheStackRebaseRefuses is a --tip-only
// ship of a branch whose own pull request landed before its latest commit: the
// stack rebase refused after the move had already replayed the branch onto the
// parent's local head, and the refusal left it there.
func TestShipGTParentPutsTheBranchBackWhenTheStackRebaseRefuses(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "p")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "c")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	stackCommit(t, f, "later.txt")
	stubStackPRs(t, f, map[string]*stackPR{
		"p": {Number: 6, Title: "p", State: "OPEN", Base: "base"},
		"c": {Number: 7, Title: "c", State: "CLOSED", Head: landedAt, Landed: true},
	})
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	shipResetLog(t, f)

	_, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "p")
	if err == nil || !strings.Contains(err.Error(), "holds commits past it") {
		t.Fatalf("ship = %v, want the stack rebase's refusal", err)
	}
	if want := "put c back at " + before[:12] + " on main, where it was before the move onto p"; !strings.Contains(err.Error(), want) {
		t.Errorf("ship = %v, want it to carry %q", err, want)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); after != before {
		t.Errorf("c moved from %s to %s on a refusal", before, after)
	}
	if parent := shipParentOf(t, f, "c"); parent != "main" {
		t.Errorf("c's parent = %s, want it left on main", parent)
	}
	if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "" {
		t.Errorf("working copy = %q, want it back on c's head", status)
	}
}

// TestShipGTTipOnlyLeavesALandedAncestorWithLaterCommitsToItsLane is a
// --tip-only ship over an ancestor another lane kept committing to after its
// pull request landed: the ship moves none of that ancestor, so it ships the tip
// onto its parent's published head instead of refusing over the ancestor, and
// it names the branch holders without a git worktree list.
func TestShipGTTipOnlyLeavesALandedAncestorWithLaterCommitsToItsLane(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	stackCommit(t, f, "later.txt")
	shipGTStack(t, f, "p")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "c")
	stubStackPRs(t, f, map[string]*stackPR{
		"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true},
		"p":    {Number: 6, Title: "p", State: "OPEN", Base: "base"},
	})
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	shipResetLog(t, f)

	if _, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "p"); err != nil {
		t.Fatalf("ship = %v, want c shipped onto p", err)
	}
	if parent := shipParentOf(t, f, "c"); parent != "p" {
		t.Errorf("c's parent = %s, want p", parent)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); after != base {
		t.Errorf("base moved from %s to %s, want it left to its lane", base, after)
	}
	for _, inv := range shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "git") {
		if i := slices.Index(inv, "worktree"); i >= 0 && i+1 < len(inv) && inv[i+1] == "list" {
			t.Errorf("ship ran %v, want the holders read without git worktree list", inv)
		}
	}
}

// TestShipGTTipOnlyRefusesToRehomeOntoAReusedLandedAncestor is a --tip-only
// ship whose landed parent would re-home the tip onto an ancestor another lane
// kept committing to after its own pull request landed: the tip never held
// those commits, so the ship refuses rather than replay it onto them.
func TestShipGTTipOnlyRefusesToRehomeOntoAReusedLandedAncestor(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	shipGTStack(t, f, "mid", "c")
	mid := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	stackCommit(t, f, "later.txt")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	stubStackPRs(t, f, map[string]*stackPR{
		"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true},
		"mid":  {Number: 6, Title: "mid", State: "CLOSED", Head: mid, Landed: true},
	})
	shipResetLog(t, f)

	_, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--tip-only")
	if err == nil || !strings.Contains(err.Error(), "holds commits past it") {
		t.Fatalf("ship = %v, want the refusal to re-home c onto base's later commits", err)
	}
}

// TestShipGTTipOnlyReparentsAMidStackBranchOntoTrunkWithItsOwnCommitsAlone is
// port-serve-s6's #44730: --parent main --tip-only --restack on a branch above
// two published parents with open pull requests replayed every commit above
// trunk as the branch's own, so its pull request carried both parents' work,
// and left the lane's working copy on the old tree.
func TestShipGTTipOnlyReparentsAMidStackBranchOntoTrunkWithItsOwnCommitsAlone(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	lane := f.WorktreePath("lane")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", lane, "c")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 5, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 6, Title: "b", State: "OPEN", Base: "a"},
		"c": {Number: 7, Title: "c", State: "OPEN", Base: "b"},
	})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	got, errStr, err := runShipCmdFull(f.ContextIn(lane), t, "--no-commit", "--no-watch", "--parent", "main", "--tip-only", "--restack")
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q), want c moved onto main", err, errStr)
	}
	if want := "reparented c from b onto main (replayed its 1 own commit(s))"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "c"); parent != "main" {
		t.Errorf("c's parent = %s, want main", parent)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "origin/main..c"); subjects != "c" {
		t.Errorf("c carries %q above trunk, want its own commit alone", subjects)
	}
	if subjects := gitAt(t, f.Env(), f.RemoteDir, "log", "--format=%s", "main..c"); subjects != "c" {
		t.Errorf("origin c carries %q above trunk, want its own commit alone", subjects)
	}
	if status := gitAt(t, f.Env(), lane, "status", "--porcelain"); status != "" {
		t.Errorf("lane = %q, want it clean at c's new head", status)
	}
}

// TestShipGTReparentOntoAParentBelowTheOldOneDropsTheOldParentsCommits is
// Forge-AI/monorepo#45419: --parent onto a branch the old parent was stacked
// on found the new parent already in the branch's history, moved nothing, and
// reported the reparent, so the pull request kept the old parent's commit.
func TestShipGTReparentOntoAParentBelowTheOldOneDropsTheOldParentsCommits(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c")
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 5, Title: "a", State: "OPEN", Base: "main"},
		"b": {Number: 6, Title: "b", State: "OPEN", Base: "a"},
		"c": {Number: 7, Title: "c", State: "OPEN", Base: "b"},
	})
	was := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")
	shipResetLog(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--parent", "a", "--tip-only")
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q), want c moved onto a", err, errStr)
	}
	if want := "reparented c from b onto a (replayed its 1 own commit(s))"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if now := gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); now == was {
		t.Errorf("c stayed at %s, want its own commit replayed onto a", was)
	}
	if subjects := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "a..c"); subjects != "c" {
		t.Errorf("c carries %q above a, want its own commit alone", subjects)
	}
}

// TestShipGTTipOnlyRetracksAParentGraphiteCallsDiverged is p8a-step7's
// #207148: the landing runner rebased the parent in the shared clone, so
// Graphite's record of it named a base its history no longer held, and gt
// refused to track the new branch onto a diverged branch. --tip-only re-tracks
// the parent with gt's own remedy, then ships the branch onto it alone.
func TestShipGTTipOnlyRetracksAParentGraphiteCallsDiverged(t *testing.T) {
	f := shipGTRepo(t)
	stackCommit(t, f, "m1.txt")
	shipGTStack(t, f, "p")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "main~1", "main", "p")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "-f", "origin", "p")
	mustRun(t, f.Env(), f.Dir, "gt", "log", "short", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "n")
	stackCommit(t, f, "n.txt")
	stubStackPRs(t, f, map[string]*stackPR{"p": {Number: 6, Title: "p", State: "OPEN", Base: "main"}})
	shipResetLog(t, f)

	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("## Summary\n\nn on p.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "p", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q), want n shipped onto p", err, errStr)
	}
	if parent := shipParentOf(t, f, "n"); parent != "p" {
		t.Errorf("n's parent = %s, want p", parent)
	}
	if !strings.Contains(got, "re-tracked p onto main") {
		t.Errorf("ship output = %q, want it to name the re-track of p", got)
	}
}

// TestShipGTTipOnlyRetracksADivergedParentOntoTrunkOnceItsRecordedParentLeftItsHistory
// is p8a-step7's #210347: dar-p8a-step7-stream-relay was rebased off the
// durable-v3/ssql-signal-5 branch Graphite still recorded as its parent, and gt
// refused to re-track it onto a branch no longer in its history.
func TestShipGTTipOnlyRetracksADivergedParentOntoTrunkOnceItsRecordedParentLeftItsHistory(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "q", "p")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "main", "q", "p")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "-f", "origin", "p")
	mustRun(t, f.Env(), f.Dir, "gt", "log", "short", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "n")
	stackCommit(t, f, "n.txt")
	stubStackPRs(t, f, map[string]*stackPR{"p": {Number: 6, Title: "p", State: "OPEN", Base: "main"}})
	shipResetLog(t, f)

	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("## Summary\n\nn on p.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "p", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q), want n shipped onto p", err, errStr)
	}
	if parent := shipParentOf(t, f, "n"); parent != "p" {
		t.Errorf("n's parent = %s, want p", parent)
	}
	if !strings.Contains(got, "re-tracked p onto main, since its recorded parent q is no longer in its history") {
		t.Errorf("ship output = %q, want it to name the re-track of p onto main", got)
	}
}

func TestShipGTTipOnlyMovesOntoTheParentsPublishedHeadWhenItsTrackingRefIsStale(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	stale := strings.TrimSpace(mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "p"))
	edit := func(content, message string) {
		if err := os.WriteFile(filepath.Join(f.Dir, "x.txt"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		mustRun(t, f.Env(), f.Dir, "git", "add", "x.txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", message)
	}
	edit("one\n", "p: x")
	published := strings.TrimSpace(mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "p"))
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "p")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/remotes/origin/p", stale)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "n")
	edit("two\n", "n: x")
	stubStackPRs(t, f, map[string]*stackPR{"p": {Number: 6, Title: "p", State: "OPEN", Base: "main"}})
	shipResetLog(t, f)

	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("## Summary\n\nn on p.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "p", "--pr-body-file", body); err != nil {
		t.Fatalf("ship = %v (stderr=%q), want n shipped onto p's published head", err, errStr)
	}
	if parent := shipParentOf(t, f, "n"); parent != "p" {
		t.Errorf("n's parent = %s, want p", parent)
	}
	mustRun(t, f.Env(), f.Dir, "git", "merge-base", "--is-ancestor", published, "n")
}

// TestShipDryRunUnderTipOnlyPublishesAnUntrackedBranchAlone is the dry run
// behind #207148, which told p8a-step7 an untracked branch shipped with
// --tip-only would publish its parent's whole downstack.
func TestShipDryRunUnderTipOnlyPublishesAnUntrackedBranchAlone(t *testing.T) {
	r := &shipDryRun{branch: "n", parent: "p", trunk: "main"}
	if anchor := dryRunAnchor(gtState{}, true, r); anchor != "" || !slices.Equal(r.notes, []string{"n is untracked; --tip-only records it on p's published head and publishes it alone"}) {
		t.Errorf("anchor = %q, notes = %q, want n published alone", anchor, r.notes)
	}
}
