package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// unquotableNames are the three names git cannot print outside a -z stream
// without C-quoting them: a zero-width joiner, an embedded newline, and a double
// quote. The newline is the sharp one — a listing split on newlines reads it as
// two files, neither of which exists.
var unquotableNames = []string{"zwj\u200djoin.go", "new\nline.go", "quote\"name.go"}

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
	dir := vcstest.Repo(t).Dir
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

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "uncommitted")
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
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n\nvar X = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	write(t, dir, "a.go", "package a\n\nvar X = 2\n")
	runGit(t, dir, "add", "a.go")
	// a further unstaged edit must not appear on the staged after side.
	write(t, dir, "a.go", "package a\n\nvar X = 3\n")

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "staged")
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
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n\nvar X = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c1")
	write(t, dir, "a.go", "package a\n\nvar X = 2\n")
	write(t, dir, "b.go", "package a\n\nvar Y = 1\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c2")

	// range HEAD~1..HEAD: committed endpoints, worktree untouched.
	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "HEAD~1..HEAD")
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
	bare, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "HEAD~1")
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
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	if _, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "no-such-ref..HEAD"); err == nil {
		t.Fatal("want error for a bogus range endpoint")
	}
}

// TestResolveDiffPlanGitRefusesOptionInjection drives a source spelled like a git
// option through every branch of the diff-source switch. `git diff --output=FILE`
// writes FILE, so an endpoint reaching git's flag surface is an arbitrary file
// write: `ccx vcs diff -- '--output=/tmp/x'` created /tmp/x holding the name-status
// listing. The last case proves the refusal is structural rather than a property
// of the rev-parse gate ahead of it — handed the option directly, the argv builder
// still puts --end-of-options in front of it.
func TestResolveDiffPlanGitRefusesOptionInjection(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c1")
	write(t, dir, "a.go", "package a\n\nvar X = 1\n")
	runGit(t, dir, "commit", "-qam", "c2")

	// shape places the option-shaped endpoint into one branch of the switch; each
	// case gets its own target, so a file one case writes cannot mask another.
	tests := []struct {
		name  string
		shape string
	}{
		{"bare ref", "%s"},
		{"range left endpoint", "%s..HEAD"},
		{"range right endpoint", "HEAD~1..%s"},
		{"symmetric range", "%s...HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "pwned.txt")
			source := fmt.Sprintf(tt.shape, "--output="+target)
			_, err := ResolveDiffPlan(context.Background(), render.Dir(dir), source)
			assertUnwritten(t, target)
			if err == nil {
				t.Fatalf("ResolveDiffPlan(%q) = nil error, want a refusal", source)
			}
			// rev-parse calls the option invalid only because --end-of-options
			// precedes it there too; drop that and the endpoint validates.
			if !strings.Contains(err.Error(), "unknown git revision") {
				t.Errorf("error = %v, want the endpoint refused as a revision", err)
			}
		})
	}

	t.Run("past the validation gate", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "pwned.txt")
		_, _, err := gitDiffFiles(context.Background(), GitArgs{
			Dir:  render.Dir(dir),
			Sub:  []string{"diff", "-M"},
			Revs: []GitRef{UnsafeRef("--output=" + target)},
		})
		assertUnwritten(t, target)
		if err == nil {
			t.Fatal("gitDiffFiles accepted an option-shaped rev")
		}
	})
}

// assertUnwritten fails the test when git created path, the observable an argv
// assertion alone cannot make.
func assertUnwritten(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git wrote %q (stat err %v)", path, err)
	}
}

// TestResolveDiffPlanGitUnquotableNames is the git analogue of the jj ZWJ test.
// Outside a -z stream git renders these names C-quoted, so a listing split on
// newlines hands back a leading '"' glued to the name — a path no blob accessor
// resolves, which renders a modification as a whole-file addition — and splits the
// newline name into two entries that name no file at all.
func TestResolveDiffPlanGitUnquotableNames(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	for _, name := range unquotableNames {
		write(t, dir, name, "one\ntwo\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	for _, name := range unquotableNames {
		write(t, dir, name, "one\nTWO CHANGED\n")
	}
	write(t, dir, "untracked\nname.go", "brand new\n")

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "uncommitted")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	want := append([]string{"untracked\nname.go"}, unquotableNames...)
	sort.Strings(want)
	if got := sortedFiles(plan); !slices.Equal(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
	for _, name := range unquotableNames {
		if got := blob(t, plan.Before, name); got != "one\ntwo\n" {
			t.Errorf("before %q = %q, want the committed blob — an empty base renders a modification as a whole-file addition", name, got)
		}
		if got := blob(t, plan.After, name); got != "one\nTWO CHANGED\n" {
			t.Errorf("after %q = %q", name, got)
		}
	}
	if got := blob(t, plan.Before, "untracked\nname.go"); got != "" {
		t.Errorf("before the untracked file = %q, want empty", got)
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
	dir := vcstest.Repo(t).Dir
	write(t, dir, "mod.go", modV1)
	write(t, dir, "clean.go", cleanV1)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	runGit(t, dir, "mv", "clean.go", "renamed.go") // clean rename, no edit
	runGit(t, dir, "mv", "mod.go", "moved.go")     // rename with a subsequent edit
	write(t, dir, "moved.go", modV2)

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "uncommitted")
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
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
	write(t, dir, "mod.go", modV1)
	write(t, dir, "clean.go", cleanV1)
	runJJ(t, dir, "commit", "-m", "init")

	mustRename(t, dir, "clean.go", "renamed.go") // clean rename
	mustRename(t, dir, "mod.go", "moved.go")     // rename with a subsequent edit
	write(t, dir, "moved.go", modV2)

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "")
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
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\n")
	runJJ(t, dir, "commit", "-m", "init")
	// mutate the working copy (@ vs @-).
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 2 }\n")

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "")
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
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
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
			plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), tt.source)
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

// zwjPath names a file whose grapheme cluster carries U+200D zero-width joiners.
// Go's %q spells those \u200d, an escape jj 0.43's fileset grammar has no reading
// for, so the pattern fails to parse — and a failure read as "absent from the base"
// renders the modified file as a whole-file addition.
const zwjPath = "\U0001F468\u200D\U0001F469\u200D\U0001F466.txt"

func TestResolveDiffPlanJJZWJPath(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
	write(t, dir, "plain.txt", "one\ntwo\n")
	write(t, dir, zwjPath, "one\ntwo\n")
	runJJ(t, dir, "commit", "-m", "init")
	write(t, dir, "plain.txt", "one\nTWO CHANGED\n")
	write(t, dir, zwjPath, "one\nTWO CHANGED\n")
	write(t, dir, "added.txt", "brand new\n")

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "")
	if err != nil {
		t.Fatalf("ResolveDiffPlan: %v", err)
	}
	if !plan.Symbolic {
		t.Fatalf("plan = %+v, want symbolic", plan)
	}
	if got, want := sortedFiles(plan), []string{"added.txt", "plain.txt", zwjPath}; !slices.Equal(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
	if got, want := blob(t, plan.Before, zwjPath), "one\ntwo\n"; got != want {
		t.Errorf("before %q = %q, want %q — an empty base renders a modification as a whole-file addition", zwjPath, got, want)
	}
	if got, want := blob(t, plan.After, zwjPath), "one\nTWO CHANGED\n"; got != want {
		t.Errorf("after %q = %q, want %q", zwjPath, got, want)
	}
	if got := blob(t, plan.Before, "added.txt"); got != "" {
		t.Errorf("before added.txt = %q, want empty — a path genuinely absent from @- stays absent", got)
	}
}

// masterTrunkJJ stands up a colocated jj repo whose only branch is master,
// carrying one committed revision of a.go and an uncommitted edit on top, so a
// source naming main has nothing to resolve to and a rewrite cannot hide.
func masterTrunkJJ(t *testing.T, opts ...vcstest.Opt) string {
	t.Helper()
	dir := vcstest.Repo(t, append([]vcstest.Opt{vcstest.JJ(), vcstest.Trunk("master")}, opts...)...).Dir
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\n")
	runJJ(t, dir, "commit", "-m", "a.go v1")
	runJJ(t, dir, "bookmark", "set", "master", "-r", "@-")
	write(t, dir, "a.go", "package a\n\nfunc Foo() int { return 2 }\n")
	return dir
}

// assertMasterAgainstWorking fails unless plan diffs the master revision against
// the live working copy.
func assertMasterAgainstWorking(t *testing.T, plan DiffPlan) {
	t.Helper()
	if !plan.Symbolic {
		t.Fatalf("plan = %+v, want symbolic", plan)
	}
	if got, want := plan.Files, []string{"a.go"}; !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := blob(t, plan.Before, "a.go"); got != "package a\n\nfunc Foo() int { return 1 }\n" {
		t.Errorf("before a.go = %q, want master's blob", got)
	}
	if got := blob(t, plan.After, "a.go"); got != "package a\n\nfunc Foo() int { return 2 }\n" {
		t.Errorf("after a.go = %q, want the live worktree", got)
	}
}

// TestResolveDiffPlanJJHonorsLiteralBranchNames pins the rewrite that used to
// happen underneath the user: master..@ was classified as "the default branch"
// and re-spelled as whatever branch this package guessed, so in a master-trunk
// repo it died with `Revision "main" doesn't exist` — naming a branch the user
// never typed. A name in the source is now the name that reaches jj.
func TestResolveDiffPlanJJHonorsLiteralBranchNames(t *testing.T) {
	dir := masterTrunkJJ(t)

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "master..@")
	if err != nil {
		t.Fatalf("ResolveDiffPlan(master..@): %v", err)
	}
	assertMasterAgainstWorking(t, plan)

	_, err = ResolveDiffPlan(context.Background(), render.Dir(dir), "main..@")
	if err == nil {
		t.Fatal("ResolveDiffPlan(main..@) succeeded in a repo with no main")
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("error = %v, want it to name the branch the user asked for", err)
	}
}

// TestResolveDiffPlanJJTrunkResolvesTheDesignatedBranch covers the one source
// that still consults the repository: trunk() reads refs/remotes/origin/HEAD, so
// a master-trunk repo resolves to master rather than to a fabricated main.
func TestResolveDiffPlanJJTrunkResolvesTheDesignatedBranch(t *testing.T) {
	dir := masterTrunkJJ(t, vcstest.Remote())

	plan, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "trunk()..@")
	if err != nil {
		t.Fatalf("ResolveDiffPlan(trunk()..@): %v", err)
	}
	assertMasterAgainstWorking(t, plan)
}

// TestResolveDiffPlanJJTrunkWithoutADefaultBranch pins the deleted guess: a
// repository designating no default branch used to be answered with "main"
// regardless, which surfaced as jj rejecting a branch nobody named. The miss is
// now ErrNoTrunk, carrying the command that fixes it.
func TestResolveDiffPlanJJTrunkWithoutADefaultBranch(t *testing.T) {
	dir := masterTrunkJJ(t)

	_, err := ResolveDiffPlan(context.Background(), render.Dir(dir), "trunk()..@")
	if !errors.Is(err, ErrNoTrunk) {
		t.Fatalf("ResolveDiffPlan(trunk()..@) error = %v, want ErrNoTrunk", err)
	}
	if strings.Contains(err.Error(), "main") {
		t.Errorf("error = %v, want no fabricated branch name in it", err)
	}
}

// TestTreeHasPathJJReportsFailure proves a jj probe that fails comes back as a
// failure rather than as "absent from the base", the swallow that hid the
// unparseable ZWJ fileset behind a phantom whole-file addition.
func TestTreeHasPathJJReportsFailure(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
	write(t, dir, "a.go", "package a\n")
	runJJ(t, dir, "commit", "-m", "init")

	ctx := context.Background()
	if _, err := treeHasPath(ctx, render.Dir(dir), JJ, "nosuchrev", "a.go"); err == nil {
		t.Error("treeHasPath at an unknown revision returned no error")
	}
	has, err := treeHasPath(ctx, render.Dir(dir), JJ, "@-", "a.go")
	if err != nil || !has {
		t.Errorf("treeHasPath(@-, a.go) = %v, %v; want true, nil", has, err)
	}
	has, err = treeHasPath(ctx, render.Dir(dir), JJ, "@-", "nope.go")
	if err != nil || has {
		t.Errorf("treeHasPath(@-, nope.go) = %v, %v; want false, nil", has, err)
	}
}

// TestTreeHasPathJJWhitespaceName proves a tracked name made entirely of
// whitespace still reads as present: `jj file list` prints that name and a
// newline, so trimming the listing before measuring it erases the file.
func TestTreeHasPathJJWhitespaceName(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.JJ()).Dir
	write(t, dir, " ", "one\n")
	runJJ(t, dir, "commit", "-m", "init")

	has, err := treeHasPath(context.Background(), render.Dir(dir), JJ, "@-", " ")
	if err != nil || !has {
		t.Errorf("treeHasPath(@-, %q) = %v, %v; want true, nil", " ", has, err)
	}
}

// TestTreeHasPathGitReportsFailure proves a git probe that fails comes back as a
// failure rather than as "absent from the base". `git cat-file -e` cannot tell the
// two apart — it exits 128 for a path the tree lacks and for a rev or object store
// it cannot read alike — so the probe has to enumerate to separate them.
func TestTreeHasPathGitReportsFailure(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	ctx := context.Background()
	if _, err := treeHasPath(ctx, render.Dir(dir), Git, "nosuchrev", "a.go"); err == nil {
		t.Error("treeHasPath at an unknown revision returned no error")
	}
	has, err := treeHasPath(ctx, render.Dir(dir), Git, "HEAD", "a.go")
	if err != nil || !has {
		t.Errorf("treeHasPath(HEAD, a.go) = %v, %v; want true, nil", has, err)
	}
	has, err = treeHasPath(ctx, render.Dir(dir), Git, "HEAD", "nope.go")
	if err != nil || has {
		t.Errorf("treeHasPath(HEAD, nope.go) = %v, %v; want false, nil", has, err)
	}
}

// TestTreeHasPathGitIndex covers the staged side, which lists the index instead of
// a tree: a conflicted path is carried at stages 1-3 with no stage-0 blob to read,
// a probed name carrying glob metacharacters has to match itself rather than the
// neighbors `git ls-files` would glob it onto, and a name git can only print
// C-quoted has to survive the framing intact — as the sole record of a clean
// entry and as the three records a conflict spreads it across.
func TestTreeHasPathGitIndex(t *testing.T) {
	conflicted := unquotableNames[1]
	dir := vcstest.Repo(t).Dir
	write(t, dir, "c.txt", "base\n")
	write(t, dir, "i.tsx", "i\n")
	for _, name := range unquotableNames {
		write(t, dir, name, "u\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "tag", "base")
	runGit(t, dir, "checkout", "-q", "-b", "side")
	write(t, dir, "c.txt", "side\n")
	write(t, dir, conflicted, "side\n")
	runGit(t, dir, "commit", "-qam", "side")
	runGit(t, dir, "checkout", "-q", "-")
	write(t, dir, "c.txt", "main\n")
	write(t, dir, conflicted, "main\n")
	runGit(t, dir, "commit", "-qam", "main")
	runGit(t, dir, "read-tree", "-m", "base", "HEAD", "side")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"stage 0 entry", "i.tsx", true},
		{"conflicted path carries no stage 0", "c.txt", false},
		{"glob metacharacters match literally", "[id].tsx", false},
		{"zero-width joiner", unquotableNames[0], true},
		{"embedded quote", unquotableNames[2], true},
		{"conflicted name with an embedded newline", conflicted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			has, err := treeHasPath(context.Background(), render.Dir(dir), Git, gitIndexRev, tt.path)
			if err != nil || has != tt.want {
				t.Errorf("treeHasPath(:0, %q) = %v, %v; want %v, nil", tt.path, has, err, tt.want)
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
