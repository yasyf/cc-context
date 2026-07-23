package index

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"
)

func TestWalkFiles(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(".gitignore", "ignored_by_git.go\nvendor/\n!special.kjs\n")
	write(".sembleignore", "semskip.go\n")
	write("keep.go", "package keep\n")
	write("notes.md", "# notes\n")
	write("data.json", "{}\n")                  // extension not in the requested set
	write("README", "readme\n")                 // no extension
	write("ignored_by_git.go", "package x\n")   // .gitignore
	write("semskip.go", "package x\n")          // .sembleignore
	write("special.kjs", "custom\n")            // included via the !negation extension bypass
	write("node_modules/dep.go", "package d\n") // denylist dir
	write(".git/config", "[core]\n")            // denylist dir
	write("vendor/lib.go", "package v\n")       // gitignored dir
	write("src/nested.go", "package n\n")
	write("src/.gitignore", "local_ignore.go\n")
	write("src/local_ignore.go", "package x\n") // per-directory .gitignore

	// A symlinked file is never followed.
	if err := os.Symlink(filepath.Join(root, "keep.go"), filepath.Join(root, "link.go")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink unsupported")
		}
		t.Fatalf("symlink: %v", err)
	}

	got, err := WalkFiles(root, []string{".go", ".md"})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	rels := make([]string, len(got))
	for i, p := range got {
		rel, _ := filepath.Rel(root, p)
		rels[i] = filepath.ToSlash(rel)
	}
	sort.Strings(rels)

	want := []string{"keep.go", "notes.md", "special.kjs", "src/nested.go"}
	if !slices.Equal(rels, want) {
		t.Errorf("WalkFiles =\n  %v\nwant\n  %v", rels, want)
	}
}
