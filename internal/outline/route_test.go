package outline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestRoute(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	tests := []struct {
		name string
		path string
		lang string
		want backend.Op
	}{
		{"directory always ast-grep", dir, "", backend.OpStructOutline},
		{"go file", write("a.go"), "", backend.OpStructOutline},
		{"rust file", write("a.rs"), "", backend.OpStructOutline},
		{"python file", write("a.py"), "", backend.OpStructOutline},
		{"tsx file", write("a.tsx"), "", backend.OpStructOutline},
		{"ruby file falls back to native fallback", write("a.rb"), "", backend.OpOutline},
		{"c file falls back to native fallback", write("a.c"), "", backend.OpOutline},
		{"yaml file falls back to native fallback", write("a.yaml"), "", backend.OpOutline},
		{"no extension falls back to native fallback", write("Makefile"), "", backend.OpOutline},
		{"lang override to supported", write("a.txt"), "go", backend.OpStructOutline},
		{"lang override to unsupported", write("a.go"), "ruby", backend.OpOutline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Route(backend.Args{Path: tt.path, Lang: tt.lang})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if got != tt.want {
				t.Errorf("Route(%q, lang=%q) = %q, want %q", tt.path, tt.lang, got, tt.want)
			}
		})
	}
}

func TestLangForExt(t *testing.T) {
	tests := []struct {
		path     string
		wantLang string
		wantOK   bool
	}{
		{"a.go", "go", true},
		{"pkg/b.py", "python", true},
		{"c.pyi", "python", true},
		{"d.ts", "typescript", true},
		{"e.tsx", "tsx", true},
		{"f.jsx", "javascript", true},
		{"g.RS", "rust", true},
		{"h.kt", "kotlin", true},
		{"i.rb", "", false},
		{"Makefile", "", false},
		{"j.yaml", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			lang, ok := LangForExt(tt.path)
			if lang != tt.wantLang || ok != tt.wantOK {
				t.Errorf("LangForExt(%q) = (%q, %v), want (%q, %v)", tt.path, lang, ok, tt.wantLang, tt.wantOK)
			}
		})
	}
}

func TestRouteStatError(t *testing.T) {
	if _, err := Route(backend.Args{Path: filepath.Join(t.TempDir(), "missing.go")}); err == nil {
		t.Fatal("Route: want error for a non-existent path")
	}
}

func TestValidateSection(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "a.go")
	if err := os.WriteFile(goFile, []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write go file: %v", err)
	}
	rbFile := filepath.Join(dir, "a.rb")
	if err := os.WriteFile(rbFile, []byte("x = 1\n"), 0o600); err != nil {
		t.Fatalf("write rb file: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		section   string
		op        backend.Op
		wantStart int
		wantEnd   int
		wantErr   bool
	}{
		{"no section passes", goFile, "", backend.OpStructOutline, 0, 0, false},
		{"ast-grep file with range passes", goFile, "40-95", backend.OpStructOutline, 40, 95, false},
		{"comma range passes", goFile, "40,95", backend.OpStructOutline, 40, 95, false},
		{"non-numeric section rejected", goFile, "## Heading", backend.OpStructOutline, 0, 0, true},
		{"reversed range rejected", goFile, "95-40", backend.OpStructOutline, 0, 0, true},
		{"directory rejected", dir, "40-95", backend.OpStructOutline, 0, 0, true},
		{"fallback lane rejected", rbFile, "40-95", backend.OpOutline, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := ValidateSection(backend.Args{Path: tt.path, Section: tt.section}, tt.op)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSection(%q, %q, %q) err = %v, wantErr %v", tt.path, tt.section, tt.op, err, tt.wantErr)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("ValidateSection(%q, %q, %q) = (%d, %d), want (%d, %d)", tt.path, tt.section, tt.op, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}
