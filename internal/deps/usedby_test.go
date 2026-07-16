package deps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// miniRepo writes files (path→content, path relative to the temp root) under a
// fresh t.TempDir, chdirs into it, and returns a classCtx rooted there. It skips
// the test when ripgrep is unavailable, since the used-by scan needs a real
// engine.
func miniRepo(t *testing.T, files map[string]string) classCtx {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		mkdirAll(t, filepath.Dir(full))
		writeFile(t, full, content)
	}
	t.Chdir(root)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return classCtx{root: cwd}
}

func TestFindDependentsGo(t *testing.T) {
	cc := miniRepo(t, map[string]string{
		"go.mod":     "module github.com/acme/x\n\ngo 1.26\n",
		"foo/foo.go": "package foo\n\nfunc Hello() string { return \"hi\" }\n",
		// real dependent, uses the package symbol
		"a/a.go": "package a\n\nimport \"github.com/acme/x/foo\"\n\nfunc Use() string { return foo.Hello() }\n",
		// blank import: a dependent, but no qualified symbol access
		"b/b.go": "package b\n\nimport (\n\t_ \"github.com/acme/x/foo\"\n)\n",
		// comment mention only: must not count
		"c/c.go": "package c\n\n// see github.com/acme/x/foo for details\n",
		// prefix collision: importing a different package, must not count
		"d/d.go": "package d\n\nimport \"github.com/acme/x/foobar\"\n",
		// sub-package: must not be attributed to the parent
		"e/e.go": "package e\n\nimport \"github.com/acme/x/foo/deep\"\n",
	})
	cc.mod, _ = resolveGoModule("foo/foo.go")

	got, _, err := findDependents(context.Background(), "foo/foo.go", familyGo, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	want := []dependent{
		{path: "a/a.go", line: 3, symbols: []string{"foo.Hello"}},
		{path: "b/b.go", line: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findDependents(go) = %+v, want %+v", got, want)
	}
}

func TestFindDependentsPython(t *testing.T) {
	cc := miniRepo(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/mod.py":      "def run():\n    return 1\n",
		// namespace import of the dotted module
		"a.py": "import pkg.mod\n\nx = pkg.mod.run()\n",
		// from-parent import binding the module name, then namespace access
		"b.py": "from pkg import mod\n\ny = mod.helper()\n",
		// named import: a dependent, but no namespace access -> no symbols
		"c.py": "from pkg.mod import run\n\nz = run()\n",
		// unrelated
		"d.py": "import os\n",
	})

	got, _, err := findDependents(context.Background(), "pkg/mod.py", familyPython, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	want := []dependent{
		{path: "a.py", line: 1, symbols: []string{"mod.run"}},
		{path: "b.py", line: 1, symbols: []string{"mod.helper"}},
		{path: "c.py", line: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findDependents(python) = %+v, want %+v", got, want)
	}
}

func TestFindDependentsJS(t *testing.T) {
	cc := miniRepo(t, map[string]string{
		"lib/util.ts": "export function foo() { return 1; }\n",
		// namespace import: symbol access enriched
		"a.ts": "import * as u from './lib/util';\n\nu.foo();\n",
		// named import: a dependent, no namespace symbols
		"b.ts": "import { bar } from './lib/util';\n\nbar();\n",
		// comment mention: must not count
		"c.ts": "// pulls from ./lib/util\n",
		// bare-package collision on the basename: must not count
		"d.ts": "import util from 'util';\n",
	})

	got, _, err := findDependents(context.Background(), "lib/util.ts", familyJS, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	want := []dependent{
		{path: "a.ts", line: 1, symbols: []string{"u.foo"}},
		{path: "b.ts", line: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findDependents(js) = %+v, want %+v", got, want)
	}
}

func TestFindDependentsRuby(t *testing.T) {
	// The regex-fallback used-by path: a require-shaped scan over the basename,
	// no symbol enrichment.
	cc := miniRepo(t, map[string]string{
		"lib/util.rb":  "def util_fn\n  1\nend\n",
		"a.rb":         "require_relative 'lib/util'\n\nutil_fn\n",
		"b.rb":         "require './lib/util'\n",
		"unrelated.rb": "puts 'hello'\n",
	})

	got, _, err := findDependents(context.Background(), "lib/util.rb", familyRuby, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	want := []dependent{
		{path: "a.rb", line: 1},
		{path: "b.rb", line: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findDependents(ruby) = %+v, want %+v", got, want)
	}
}

// TestFindDependentsRust proves a Rust target is not scanned by basename — its
// module-path imports make a basename needle categorically wrong — and instead
// yields the honest not-scanned note pointing at a grep.
func TestFindDependentsRust(t *testing.T) {
	cc := miniRepo(t, map[string]string{
		"src/util.rs": "pub fn helper() -> i32 { 1 }\n",
		"src/main.rs": "mod util;\nuse crate::util::helper;\n",
		"README.md":   "The util module lives in src/util.rs.\n",
	})
	got, note, err := findDependents(context.Background(), "src/util.rs", familyRust, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	if got != nil {
		t.Errorf("rust should not scan, got deps %+v", got)
	}
	if !strings.Contains(note, "not scanned for .rs") || !strings.Contains(note, "ccx code grep -w util") {
		t.Errorf("rust note = %q, want the honest not-scanned line", note)
	}
}

// TestFindDependentsProseNotADependent proves the used-by scan scopes to the
// family's own extensions and shapes each hit: a real .java importer registers,
// while a markdown file carrying the same import-shaped line in a code fence and a
// bare comment mention never do.
func TestFindDependentsProseNotADependent(t *testing.T) {
	cc := miniRepo(t, map[string]string{
		"Widget.java": "package app;\npublic class Widget {}\n",
		"Screen.java": "package app;\nimport app.Widget;\npublic class Screen { Widget w; }\n",
		"NOTES.md":    "```java\nimport app.Widget;\n```\n",
		"Other.java":  "package app;\n// Widget is only mentioned in a comment\n",
	})
	got, note, err := findDependents(context.Background(), "Widget.java", familyJava, cc)
	if err != nil {
		t.Fatalf("findDependents: %v", err)
	}
	if note != "" {
		t.Errorf("java note = %q, want none", note)
	}
	want := []dependent{{path: "Screen.java", line: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findDependents(java) = %+v, want %+v", got, want)
	}
}

func TestGoImportPath(t *testing.T) {
	mod := goModule{root: filepath.FromSlash("/repo"), path: "github.com/acme/x"}
	tests := []struct {
		name string
		path string
		mod  goModule
		want string
	}{
		{"nested package", filepath.FromSlash("/repo/internal/foo/foo.go"), mod, "github.com/acme/x/internal/foo"},
		{"module root", filepath.FromSlash("/repo/main.go"), mod, "github.com/acme/x"},
		{"no module", filepath.FromSlash("/repo/main.go"), goModule{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := goImportPath(tt.path, tt.mod); got != tt.want {
				t.Errorf("goImportPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestPythonDotted(t *testing.T) {
	root := filepath.FromSlash("/repo")
	tests := []struct {
		name       string
		path       string
		wantDotted string
		wantParent string
		wantMod    string
	}{
		{"nested module", filepath.FromSlash("/repo/pkg/sub/mod.py"), "pkg.sub.mod", "pkg.sub", "mod"},
		{"top module", filepath.FromSlash("/repo/mod.py"), "mod", "", "mod"},
		{"package init", filepath.FromSlash("/repo/pkg/__init__.py"), "pkg", "", "pkg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dotted, parent, mod := pythonDotted(tt.path, root)
			if dotted != tt.wantDotted || parent != tt.wantParent || mod != tt.wantMod {
				t.Errorf("pythonDotted(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tt.path, dotted, parent, mod, tt.wantDotted, tt.wantParent, tt.wantMod)
			}
		})
	}
}
