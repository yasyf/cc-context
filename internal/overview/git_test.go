package overview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// commitlessRepo leaves HEAD unborn by deleting the fixture's only branch ref,
// the state `git init` leaves and vcstest.Repo's own commit moves past.
func commitlessRepo(t *testing.T) string {
	t.Helper()
	dir := vcstest.Repo(t).Dir
	mustGit(t, dir, "update-ref", "-d", "refs/heads/main")
	return dir
}

// mustGit runs git in dir through the same runner the production probes use and
// returns its trimmed stdout, failing the test on a nonzero exit.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := render.RunCLIEnvDir(context.Background(), dir, "git", args, nil)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimRight(out, "\n")
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// zwjName carries a zero-width joiner, the byte class git quotes without -z; newline
// and quote names cover the other two escapes.
const (
	zwjName     = "zwj\u200djoin.go"
	newlineName = "new\nline.go"
	quoteName   = `quote"name.go`
)

func TestGitSection(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	write(t, dir, "keep.go", "package a\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "release: v0.22.0")
	hash := mustGit(t, dir, "log", "-1", "--format=%h")

	got, err := gitSection(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("git: main @ %s %q · 2 commits", hash, "release: v0.22.0")
	if got != want {
		t.Errorf("gitSection = %q, want %q", got, want)
	}
}

// TestGitSectionCountsDirtyEntries pins the count against a rename, whose
// porcelain entry spends a second token on the origin path, and against the three
// filenames git quotes without -z.
func TestGitSectionCountsDirtyEntries(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "old.go", "package a\n")
	write(t, dir, "mod.go", "package a\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "init")
	mustGit(t, dir, "mv", "old.go", "new.go")
	write(t, dir, "mod.go", "package a\n\nvar X = 1\n")
	write(t, dir, zwjName, "package a\n")
	write(t, dir, newlineName, "package a\n")
	write(t, dir, quoteName, "package a\n")
	hash := mustGit(t, dir, "log", "-1", "--format=%h")

	got, err := gitSection(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("git: main @ %s %q · 5 dirty · 2 commits", hash, "init")
	if got != want {
		t.Errorf("gitSection = %q, want %q", got, want)
	}
}

func TestGitSectionDetachedHeadDropsBranch(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "wip")
	mustGit(t, dir, "checkout", "-q", "--detach")
	hash := mustGit(t, dir, "log", "-1", "--format=%h")

	got, err := gitSection(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("git: @ %s %q · 2 commits", hash, "wip")
	if got != want {
		t.Errorf("gitSection = %q, want %q", got, want)
	}
}

func TestGitSectionNoCommits(t *testing.T) {
	dir := commitlessRepo(t)
	got, err := gitSection(context.Background(), dir)
	if err != nil {
		t.Fatalf("gitSection on a commitless repo: %v", err)
	}
	if got != "" {
		t.Errorf("gitSection with no commits = %q, want \"\"", got)
	}
}

// TestGitSectionSurfacesStatusFailure pins the segment that used to vanish: a bare
// repo answers log, rev-parse and rev-list but refuses status, and the section must
// report that rather than render a clean-looking line with no dirty count.
func TestGitSectionSurfacesStatusFailure(t *testing.T) {
	src := vcstest.Repo(t).Dir
	write(t, src, "a.go", "package a\n")
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-qm", "init")
	bare := filepath.Join(t.TempDir(), "bare.git")
	mustGit(t, src, "clone", "-q", "--bare", src, bare)

	got, err := gitSection(context.Background(), bare)
	if err == nil {
		t.Fatalf("gitSection on a bare repo = %q, want an error naming the status failure", got)
	}
	if !strings.Contains(err.Error(), "must be run in a work tree") {
		t.Errorf("gitSection error = %v, want git's work-tree refusal", err)
	}
}

// TestGitSectionSurfacesLogFailure pins the swallow the commitless case used to
// hide: a repo whose HEAD ref still resolves but whose object store is gone
// answers rev-parse and fails log, and that is a failure to report, not the
// repository having no commits yet.
func TestGitSectionSurfacesLogFailure(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	write(t, dir, "a.go", "package a\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "init")
	// The objects directory itself has to stay: without it git stops recognizing
	// the directory as a repository at all, which is a different failure.
	objects := filepath.Join(dir, ".git", "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(objects, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	got, sectionErr := gitSection(context.Background(), dir)
	if sectionErr == nil {
		t.Fatalf("gitSection over a repo with no object store = %q, want an error", got)
	}
	if !strings.Contains(sectionErr.Error(), "bad object") {
		t.Errorf("gitSection error = %v, want git's bad-object failure", sectionErr)
	}
}

// TestGitLinesNoCommits pins the pairing: the churn probe fails on a
// commitless repo exactly where the state probe does, so neither line is attempted.
func TestGitLinesNoCommits(t *testing.T) {
	dir := commitlessRepo(t)
	lines, err := gitLines(context.Background(), dir)
	if err != nil {
		t.Fatalf("gitLines on a commitless repo: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("gitLines with no commits = %q, want none", lines)
	}
}

// TestHotLine pins the churn aggregation against the three filenames git quotes
// without -z: unquoted they all attribute to internal/cli, and a leading '"' never
// reaches a directory key.
func TestHotLine(t *testing.T) {
	dir := vcstest.Repo(t).Dir
	for _, p := range []string{
		"internal/cli/a.go",
		"internal/cli/" + zwjName,
		"internal/cli/" + newlineName,
		"internal/cli/" + quoteName,
		"internal/web/c.go",
		"cmd/ccx/main.go",
		"README.md",
	} {
		write(t, dir, p, "x\n")
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "init")

	// internal/cli leads at 4; cmd/ccx and internal/web tie at 1 → name-ascending;
	// the root-level README.md is not attributable to a dir and is dropped.
	want := "hot (90d): internal/cli (4), cmd/ccx (1), internal/web (1)"
	got, err := hotLine(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("hotLine = %q, want %q", got, want)
	}
}

// TestHotLineSurfacesLogFailure pins the other half of the segment bug: a churn
// probe that cannot answer is an error, not an omitted line.
func TestHotLineSurfacesLogFailure(t *testing.T) {
	dir := commitlessRepo(t)
	got, err := hotLine(context.Background(), dir)
	if err == nil {
		t.Fatalf("hotLine on a commitless repo = %q, want an error", got)
	}
}

func TestHotKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{"internal/cli/foo.go", "internal/cli"},
		{"internal/cli/sub/foo.go", "internal/cli"},
		{"cmd/ccx/main.go", "cmd/ccx"},
		{"docs/x.md", "docs"},
		{"README.md", ""},
		{"./internal/web/a.go", "internal/web"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := hotKey(tt.path); got != tt.want {
				t.Errorf("hotKey(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
