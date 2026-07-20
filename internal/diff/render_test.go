package diff

import (
	"fmt"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
)

func TestRenderSymbols(t *testing.T) {
	after := []byte("package a\n\nfunc New() {}\n\nfunc Mod() {}\n")
	fc := fileClass{
		symbols: []symChange{
			{kind: changeAdded, name: "New", start: 3, end: 3, sig: "func New()"},
			{kind: changeModified, name: "Mod", start: 5, end: 5},
			{kind: changeRemoved, name: "Old", start: 8, end: 9},
		},
		miscAdded:   2,
		miscRemoved: 0,
	}
	m := diffModel{
		label: "uncommitted", added: 1, changed: 1, removed: 1,
		files: []fileReport{{path: "a.go", kind: fileKindSymbols, class: fc, after: after}},
	}

	h3 := anchor.Of("func New() {}").String()
	h5 := anchor.Of("func Mod() {}").String()
	want := "# diff uncommitted — 1 files · +1 ~1 −1 symbols\n" +
		"## a.go (+1 ~1 −1)\n" +
		fmt.Sprintf("[+] New  L3#%s   func New()\n", h3) +
		fmt.Sprintf("[~] Mod  L5#%s   body\n", h5) +
		"[−] Old  (was L8-9)\n" +
		"… +2/−0 lines outside symbols\n" +
		"hunks hidden — --full inlines per-file hunks\n"

	if got := render(m, false); got != want {
		t.Errorf("render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderRenameNoContentChange(t *testing.T) {
	m := diffModel{
		label: "uncommitted",
		files: []fileReport{{path: "new.go", renamedFrom: "old.go", kind: fileKindRenamed}},
	}
	want := "# diff uncommitted — 1 files · +0 ~0 −0 symbols\n" +
		"## old.go → new.go — renamed, no content change\n"
	if got := render(m, false); got != want {
		t.Errorf("rename render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderRenameWithEdits(t *testing.T) {
	after := []byte("package a\n\nfunc Foo() int { return 2 }\n")
	fc := fileClass{symbols: []symChange{{kind: changeModified, name: "Foo", start: 3, end: 3}}}
	m := diffModel{
		label: "uncommitted", changed: 1,
		files: []fileReport{{path: "new.go", renamedFrom: "old.go", kind: fileKindSymbols, class: fc, after: after}},
	}
	h3 := anchor.Of("func Foo() int { return 2 }").String()
	want := "# diff uncommitted — 1 files · +0 ~1 −0 symbols\n" +
		"## old.go → new.go (~1)\n" +
		fmt.Sprintf("[~] Foo  L3#%s   body\n", h3) +
		"hunks hidden — --full inlines per-file hunks\n"
	if got := render(m, false); got != want {
		t.Errorf("rename-with-edits render mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderRawHunks(t *testing.T) {
	before := []byte("a = 1\nb = 2\n")
	after := []byte("a = 1\nb = 3\n")
	hunks := hunk.Compute(before, after)
	m := diffModel{
		label: "uncommitted",
		files: []fileReport{{path: "config/foo.rb", kind: fileKindRawHunks, ext: ".rb", before: before, hunks: hunks}},
	}

	wantTerse := "# diff uncommitted — 1 files · +0 ~0 −0 symbols\n" +
		"## config/foo.rb — no ast-grep rules for .rb; raw hunks\n" +
		"@@ -2,1 +2,1 @@\n-b = 2\n+b = 3\n"
	if got := render(m, false); got != wantTerse {
		t.Errorf("terse mismatch\n got: %q\nwant: %q", got, wantTerse)
	}

	wantFull := "# diff uncommitted — 1 files · +0 ~0 −0 symbols\n" +
		"## config/foo.rb — no ast-grep rules for .rb; raw hunks\n" +
		"@@ -1,2 +1,2 @@\n a = 1\n-b = 2\n+b = 3\n"
	if got := render(m, true); got != wantFull {
		t.Errorf("full mismatch\n got: %q\nwant: %q", got, wantFull)
	}
}

func TestRenderBinary(t *testing.T) {
	m := diffModel{
		label: "uncommitted",
		files: []fileReport{{path: "assets/logo.png", kind: fileKindBinary}},
	}
	want := "# diff uncommitted — 1 files · +0 ~0 −0 symbols\n" +
		"## assets/logo.png — binary\n"
	if got := render(m, false); got != want {
		t.Errorf("binary mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderCapDisclosure(t *testing.T) {
	x := []hunk.Hunk{
		{OldStart: 1, OldEnd: 1, NewStart: 1, NewEnd: 2, Old: []string{"a"}, New: []string{"A", "B"}},
		{OldStart: 5, OldEnd: 4, NewStart: 6, NewEnd: 6, New: []string{"C"}},
	}
	y := []hunk.Hunk{{OldStart: 3, OldEnd: 2, NewStart: 3, NewEnd: 3, New: []string{"z"}}}
	m := diffModel{
		label: "uncommitted",
		files: []fileReport{
			{path: "x.go", kind: fileKindCapped, hunks: x},
			{path: "y.go", kind: fileKindCapped, hunks: y},
		},
	}
	want := "# diff uncommitted — 2 files · +0 ~0 −0 symbols\n" +
		"# … 2 files beyond the 30-file classification cap — hunk counts only:\n" +
		"## x.go — 2 hunks (+3/−1)\n" +
		"## y.go — 1 hunks (+1/−0)\n"
	if got := render(m, false); got != want {
		t.Errorf("cap mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderSymbolsFullInlinesHunks(t *testing.T) {
	before := []byte("package a\n\nfunc Mod() {\n\treturn 1\n}\n")
	after := []byte("package a\n\nfunc Mod() {\n\treturn 2\n}\n")
	hunks := hunk.Compute(before, after)
	fc := fileClass{symbols: []symChange{{kind: changeModified, name: "Mod", start: 3, end: 5}}}
	m := diffModel{
		label: "uncommitted", changed: 1,
		files: []fileReport{{path: "a.go", kind: fileKindSymbols, class: fc, before: before, after: after, hunks: hunks}},
	}
	h3 := anchor.Of("func Mod() {").String()
	want := "# diff uncommitted — 1 files · +0 ~1 −0 symbols\n" +
		"## a.go (~1)\n" +
		fmt.Sprintf("[~] Mod  L3-5#%s   body\n", h3) +
		"@@ -1,5 +1,5 @@\n package a\n \n func Mod() {\n-\treturn 1\n+\treturn 2\n }\n"
	if got := render(m, true); got != want {
		t.Errorf("full symbol mismatch\n got: %q\nwant: %q", got, want)
	}
}
