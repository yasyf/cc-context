package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLanguageCensus(t *testing.T) {
	root := t.TempDir()
	files := []string{
		"a.go", "b.go", "c.go",
		"x.py",
		"web/app.ts", "web/util.ts",
		"docs/readme.md",
		"node_modules/dep/index.js", // skipped dir
		".hidden/secret.go",         // skipped (dotdir)
		"data.json",                 // not a counted source ext
	}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Sorted by count desc, then extension asc.
	want := "languages: go (3), ts (2), md (1), py (1)"
	if got := languageCensus(root); got != want {
		t.Errorf("languageCensus = %q, want %q", got, want)
	}
}

func TestLanguageCensusEmpty(t *testing.T) {
	if got := languageCensus(t.TempDir()); got != "" {
		t.Errorf("empty-dir census = %q, want \"\"", got)
	}
}
