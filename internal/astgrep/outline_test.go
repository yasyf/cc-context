package astgrep

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
)

func TestParseOutline(t *testing.T) {
	files, err := ParseOutline(readFixture(t, "outline_stream.jsonl"))
	if err != nil {
		t.Fatalf("ParseOutline: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("parsed %d files, want 2", len(files))
	}
	if files[0].Path != "internal/backend/astgrep.go" || files[0].Language != "Go" {
		t.Errorf("file0 = %s (%s), want internal/backend/astgrep.go (Go)", files[0].Path, files[0].Language)
	}
	if len(files[0].Items) != 7 {
		t.Errorf("file0 items = %d, want 7", len(files[0].Items))
	}
	// First item is the AstGrep struct, captured straight from the stream.
	it := files[0].Items[0]
	if it.SymbolType != "struct" || it.Name != "AstGrep" || it.Signature != "type AstGrep struct {" || it.Range.Start.Line != 13 || !it.IsExported {
		t.Errorf("item0 = %+v, want struct AstGrep at 0-based line 13, exported", it)
	}
	// Its single member is the Bin field, with an expanded signature and its line.
	if len(it.Members) != 1 {
		t.Fatalf("item0 members = %d, want 1", len(it.Members))
	}
	if m := it.Members[0]; m.Name != "Bin" || m.Signature != "Bin string" || m.Range.Start.Line != 16 {
		t.Errorf("member0 = %+v, want Bin \"Bin string\" at 0-based line 16", m)
	}
	if files[1].Path != "internal/backend/pathcheck.go" {
		t.Errorf("file1 = %s, want internal/backend/pathcheck.go", files[1].Path)
	}
}

func TestParseOutlineEmptyStream(t *testing.T) {
	files, err := ParseOutline([]byte("\n  \n"))
	if err != nil {
		t.Fatalf("ParseOutline empty: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("parsed %d files from blank stream, want 0", len(files))
	}
}

func TestParseOutlineMalformed(t *testing.T) {
	if _, err := ParseOutline([]byte("{not json}")); err == nil {
		t.Fatal("ParseOutline: want error for malformed json line")
	}
}

func TestRenderOutline(t *testing.T) {
	files, err := ParseOutline(readFixture(t, "outline_stream.jsonl"))
	if err != nil {
		t.Fatalf("ParseOutline: %v", err)
	}
	// The fixture's paths do not resolve under an empty root, so every span
	// stays bare — this exercises the cache-miss degradation path.
	got := RenderOutline(files, anchor.NewFiles(t.TempDir()), maxOutlineDepth)

	if !strings.Contains(got, "# internal/backend/astgrep.go\n") {
		t.Errorf("missing per-file header:\n%s", got)
	}
	// ast-grep's 0-based line 13 renders as the 1-based L14.
	if !strings.Contains(got, "L14  type AstGrep struct {\n") {
		t.Errorf("missing top-level item line (1-based):\n%s", got)
	}
	// The member is indented two spaces and carries its own 1-based line (16→17).
	if !strings.Contains(got, "\n  L17  Bin string\n") {
		t.Errorf("missing indented member line:\n%s", got)
	}
	if strings.Contains(got, "\t") {
		t.Errorf("rendered outline must not carry raw tabs:\n%s", got)
	}
}

// mkItem builds an outline item spanning 0-based lines [start, end] with the
// given members, mirroring the ast-grep JSON shape WindowOutline reads.
func mkItem(name string, start, end int, members ...OutlineItem) OutlineItem {
	it := OutlineItem{Name: name, Members: members}
	it.Range.Start.Line = start
	it.Range.End.Line = end
	return it
}

func TestWindowOutline(t *testing.T) {
	// 0-based spans: Alpha 1-3 (1-based 2-4), Widget 5-20 (6-21) with fields Name
	// (7) and Size (19), Zeta 25-30 (26-31).
	items := []OutlineItem{
		mkItem("Alpha", 1, 3),
		mkItem("Widget", 5, 20, mkItem("Name", 6, 6), mkItem("Size", 18, 18)),
		mkItem("Zeta", 25, 30),
	}
	tests := []struct {
		name    string
		start   int
		end     int
		want    []string
		members map[string][]string
	}{
		{"whole file keeps all", 1, 100, []string{"Alpha", "Widget", "Zeta"}, map[string][]string{"Widget": {"Name", "Size"}}},
		{"inside struct keeps container and overlapping member", 7, 7, []string{"Widget"}, map[string][]string{"Widget": {"Name"}}},
		{"tail keeps only last", 26, 31, []string{"Zeta"}, nil},
		{"gap between items keeps nothing", 22, 25, nil, nil},
		{"boundary is inclusive", 4, 4, []string{"Alpha"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := WindowOutline([]OutlineFile{{Path: "x.go", Items: items}}, tt.start, tt.end)
			if len(out) != 1 {
				t.Fatalf("WindowOutline returned %d files, want 1", len(out))
			}
			var got []string
			for _, it := range out[0].Items {
				got = append(got, it.Name)
				if want, ok := tt.members[it.Name]; ok {
					var mnames []string
					for _, m := range it.Members {
						mnames = append(mnames, m.Name)
					}
					if !reflect.DeepEqual(mnames, want) {
						t.Errorf("%s members = %v, want %v", it.Name, mnames, want)
					}
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("WindowOutline(%d-%d) items = %v, want %v", tt.start, tt.end, got, tt.want)
			}
		})
	}
}

func TestRenderOutlineNoSymbols(t *testing.T) {
	got := RenderOutline([]OutlineFile{{Path: "empty.rb", Language: "Ruby"}}, anchor.NewFiles("testdata"), maxOutlineDepth)
	if got != "# no symbols\n" {
		t.Errorf("RenderOutline(no items) = %q, want %q", got, "# no symbols\n")
	}
}

func TestRenderOutlineAnchored(t *testing.T) {
	files, err := ParseOutline(readFixture(t, "outline_src_stream.jsonl"))
	if err != nil {
		t.Fatalf("ParseOutline: %v", err)
	}
	// Depth-0 items carry an anchor hashing their real source line in
	// testdata/outline_src.go; members stay bare. Hashes are Of("func Alpha(x
	// int) int {") = xbmn and Of("type Widget struct {") = gpks.
	got := RenderOutline(files, anchor.NewFiles("testdata"), maxOutlineDepth)
	want := "# outline_src.go\n" +
		"L3#xbmn  func Alpha(x int) int {\n" +
		"L7#gpks  type Widget struct {\n" +
		"  L8  Name string\n" +
		"  L9  Size int\n"
	if got != want {
		t.Errorf("RenderOutline anchored =\n%q\nwant\n%q", got, want)
	}
}

// TestRenderOutlineTerse renders at depth 0: top-level items keep their anchors,
// each container's members collapse to a "(+N members)" note, and a single
// --deep/--full hint trails the output.
func TestRenderOutlineTerse(t *testing.T) {
	files, err := ParseOutline(readFixture(t, "outline_src_stream.jsonl"))
	if err != nil {
		t.Fatalf("ParseOutline: %v", err)
	}
	got := RenderOutline(files, anchor.NewFiles("testdata"), 0)
	want := "# outline_src.go\n" +
		"L3#xbmn  func Alpha(x int) int {\n" +
		"L7#gpks  type Widget struct {  (+2 members)\n" +
		"members hidden — --deep or --full to expand\n"
	if got != want {
		t.Errorf("RenderOutline terse =\n%q\nwant\n%q", got, want)
	}
}
