package find

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/workspace"
)

// writeTree materializes files under root; a map value is the file's contents. It
// also seeds a bare .git directory so root is a self-contained git root — the
// ancestor-ignore matcher stops there instead of walking up into whatever real
// repo happens to enclose t.TempDir().
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, files)
}

// writeFiles materializes files under root without seeding a git root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeLinkedWorktree materializes a main checkout at root/main — its git dir
// holding the shared info/exclude and the linked worktree's administrative dir,
// whose commondir points back at it — plus the worktree tree at root/wt. The
// caller writes the worktree's .git pointer file, since its target is what varies.
func writeLinkedWorktree(t *testing.T, root, exclude string, files map[string]string) (wt, gitDir string) {
	t.Helper()
	main := filepath.Join(root, "main")
	gitDir = filepath.Join(main, ".git", "worktrees", "wt")
	writeFiles(t, main, map[string]string{
		".git/info/exclude":           exclude,
		".git/worktrees/wt/commondir": "../..\n",
	})
	wt = filepath.Join(root, "wt")
	if err := os.MkdirAll(wt, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, wt, files)
	return wt, gitDir
}

// writePointer writes the gitdir pointer file that stands in for .git in a linked
// worktree.
func writePointer(t *testing.T, wt, gitDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tempRoot returns a symlink-resolved temp directory, so a test that chdirs into
// it sees the path os.Getwd reports (macOS resolves /var to /private/var).
func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	return root
}

func run(t *testing.T, glob, root string, budget int) string {
	t.Helper()
	return runGlobs(t, []string{glob}, root, budget)
}

// runGlobs lists globs from root, which Run reads as the cwd.
func runGlobs(t *testing.T, globs []string, root string, budget int) string {
	t.Helper()
	t.Chdir(root)
	out, err := Run(context.Background(), backend.Args{Globs: globs, Budget: budget})
	if err != nil {
		t.Fatalf("Run(%q) in %q: %v", globs, root, err)
	}
	return out
}

func mustContain(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(out, s) {
			t.Errorf("output unexpectedly contains %q:\n%s", s, out)
		}
	}
}

func TestGitignoreChainHonored(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore":     "ignored.go\n",
		"keep.go":        "package a\n",
		"ignored.go":     "package a\n",
		"sub/keep2.go":   "package b\n",
		"sub/.gitignore": "local.go\n",
		"sub/local.go":   "package b\n",
	})
	out := run(t, "*.go", root, 0)
	mustContain(t, out, "keep.go", "sub/keep2.go", "— 2 files")
	mustNotContain(t, out, "ignored.go", "sub/local.go")
}

func TestGitInfoExcludeHonored(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".git/info/exclude": "excluded.txt\n",
		"keep.txt":          "keep\n",
		"excluded.txt":      "gone\n",
	})
	out := run(t, "*.txt", root, 0)
	mustContain(t, out, "keep.txt", "— 1 files", "1 ignored files hidden")
	mustNotContain(t, out, "excluded.txt")
}

func TestWorktreeInfoExcludeFromCommonDir(t *testing.T) {
	tests := []struct {
		name    string
		gitdir  func(wt, gitDir string) string
		want    []string
		notWant []string
	}{
		{
			name:    "absolute gitdir",
			gitdir:  func(_, gitDir string) string { return gitDir },
			want:    []string{"keep.txt", "— 1 files", "1 ignored files hidden"},
			notWant: []string{"excluded.txt"},
		},
		{
			name:    "relative gitdir",
			gitdir:  func(string, string) string { return "../main/.git/worktrees/wt" },
			want:    []string{"keep.txt", "— 1 files", "1 ignored files hidden"},
			notWant: []string{"excluded.txt"},
		},
		{
			name:    "dangling gitdir",
			gitdir:  func(wt, _ string) string { return filepath.Join(wt, "gone") },
			want:    []string{"keep.txt", "excluded.txt", "— 2 files"},
			notWant: []string{"ignored files hidden"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tempRoot(t)
			wt, gitDir := writeLinkedWorktree(t, root, "excluded.txt\n", map[string]string{
				"keep.txt":     "keep\n",
				"excluded.txt": "gone\n",
			})
			writePointer(t, wt, tt.gitdir(wt, gitDir))
			out := run(t, "*.txt", wt, 0)
			mustContain(t, out, tt.want...)
			mustNotContain(t, out, tt.notWant...)
		})
	}
}

func TestWorktreeGitPointerFileNotListed(t *testing.T) {
	root := tempRoot(t)
	wt, gitDir := writeLinkedWorktree(t, root, "", map[string]string{"keep.txt": "keep\n"})
	writePointer(t, wt, gitDir)

	named := run(t, ".git", wt, 0)
	mustContain(t, named, "— 0 files")
	mustNotContain(t, named, ".git  (")

	full := run(t, "**/*", wt, 0)
	mustContain(t, full, "keep.txt", "— 1 files")
	mustNotContain(t, full, "ignored files hidden")
}

// TestVCSNamedRegularFilesListed pins the boundary of the rule above: only .git
// reaches a walk as a regular file, so a file the user named .jj, .hg, or .svn
// is theirs and stays listed.
func TestVCSNamedRegularFilesListed(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".jj":  "a note\n",
		".hg":  "a note\n",
		".svn": "a note\n",
	})
	for _, name := range []string{".jj", ".hg", ".svn"} {
		t.Run(name, func(t *testing.T) {
			mustContain(t, run(t, name, root, 0), name, "— 1 files")
		})
	}
}

func TestPopulatedJJExcluded(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".jj/repo/store/op.txt": "internal\n",
		"real.txt":              "content\n",
	})
	out := run(t, "*.txt", root, 0)
	mustContain(t, out, "real.txt", "— 1 files")
	// A hard-skipped VCS store is neither shown nor counted as ignore-hidden.
	mustNotContain(t, out, "op.txt", "ignored files hidden")
}

func TestBinaryRowNoTokenEstimate(t *testing.T) {
	root := tempRoot(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 48128-8)...) // 47KB PNG
	if err := os.WriteFile(filepath.Join(root, "logo.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{"note.txt": strings.Repeat("x", 40)})

	out := run(t, "*", root, 0)
	var pngLine, txtLine string
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "logo.png"):
			pngLine = ln
		case strings.HasPrefix(ln, "note.txt"):
			txtLine = ln
		}
	}
	if pngLine != "logo.png  (binary, 47KB, image/png)" {
		t.Errorf("png row = %q, want %q", pngLine, "logo.png  (binary, 47KB, image/png)")
	}
	if strings.Contains(pngLine, "tokens") {
		t.Errorf("binary row must carry no token estimate: %q", pngLine)
	}
	if txtLine != "note.txt  (~10 tokens)" {
		t.Errorf("text row = %q, want %q", txtLine, "note.txt  (~10 tokens)")
	}
}

func TestEscapeHatchReachesIgnored(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore":       ".venv/\n",
		"app.py":           "print(1)\n",
		".venv/lib/pkg.py": "x = 1\n",
	})

	// Default semantics hide .venv and disclose the count.
	plain := run(t, "*.py", root, 0)
	mustContain(t, plain, "app.py", "1 ignored files hidden")
	mustNotContain(t, plain, "pkg.py")

	// The anchored glob walks the ignored subtree.
	anchored := run(t, ".venv/**/*.py", root, 0)
	mustContain(t, anchored, ".venv/lib/pkg.py", "— 1 files")
	mustNotContain(t, anchored, "ignored files hidden")
}

func TestAnchoredRestStaysDirectChildren(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore":        ".venv/\n",
		".venv/direct.py":   "x = 1\n",
		".venv/lib/deep.py": "y = 2\n",
	})
	out := run(t, ".venv/*.py", root, 0)
	mustContain(t, out, ".venv/direct.py", "— 1 files")
	mustNotContain(t, out, "deep.py")
}

func TestRecursiveBasenameGlob(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"a.go":              "package a\n",
		"deep/b.go":         "package b\n",
		"deep/deeper/c.go":  "package c\n",
		"deep/deeper/d.txt": "not go\n",
	})
	out := run(t, "*.go", root, 0)
	mustContain(t, out, "a.go", "deep/b.go", "deep/deeper/c.go", "— 3 files")
	mustNotContain(t, out, "d.txt")
}

func TestSeveralGlobsApplyInOrder(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"a.go":            "package a\n",
		"a_test.go":       "package a\n",
		"notes.md":        "# hi\n",
		"data.json":       "{}\n",
		"vendor/dep.go":   "package dep\n",
		"vendor/dep.md":   "# dep\n",
		"deep/nested.go":  "package deep\n",
		"deep/nested.txt": "text\n",
	})

	// Two includes union.
	out := runGlobs(t, []string{"*.go", "*.md"}, root, 0)
	mustContain(t, out, "a.go", "notes.md", "vendor/dep.go", "vendor/dep.md", "deep/nested.go", "— 6 files")
	mustNotContain(t, out, "data.json", "nested.txt")

	// A trailing exclusion narrows the union; last match wins.
	out = runGlobs(t, []string{"*.go", "*.md", "!*_test.go"}, root, 0)
	mustContain(t, out, "a.go", "notes.md", "— 5 files")
	mustNotContain(t, out, "a_test.go")

	// An exclusion matching an ancestor prunes the subtree.
	out = runGlobs(t, []string{"*.go", "!vendor"}, root, 0)
	mustContain(t, out, "a.go", "a_test.go", "deep/nested.go", "— 3 files")
	mustNotContain(t, out, "vendor/dep.go")

	// An exclusion-only list selects everything it does not exclude.
	out = runGlobs(t, []string{"!*.go", "!*.md"}, root, 0)
	mustContain(t, out, "data.json", "deep/nested.txt", "— 2 files")

	// The header echoes every glob the caller typed.
	mustContain(t, runGlobs(t, []string{"*.go", "!*_test.go"}, root, 0), `# Glob: "*.go", "!*_test.go" in `)
}

func TestSeveralGlobsShareAnAnchor(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore":     "build/\n",
		"build/out.go":   "package build\n",
		"build/out.md":   "# out\n",
		"build/skip.txt": "x\n",
		"src/keep.go":    "package src\n",
	})
	// Both includes anchor at the ignored build/, so the escape hatch fires and the
	// walk re-roots there — src/ is outside the anchor and never listed.
	out := runGlobs(t, []string{"build/*.go", "build/*.md"}, root, 0)
	mustContain(t, out, "build/out.go", "build/out.md", "— 2 files")
	mustNotContain(t, out, "skip.txt", "keep.go", "ignored files hidden")

	// Differing anchors share none, so the walk stays at the root and the ignore
	// chain applies again — build/ is hidden, and disclosed.
	out = runGlobs(t, []string{"build/*.go", "src/*.go"}, root, 0)
	mustContain(t, out, "src/keep.go", "— 1 files", "1 ignored files hidden")
	mustNotContain(t, out, "build/out.go")
}

func TestAbsoluteGlobRelativizes(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"pkg/a.go": "package pkg\n",
		"pkg/a.md": "# a\n",
		"top.go":   "package top\n",
	})
	out := run(t, filepath.ToSlash(root)+"/pkg/*.go", root, 0)
	mustContain(t, out, "pkg/a.go", "— 1 files")
	mustNotContain(t, out, "a.md", "top.go")
}

func TestDotfileInclusion(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".env":                 "SECRET=1\n",
		"visible.txt":          "hi\n",
		".config/settings.ini": "[a]\n",
	})
	out := run(t, "**/*", root, 0)
	mustContain(t, out, ".env", "visible.txt", ".config/settings.ini", "— 3 files")
}

func TestEmptyResult(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	out := run(t, "*.py", root, 0)
	mustContain(t, out, "— 0 files", "no files match", "go")
	mustNotContain(t, out, "ignored files hidden")
}

func TestEmptyResultDisclosesHidden(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore": "secret.go\n",
		"secret.go":  "package a\n",
	})
	// Nothing shown (the only .go is ignored), but the disclosure line still fires
	// and supersedes the extensions hint.
	out := run(t, "*.go", root, 0)
	mustContain(t, out, "— 0 files", "1 ignored files hidden")
	mustNotContain(t, out, "no files match")
}

func TestBudgetOverflowFooter(t *testing.T) {
	root := tempRoot(t)
	const n, size = 50, 400
	files := map[string]string{}
	for i := 0; i < n; i++ {
		files[itoa2(i)+".txt"] = strings.Repeat("x", size)
	}
	writeTree(t, root, files)

	out := run(t, "*.txt", root, 200)
	rendered := 0
	var footer string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasSuffix(ln, " tokens)") && !strings.HasPrefix(ln, "…") {
			rendered++
		}
		if strings.HasPrefix(ln, "… and ") {
			footer = ln
		}
	}
	if footer == "" {
		t.Fatalf("expected an overflow footer with a tight budget:\n%s", out)
	}
	m := regexp.MustCompile(`^… and (\d[\d,]*) more files \(~(\S+) tokens\) —`).FindStringSubmatch(footer)
	if m == nil {
		t.Fatalf("footer shape mismatch: %q", footer)
	}
	gotMore, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	wantMore := n - rendered
	if gotMore != wantMore {
		t.Errorf("footer withheld count = %d, want %d (rendered %d of %d)", gotMore, wantMore, rendered, n)
	}
	if wantTok := humanTokens(wantMore * size / bytesPerToken); m[2] != wantTok {
		t.Errorf("footer withheld tokens = %q, want %q", m[2], wantTok)
	}

	// A generous budget shows everything and drops the footer.
	full := run(t, "*.txt", root, 100000)
	mustNotContain(t, full, "more files")
	mustContain(t, full, "— 50 files")
}

func TestIgnoreDisclosureOnlyWhenHidden(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	out := run(t, "*.go", root, 0)
	mustContain(t, out, "— 2 files")
	mustNotContain(t, out, "ignored files hidden")
}

func TestDeterministic(t *testing.T) {
	root := tempRoot(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	if err := os.WriteFile(filepath.Join(root, "img.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		"z.go": "package z\n", "a.go": "package a\n",
		"m/x.go": "package m\n", "m/b.go": "package m\n",
	})
	first := run(t, "**/*", root, 0)
	second := run(t, "**/*", root, 0)
	if first != second {
		t.Errorf("non-deterministic output:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestScopeReRooting(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"outer.go":     "package a\n",
		"pkg/inner.go": "package b\n",
	})
	scope := filepath.Join(root, "pkg")
	out := run(t, "*.go", scope, 0)
	mustContain(t, out, "inner.go", "— 1 files")
	mustNotContain(t, out, "outer.go")
}

func TestRootAnchorKeepsVCSExcluded(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"real.txt":  "content\n",
	})
	// An absolute glob spanning the walk root relativizes to a plain "**/*" — a
	// default walk, not an escape, so the VCS store stays excluded.
	out := run(t, filepath.ToSlash(root)+"/**/*", root, 0)
	mustContain(t, out, "real.txt")
	mustNotContain(t, out, "HEAD")
}

func TestEscapedSubtreeKeepsVCSExcluded(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"project/real.txt":        "ok\n",
		"project/.git/secret.txt": "g\n",
		"project/.jj/secret.txt":  "j\n",
		"project/.hg/secret.txt":  "h\n",
		"project/.svn/secret.txt": "s\n",
	})
	// The escape hatch disables ignore FILES but keeps the VCS stores excluded,
	// since the anchor "project" is not itself a store.
	out := run(t, "project/**/*.txt", root, 0)
	mustContain(t, out, "project/real.txt", "— 1 files")
	mustNotContain(t, out, "secret.txt")
}

func TestEscapedIntoVCSStoreListsInternals(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".jj/repo/store/op.txt": "internal\n",
		"real.txt":              "content\n",
	})
	// Explicitly naming the store in the anchor is the only way in.
	out := run(t, ".jj/**", root, 0)
	mustContain(t, out, ".jj/repo/store/op.txt")
}

func TestAncestorGitignoreScoped(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".gitignore":    "sub/secret.go\n",
		"sub/secret.go": "package sub\n",
		"sub/keep.go":   "package sub\n",
	})
	scope := filepath.Join(root, "sub")
	// The root .gitignore's anchored rule must hide sub/secret.go even when the
	// walk is scoped into sub, and disclose it in the hidden count.
	out := run(t, "*.go", scope, 0)
	mustContain(t, out, "keep.go", "— 1 files", "1 ignored files hidden")
	mustNotContain(t, out, "secret.go")
}

func TestAncestorInfoExcludeFromGitRoot(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		".git/info/exclude": "sub/hidden.txt\n",
		"sub/hidden.txt":    "x\n",
		"sub/keep.txt":      "y\n",
	})
	scope := filepath.Join(root, "sub")
	// .git/info/exclude is read from the git root, not the scoped walk root.
	out := run(t, "*.txt", scope, 0)
	mustContain(t, out, "keep.txt", "— 1 files", "1 ignored files hidden")
	mustNotContain(t, out, "hidden.txt")
}

func TestEscapeHatchIgnoresDotIgnore(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"sub/.ignore":   "hidden.py\n",
		"sub/hidden.py": "x = 1\n",
		"sub/shown.py":  "y = 2\n",
	})
	// The escape hatch disables .ignore processing, so the .ignore'd file shows.
	out := run(t, "sub/**/*.py", root, 0)
	mustContain(t, out, "sub/hidden.py", "sub/shown.py", "— 2 files")
}

func TestEscapeHatchIgnoresGitModules(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{
		"sub/.gitmodules":     "[submodule \"v\"]\n\tpath = vendored\n\turl = https://example.com/v.git\n",
		"sub/vendored/dep.py": "x = 1\n",
		"sub/keep.py":         "y = 2\n",
	})
	// The escape hatch disables .gitmodules processing, so the submodule tree shows.
	out := run(t, "sub/**/*.py", root, 0)
	mustContain(t, out, "sub/vendored/dep.py", "sub/keep.py", "— 2 files")
}

func TestBackslashInFilenameNotRewritten(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("a backslash is a path separator on this OS")
	}
	root := tempRoot(t)
	writeTree(t, root, map[string]string{`a\b.txt`: "content\n"})
	// A literal backslash inside a POSIX filename is legal and must survive; only
	// the root's separators are normalized to slashes.
	out := run(t, "*.txt", root, 0)
	mustContain(t, out, `a\b.txt`)
}

func TestBudgetMaxIntNoPanic(t *testing.T) {
	root := tempRoot(t)
	writeTree(t, root, map[string]string{"a.txt": "x\n", "b.txt": "y\n"})
	// A math.MaxInt64 budget must clamp, not overflow the cutoff multiply.
	out := run(t, "*.txt", root, math.MaxInt64)
	mustContain(t, out, "a.txt", "b.txt", "— 2 files")
	mustNotContain(t, out, "more files")
}

// itoa2 renders i as a fixed two-digit string so fixture filenames sort and size
// uniformly.
func itoa2(i int) string {
	s := strconv.Itoa(i)
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

func TestPinnedRootOverridesCwd(t *testing.T) {
	pinned := tempRoot(t)
	writeTree(t, pinned, map[string]string{"pinned.go": "package a\n"})
	cwd := tempRoot(t)
	writeTree(t, cwd, map[string]string{"cwd.go": "package b\n"})
	t.Chdir(cwd)

	workspace.SetRoot(pinned)
	t.Cleanup(func() { workspace.SetRoot("") })
	out, err := Run(context.Background(), backend.Args{Globs: []string{"*.go"}})
	if err != nil {
		t.Fatalf("Run pinned at %q: %v", pinned, err)
	}
	mustContain(t, out, "pinned.go")
	mustNotContain(t, out, "cwd.go")

	workspace.SetRoot("")
	out, err = Run(context.Background(), backend.Args{Globs: []string{"*.go"}})
	if err != nil {
		t.Fatalf("Run unpinned at %q: %v", cwd, err)
	}
	mustContain(t, out, "cwd.go")
	mustNotContain(t, out, "pinned.go")
}
