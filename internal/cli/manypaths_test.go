package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/workspace"
)

func runManyPaths(t *testing.T, dir string, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(workspace.WithRoot(t.Context(), dir))
	return out.String(), err
}

func eachPathOutput(t *testing.T, dir string, newCmd func() *cobra.Command, paths []string, flags ...string) string {
	t.Helper()
	var want strings.Builder
	for _, path := range paths {
		out, err := runManyPaths(t, dir, newCmd(), append([]string{path}, flags...)...)
		if err != nil {
			t.Fatalf("%s alone: %v\n%s", path, err, out)
		}
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		want.WriteString(out)
	}
	return want.String()
}

func writeManyPaths(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadTakesSeveralPathsUnderOneBudget(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeManyPaths(t, dir, map[string]string{"big.txt": strings.Repeat("a long line that fills the budget\n", 200), "small.txt": "the small file\n"})

	got, err := runManyPaths(t, dir, newReadCmd(), "big.txt", "small.txt", "--full", "--budget", "400")
	if err != nil {
		t.Fatalf("read big.txt small.txt: %v\n%s", err, got)
	}
	if want := eachPathOutput(t, dir, newReadCmd, []string{"big.txt", "small.txt"}, "--full", "--budget", "200"); got != want {
		t.Errorf("read big.txt small.txt --budget 400 =\n%s\nwant each file read under half of it:\n%s", got, want)
	}
	if !strings.Contains(got, "the small file") {
		t.Errorf("big.txt spent the budget small.txt shares:\n%s", got)
	}

	if _, err := runManyPaths(t, dir, newReadCmd(), "big.txt", "small.txt", "--section", "1-2"); err == nil || !strings.Contains(err.Error(), "--section names lines of one file") {
		t.Errorf("read --section over two paths = %v, want a refusal", err)
	}
}

func TestOutlineTakesSeveralPathsUnderOneBudget(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeManyPaths(t, dir, map[string]string{"a.md": "# Alpha heading\n\ntext\n", "b.md": "# Beta heading\n\ntext\n"})

	got, err := runManyPaths(t, dir, newOutlineCmd(), "a.md", "b.md", "--budget", "400")
	if err != nil {
		t.Fatalf("outline a.md b.md: %v\n%s", err, got)
	}
	if want := eachPathOutput(t, dir, newOutlineCmd, []string{"a.md", "b.md"}, "--budget", "200"); got != want {
		t.Errorf("outline a.md b.md =\n%s\nwant each outlined under half the budget:\n%s", got, want)
	}
	if !strings.Contains(got, "Alpha heading") || !strings.Contains(got, "Beta heading") {
		t.Errorf("outline a.md b.md = %q, want both files' headings", got)
	}
}

func TestDepsTakesSeveralPathsUnderOneBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ast-grep script is POSIX-only")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	writeManyPaths(t, dir, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	bin := t.TempDir()
	execstub.Write(t, filepath.Join(bin, "ast-grep"), `#!/bin/sh
if [ "$1" = "--version" ]; then echo "ast-grep 0.44.0"; exit 0; fi
echo '{"path":"a.go","language":"Go","items":[]}'
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := runManyPaths(t, dir, newDepsCmd(), "a.go", "b.go")
	if err != nil {
		t.Fatalf("deps a.go b.go: %v\n%s", err, got)
	}
	if want := eachPathOutput(t, dir, newDepsCmd, []string{"a.go", "b.go"}, "--budget", "1000"); got != want {
		t.Errorf("deps a.go b.go =\n%s\nwant each under half the default budget:\n%s", got, want)
	}
	if !strings.Contains(got, "# deps a.go") || !strings.Contains(got, "# deps b.go") {
		t.Errorf("deps a.go b.go = %q, want a report for each", got)
	}
}

func TestHistoryTakesSeveralPaths(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "a.txt", "alpha\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "add a")
	historyWrite(t, dir, "b.txt", "a different file entirely, so --follow finds no copy of a.txt\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "add b")

	want := "# history a.txt\n" + historyBlock(t, dir, "HEAD~1", "add a", "(added)") +
		"# history b.txt\n" + historyBlock(t, dir, "HEAD", "add b", "(+1/-0)")
	if got := historyRun(t, "a.txt", "b.txt"); got != want {
		t.Fatalf("history a.txt b.txt =\n%q\nwant\n%q", got, want)
	}
}
