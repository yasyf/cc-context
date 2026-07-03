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

	"github.com/yasyf/cc-context/internal/anchor"
)

func TestSupplementDiff(t *testing.T) {
	const commentOnly = "# Diff: uncommitted — 1 file\n\n## c/a.go w/a.go (0 symbols)\n"
	const withBody = "# Diff\n\n## c/a.go w/a.go (2 symbols)\n+func Added() {}\n"
	const zeroButBodied = "# Diff\n\n## c/a.go w/a.go (0 symbols)\n@@ -1 +1 @@\n-old\n+new\n"
	const twoFiles = "# Diff\n\n## c/a.go w/a.go (0 symbols)\n\n## c/b.go w/b.go (3 symbols)\n+func B() {}\n"
	const commitRange = "# Diff\n\n## a.go (0 symbols)\n"
	const indexPrefix = "# Diff\n\n## i/a.go w/a.go (0 symbols)\n"

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
			name:    "commit-range header (no c//w/ prefix) gets the raw hunk",
			in:      commitRange,
			fetch:   hunk("@@ -3 +3 @@\n-// old\n+// new\n"),
			wantSub: []string{"## a.go (0 symbols)", "-// old", "+// new"},
		},
		{
			name:    "index-prefixed header gets the raw hunk",
			in:      indexPrefix,
			fetch:   hunk("@@ -3 +3 @@\n-// old\n+// new\n"),
			wantSub: []string{"## i/a.go w/a.go (0 symbols)", "-// old", "+// new"},
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
		{
			name:       "zero-symbol body opening with a markdown heading is non-empty (no splice)",
			in:         "# Diff\n\n## c/a.go w/a.go (0 symbols)\n## Existing markdown heading\nreal body content\n\n## c/b.go w/b.go (2 symbols)\n+func B() {}\n",
			fetch:      hunk("SHOULD NOT APPEAR"),
			wantSub:    []string{"## Existing markdown heading", "real body content", "## c/b.go w/b.go (2 symbols)"},
			wantAbsent: []string{"SHOULD NOT APPEAR"},
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

// TestRunDiffCLISupplementsRawHunkJJ proves RunDiffCLI's jj-aware resolution
// diffs the @-..@ commit range and splices raw hunks into commit-range
// "(0 symbols)" sections for both a code and a non-code file.
func TestRunDiffCLISupplementsRawHunkJJ(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	ctx := context.Background()
	repo := initJJRepo(t)
	t.Chdir(repo)

	// Overwrite both files so @ differs from @-: change the function body and a note line.
	writeFile(t, filepath.Join(repo, "a.go"), "package main\n\nfunc Greet() { println(\"warm hello\") }\n")
	writeFile(t, filepath.Join(repo, "notes.txt"), "second note line\n")

	tilth := writeFakeBin(t, "tilth",
		"#!/bin/sh\nprintf '# Diff\\n\\n## a.go (0 symbols)\\n\\n## notes.txt (0 symbols)\\n'\n")

	got, err := RunDiffCLI(ctx, tilth, []string{"diff"}, "uncommitted", 0)
	if err != nil {
		t.Fatalf("RunDiffCLI() err: %v", err)
	}
	for _, want := range []string{
		"## a.go (0 symbols)",
		"## notes.txt (0 symbols)",
		"+func Greet() { println(\"warm hello\") }",
		"-func Greet() {}",
		"+second note line",
		"-first note line",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func initJJRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runJJ(t, dir, "git", "init", "--colocate")
	runJJ(t, dir, "config", "set", "--repo", "user.name", "t")
	runJJ(t, dir, "config", "set", "--repo", "user.email", "t@t.t")
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n\nfunc Greet() {}\n")
	writeFile(t, filepath.Join(dir, "notes.txt"), "first note line\n")
	runJJ(t, dir, "commit", "-m", "init")
	return dir
}

func runJJ(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("jj", args...) //nolint:gosec // fixed jj argv; dir is a test TempDir, args are literals
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
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

func TestShortenDiffSHAs(t *testing.T) {
	const shaA = "6d67960cd834b1a9914f9e2eda5a936621a9fe61"
	const shaB = "b2902b916deaa4c71c8bbca315b790b2da694e0f"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "show header shortens both range SHAs and keeps the ..",
			in:   "# Diff: " + shaA + ".." + shaB + " — 2 files, 1 modified, 1 added (~81 tokens)\n\n## greet.go (1 symbols)\n",
			want: "# Diff: 6d67960cd8..b2902b916d — 2 files, 1 modified, 1 added (~81 tokens)\n\n## greet.go (1 symbols)\n",
		},
		{
			name: "working-tree header has no SHAs and passes through",
			in:   "# Diff: HEAD~1 — 1 file, 0 modified, 0 added (~21 tokens)\n\n## c/a.go w/a.go (0 symbols)\n",
			want: "# Diff: HEAD~1 — 1 file, 0 modified, 0 added (~21 tokens)\n\n## c/a.go w/a.go (0 symbols)\n",
		},
		{
			name: "a 40-hex in the hunk body is left untouched",
			in:   "# Diff: " + shaA + ".." + shaB + " — 1 file\n\n## c/x.go w/x.go (0 symbols)\n+var h = \"" + shaA + "\"\n",
			want: "# Diff: 6d67960cd8..b2902b916d — 1 file\n\n## c/x.go w/x.go (0 symbols)\n+var h = \"" + shaA + "\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortenDiffSHAs(tt.in); got != tt.want {
				t.Errorf("shortenDiffSHAs()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestCollapsePreambles(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "in-place modify drops diff/index/---/+++ and keeps the hunk",
			in: "## c/mod.go w/mod.go (0 symbols)\n\n" +
				"diff --git c/mod.go i/mod.go\n" +
				"index 9beff6c..7ead6cc 100644\n" +
				"--- c/mod.go\n" +
				"+++ i/mod.go\n" +
				"@@ -1,3 +1,3 @@\n package main\n-func Greet() string { return \"hi\" }\n+func Greet() string { return \"yo\" }\n",
			want: "## c/mod.go w/mod.go (0 symbols)\n\n" +
				"@@ -1,3 +1,3 @@\n package main\n-func Greet() string { return \"hi\" }\n+func Greet() string { return \"yo\" }\n",
		},
		{
			name: "new file keeps its mode line and drops the /dev/null preamble",
			in: "## c/newfile.txt w/newfile.txt (0 symbols)\n\n" +
				"diff --git c/newfile.txt i/newfile.txt\n" +
				"new file mode 100644\n" +
				"index 0000000..d5a09df\n" +
				"--- /dev/null\n" +
				"+++ i/newfile.txt\n" +
				"@@ -0,0 +1 @@\n+brand new\n",
			want: "## c/newfile.txt w/newfile.txt (0 symbols)\n\n" +
				"new file mode 100644\n" +
				"@@ -0,0 +1 @@\n+brand new\n",
		},
		{
			name: "delete keeps its mode line and drops the /dev/null preamble",
			in: "## c/del.txt w/del.txt (0 symbols)\n\n" +
				"diff --git c/del.txt i/del.txt\n" +
				"deleted file mode 100644\n" +
				"index a0c42b6..0000000\n" +
				"--- c/del.txt\n" +
				"+++ /dev/null\n" +
				"@@ -1 +0,0 @@\n-old content line\n",
			want: "## c/del.txt w/del.txt (0 symbols)\n\n" +
				"deleted file mode 100644\n" +
				"@@ -1 +0,0 @@\n-old content line\n",
		},
		{
			name: "mode-only change keeps old/new mode and drops just diff --git",
			in: "## c/script.sh w/script.sh (0 symbols)\n\n" +
				"diff --git c/script.sh i/script.sh\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
			want: "## c/script.sh w/script.sh (0 symbols)\n\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
		},
		{
			name: "rename keeps its full preamble because both sides differ from P",
			in: "## c/newname.go w/newname.go (0 symbols)\n\n" +
				"diff --git c/oldname.go i/newname.go\n" +
				"similarity index 92%\n" +
				"rename from oldname.go\n" +
				"rename to newname.go\n" +
				"--- c/oldname.go\n" +
				"+++ i/newname.go\n" +
				"@@ -1,2 +1,2 @@\n-// old\n+// new\n",
			want: "## c/newname.go w/newname.go (0 symbols)\n\n" +
				"diff --git c/oldname.go i/newname.go\n" +
				"similarity index 92%\n" +
				"rename from oldname.go\n" +
				"rename to newname.go\n" +
				"--- c/oldname.go\n" +
				"+++ i/newname.go\n" +
				"@@ -1,2 +1,2 @@\n-// old\n+// new\n",
		},
		{
			name: "binary line is kept while diff/index are dropped",
			in: "## c/blob.bin w/blob.bin (0 symbols)\n\n" +
				"diff --git c/blob.bin i/blob.bin\n" +
				"index 774cb84..6bcbf48 100644\n" +
				"Binary files c/blob.bin and i/blob.bin differ\n",
			want: "## c/blob.bin w/blob.bin (0 symbols)\n\n" +
				"Binary files c/blob.bin and i/blob.bin differ\n",
		},
		{
			name: "a hunk line resembling a preamble past the @@ boundary is kept",
			in: "## c/mod.go w/mod.go (0 symbols)\n\n" +
				"diff --git c/mod.go i/mod.go\n" +
				"--- c/mod.go\n" +
				"+++ i/mod.go\n" +
				"@@ -1,2 +1,2 @@\n keep\n--- mod.go\n",
			want: "## c/mod.go w/mod.go (0 symbols)\n\n" +
				"@@ -1,2 +1,2 @@\n keep\n--- mod.go\n",
		},
		{
			name: "commit-range bare heading collapses its prefixed preamble",
			in: "## greet.go (1 symbols)\n\n" +
				"diff --git a/greet.go b/greet.go\n" +
				"index 111..222 100644\n" +
				"--- a/greet.go\n" +
				"+++ b/greet.go\n" +
				"@@ -1 +1 @@\n-a\n+b\n",
			want: "## greet.go (1 symbols)\n\n" +
				"@@ -1 +1 @@\n-a\n+b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapsePreambles(tt.in); got != tt.want {
				t.Errorf("collapsePreambles()\n got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestAnnotateDiffSymbols(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "greet.go"),
		"package main\n\n// Greet greets the world warmly.\nfunc Greet() string {\n\treturn \"hello, friend\"\n}\n")
	files := anchor.NewFiles(dir)
	hash := string(anchor.Of("func Greet() string {"))

	in := "# Diff: a..b\n\n" +
		"## greet.go (1 symbols)\n" +
		"  [~]      Greet                                    L4  (body, 3→3 lines)\n" +
		"  [~]      Greet                                    L999  (body, 1→1 lines)\n\n" +
		"## missing.go (1 symbols)\n" +
		"  [+]      Ghost                                    L2  (new, 3 lines)\n"

	want := "# Diff: a..b\n\n" +
		"## greet.go (1 symbols)\n" +
		"  [~]      Greet                                    L4#" + hash + "  (body, 3→3 lines)\n" +
		"  [~]      Greet                                    L999  (body, 1→1 lines)\n\n" +
		"## missing.go (1 symbols)\n" +
		"  [+]      Ghost                                    L2  (new, 3 lines)\n"

	if got := annotateDiffSymbols(in, files); got != want {
		t.Errorf("annotateDiffSymbols()\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestAnnotateDiffSymbolsSkipsHunkContent proves a supplemented hunk's context
// line that happens to be shaped like a symbol row is left byte-identical, while
// a genuine symbol row sitting before the hunk still anchors.
func TestAnnotateDiffSymbolsSkipsHunkContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "greet.go"),
		"package main\n\n// Greet greets the world warmly.\nfunc Greet() string {\n\treturn \"hello, friend\"\n}\n\n// Farewell says goodbye.\nfunc Farewell() string {\n\treturn \"goodbye\"\n}\nfunc extra() {}\n")
	files := anchor.NewFiles(dir)
	hash := string(anchor.Of("func Greet() string {"))

	// L12 ("func extra() {}") resolves in greet.go, so without the in-hunk guard
	// the context line below would corrupt into "L12#…".
	in := "## greet.go (1 symbols)\n" +
		"  [~]      Greet                                    L4  (body, 3→3 lines)\n" +
		"@@ -1 +1 @@\n" +
		" [x] task L12  (kept)\n"

	want := "## greet.go (1 symbols)\n" +
		"  [~]      Greet                                    L4#" + hash + "  (body, 3→3 lines)\n" +
		"@@ -1 +1 @@\n" +
		" [x] task L12  (kept)\n"

	if got := annotateDiffSymbols(in, files); got != want {
		t.Errorf("annotateDiffSymbols()\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestAnnotateDiffSymbolsCRLFHeading proves a CRLF-terminated file header still
// switches the active file, so the following symbol row hashes the right file's
// content (b.go's line 1, never a.go's) and its trailing "\r" survives.
func TestAnnotateDiffSymbolsCRLFHeading(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package alpha\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package beta\n")
	files := anchor.NewFiles(dir)
	hashA := string(anchor.Of("package alpha"))
	hashB := string(anchor.Of("package beta"))

	in := "## a.go (1 symbols)\n" +
		"  [~]      Alpha                                    L1  (body)\n" +
		"## b.go (1 symbols)\r\n" +
		"  [~]      Beta                                     L1  (body)\r\n"

	want := "## a.go (1 symbols)\n" +
		"  [~]      Alpha                                    L1#" + hashA + "  (body)\n" +
		"## b.go (1 symbols)\r\n" +
		"  [~]      Beta                                     L1#" + hashB + "  (body)\r\n"

	if got := annotateDiffSymbols(in, files); got != want {
		t.Errorf("annotateDiffSymbols()\n got:\n%q\nwant:\n%q", got, want)
	}
}
