package astgrep

import (
	"reflect"
	"testing"
)

// item builds an OutlineItem with the given fields; start/end are 0-based
// ast-grep lines, matching the JSON shape Flatten shifts to 1-based.
func item(symType, name, sig string, start, end int, members ...OutlineItem) OutlineItem {
	it := OutlineItem{SymbolType: symType, Name: name, Signature: sig, Members: members}
	it.Range.Start.Line = start
	it.Range.End.Line = end
	return it
}

func TestFlatten(t *testing.T) {
	tests := []struct {
		name  string
		items []OutlineItem
		want  []FlatItem
	}{
		{
			name:  "top-level only",
			items: []OutlineItem{item("func", "Alpha", "func Alpha(x int) int {", 2, 4)},
			want: []FlatItem{
				{Name: "Alpha", Qualified: "Alpha", SymbolType: "func", Signature: "func Alpha(x int) int {", StartLine: 3, EndLine: 5, Depth: 0},
			},
		},
		{
			name: "container with member fields",
			items: []OutlineItem{item("struct", "Widget", "type Widget struct {", 6, 9,
				item("field", "Name", "Name string", 7, 7),
				item("field", "Size", "Size int", 8, 8),
			)},
			want: []FlatItem{
				{Name: "Widget", Qualified: "Widget", SymbolType: "struct", Signature: "type Widget struct {", StartLine: 7, EndLine: 10, Depth: 0},
				{Name: "Name", Qualified: "Widget.Name", SymbolType: "field", Signature: "Name string", StartLine: 8, EndLine: 8, Depth: 1},
				{Name: "Size", Qualified: "Widget.Size", SymbolType: "field", Signature: "Size int", StartLine: 9, EndLine: 9, Depth: 1},
			},
		},
		{
			name: "deeply nested member qualifies and deepens",
			items: []OutlineItem{item("class", "Outer", "class Outer:", 0, 9,
				item("method", "run", "def run(self):", 1, 9,
					item("function", "inner", "def inner():", 2, 3),
				),
			)},
			want: []FlatItem{
				{Name: "Outer", Qualified: "Outer", SymbolType: "class", Signature: "class Outer:", StartLine: 1, EndLine: 10, Depth: 0},
				{Name: "run", Qualified: "Outer.run", SymbolType: "method", Signature: "def run(self):", StartLine: 2, EndLine: 10, Depth: 1},
				{Name: "inner", Qualified: "Outer.run.inner", SymbolType: "function", Signature: "def inner():", StartLine: 3, EndLine: 4, Depth: 2},
			},
		},
		{
			name:  "empty file flattens to nothing",
			items: nil,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OutlineFile{Path: "x.go", Items: tt.items}.Flatten()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Flatten()\n got = %+v\nwant = %+v", got, tt.want)
			}
		})
	}
}
