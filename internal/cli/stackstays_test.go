package cli

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackPublishedBehindTrunk(t *testing.T, file string) (*vcstest.Fixture, map[string]string) {
	t.Helper()
	f := shipGTRepo(t)
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
		name     string
		args     []string
		wantPlan string
		restack  bool
	}{
		{"stays", nil, "base" + shipSep + "stays on ", false},
		{"restack", []string{"--restack"}, "base" + shipSep + "onto main" + shipSep + "from ", true},
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
			if clean := "merges cleanly onto main@" + pin[:12]; strings.Contains(out, clean) == tc.restack {
				t.Errorf("plan = %q, want %q named only when base stays", out, clean)
			}
			base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if moved := base != published["base"]; moved != tc.restack {
				t.Errorf("origin base moved = %t (%.12s → %.12s), want %t", moved, published["base"], base, tc.restack)
			}
			if moved := feature != published["feature"]; moved != tc.restack {
				t.Errorf("origin feature moved = %t (%.12s → %.12s), want %t", moved, published["feature"], feature, tc.restack)
			}
			if onTrunk := stackOnto(t, f, pin, base); onTrunk != tc.restack {
				t.Errorf("origin base on the fetched trunk = %t, want %t", onTrunk, tc.restack)
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

func TestStackSubmitRebasesWhenAChildConflictsBesideACleanMerge(t *testing.T) {
	f := shipGTRepo(t)
	writeShipFile(t, f.Dir, "a/x", "a\nb\nc\n")
	writeShipFile(t, f.Dir, "b/y", "1\n2\n3\n4\n5\n6\n7\n8\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "files")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	shipGTStack(t, f, "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "a/x", "a\nfeature\nc\n")
	writeShipFile(t, f.Dir, "b/y", "1\nfeature\n3\n4\n5\n6\n7\n8\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := map[string]string{}
	for _, branch := range []string{"base", "feature"} {
		published[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	}
	restackAdvanceRemote(t, f, "main", "a/x", "a\nmain\nc\n")
	stackAdvanceTrunk(t, f, "b/y", "1\n2\n3\n4\n5\n6\n7\nmain\n")
	pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit onto a trunk feature conflicts with succeeded, want a conflict stop")
	}
	for _, want := range []string{
		"base" + shipSep + "onto main" + shipSep + "from ",
		"feature" + shipSep + "onto base" + shipSep + "from " + published["base"][:12] + shipSep + "conflicts with main@" + pin[:12] + " in a/x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan = %q, want %q", out, want)
		}
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Conflict == nil || run.Conflict.Branch != "feature" {
		t.Errorf("run conflict = %+v, want a stop on feature", run.Conflict)
	}
	for branch, head := range published {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != head {
			t.Errorf("origin %s moved to %.12s before the conflict was resolved", branch, got)
		}
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
		name    string
		args    []string
		restack bool
	}{
		{"stays", nil, false},
		{"restack", []string{"--restack"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
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
			if onTrunk := stackOnto(t, f, "origin/main", published); onTrunk != tc.restack {
				t.Errorf("origin feature on the fetched trunk = %t, want %t", onTrunk, tc.restack)
			}
		})
	}
}

func TestStackSubmitKeepsNewWorkOnAPublishedTrunkBranch(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	stackCommit(t, f, "more.txt")
	local := shipHead(t, f)
	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("submit new work = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != local {
		t.Errorf("published feature = %s, want local head %s without replay", got, local)
	}
	if stackOnto(t, f, "origin/main", local) {
		t.Error("new work was replayed onto the newer trunk")
	}
}

func TestStackSubmitReplaysANeverPublishedBranch(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	local := shipHead(t, f)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first submit = %v (stderr=%q)", err, errStr)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if published == local || !stackOnto(t, f, "origin/main", published) {
		t.Errorf("first publication = %s, want %s replayed onto trunk", published, local)
	}
}

func TestStackSubmitRefusesThinHistoryBeforeTheMergeProbe(t *testing.T) {
	f := thinRepo(t)
	thinGrowTrunk(t, f, 12)
	store := thinTestStore(t, f)
	_, lane := thinNew(t, f, f.Dir, "lane1", "--thin", "--depth", "4")
	thinCommit(t, f, lane, "lane1.txt", "lane\n")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "deep", "origin/main~10")
	cut := thinCommit(t, f, f.Dir, "deep.txt", "deep\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", cut+":refs/heads/lane1")
	mustRun(t, f.Env(), store, "git", "fetch", "-q", "origin", "+refs/heads/lane1:refs/remotes/origin/lane1")
	mustRun(t, f.Env(), lane, "git", "reset", "-q", "--keep", cut)
	stubOpenPRs(t, f, nil, "lane1")
	shipResetLog(t, f)
	_, errStr, err := runStackCmdIn(t, f, lane, "submit")
	if err == nil || !strings.Contains(err.Error(), "shallow boundary") {
		t.Fatalf("submit over cut history = %v (stderr=%q), want the history refusal", err, errStr)
	}
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) > 1 && argv[0] == "git" && argv[1] == "merge-tree" {
			t.Errorf("merge probe ran before the history refusal: %v", argv)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "lane1"); got != cut {
		t.Errorf("refused submit changed the remote to %s, want %s", got, cut)
	}
}

func TestStackSubmitRebasesABranchThatConflictsUnderTrunksMergeAttributes(t *testing.T) {
	f := shipGTRepo(t)
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = strconv.Itoa(i + 1)
	}
	generated := func(at int, line string) string {
		edited := slices.Clone(lines)
		edited[at] = line
		return strings.Join(edited, "\n") + "\n"
	}
	commit := func(content string) {
		writeShipFile(t, f.Dir, "gen.txt", content)
		mustRun(t, f.Env(), f.Dir, "git", "add", "gen.txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "gen")
	}
	commit(generated(0, "1"))
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "base")
	commit(generated(4, "five"))
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	shipGTStack(t, f, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	restackAdvanceRemote(t, f, "main", ".gitattributes", "gen.txt merge=binary\n")
	stackAdvanceTrunk(t, f, "gen.txt", generated(44, "forty-five"))
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit onto a trunk whose merge=binary file conflicts succeeded, want a conflict stop")
	}
	if want := "base" + shipSep + "onto main" + shipSep + "from "; !strings.Contains(out, want) || strings.Contains(out, "merges cleanly") {
		t.Errorf("plan = %q, want %q and no clean-merge verdict", out, want)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Conflict == nil || run.Conflict.Branch != "base" {
		t.Errorf("run conflict = %+v, want a stop on base", run.Conflict)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published {
		t.Errorf("origin base moved to %.12s before the conflict was resolved", got)
	}
}

func TestStackSubmitRebasesABranchWhoseChildConflictsWithTrunk(t *testing.T) {
	f, published := stackPublishedBehindTrunk(t, "feature.txt")

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit under a child that conflicts with trunk succeeded, want a conflict stop")
	}
	if want := "base" + shipSep + "onto main" + shipSep + "from "; !strings.Contains(out, want) || strings.Contains(out, "merges cleanly") {
		t.Errorf("plan = %q, want %q and no clean-merge verdict", out, want)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Conflict == nil || run.Conflict.Branch != "feature" {
		t.Errorf("run conflict = %+v, want a stop on feature", run.Conflict)
	}
	for branch, head := range published {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != head {
			t.Errorf("origin %s moved to %.12s before the conflict was resolved", branch, got)
		}
	}
}
