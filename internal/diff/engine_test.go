package diff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

// e2eGit runs a git command in dir with the developer's ambient config detached,
// so a global commit.gpgsign cannot break the scripted commits.
func e2eGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git verb; dir is a test TempDir and args are literals
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func e2eWrite(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunE2E scripts a real git repo — a Go file (symbol changes), a plain-text
// file (raw hunks), and a binary — then drives Run end to end, exercising the
// ast-grep blob outlining and the full render. It needs git and ast-grep.
func TestRunE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}

	dir := t.TempDir()
	e2eGit(t, dir, "init", "-q")
	e2eGit(t, dir, "config", "user.email", "t@example.com")
	e2eGit(t, dir, "config", "user.name", "Test")

	before := "package sample\n\nimport \"errors\"\n\nfunc Run(ctx int) error {\n\treturn errors.New(\"todo\")\n}\n\nfunc Removed() {\n\tprintln(\"bye\")\n}\n"
	e2eWrite(t, dir, "a.go", []byte(before))
	e2eWrite(t, dir, "notes.txt", []byte("alpha\n"))
	e2eWrite(t, dir, "logo.bin", []byte("\x00\x01\x02BIN\x00\x00payload"))
	e2eGit(t, dir, "add", "-A")
	e2eGit(t, dir, "commit", "-qm", "init")

	after := "package sample\n\nimport \"errors\"\n\nfunc Run(ctx int) error {\n\treturn nil\n}\n\nfunc Added() int {\n\treturn 42\n}\n"
	e2eWrite(t, dir, "a.go", []byte(after))
	e2eWrite(t, dir, "notes.txt", []byte("alpha beta\n"))
	e2eWrite(t, dir, "logo.bin", []byte("\x00\x01\x02BINX\x00\x00payload2"))

	t.Chdir(dir)
	out, _, err := Run(context.Background(), backend.Args{Source: "uncommitted", Full: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("\n%s", out)

	for _, want := range []string{
		"# diff uncommitted — 3 files",
		"## a.go",
		"[+] Added",
		"[~] Run",
		"[−] Removed",
		"## logo.bin — binary",
		"## notes.txt — no ast-grep rules for .txt; raw hunks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRunRename scripts a real git repo with a clean rename and a rename-with-edits,
// then drives Run end to end: the clean rename renders as "old → new — renamed, no
// content change" and the edited rename shows the changed symbol under "old → new",
// never a vanished deletion or an all-new destination.
func TestRunRename(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}

	dir := t.TempDir()
	e2eGit(t, dir, "init", "-q")
	e2eGit(t, dir, "config", "user.email", "t@example.com")
	e2eGit(t, dir, "config", "user.name", "Test")

	mod := "package a\n\nfunc Alpha() int { return 1 }\nfunc Beta() int { return 2 }\nfunc Gamma() int { return 3 }\nfunc Delta() int { return 4 }\n"
	e2eWrite(t, dir, "mod.go", []byte(mod))
	e2eWrite(t, dir, "clean.go", []byte("package a\n\nfunc Foo() int { return 1 }\n"))
	e2eGit(t, dir, "add", "-A")
	e2eGit(t, dir, "commit", "-qm", "init")

	e2eGit(t, dir, "mv", "clean.go", "renamed.go")
	e2eGit(t, dir, "mv", "mod.go", "moved.go")
	e2eWrite(t, dir, "moved.go", []byte(strings.Replace(mod, "return 4 }", "return 40 }", 1)))

	t.Chdir(dir)
	out, _, err := Run(context.Background(), backend.Args{Source: "uncommitted"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("\n%s", out)

	for _, want := range []string{
		"## clean.go → renamed.go — renamed, no content change",
		"## mod.go → moved.go (~1)",
		"[~] Delta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "## renamed.go\n") || strings.Contains(out, "## moved.go\n") {
		t.Errorf("rename destination rendered without its source arrow\n---\n%s", out)
	}
}

// TestChangedSymbols verifies the sigil list the history summary consumes.
func TestChangedSymbols(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}

	dir := t.TempDir()
	e2eGit(t, dir, "init", "-q")
	e2eGit(t, dir, "config", "user.email", "t@example.com")
	e2eGit(t, dir, "config", "user.name", "Test")
	e2eWrite(t, dir, "a.go", []byte("package a\n\nfunc Run() {}\n\nfunc Gone() {}\n"))
	e2eGit(t, dir, "add", "-A")
	e2eGit(t, dir, "commit", "-qm", "c1")
	e2eWrite(t, dir, "a.go", []byte("package a\n\nfunc Run(x int) {}\n\nfunc New() {}\n"))
	e2eGit(t, dir, "add", "-A")
	e2eGit(t, dir, "commit", "-qm", "c2")

	syms, err := ChangedSymbols(context.Background(), dir, "HEAD~1..HEAD", "a.go")
	if err != nil {
		t.Fatalf("ChangedSymbols: %v", err)
	}
	got := strings.Join(syms, " ")
	for _, want := range []string{"~Run", "+New", "-Gone"} {
		if !strings.Contains(got, want) {
			t.Errorf("changed symbols %q missing %q", got, want)
		}
	}
}
