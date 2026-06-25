package render

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSupplementDiff(t *testing.T) {
	const commentOnly = "# Diff: uncommitted — 1 file\n\n## c/a.go w/a.go (0 symbols)\n"
	const withBody = "# Diff\n\n## c/a.go w/a.go (2 symbols)\n+func Added() {}\n"
	const zeroButBodied = "# Diff\n\n## c/a.go w/a.go (0 symbols)\n@@ -1 +1 @@\n-old\n+new\n"
	const twoFiles = "# Diff\n\n## c/a.go w/a.go (0 symbols)\n\n## c/b.go w/b.go (3 symbols)\n+func B() {}\n"

	hunk := func(want string) func(string) (string, error) {
		return func(string) (string, error) { return want, nil }
	}

	tests := []struct {
		name       string
		in         string
		fetch      func(string) (string, error)
		wantSub    []string
		wantAbsent []string
	}{
		{
			name:    "comment-only zero-symbol section gets the raw hunk",
			in:      commentOnly,
			fetch:   hunk("@@ -3 +3 @@\n-// old\n+// new\n"),
			wantSub: []string{"## c/a.go w/a.go (0 symbols)", "-// old", "+// new"},
		},
		{
			name:       "section with a body is left untouched (no double-print)",
			in:         withBody,
			fetch:      hunk("SHOULD NOT APPEAR"),
			wantSub:    []string{"+func Added() {}"},
			wantAbsent: []string{"SHOULD NOT APPEAR"},
		},
		{
			name:       "zero symbols but already has a hunk body is not re-fetched",
			in:         zeroButBodied,
			fetch:      hunk("SHOULD NOT APPEAR"),
			wantSub:    []string{"@@ -1 +1 @@", "-old", "+new"},
			wantAbsent: []string{"SHOULD NOT APPEAR"},
		},
		{
			name:       "only the empty zero-symbol section is supplemented in a mixed diff",
			in:         twoFiles,
			fetch:      hunk("@@ A-HUNK @@"),
			wantSub:    []string{"## c/a.go w/a.go (0 symbols)", "@@ A-HUNK @@", "+func B() {}"},
			wantAbsent: []string{"@@ A-HUNK @@\n+func B"},
		},
		{
			name:       "fetch returning empty leaves the section as-is",
			in:         commentOnly,
			fetch:      hunk(""),
			wantSub:    []string{"## c/a.go w/a.go (0 symbols)"},
			wantAbsent: []string{"@@"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SupplementDiff(tt.in, tt.fetch)
			if err != nil {
				t.Fatalf("SupplementDiff() unexpected err: %v", err)
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("output missing %q\n--- got ---\n%s", sub, got)
				}
			}
			for _, sub := range tt.wantAbsent {
				if strings.Contains(got, sub) {
					t.Errorf("output unexpectedly contains %q\n--- got ---\n%s", sub, got)
				}
			}
		})
	}
}

func TestSupplementDiffFetchErrorPropagates(t *testing.T) {
	in := "## c/a.go w/a.go (0 symbols)\n"
	boom := errors.New("git exploded")
	_, err := SupplementDiff(in, func(string) (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Fatalf("SupplementDiff() err = %v, want it to wrap %v", err, boom)
	}
}

// TestRunDiffCLISupplementsRawHunk proves RunDiffCLI splices a raw git hunk into an empty "(0 symbols)" section.
func TestRunDiffCLISupplementsRawHunk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	ctx := context.Background()
	repo := initGitRepo(t)
	t.Chdir(repo)

	writeFile(t, filepath.Join(repo, "a.go"), "package main\n\n// Greet now warmly.\nfunc Greet() {}\n")

	tilth := writeFakeBin(t, "tilth",
		"#!/bin/sh\nprintf '# Diff: uncommitted\\n\\n## c/a.go w/a.go (0 symbols)\\n'\n")

	got, err := RunDiffCLI(ctx, tilth, []string{"diff"}, "uncommitted", 0)
	if err != nil {
		t.Fatalf("RunDiffCLI() err: %v", err)
	}
	for _, want := range []string{
		"## c/a.go w/a.go (0 symbols)",
		"-// Greet greets.",
		"+// Greet now warmly.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestRunDiffCLILeavesBodiedSectionAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	ctx := context.Background()
	repo := initGitRepo(t)
	t.Chdir(repo)
	writeFile(t, filepath.Join(repo, "a.go"), "package main\n\nfunc Greet() {}\nfunc Added() {}\n")

	// tilth already emits a body here; the supplement must not run git diff (no `@@` marker may appear).
	tilth := writeFakeBin(t, "tilth",
		"#!/bin/sh\nprintf '# Diff\\n\\n## c/a.go w/a.go (1 symbols)\\n+func Added() {}\\n'\n")

	got, err := RunDiffCLI(ctx, tilth, []string{"diff"}, "uncommitted", 0)
	if err != nil {
		t.Fatalf("RunDiffCLI() err: %v", err)
	}
	if strings.Contains(got, "@@") {
		t.Errorf("bodied section was double-printed with a raw hunk:\n%s", got)
	}
	if !strings.Contains(got, "+func Added() {}") {
		t.Errorf("output missing tilth's own body:\n%s", got)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t.t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n\n// Greet greets.\nfunc Greet() {}\n")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func writeFakeBin(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake binary must be owner-executable
		t.Fatalf("write fake %q: %v", name, err)
	}
	return path
}

func TestSupplementDiffNoHeadersPassesThrough(t *testing.T) {
	in := "diff error: file not found in diff\n"
	got, err := SupplementDiff(in, func(string) (string, error) {
		t.Fatal("fetch must not be called when there are no file headers")
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != in {
		t.Fatalf("passthrough mismatch: got %q want %q", got, in)
	}
}
