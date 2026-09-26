package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const regenCat = "cat src/*.txt > gen/out.txt"

func regenRepo(t *testing.T, run string) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	writeShipFile(t, f.Dir, regenFile, fmt.Sprintf("[[generated]]\npaths = [\"gen/*.txt\"]\nrun = %q\n", run))
	writeShipFile(t, f.Dir, "src/a.txt", "a\n")
	writeShipFile(t, f.Dir, "gen/out.txt", "a\n")
	writeShipFile(t, f.Dir, "gen/other.txt", "other\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "generated")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	return f
}

func regenBranch(t *testing.T, f *vcstest.Fixture, files map[string]string, removed ...string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	for name, content := range files {
		writeShipFile(t, f.Dir, name, content)
	}
	for _, name := range removed {
		mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
}

func regenAdvanceTrunk(t *testing.T, f *vcstest.Fixture, files ...[2]string) {
	t.Helper()
	for _, file := range files {
		restackAdvanceRemote(t, f, "main", file[0], file[1])
	}
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	shipResetLog(t, f)
}

func TestStackRebaseRegeneratesAGeneratedOnlyConflict(t *testing.T) {
	f := regenRepo(t, regenCat)
	regenBranch(t, f, map[string]string{"src/f.txt": "f\n", "gen/out.txt": "a\nf\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/t.txt", "t\n"}, [2]string{"gen/out.txt", "a\nt\n"})

	plan, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if want := "feature" + shipSep + "regenerates gen/out.txt on conflict" + shipSep + regenCat; !strings.Contains(plan, want) {
		t.Errorf("plan = %q, want %q", plan, want)
	}

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if want := "regenerated gen/out.txt" + shipSep + regenCat; !strings.Contains(out, want) {
		t.Errorf("output = %q, want %q", out, want)
	}
	if strings.Contains(out, "regenerated gen/other.txt") {
		t.Errorf("output = %q, want gen/other.txt left alone", out)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature is not on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/out.txt"); got != "a\nf\nt" {
		t.Errorf("feature's gen/out.txt = %q, want the regenerated a, f, t", got)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..feature"); n != "1" {
		t.Errorf("feature holds %s commits over trunk, want its own 1", n)
	}
}

func TestStackRebaseLeavesAnUnconflictedGeneratedFileAlone(t *testing.T) {
	f := regenRepo(t, "echo regenerated > gen/other.txt; "+regenCat)
	regenBranch(t, f, map[string]string{"gen/other.txt": "stale by hand\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/t.txt", "t\n"})

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if strings.Contains(out, "regenerated") {
		t.Errorf("output = %q, want no generator run on a clean rebase", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/other.txt"); got != "stale by hand" {
		t.Errorf("feature's gen/other.txt = %q, want the branch's own content", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/out.txt"); got != "a" {
		t.Errorf("feature's gen/out.txt = %q, want it untouched", got)
	}
}

func TestStackContinueRegeneratesAfterAMixedConflict(t *testing.T) {
	f := regenRepo(t, regenCat)
	regenBranch(t, f, map[string]string{"src/a.txt": "a-feature\n", "src/f.txt": "f\n", "gen/out.txt": "a-feature\nf\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/a.txt", "a-trunk\n"}, [2]string{"gen/out.txt", "a-trunk\n"})
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the mixed conflict to stop")
	}
	for _, want := range []string{"src/a.txt", "generated, rerun from .ccx.toml by continue: gen/out.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("brief = %v, want %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved to %s on a stopped rebase", got)
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "src/a.txt", "a-both\n")
	mustRun(t, f.Env(), ws, "git", "add", "src/a.txt")

	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if want := "regenerated gen/out.txt" + shipSep + regenCat; !strings.Contains(out, want) {
		t.Errorf("continue output = %q, want %q", out, want)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/out.txt"); got != "a-both\nf" {
		t.Errorf("feature's gen/out.txt = %q, want it regenerated from the resolved sources", got)
	}
}

func TestStackRebaseStopsCleanlyOnAFailingGenerator(t *testing.T) {
	f := regenRepo(t, "echo generated half > gen/out.txt; echo boom >&2; exit 3")
	regenBranch(t, f, map[string]string{"src/f.txt": "f\n", "gen/out.txt": "a\nf\n"})
	regenAdvanceTrunk(t, f, [2]string{"gen/out.txt", "a\nt\n"})
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded over a failing generator")
	}
	for _, want := range []string{"regenerating gen/out.txt failed, and nothing was committed", "exited 3", "boom", "echo generated half > gen/out.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved to %s past a failed generator", got)
	}
	ws := stackWorkspaceOf(t, err)
	if unmerged := gitAt(t, f.Env(), ws, "diff", "--name-only", "--diff-filter=U"); unmerged != "gen/out.txt" {
		t.Errorf("unmerged = %q, want gen/out.txt still conflicted", unmerged)
	}
	if out, _, err := runStackCmd(t, f, "abort"); err != nil || !strings.Contains(out, "aborted") {
		t.Fatalf("abort = %q, %v", out, err)
	}
}

func TestStackRebaseScrubsGitEnvFromTheGenerator(t *testing.T) {
	f := regenRepo(t, `for v in GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX GIT_COMMON_DIR; do eval "test -z \"\${$v+set}\"" || { echo "$v leaked" >&2; exit 9; }; done; `+regenCat)
	regenBranch(t, f, map[string]string{"src/f.txt": "f\n", "gen/out.txt": "a\nf\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/t.txt", "t\n"}, [2]string{"gen/out.txt", "a\nt\n"})
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "leaked"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())

	cmd := newStackCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(os.Stderr)
	cmd.SetArgs([]string{"rebase", "--no-push"})
	err := cmd.ExecuteContext(render.WithEnv(f.Context(), "GIT_PREFIX=leaked/"))
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		if uerr := os.Unsetenv(name); uerr != nil {
			t.Fatal(uerr)
		}
	}
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/out.txt"); got != "a\nf\nt" {
		t.Errorf("feature's gen/out.txt = %q, want the regenerated a, f, t", got)
	}
}

func TestStackRebaseKeepsAReplayedDeletionOfAGeneratedFile(t *testing.T) {
	f := regenRepo(t, "echo regenerated > gen/out.txt")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "gen/out.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	regenAdvanceTrunk(t, f, [2]string{"gen/out.txt", "a\nt\n"})

	plan, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if strings.Contains(plan, "regenerates gen/out.txt") {
		t.Errorf("plan = %q, want no regeneration planned for a deleted path", plan)
	}
	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if strings.Contains(out, "regenerated gen/out.txt") {
		t.Errorf("output = %q, want no generator run for a deleted path", out)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature is not on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "ls-tree", "--name-only", "feature", "gen/"); got != "gen/other.txt" {
		t.Errorf("feature's gen/ = %q, want gen/out.txt still deleted", got)
	}
}

func TestStackRebaseKeepsDeletedOutputOfSharedGenerator(t *testing.T) {
	f := regenRepo(t, "echo regenerated > gen/out.txt; echo refreshed > gen/other.txt")
	regenBranch(t, f, map[string]string{"gen/other.txt": "feature\n"}, "gen/out.txt")
	regenAdvanceTrunk(t, f, [2]string{"gen/out.txt", "trunk\n"}, [2]string{"gen/other.txt", "trunk\n"})

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if !strings.Contains(out, "regenerated gen/other.txt") {
		t.Errorf("output = %q, want shared generator to run", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "ls-tree", "--name-only", "feature", "--", "gen/out.txt"); got != "" {
		t.Errorf("feature carries %q, want deleted output absent", got)
	}
}

func TestStackContinueCommitsAHumanDeletionOfAGeneratedFile(t *testing.T) {
	f := regenRepo(t, "echo regenerated > gen/out.txt")
	regenBranch(t, f, map[string]string{"src/a.txt": "a-feature\n", "gen/out.txt": "a-feature\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/a.txt", "a-trunk\n"}, [2]string{"gen/out.txt", "a-trunk\n"})

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the mixed conflict to stop")
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "src/a.txt", "a-both\n")
	mustRun(t, f.Env(), ws, "git", "add", "src/a.txt")
	mustRun(t, f.Env(), ws, "git", "rm", "-q", "gen/out.txt")

	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if strings.Contains(out, "regenerated gen/out.txt") {
		t.Errorf("continue output = %q, want no generator run for a path resolved as deleted", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "ls-tree", "--name-only", "feature", "gen/"); got != "gen/other.txt" {
		t.Errorf("feature's gen/ = %q, want gen/out.txt deleted as resolved", got)
	}
}

func TestStackContinueReadsTheGeneratorsFromTheReplayedCommit(t *testing.T) {
	f := regenRepo(t, "echo \"unknown command 'write'\" >&2; exit 1")
	regenBranch(t, f, map[string]string{regenFile: "", "src/a.txt": "a-feature\n", "gen/out.txt": "a by hand\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/a.txt", "a-trunk\n"}, [2]string{"gen/out.txt", "a-trunk\n"})

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict to stop")
	}
	if strings.Contains(err.Error(), "generated, rerun") || strings.Contains(err.Error(), "unknown command") {
		t.Errorf("brief = %v, want gen/out.txt left to the human now that the replayed commit retires its generator", err)
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "src/a.txt", "a-both\n")
	writeShipFile(t, ws, "gen/out.txt", "a-both by hand\n")
	mustRun(t, f.Env(), ws, "git", "add", "src/a.txt", "gen/out.txt")

	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if strings.Contains(out, "regenerated") {
		t.Errorf("continue output = %q, want the retired generator never run", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:gen/out.txt"); got != "a-both by hand" {
		t.Errorf("feature's gen/out.txt = %q, want the human's resolution", got)
	}
}

func TestStackRebaseRegeneratesEveryGeneratedPathTheStoppedCommitTouches(t *testing.T) {
	const upper = "tr a-z A-Z < gen/out.txt > up/out.txt"
	f := regenRepo(t, regenCat)
	writeShipFile(t, f.Dir, regenFile, fmt.Sprintf("[[generated]]\npaths = [\"gen/*.txt\"]\nrun = %q\n\n[[generated]]\npaths = [\"up/*.txt\"]\nrun = %q\n", regenCat, regenCat+" && "+upper))
	writeShipFile(t, f.Dir, "up/out.txt", "A\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "upper")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	regenBranch(t, f, map[string]string{"src/f.txt": "f\n", "gen/out.txt": "a\nf\n", "up/out.txt": "A\nF\n"})
	regenAdvanceTrunk(t, f, [2]string{"src/t.txt", "t\n"}, [2]string{"gen/out.txt", "a\nt\n"})
	config, err := os.ReadFile(filepath.Join(f.Dir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if want := "regenerated up/out.txt" + shipSep + regenCat + " && " + upper; !strings.Contains(out, want) {
		t.Errorf("output = %q, want %q", out, want)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:up/out.txt"); got != "A\nF\nT" {
		t.Errorf("feature's up/out.txt = %q, want it regenerated over trunk's t", got)
	}
	if after, err := os.ReadFile(filepath.Join(f.Dir, ".git", "config")); err != nil {
		t.Fatal(err)
	} else if string(after) != string(config) {
		t.Errorf(".git/config changed across a regenerating rebase:\nbefore:\n%s\nafter:\n%s", config, after)
	}
}
