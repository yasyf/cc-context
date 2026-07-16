package astgrep

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// goFixture is a Go source with two imports, a struct with fields, a method whose
// receiver names the struct, and a free function — enough to exercise plain
// outline, --items imports, and --match's signature-level regex.
const goFixture = `package a

import (
	"fmt"
	"strings"
)

type Widget struct {
	Name string
	Size int
}

func (w Widget) Render() string {
	return fmt.Sprintf("%s", strings.ToUpper(w.Name))
}

func Alpha(x int) int {
	return x + 1
}
`

// requireAstGrep skips the test when no ast-grep at the version floor resolves.
func requireAstGrep(t *testing.T) {
	t.Helper()
	if _, err := resolveBin(""); err != nil {
		t.Skipf("ast-grep unavailable: %v", err)
	}
}

// topNames collects the top-level item names of the sole outlined file.
func topNames(t *testing.T, files []OutlineFile) []string {
	t.Helper()
	if len(files) != 1 {
		t.Fatalf("outlined %d files, want 1", len(files))
	}
	names := make([]string, 0, len(files[0].Items))
	for _, it := range files[0].Items {
		names = append(names, it.Name)
	}
	return names
}

// TestOutlinePaths_Live drives the real ast-grep outline through OutlinePaths:
// plain outline, --items imports, --match filtering (including the signature-level
// match that pulls in Render via its Widget receiver), a no-match, and a
// two-path run.
func TestOutlinePaths_Live(t *testing.T) {
	requireAstGrep(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(goFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.py"), []byte("import os\n\ndef helper():\n    return os.getcwd()\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	ctx := context.Background()

	t.Run("plain outline lists declarations, not imports", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got, want := topNames(t, files), []string{"Widget", "Render", "Alpha"}; !reflect.DeepEqual(got, want) {
			t.Errorf("plain outline names = %v, want %v", got, want)
		}
		// The struct carries its fields as members.
		if got := files[0].Items[0]; got.Name != "Widget" || len(got.Members) != 2 {
			t.Errorf("Widget members = %+v, want Name+Size", got.Members)
		}
	})

	t.Run("--items imports lists module imports", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{Items: "imports"})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got, want := topNames(t, files), []string{"fmt", "strings"}; !reflect.DeepEqual(got, want) {
			t.Errorf("imports names = %v, want %v", got, want)
		}
		for _, it := range files[0].Items {
			if it.SymbolType != "module" {
				t.Errorf("import %q symbolType = %q, want module", it.Name, it.SymbolType)
			}
		}
	})

	t.Run("--match filters by signature regex", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{Match: "Alpha"})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got, want := topNames(t, files), []string{"Alpha"}; !reflect.DeepEqual(got, want) {
			t.Errorf("--match Alpha names = %v, want %v", got, want)
		}
	})

	t.Run("--match matches signatures, not just names", func(t *testing.T) {
		// Render's signature "func (w Widget) Render() string" contains "Widget",
		// so a --match Widget keeps both the struct and the method.
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{Match: "Widget"})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got, want := topNames(t, files), []string{"Widget", "Render"}; !reflect.DeepEqual(got, want) {
			t.Errorf("--match Widget names = %v, want %v", got, want)
		}
	})

	t.Run("no match parses to a file with no items", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{Match: "NoSuchSymbol"})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got := topNames(t, files); len(got) != 0 {
			t.Errorf("no-match names = %v, want none", got)
		}
	})

	t.Run("two paths outline both files", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go", "b.py"}, OutlineOpts{})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if len(files) != 2 {
			t.Fatalf("outlined %d files, want 2", len(files))
		}
		langs := map[string]bool{}
		for _, f := range files {
			langs[f.Language] = true
		}
		if !langs["Go"] || !langs["Python"] {
			t.Errorf("languages = %v, want Go and Python", langs)
		}
	})

	t.Run("-l forces the language", func(t *testing.T) {
		files, err := OutlinePaths(ctx, []string{"a.go"}, OutlineOpts{Lang: "go", Match: "Alpha"})
		if err != nil {
			t.Fatalf("OutlinePaths: %v", err)
		}
		if got, want := topNames(t, files), []string{"Alpha"}; !reflect.DeepEqual(got, want) {
			t.Errorf("-l go --match Alpha names = %v, want %v", got, want)
		}
	})
}
