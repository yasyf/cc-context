package deps

import (
	"os"
	"path/filepath"
	"testing"
)

// classFixture builds a module tree under t.TempDir with a go.mod for
// github.com/acme/x and a couple of real package directories and Python modules,
// returning a classCtx rooted there.
func classFixture(t *testing.T) classCtx {
	t.Helper()
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "internal", "foo"))
	mkdirAll(t, filepath.Join(root, "pypkg"))
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/acme/x\n\ngo 1.26\n")
	writeFile(t, filepath.Join(root, "pymod.py"), "x = 1\n")
	return classCtx{root: root, mod: goModule{root: root, path: "github.com/acme/x"}}
}

func TestClassifyGo(t *testing.T) {
	cc := classFixture(t)
	tests := []struct {
		name        string
		imp         string
		wantClass   string
		wantDisplay string
	}{
		{"stdlib single", "context", classStd, "context"},
		{"stdlib nested", "path/filepath", classStd, "path/filepath"},
		{"local existing dir", "github.com/acme/x/internal/foo", classLocal, "internal/foo"},
		{"under prefix but no dir", "github.com/acme/x/internal/missing", classUnresolved, "github.com/acme/x/internal/missing"},
		{"third party", "github.com/other/pkg", classExternal, "github.com/other/pkg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotDisplay := classifyGo(tt.imp, cc.mod)
			if gotClass != tt.wantClass || gotDisplay != tt.wantDisplay {
				t.Errorf("classifyGo(%q) = (%q, %q), want (%q, %q)", tt.imp, gotClass, gotDisplay, tt.wantClass, tt.wantDisplay)
			}
		})
	}
}

func TestClassifyGoNoModule(t *testing.T) {
	// With no module resolved, local can never fire; std/external still split on
	// the first-segment dot.
	tests := []struct {
		imp  string
		want string
	}{
		{"fmt", classStd},
		{"github.com/acme/x/internal/foo", classExternal},
	}
	for _, tt := range tests {
		t.Run(tt.imp, func(t *testing.T) {
			if got, _ := classifyGo(tt.imp, goModule{}); got != tt.want {
				t.Errorf("classifyGo(%q, zero) = %q, want %q", tt.imp, got, tt.want)
			}
		})
	}
}

func TestClassifyPython(t *testing.T) {
	cc := classFixture(t)
	tests := []struct {
		name string
		imp  string
		want string
	}{
		{"relative dot", ".", classLocal},
		{"relative dotted", ".pkg", classLocal},
		{"relative parent", "..parent", classLocal},
		{"local package dir", "pypkg", classLocal},
		{"local module file", "pymod", classLocal},
		{"local dotted under pkg", "pypkg.sub", classLocal},
		{"stdlib is unresolved", "os", classUnresolved},
		{"third party is unresolved", "requests", classUnresolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPython(tt.imp, cc.root); got != tt.want {
				t.Errorf("classifyPython(%q) = %q, want %q", tt.imp, got, tt.want)
			}
		})
	}
}

func TestClassifyJS(t *testing.T) {
	tests := []struct {
		imp  string
		want string
	}{
		{"./local", classLocal},
		{"../up/mod", classLocal},
		{"external-pkg", classExternal},
		{"@scope/pkg", classExternal},
	}
	for _, tt := range tests {
		t.Run(tt.imp, func(t *testing.T) {
			if got := classifyJS(tt.imp); got != tt.want {
				t.Errorf("classifyJS(%q) = %q, want %q", tt.imp, got, tt.want)
			}
		})
	}
}

func TestClassifyRust(t *testing.T) {
	tests := []struct {
		imp  string
		want string
	}{
		{"crate::foo::Bar", classLocal},
		{"super::baz", classLocal},
		{"self::inner", classLocal},
		{"std::collections::HashMap", classStd},
		{"core::mem", classStd},
		{"alloc::vec::Vec", classStd},
		{"serde::Serialize", classExternal},
	}
	for _, tt := range tests {
		t.Run(tt.imp, func(t *testing.T) {
			if got := classifyRust(tt.imp); got != tt.want {
				t.Errorf("classifyRust(%q) = %q, want %q", tt.imp, got, tt.want)
			}
		})
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
