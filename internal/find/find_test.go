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

func run(t *testing.T, glob, scope string, budget int) string {
	t.Helper()
	out, err := Run(context.Background(), backend.Args{Glob: glob, Scope: scope, Budget: budget})
	if err != nil {
		t.Fatalf("Run(%q, %q): %v", glob, scope, err)
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
	root := t.TempDir()
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
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/info/exclude": "excluded.txt\n",
		"keep.txt":          "keep\n",
		"excluded.txt":      "gone\n",
	})
	out := run(t, "*.txt", root, 0)
	mustContain(t, out, "keep.txt", "— 1 files", "1 ignored files hidden")
	mustNotContain(t, out, "excluded.txt")
}

func TestPopulatedJJExcluded(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
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

func TestDotfileInclusion(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".env":                 "SECRET=1\n",
		"visible.txt":          "hi\n",
		".config/settings.ini": "[a]\n",
	})
	out := run(t, "**/*", root, 0)
	mustContain(t, out, ".env", "visible.txt", ".config/settings.ini", "— 3 files")
}

func TestEmptyResult(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	out := run(t, "*.py", root, 0)
	mustContain(t, out, "— 0 files", "no files match", "go")
	mustNotContain(t, out, "ignored files hidden")
}

func TestEmptyResultDisclosesHidden(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	out := run(t, "*.go", root, 0)
	mustContain(t, out, "— 2 files")
	mustNotContain(t, out, "ignored files hidden")
}

func TestDeterministic(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"outer.go":     "package a\n",
		"pkg/inner.go": "package b\n",
	})
	scope := filepath.Join(root, "pkg")
	out := run(t, "*.go", scope, 0)
	mustContain(t, out, "inner.go", "— 1 files")
	mustNotContain(t, out, "outer.go")
}

func TestDotAnchorKeepsVCSExcluded(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"real.txt":  "content\n",
	})
	// "./**/*" anchors at the walk root itself — a normal default walk, not an
	// escape, so the VCS store stays excluded.
	out := run(t, "./**/*", root, 0)
	mustContain(t, out, "real.txt")
	mustNotContain(t, out, "HEAD")
}

func TestEscapedSubtreeKeepsVCSExcluded(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".jj/repo/store/op.txt": "internal\n",
		"real.txt":              "content\n",
	})
	// Explicitly naming the store in the anchor is the only way in.
	out := run(t, ".jj/**", root, 0)
	mustContain(t, out, ".jj/repo/store/op.txt")
}

func TestAncestorGitignoreScoped(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
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
	root := t.TempDir()
	writeTree(t, root, map[string]string{`a\b.txt`: "content\n"})
	// A literal backslash inside a POSIX filename is legal and must survive; only
	// the root's separators are normalized to slashes.
	out := run(t, "*.txt", root, 0)
	mustContain(t, out, `a\b.txt`)
}

func TestBudgetMaxIntNoPanic(t *testing.T) {
	root := t.TempDir()
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
