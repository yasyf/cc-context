package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

// gitRepo initializes a fresh git repo in a temp dir with a deterministic identity
// (reusing the env-isolated runGit helper) and returns its path; it skips the test
// when git is not on PATH.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// blob resolves a plan accessor and returns its bytes as a string, failing on error.
func blob(t *testing.T, f func(string) ([]byte, error), path string) string {
	t.Helper()
	b, err := f(path)
	if err != nil {
		t.Fatalf("blob %q: %v", path, err)
	}
	return string(b)
}

func sortedFiles(p DiffPlan) []string {
	out := append([]string(nil), p.Files...)
	sort.Strings(out)
	return out
}

func TestResolveDiffPlanGitUncommitted(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\n")
	write(t, dir, "keep.go", "package a\n\nvar Keep = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	// modify a.go, delete keep.go, and leave new.go purely untracked (never staged)
	// so the plan must fold in `git ls-files --others` rather than only tracked diffs.
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 2 }\n")
	if err := os.Remove(filepath.Join(dir, "keep.go")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "new.go", "package a\n\nfunc Bar() {}\n")

	plan, err := ResolveDiffPlan(context.Background(), dir, "uncommitted")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if plan.Label != "uncommitted" || !plan.Symbolic || plan.Raw != nil {
		t.Fatalf("plan = %+v, want uncommitted/symbolic/no-raw", plan)
	}
	if got, want := sortedFiles(plan), []string{"a.go", "keep.go", "new.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := blob(t, plan.Before, "a.go"); got != "package a\n\nfunc Foo() int { return 1 }\n" {
		t.Errorf("before a.go = %q", got)
	}
	if got := blob(t, plan.After, "a.go"); got != "package a\n\nfunc Foo() int { return 2 }\n" {
		t.Errorf("after a.go = %q", got)
	}
	if got := blob(t, plan.Before, "new.go"); got != "" {
		t.Errorf("before new.go = %q, want empty (added)", got)
	}
	if got := blob(t, plan.After, "new.go"); got != "package a\n\nfunc Bar() {}\n" {
		t.Errorf("after new.go = %q", got)
	}
	if got := blob(t, plan.After, "keep.go"); got != "" {
		t.Errorf("after keep.go = %q, want empty (deleted)", got)
	}
	if got := blob(t, plan.Before, "keep.go"); got != "package a\n\nvar Keep = 1\n" {
		t.Errorf("before keep.go = %q", got)
	}
}

func TestResolveDiffPlanGitStaged(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n\nvar X = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	write(t, dir, "a.go", "package a\n\nvar X = 2\n")
	runGit(t, dir, "add", "a.go")
	// a further unstaged edit must not appear on the staged after side.
	write(t, dir, "a.go", "package a\n\nvar X = 3\n")

	plan, err := ResolveDiffPlan(context.Background(), dir, "staged")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if plan.Label != "staged" || !plan.Symbolic {
		t.Fatalf("plan = %+v, want staged/symbolic", plan)
	}
	if got, want := plan.Files, []string{"a.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := blob(t, plan.Before, "a.go"); got != "package a\n\nvar X = 1\n" {
		t.Errorf("before = %q", got)
	}
	if got := blob(t, plan.After, "a.go"); got != "package a\n\nvar X = 2\n" {
		t.Errorf("after (staged) = %q, want the staged blob, not the worktree", got)
	}
}

func TestResolveDiffPlanGitRangeAndBareRef(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n\nvar X = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c1")
	write(t, dir, "a.go", "package a\n\nvar X = 2\n")
	write(t, dir, "b.go", "package a\n\nvar Y = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c2")

	// range HEAD~1..HEAD: committed endpoints, worktree untouched.
	plan, err := ResolveDiffPlan(context.Background(), dir, "HEAD~1..HEAD")
	if err != nil {
		t.Fatalf("range plan: %v", err)
	}
	if got, want := sortedFiles(plan), []string{"a.go", "b.go"}; !slices.Equal(got, want) {
		t.Fatalf("range files = %v, want %v", got, want)
	}
	if got := blob(t, plan.Before, "a.go"); got != "package a\n\nvar X = 1\n" {
		t.Errorf("range before a.go = %q", got)
	}
	if got := blob(t, plan.After, "a.go"); got != "package a\n\nvar X = 2\n" {
		t.Errorf("range after a.go = %q", got)
	}
	if got := blob(t, plan.Before, "b.go"); got != "" {
		t.Errorf("range before b.go = %q, want empty (added at c2)", got)
	}

	// bare ref: HEAD~1 vs the current worktree.
	write(t, dir, "a.go", "package a\n\nvar X = 9\n")
	bare, err := ResolveDiffPlan(context.Background(), dir, "HEAD~1")
	if err != nil {
		t.Fatalf("bare plan: %v", err)
	}
	if got := blob(t, bare.After, "a.go"); got != "package a\n\nvar X = 9\n" {
		t.Errorf("bare after a.go = %q, want the worktree", got)
	}
	if got := blob(t, bare.Before, "a.go"); got != "package a\n\nvar X = 1\n" {
		t.Errorf("bare before a.go = %q", got)
	}
}

func TestResolveDiffPlanGitBogusRef(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	if _, err := ResolveDiffPlan(context.Background(), dir, "no-such-ref..HEAD"); err == nil {
		t.Fatal("want error for a bogus range endpoint")
	}
}

const modV1 = "package a\n\nfunc Alpha() int { return 1 }\nfunc Beta() int { return 2 }\nfunc Gamma() int { return 3 }\nfunc Delta() int { return 4 }\n"

const modV2 = "package a\n\nfunc Alpha() int { return 1 }\nfunc Beta() int { return 2 }\nfunc Gamma() int { return 3 }\nfunc Delta() int { return 40 }\n"

const cleanV1 = "package a\n\nfunc Foo() int { return 1 }\n"

// TestResolveDiffPlanGitRename proves a git working-tree rename renders both sides:
// a clean rename (clean.go → renamed.go) keeps Before == After under the new path
// so it classifies as zero symbol changes, and a rename-with-edits (mod.go →
// moved.go) reads the pre-image at the old path so the edit classifies. The old
// path's deletion never vanishes into an all-new destination.
func TestResolveDiffPlanGitRename(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "mod.go", modV1)
	write(t, dir, "clean.go", cleanV1)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	runGit(t, dir, "mv", "clean.go", "renamed.go") // clean rename, no edit
	runGit(t, dir, "mv", "mod.go", "moved.go")     // rename with a subsequent edit
	write(t, dir, "moved.go", modV2)

	plan, err := ResolveDiffPlan(context.Background(), dir, "uncommitted")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if got, want := sortedFiles(plan), []string{"moved.go", "renamed.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v (rename sources fold into the destination)", got, want)
	}
	if plan.Renames["renamed.go"] != "clean.go" {
		t.Errorf("Renames[renamed.go] = %q, want clean.go", plan.Renames["renamed.go"])
	}
	if plan.Renames["moved.go"] != "mod.go" {
		t.Errorf("Renames[moved.go] = %q, want mod.go", plan.Renames["moved.go"])
	}
	// Clean rename: Before reads the old blob, After the (identical) worktree file.
	if got := blob(t, plan.Before, "renamed.go"); got != cleanV1 {
		t.Errorf("before renamed.go = %q, want the old clean.go blob", got)
	}
	if got := blob(t, plan.After, "renamed.go"); got != cleanV1 {
		t.Errorf("after renamed.go = %q, want the worktree content", got)
	}
	// Rename with edits: Before is the pre-image at the old path, After the edit.
	if got := blob(t, plan.Before, "moved.go"); got != modV1 {
		t.Errorf("before moved.go = %q, want the old mod.go blob", got)
	}
	if got := blob(t, plan.After, "moved.go"); got != modV2 {
		t.Errorf("after moved.go = %q, want the edited content", got)
	}
}

// TestResolveDiffPlanJJRename is the jj colocated analogue of the git rename test,
// parsing jj's compact "R <prefix>{old => new}<suffix>" summary. It skips when jj
// or git is absent.
func TestResolveDiffPlanJJRename(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runJJ(t, dir, "git", "init", "--colocate")
	runJJ(t, dir, "config", "set", "--repo", "user.email", "t@example.com")
	runJJ(t, dir, "config", "set", "--repo", "user.name", "Test")
	write(t, dir, "mod.go", modV1)
	write(t, dir, "clean.go", cleanV1)
	runJJ(t, dir, "commit", "-m", "init")

	mustRename(t, dir, "clean.go", "renamed.go") // clean rename
	mustRename(t, dir, "mod.go", "moved.go")     // rename with a subsequent edit
	write(t, dir, "moved.go", modV2)

	plan, err := ResolveDiffPlan(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if got, want := sortedFiles(plan), []string{"moved.go", "renamed.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if plan.Renames["renamed.go"] != "clean.go" {
		t.Errorf("Renames[renamed.go] = %q, want clean.go", plan.Renames["renamed.go"])
	}
	if plan.Renames["moved.go"] != "mod.go" {
		t.Errorf("Renames[moved.go] = %q, want mod.go", plan.Renames["moved.go"])
	}
	if got := blob(t, plan.Before, "renamed.go"); got != cleanV1 {
		t.Errorf("before renamed.go = %q, want the old clean.go blob", got)
	}
	if got := blob(t, plan.Before, "moved.go"); got != modV1 {
		t.Errorf("before moved.go = %q, want the old mod.go blob", got)
	}
	if got := blob(t, plan.After, "moved.go"); got != modV2 {
		t.Errorf("after moved.go = %q, want the edited content", got)
	}
}

// mustRename moves old to newName inside dir, failing the test on error.
func mustRename(t *testing.T, dir, old, newName string) {
	t.Helper()
	if err := os.Rename(filepath.Join(dir, old), filepath.Join(dir, newName)); err != nil {
		t.Fatalf("rename %s → %s: %v", old, newName, err)
	}
}

// TestResolveDiffPlanJJ exercises the jj working-tree lane against a real colocated
// repo; it skips when jj is absent.
func TestResolveDiffPlanJJ(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runJJ(t, dir, "git", "init", "--colocate")
	runJJ(t, dir, "config", "set", "--repo", "user.email", "t@example.com")
	runJJ(t, dir, "config", "set", "--repo", "user.name", "Test")
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\n")
	runJJ(t, dir, "commit", "-m", "init")
	// mutate the working copy (@ vs @-).
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 2 }\n")

	plan, err := ResolveDiffPlan(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if !plan.Symbolic || plan.Label != "uncommitted" {
		t.Fatalf("plan = %+v, want symbolic/uncommitted", plan)
	}
	if got, want := plan.Files, []string{"a.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := blob(t, plan.Before, "a.go"); got != "package a\n\nfunc Foo() int { return 1 }\n" {
		t.Errorf("before a.go = %q", got)
	}
	if got := blob(t, plan.After, "a.go"); got != "package a\n\nfunc Foo() int { return 2 }\n" {
		t.Errorf("after a.go = %q", got)
	}
}

func TestResolveDiffPlanJJColocatedGitSyntax(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runJJ(t, dir, "git", "init", "--colocate")
	runJJ(t, dir, "config", "set", "--repo", "user.email", "t@example.com")
	runJJ(t, dir, "config", "set", "--repo", "user.name", "Test")
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\n")
	runJJ(t, dir, "commit", "-m", "one")
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 2 }\n")
	runJJ(t, dir, "commit", "-m", "two")

	// jj rejects HEAD~1/HEAD outright; a colocated repo resolves them via git.
	tests := []struct {
		name   string
		source string
	}{
		{"git range", "HEAD~1..HEAD"},
		{"git ref vs working", "HEAD~1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := ResolveDiffPlan(context.Background(), dir, tt.source)
			if err != nil {
				t.Fatalf("ResolveDiffPlan(%q): %v", tt.source, err)
			}
			if !plan.Symbolic {
				t.Fatalf("plan = %+v, want symbolic", plan)
			}
			if got, want := plan.Files, []string{"a.go"}; !slices.Equal(got, want) {
				t.Fatalf("files = %v, want %v", got, want)
			}
			if got := blob(t, plan.Before, "a.go"); got != "package a\n\nfunc Foo() int { return 1 }\n" {
				t.Errorf("before a.go = %q", got)
			}
		})
	}
}

func runJJ(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...) //nolint:gosec // fixed jj verb; dir is a test TempDir and args are literals
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
	return string(out)
}
