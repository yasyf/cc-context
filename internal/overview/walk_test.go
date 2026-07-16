package overview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaffold writes files (slash-relative paths → contents) into a fresh temp dir and
// returns its root.
func scaffold(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLanguageCensus(t *testing.T) {
	root := scaffold(t, map[string]string{
		"a.go":                  "x",
		"b.go":                  "x",
		"c.go":                  "x",
		"x.py":                  "x",
		"web/app.ts":            "x",
		"web/util.ts":           "x",
		"docs/readme.md":        "x",
		"node_modules/dep/i.js": "x", // top-level dep dir — skipped
		".hidden/secret.go":     "x", // skipped (hidden)
		"data.json":             "x", // not a counted source ext
	})
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := "languages: go (3), ts (2), md (1), py (1)"
	if got := languagesLine(c.exts); got != want {
		t.Errorf("languagesLine = %q, want %q", got, want)
	}
}

func TestLanguageCensusEmpty(t *testing.T) {
	c, err := walkRepo(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := languagesLine(c.exts); got != "" {
		t.Errorf("empty-dir census = %q, want \"\"", got)
	}
}

// TestWalkSkipsTopLevelDepDirsOnly proves a build/dependency dir is skipped only at
// the root: a top-level vendor/ vanishes while a nested internal/vendor is walked.
func TestWalkSkipsTopLevelDepDirsOnly(t *testing.T) {
	root := scaffold(t, map[string]string{
		"main.go":                 "x",
		"vendor/dep/lib.go":       "x", // top-level vendor: skipped
		"internal/vendor/keep.go": "x", // nested vendor: a real package, counted
		"internal/vendor/more.go": "x",
	})
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.exts["go"], 3; got != want {
		t.Errorf("go count = %d, want %d (top-level vendor skipped, internal/vendor kept)", got, want)
	}
	if _, ok := c.tree.children["vendor"]; ok {
		t.Error("top-level vendor/ should be skipped")
	}
	if _, ok := c.tree.children["internal"]; !ok {
		t.Error("internal/ (holding the nested vendor package) should be walked")
	}
}

func TestWalkHonorsGitignore(t *testing.T) {
	root := scaffold(t, map[string]string{
		".gitignore":       "generated/\n",
		"a.go":             "x",
		"generated/big.go": "x", // gitignored dir — must not be counted
		"web/app.ts":       "x",
	})
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := languagesLine(c.exts), "languages: go (1), ts (1)"; got != want {
		t.Errorf("languagesLine = %q, want %q", got, want)
	}
	if got := dirsLine(c.tree); strings.Contains(got, "generated") {
		t.Errorf("dirsLine %q must not contain gitignored dir", got)
	}
}

func TestDirsLine(t *testing.T) {
	root := scaffold(t, map[string]string{
		"cmd/ccx/main.go":   "x",
		"internal/cli/a.go": "x",
		"internal/web/b.go": "x",
		"plugin/hooks/h.py": "x",
	})
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// internal has the most files (2) → first; single-child no-file chains collapse
	// (cmd→cmd/ccx, plugin→plugin/hooks); a multi-pkg dir gets the pkgs annotation.
	want := "dirs: internal (2 pkgs: cli, web) · cmd/ccx · plugin/hooks"
	if got := dirsLine(c.tree); got != want {
		t.Errorf("dirsLine = %q, want %q", got, want)
	}
}

func TestDirsLineCap(t *testing.T) {
	files := map[string]string{}
	for _, d := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n"} {
		files[d+"/f.go"] = "x"
	}
	root := scaffold(t, files)
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := dirsLine(c.tree)
	if !strings.HasSuffix(got, " · +2 more") {
		t.Errorf("dirsLine = %q, want a '+2 more' trailer", got)
	}
	if n := strings.Count(got, "·"); n != maxDirs { // 12 separators for 12 shown + trailer
		t.Errorf("dirsLine shows %d separators, want %d", n, maxDirs)
	}
}

func TestTestsLine(t *testing.T) {
	root := scaffold(t, map[string]string{
		"foo_test.go":   "x",
		"bar_test.go":   "x",
		"pkg/test_x.py": "x",
		"web/a.test.ts": "x",
	})
	c, err := walkRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// go (2) leads; py and js tie at 1 → name-ascending js before py.
	want := "tests: 4 test files (go, js, py)"
	if got := testsLine(c.tests); got != want {
		t.Errorf("testsLine = %q, want %q", got, want)
	}
}

func TestTestsLineEmpty(t *testing.T) {
	if got := testsLine(map[string]int{}); got != "" {
		t.Errorf("testsLine empty = %q, want \"\"", got)
	}
}
