package astgrep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

// globsFixture writes src/a.js, src/c.min.js, and vendor/b.js — each holding one
// matchable function — into a fresh temp dir and chdirs there.
func globsFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"src/a.js", "src/c.min.js", "vendor/b.js"} {
		path := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", f, err)
		}
		if err := os.WriteFile(path, []byte("function hit() { return 1 }\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	t.Chdir(dir)
}

// TestRunStructuralNegatedGlobs pins real ast-grep's "!" --globs behavior: it
// excludes natively, so no Go-side translation is warranted, but --globs never
// filters an explicit file operand — positively or negatively.
func TestRunStructuralNegatedGlobs(t *testing.T) {
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	tests := []struct {
		name       string
		args       backend.Args
		wantFiles  []string
		absentFile string
	}{
		{
			"no glob searches the whole tree",
			backend.Args{Query: "function $F() { $$$ }", Lang: "js", Paths: []string{"."}},
			[]string{"src/a.js", "src/c.min.js", "vendor/b.js"},
			"",
		},
		{
			"negated basename glob excludes the matching file",
			backend.Args{Query: "function $F() { $$$ }", Lang: "js", Glob: "!*.min.js", Paths: []string{"."}},
			[]string{"src/a.js", "vendor/b.js"},
			"src/c.min.js",
		},
		{
			"negated dir glob excludes the whole subtree",
			backend.Args{Query: "function $F() { $$$ }", Lang: "js", Glob: "!vendor/**", Paths: []string{"."}},
			[]string{"src/a.js", "src/c.min.js"},
			"vendor/b.js",
		},
		{
			"explicit file operands bypass the negation entirely",
			backend.Args{Query: "function $F() { $$$ }", Lang: "js", Glob: "!*.min.js", Paths: []string{"src/a.js", "src/c.min.js"}},
			[]string{"src/a.js", "src/c.min.js"},
			"",
		},
		{
			"explicit file operands bypass a positive glob too",
			backend.Args{Query: "function $F() { $$$ }", Lang: "js", Glob: "*.min.js", Paths: []string{"src/a.js", "src/c.min.js"}},
			[]string{"src/a.js", "src/c.min.js"},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globsFixture(t)
			got, err := Run(context.Background(), backend.OpStructural, tt.args)
			if err != nil {
				t.Fatalf("Run structural: %v", err)
			}
			for _, want := range tt.wantFiles {
				if !strings.Contains(got, filepath.FromSlash(want)) {
					t.Errorf("missing %s in:\n%s", want, got)
				}
			}
			if tt.absentFile != "" && strings.Contains(got, filepath.FromSlash(tt.absentFile)) {
				t.Errorf("%s should have been excluded from:\n%s", tt.absentFile, got)
			}
		})
	}
}
