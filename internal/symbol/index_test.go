package symbol

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/astgrep"
)

func loadOutline(t *testing.T, name string) []astgrep.OutlineFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "outlines", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	files, err := astgrep.ParseOutline(data)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return files
}

func TestCandidates(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		query     string
		qualifier string
		fold      bool
		wantQuals []string // Qualified names of the candidates, order-independent
		wantKind  string   // symbolType of the first candidate (when exactly one)
	}{
		{name: "go top-level function", fixture: "go.jsonl", query: "decorate", wantQuals: []string{"decorate"}, wantKind: "function"},
		{name: "go type", fixture: "go.jsonl", query: "Widget", wantQuals: []string{"Widget"}, wantKind: "struct"},
		{name: "go const", fixture: "go.jsonl", query: "MaxWidgets", wantQuals: []string{"MaxWidgets"}, wantKind: "constant"},
		{name: "go struct field is a member", fixture: "go.jsonl", query: "Name", wantQuals: []string{"Widget.Name"}, wantKind: "field"},
		{name: "go method receiver qualifier", fixture: "go.jsonl", query: "Render", qualifier: "Widget", wantQuals: []string{"Render"}, wantKind: "method"},
		{name: "go method wrong receiver misses", fixture: "go.jsonl", query: "Render", qualifier: "Gadget", wantQuals: nil},
		{name: "go case-sensitive miss", fixture: "go.jsonl", query: "widget", wantQuals: nil},
		{name: "go case-insensitive hit", fixture: "go.jsonl", query: "widget", fold: true, wantQuals: []string{"Widget"}, wantKind: "struct"},
		{name: "py method is a member", fixture: "py.jsonl", query: "render", wantQuals: []string{"Widget.render"}, wantKind: "method"},
		{name: "py member with class qualifier", fixture: "py.jsonl", query: "render", qualifier: "Widget", wantQuals: []string{"Widget.render"}, wantKind: "method"},
		{name: "py member wrong qualifier misses", fixture: "py.jsonl", query: "render", qualifier: "Gadget", wantQuals: nil},
		{name: "py top-level function", fixture: "py.jsonl", query: "helper", wantQuals: []string{"helper"}, wantKind: "function"},
		{name: "ts method is a member", fixture: "ts.jsonl", query: "render", wantQuals: []string{"Widget.render"}, wantKind: "method"},
		{name: "ts top-level function", fixture: "ts.jsonl", query: "brackets", wantQuals: []string{"brackets"}, wantKind: "function"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &resolver{name: tt.query, qualifier: tt.qualifier}
			cands := r.candidates(loadOutline(t, tt.fixture), tt.fold)
			got := make([]string, len(cands))
			for i, c := range cands {
				got[i] = c.qualified
			}
			if !sameSet(got, tt.wantQuals) {
				t.Fatalf("candidates(%q, q=%q, fold=%v) quals = %v, want %v", tt.query, tt.qualifier, tt.fold, got, tt.wantQuals)
			}
			if len(tt.wantQuals) == 1 && cands[0].kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", cands[0].kind, tt.wantKind)
			}
		})
	}
}

func TestCandidateSpanAndDoc(t *testing.T) {
	// The go fixture's decorate is a top-level function; verify the flattened span
	// is 1-based and the exported flag comes from the name, not ast-grep's all-true
	// isExported.
	r := &resolver{name: "decorate"}
	cands := r.candidates(loadOutline(t, "go.jsonl"), false)
	if len(cands) != 1 {
		t.Fatalf("want 1 decorate candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.start != 20 || c.end != 22 {
		t.Errorf("decorate span = %d-%d, want 20-22", c.start, c.end)
	}
	if c.exported {
		t.Errorf("decorate is lowercase; exported should be false despite ast-grep isExported")
	}
	widget := (&resolver{name: "Widget"}).candidates(loadOutline(t, "go.jsonl"), false)
	if len(widget) != 1 || !widget[0].exported {
		t.Errorf("Widget should resolve and read as exported, got %+v", widget)
	}
}

func TestRank(t *testing.T) {
	tests := []struct {
		name  string
		query string
		in    []candidate
		want  []string // paths (or names) in ranked order
	}{
		{
			name:  "exact case beats folded",
			query: "Foo",
			in: []candidate{
				{name: "foo", path: "b.go"},
				{name: "Foo", path: "c.go"},
			},
			want: []string{"c.go", "b.go"},
		},
		{
			name:  "exported beats unexported",
			query: "Foo",
			in: []candidate{
				{name: "Foo", path: "aaaaaaaaa.go", exported: false},
				{name: "Foo", path: "b.go", exported: true},
			},
			want: []string{"b.go", "aaaaaaaaa.go"},
		},
		{
			name:  "non-test beats test",
			query: "Foo",
			in: []candidate{
				{name: "Foo", path: "a_test.go", exported: true},
				{name: "Foo", path: "impl.go", exported: true},
			},
			want: []string{"impl.go", "a_test.go"},
		},
		{
			name:  "shortest path wins",
			query: "Foo",
			in: []candidate{
				{name: "Foo", path: "internal/deep/x.go", exported: true},
				{name: "Foo", path: "x.go", exported: true},
			},
			want: []string{"x.go", "internal/deep/x.go"},
		},
		{
			name:  "exported outranks non-test — precedence order",
			query: "Foo",
			in: []candidate{
				{name: "Foo", path: "a.go", exported: false},     // non-test but unexported
				{name: "Foo", path: "z_test.go", exported: true}, // exported but test
			},
			want: []string{"z_test.go", "a.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rank(tt.in, tt.query)
			got := make([]string, len(tt.in))
			for i, c := range tt.in {
				got[i] = c.path
			}
			if !equalSlice(got, tt.want) {
				t.Errorf("rank order = %v, want %v", got, tt.want)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
