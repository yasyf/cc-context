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

func regenBranch(t *testing.T, f *vcstest.Fixture, files map[string]string) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	for name, content := range files {
		writeShipFile(t, f.Dir, name, content)
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
}

func regenAdvanceTrunk(t *testing.T, f *vcstest.Fixture, files ...string) {
	t.Helper()
	for i := 0; i < len(files); i += 2 {
		restackAdvanceRemote(t, f, "main", files[i], files[i+1])
	}
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	shipResetLog(t, f)
}

func TestStackRebaseRegeneratesAGeneratedOnlyConflict(t *testing.T) {
	f := regenRepo(t, regenCat)
	regenBranch(t, f, map[string]string{"src/f.txt": "f\n", "gen/out.txt": "a\nf\n"})
	regenAdvanceTrunk(t, f, "src/t.txt", "t\n", "gen/out.txt", "a\nt\n")

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
	regenAdvanceTrunk(t, f, "src/t.txt", "t\n")

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
	regenAdvanceTrunk(t, f, "src/a.txt", "a-trunk\n", "gen/out.txt", "a-trunk\n")
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
	regenAdvanceTrunk(t, f, "gen/out.txt", "a\nt\n")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded over a failing generator")
	}
	for _, want := range []string{"regenerating gen/out.txt failed, and nothing was staged", "exited 3", "boom", "echo generated half > gen/out.txt"} {
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
	regenAdvanceTrunk(t, f, "src/t.txt", "t\n", "gen/out.txt", "a\nt\n")
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
