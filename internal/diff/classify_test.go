package diff

import (
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/hunk"
)

// mk builds an ast-grep outline item with 0-based line coordinates (the JSON
// convention classify shifts to 1-based).
func mk(styp, name, sig string, start, end int, members ...astgrep.OutlineItem) astgrep.OutlineItem {
	it := astgrep.OutlineItem{SymbolType: styp, Name: name, Signature: sig}
	it.Range.Start.Line = start
	it.Range.End.Line = end
	it.Members = members
	return it
}

func fn(name, sig string, start, end int) astgrep.OutlineItem {
	return mk("function", name, sig, start, end)
}

func fileOf(items ...astgrep.OutlineItem) []astgrep.OutlineFile {
	return []astgrep.OutlineFile{{Path: "STDIN", Language: "Go", Items: items}}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name        string
		before      []astgrep.OutlineFile
		after       []astgrep.OutlineFile
		hunks       []hunk.Hunk
		wantSymbols []symChange
		wantMiscAdd int
		wantMiscDel int
	}{
		{
			name:   "added function",
			before: nil,
			after:  fileOf(fn("Foo", "func Foo()", 2, 4)),
			hunks: []hunk.Hunk{
				{OldStart: 3, OldEnd: 2, NewStart: 3, NewEnd: 5, New: []string{"func Foo()", "  return", "}"}},
			},
			wantSymbols: []symChange{{kind: changeAdded, name: "Foo", start: 3, end: 5, sig: "func Foo()"}},
		},
		{
			name:   "removed function",
			before: fileOf(fn("Foo", "func Foo()", 2, 4)),
			after:  nil,
			hunks: []hunk.Hunk{
				{OldStart: 3, OldEnd: 5, NewStart: 3, NewEnd: 2, Old: []string{"func Foo()", "  return", "}"}},
			},
			wantSymbols: []symChange{{kind: changeRemoved, name: "Foo", start: 3, end: 5}},
		},
		{
			name:   "signature change",
			before: fileOf(fn("Foo", "func Foo(a int)", 2, 4)),
			after:  fileOf(fn("Foo", "func Foo(a, b int)", 2, 4)),
			hunks: []hunk.Hunk{
				{OldStart: 3, OldEnd: 3, NewStart: 3, NewEnd: 3, Old: []string{"func Foo(a int) {"}, New: []string{"func Foo(a, b int) {"}},
			},
			wantSymbols: []symChange{{kind: changeModified, name: "Foo", start: 3, end: 5, sigChanged: true, sig: "func Foo(a, b int)"}},
		},
		{
			name:   "body only change",
			before: fileOf(fn("Foo", "func Foo()", 2, 6)),
			after:  fileOf(fn("Foo", "func Foo()", 2, 6)),
			hunks: []hunk.Hunk{
				{OldStart: 4, OldEnd: 4, NewStart: 4, NewEnd: 4, Old: []string{"  x := 1"}, New: []string{"  x := 2"}},
			},
			wantSymbols: []symChange{{kind: changeModified, name: "Foo", start: 3, end: 7, sigChanged: false}},
		},
		{
			name:   "change outside symbols rolls up to misc",
			before: fileOf(fn("Foo", "func Foo()", 2, 6)),
			after:  fileOf(fn("Foo", "func Foo()", 2, 6)),
			hunks: []hunk.Hunk{
				{OldStart: 1, OldEnd: 1, NewStart: 1, NewEnd: 1, Old: []string{"import a"}, New: []string{"import b"}},
			},
			wantSymbols: nil,
			wantMiscAdd: 1,
			wantMiscDel: 1,
		},
		{
			name:   "member body change leaves container unmarked",
			before: fileOf(mk("class", "Foo", "class Foo:", 0, 4, mk("method", "bar", "def bar(self):", 1, 2))),
			after:  fileOf(mk("class", "Foo", "class Foo:", 0, 4, mk("method", "bar", "def bar(self):", 1, 2))),
			hunks: []hunk.Hunk{
				{OldStart: 3, OldEnd: 3, NewStart: 3, NewEnd: 3, Old: []string{"    return 1"}, New: []string{"    return 2"}},
			},
			wantSymbols: []symChange{{kind: changeModified, name: "Foo.bar", start: 2, end: 3, sigChanged: false}},
		},
		{
			name:   "container header change marks container not member",
			before: fileOf(mk("class", "Foo", "class Foo:", 0, 4, mk("method", "bar", "def bar(self):", 1, 2))),
			after:  fileOf(mk("class", "Foo", "class Foo(Base):", 0, 4, mk("method", "bar", "def bar(self):", 1, 2))),
			hunks: []hunk.Hunk{
				{OldStart: 1, OldEnd: 1, NewStart: 1, NewEnd: 1, Old: []string{"class Foo:"}, New: []string{"class Foo(Base):"}},
			},
			wantSymbols: []symChange{{kind: changeModified, name: "Foo", start: 1, end: 5, sigChanged: true, sig: "class Foo(Base):"}},
		},
		{
			name:   "struct field change marks the struct, no field row",
			before: fileOf(mk("struct", "Bar", "type Bar struct {", 0, 2, mk("field", "X", "X int", 1, 1))),
			after:  fileOf(mk("struct", "Bar", "type Bar struct {", 0, 2, mk("field", "X", "X int64", 1, 1))),
			hunks: []hunk.Hunk{
				{OldStart: 2, OldEnd: 2, NewStart: 2, NewEnd: 2, Old: []string{"  X int"}, New: []string{"  X int64"}},
			},
			wantSymbols: []symChange{{kind: changeModified, name: "Bar", start: 1, end: 3, sigChanged: false}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.before, tt.after, tt.hunks)
			if !reflect.DeepEqual(got.symbols, tt.wantSymbols) {
				t.Errorf("symbols = %+v, want %+v", got.symbols, tt.wantSymbols)
			}
			if got.miscAdded != tt.wantMiscAdd || got.miscRemoved != tt.wantMiscDel {
				t.Errorf("misc = +%d/-%d, want +%d/-%d", got.miscAdded, got.miscRemoved, tt.wantMiscAdd, tt.wantMiscDel)
			}
		})
	}
}

// TestClassifyFromJSONL proves the ast-grep JSONL → classify path end to end,
// parsing a real outline stream instead of hand-built structs.
func TestClassifyFromJSONL(t *testing.T) {
	parse := func(s string) []astgrep.OutlineFile {
		files, err := astgrep.ParseOutline([]byte(s))
		if err != nil {
			t.Fatalf("ParseOutline: %v", err)
		}
		return files
	}
	before := parse(`{"path":"STDIN","language":"Go","items":[{"symbolType":"function","name":"Foo","signature":"func Foo() int","range":{"start":{"line":2},"end":{"line":4}}}]}`)
	after := parse(`{"path":"STDIN","language":"Go","items":[{"symbolType":"function","name":"Foo","signature":"func Foo() int","range":{"start":{"line":2},"end":{"line":4}}},{"symbolType":"function","name":"Bar","signature":"func Bar()","range":{"start":{"line":6},"end":{"line":7}}}]}`)
	hunks := []hunk.Hunk{{OldStart: 5, OldEnd: 4, NewStart: 6, NewEnd: 8, New: []string{"func Bar() {", "}", ""}}}

	got := classify(before, after, hunks)
	want := []symChange{{kind: changeAdded, name: "Bar", start: 7, end: 8, sig: "func Bar()"}}
	if !reflect.DeepEqual(got.symbols, want) {
		t.Errorf("symbols = %+v, want %+v", got.symbols, want)
	}
}
