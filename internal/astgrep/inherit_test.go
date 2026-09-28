package astgrep

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

var anchorRe = regexp.MustCompile(`(L\d+|:\d+)#[0-9a-z]{4}`)

func writeTree(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func outlineOf(t *testing.T, path string, deep bool) string {
	t.Helper()
	out, err := Run(context.Background(), backend.OpStructOutline, backend.Args{Path: path, Deep: deep, Budget: 8000})
	if err != nil {
		t.Fatalf("outline %s: %v", path, err)
	}
	return anchorRe.ReplaceAllString(out, "$1")
}

// TestOutlineSurfacesInheritedMember reproduces #17960: the subclass overrides
// three of its base's methods and inherits the fourth, which the outline must
// show under the base it comes from.
func TestOutlineSurfacesInheritedMember(t *testing.T) {
	requireAstGrep(t)
	writeTree(t, map[string]string{
		"io/ExecutorIOShared.ts": `export class ExecutorIOShared {
  getSecretText(name: string): string { return name }
  log(msg: string): void {}
  run(): void {}
  stop(): void {}
}
`,
		"io/ActionExecutorIO.ts": `import { ExecutorIOShared } from "./ExecutorIOShared"
import type { IExecutorIO } from "./IExecutorIO"

export class ActionExecutorIO extends ExecutorIOShared implements IExecutorIO {
  log(msg: string): void {}
  run(): void {}
  stop(): void {}
}
`,
	})

	deep := `# io/ActionExecutorIO.ts
L4  export class ActionExecutorIO extends ExecutorIOShared implements IExecutorIO {
  L5  log(msg: string): void {}
  L6  run(): void {}
  L7  stop(): void {}
  inherited from ExecutorIOShared (io/ExecutorIOShared.ts:1):
    L2  getSecretText(name: string): string { return name }
`
	if got := outlineOf(t, "io/ActionExecutorIO.ts", true); got != deep {
		t.Errorf("deep outline =\n%s\nwant\n%s", got, deep)
	}

	terse := `# io/ActionExecutorIO.ts
L4  export class ActionExecutorIO extends ExecutorIOShared implements IExecutorIO {  (+3 members, +1 inherited from ExecutorIOShared)
members hidden — --deep or --full to expand
`
	if got := outlineOf(t, "io/ActionExecutorIO.ts", false); got != terse {
		t.Errorf("terse outline =\n%s\nwant\n%s", got, terse)
	}
}

func TestOutlineInheritance(t *testing.T) {
	requireAstGrep(t)
	tests := []struct {
		name  string
		files map[string]string
		path  string
		want  string
	}{
		{
			name: "ts chain through a barrel, a root-relative import, and an abstract base",
			files: map[string]string{
				"lib/core/Root.ts": `export abstract class Root {
  base(): void {}
  shared(): void {}
}
`,
				"lib/core/Mid.ts": `import { Root } from "lib/core/Root"
export class Mid<T> extends Root {
  shared(): void {}
  mid(): T {}
  constructor() { super() }
  private hidden(): void {}
  #secret = 1
}
`,
				"lib/core/index.ts": "export * from \"./Mid\"\nexport { Root as RootBase } from \"./Root\"\n",
				"app/Leaf.ts": `import { Mid as Middle } from "../lib/core"
export class Leaf extends Middle<string> {
  mid(): string { return "" }
}
`,
			},
			path: "app/Leaf.ts",
			want: `# app/Leaf.ts
L2  export class Leaf extends Middle<string> {
  L3  mid(): string { return "" }
  inherited from Mid (lib/core/Mid.ts:2):
    L3  shared(): void {}
  inherited from Root (lib/core/Root.ts:1):
    L2  base(): void {}
`,
		},
		{
			name: "ts interface extends several, and unresolvable bases say why",
			files: map[string]string{
				"a.ts": `import { Remote } from "some-package"
export interface Named { name: string }
export interface Aged { age: number }
export interface Person extends Named, Aged<number> { id: string }
export class Failure extends Error {}
export class Mixed extends mixin(Named) {}
export class Far extends Remote {}
`,
			},
			path: "a.ts",
			want: `# a.ts
L2  export interface Named { name: string }
  L2  name: string
L3  export interface Aged { age: number }
  L3  age: number
L4  export interface Person extends Named, Aged<number> { id: string }
  L4  id: string
  inherited from Named (a.ts:2):
    L2  name: string
  inherited from Aged (a.ts:3):
    L3  age: number
L5  export class Failure extends Error {}
  inherits from Error: unresolved, not declared in this file or imported
L6  export class Mixed extends mixin(Named) {}
  inherits from mixin(): unresolved, not a plain type name
L7  export class Far extends Remote {}
  inherits from Remote: unresolved, imported from "some-package", which resolves to no repo file
`,
		},
		{
			name: "python relative and aliased imports skip keywords and mangled names",
			files: map[string]string{
				"pkg/__init__.py": "",
				"pkg/base.py": `class Base:
    def run(self): pass
    def secret(self): pass
    def __private(self): pass
`,
				"pkg/sub/__init__.py": "from ..base import Base as Core\n",
				"pkg/sub/leaf.py": `from . import Core
import pkg.mixins as mx

class Leaf(Core, mx.Loud, metaclass=Meta):
    def run(self): pass
`,
				"pkg/mixins.py": `class Loud:
    def shout(self): pass
`,
			},
			path: "pkg/sub/leaf.py",
			want: `# pkg/sub/leaf.py
L4  class Leaf(Core, mx.Loud, metaclass=Meta):
  L5  def run(self): pass
  inherited from Base (pkg/base.py:1):
    L3  def secret(self): pass
  inherited from Loud (pkg/mixins.py:1):
    L2  def shout(self): pass
`,
		},
		{
			name: "go embeds promote a sibling file's methods and a module package's type",
			files: map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.23\n",
				"store/store.go": `package store

type Store struct {
	Path string
}

func (s *Store) Get() string { return s.Path }
`,
				"svc/base.go": `package svc

type Base struct {
	ID int
}

func (b *Base) Secret() string { return "" }
func (b *Base) Log() {}
`,
				"svc/sub.go": `package svc

import (
	"io"

	st "example.com/m/store"
)

type Sub struct {
	*Base
	st.Store
	io.Reader
	Extra int ` + "`json:\"extra\"`" + `
}

func (s *Sub) Log() {}
`,
			},
			path: "svc/sub.go",
			want: `# svc/sub.go
L9  type Sub struct {
  L13  Extra int ` + "`json:\"extra\"`" + `
  inherited from Base (svc/base.go:3):
    L4  ID int
    L7  func (b *Base) Secret() string { return "" }
  inherited from Store (store/store.go:3):
    L4  Path string
    L7  func (s *Store) Get() string { return s.Path }
  inherits from io.Reader: unresolved, imported from "io", outside the module
L16  func (s *Sub) Log() {}
`,
		},
		{
			name: "ts static members and commented imports resolve separately",
			files: map[string]string{
				"old.ts": "export class Base { stale(): void {} }\n",
				"new.ts": "export class Base {\n  run(): void {}\n  static make(): Base { return new Base() }\n}\n",
				"leaf.ts": `// import { Base } from "./old"
import { Base } from "./new"
export class Leaf extends Base {
  static run(): void {}
}
`,
			},
			path: "leaf.ts",
			want: `# leaf.ts
L3  export class Leaf extends Base {
  L4  static run(): void {}
  inherited from Base (new.ts:1):
    L2  run(): void {}
    L3  static make(): Base { return new Base() }
`,
		},
		{
			name: "ts barrels that re-export each other terminate",
			files: map[string]string{
				"a/index.ts": "export * from \"../b\"\nexport * from \"ext\"\n",
				"b/index.ts": "export * from \"../a\"\n",
				"leaf.ts":    "import { Gone } from \"./a\"\nexport class Leaf extends Gone {}\n",
			},
			path: "leaf.ts",
			want: `# leaf.ts
L2  export class Leaf extends Gone {}
  inherits from Gone: unresolved, a/index.ts declares no Gone
`,
		},
		{
			name: "python follows the C3 order through a diamond",
			files: map[string]string{
				"d.py": `class Root:
    def run(self): pass
    def base(self): pass

class A(Root):
    def a(self): pass

class B(Root):
    def run(self): pass

class C(A, B):
    pass
`,
			},
			path: "d.py",
			want: `# d.py
L1  class Root:
  L2  def run(self): pass
  L3  def base(self): pass
L5  class A(Root):
  L6  def a(self): pass
  inherited from Root (d.py:1):
    L2  def run(self): pass
    L3  def base(self): pass
L8  class B(Root):
  L9  def run(self): pass
  inherited from Root (d.py:1):
    L3  def base(self): pass
L11  class C(A, B):
  inherited from A (d.py:5):
    L6  def a(self): pass
  inherited from B (d.py:8):
    L9  def run(self): pass
  inherited from Root (d.py:1):
    L3  def base(self): pass
`,
		},
		{
			name: "go promotes by depth, drops ambiguous names, and names each method's file",
			files: map[string]string{
				"p/types.go": `package p

type Root struct{}

type A struct{ Root }

type B struct {
	Name string
}

type Run int

type C struct {
	A
	B
	Run
}
`,
				"p/methods.go": `package p

func (Root) Run()   {}
func (Root) Deep()  {}
func (A) Shared()   {}
func (B) Shared()   {}
func (B) Only()     {}
`,
			},
			path: "p/types.go",
			want: `# p/types.go
L3  type Root struct{}
L5  type A struct{ Root }
  inherited from Root (p/types.go:3):
    p/methods.go:3  func (Root) Run()   {}
    p/methods.go:4  func (Root) Deep()  {}
L7  type B struct {
  L8  Name string
L11  type Run int
L13  type C struct {
  inherited from A (p/types.go:5): none
  inherited from B (p/types.go:7):
    L8  Name string
    p/methods.go:7  func (B) Only()     {}
  inherited from Run (p/types.go:11): none
  inherited from Root (p/types.go:3):
    p/methods.go:4  func (Root) Deep()  {}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeTree(t, tt.files)
			if got := outlineOf(t, tt.path, true); got != tt.want {
				t.Errorf("outline =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// TestOutlineInheritanceUnderSection pins that a --section window changes what
// is shown, never what is inherited: the override outside the window still
// shadows the base's method.
func TestOutlineInheritanceUnderSection(t *testing.T) {
	requireAstGrep(t)
	writeTree(t, map[string]string{
		"a.ts": `export class Base {
  run(): void {}
  kept(): void {}
}
export class Leaf extends Base {
  other(): void {}
  run(): void {}
}
`,
	})
	out, err := Run(context.Background(), backend.OpStructOutline, backend.Args{Path: "a.ts", Deep: true, Section: "5-6"})
	if err != nil {
		t.Fatalf("outline: %v", err)
	}
	want := `# a.ts
L5  export class Leaf extends Base {
  L6  other(): void {}
  inherited from Base (a.ts:1):
    L3  kept(): void {}
`
	if got := anchorRe.ReplaceAllString(out, "$1"); got != want {
		t.Errorf("windowed outline =\n%s\nwant\n%s", got, want)
	}
}
