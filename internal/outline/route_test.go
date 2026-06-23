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
		{"ruby file falls back to tilth", write("a.rb"), "", backend.OpOutline},
		{"c file falls back to tilth", write("a.c"), "", backend.OpOutline},
		{"yaml file falls back to tilth", write("a.yaml"), "", backend.OpOutline},
		{"no extension falls back to tilth", write("Makefile"), "", backend.OpOutline},
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

func TestRouteStatError(t *testing.T) {
	if _, err := Route(backend.Args{Path: filepath.Join(t.TempDir(), "missing.go")}); err == nil {
		t.Fatal("Route: want error for a non-existent path")
	}
}
