package deps

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

func TestRenderBare(t *testing.T) {
	// A Files cache rooted at an empty dir resolves no paths, so every span
	// degrades to a bare line number — isolating the layout from anchor hashes.
	files := anchor.NewFiles(t.TempDir())
	uses := []useItem{
		{line: 4, class: classStd, display: "context"},
		{line: 9, class: classLocal, display: "internal/astgrep"},
		{line: 11, class: classExternal, display: "github.com/x/y"},
	}
	deps := []dependent{
		{path: "a/a.go", line: 12, symbols: []string{"bar.Run", "bar.Parse"}},
		{path: "b/b.go", line: 31},
	}
	got := render("foo/bar.go", uses, deps, "ast-grep", "", files)
	want := strings.Join([]string{
		"# deps foo/bar.go — 3 uses (1 local · 1 std · 1 external), 2 dependents",
		"## uses",
		"L4   context (std)",
		"L9   internal/astgrep (local)",
		"L11   github.com/x/y (external)",
		"## used by",
		"a/a.go:12   → bar.Run, bar.Parse",
		"b/b.go:31",
		"# method: imports via ast-grep; dependents via ripgrep import-line scan — syntactic, not a build graph",
		"",
	}, "\n")
	if got != want {
		t.Errorf("render() mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderEmptyAndUnresolved(t *testing.T) {
	files := anchor.NewFiles(t.TempDir())
	uses := []useItem{{line: 1, class: classUnresolved, display: "os"}}
	got := render("x.py", uses, nil, "regex", "", files)
	want := strings.Join([]string{
		"# deps x.py — 1 uses (1 unresolved), 0 dependents",
		"## uses",
		"L1   os (unresolved)",
		"## used by",
		"# method: imports via regex; dependents via ripgrep import-line scan — syntactic, not a build graph",
		"",
	}, "\n")
	if got != want {
		t.Errorf("render() mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderNoUses(t *testing.T) {
	files := anchor.NewFiles(t.TempDir())
	got := render("empty.go", nil, nil, "ast-grep", "", files)
	if !strings.HasPrefix(got, "# deps empty.go — 0 uses, 0 dependents\n") {
		t.Errorf("render() header for no uses = %q", got)
	}
}

func TestRenderAnchored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "t.go"), "package t\n\nimport \"context\"\n")
	files := anchor.NewFiles(dir)
	got := render("t.go", []useItem{{line: 3, class: classStd, display: "context"}}, nil, "ast-grep", "", files)
	wantSuffix := anchor.Of("import \"context\"").String()
	wantLine := "L3#" + wantSuffix + "   context (std)"
	if !strings.Contains(got, wantLine) {
		t.Errorf("render() missing anchored line %q in:\n%s", wantLine, got)
	}
}

func TestUseBreakdown(t *testing.T) {
	tests := []struct {
		name string
		uses []useItem
		want string
	}{
		{"none", nil, ""},
		{"single std", []useItem{{class: classStd}}, " (1 std)"},
		{
			"mixed order fixed",
			[]useItem{{class: classExternal}, {class: classStd}, {class: classLocal}, {class: classUnresolved}, {class: classLocal}},
			" (2 local · 1 std · 1 external · 1 unresolved)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useBreakdown(tt.uses); got != tt.want {
				t.Errorf("useBreakdown() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLiveDeps runs the real op against a file in this repo, exercising the
// ast-grep import scan, Go classification, the ripgrep used-by scan, and symbol
// enrichment end to end.
func TestLiveDeps(t *testing.T) {
	t.Chdir("../..")
	out, err := Run(context.Background(), backend.Args{Path: "internal/backend/pathcheck.go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("live deps output:\n%s", out)
	for _, want := range []string{
		"# deps internal/backend/pathcheck.go — 5 uses (5 std),",
		"errors (std)",
		"## used by",
		"anchor/rewrite.go:",
		"→ backend.",
		"# method: imports via ast-grep; dependents via ripgrep import-line scan — syntactic, not a build graph",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("live output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunUnsupportedExt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.txt"), "hello\n")
	t.Chdir(dir)
	if _, err := Run(context.Background(), backend.Args{Path: "notes.txt"}); err == nil {
		t.Fatal("expected error for unsupported extension, got nil")
	}
}

func TestRunReadFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := Run(context.Background(), backend.Args{Path: "does-not-exist.go"})
	if err == nil || !strings.Contains(err.Error(), "deps: read") {
		t.Fatalf("want wrapped read error, got %v", err)
	}
}
