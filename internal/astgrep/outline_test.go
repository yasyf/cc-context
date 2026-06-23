package astgrep

import (
	"strings"
	"testing"
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
	if files[1].Path != "internal/backend/tilth.go" {
		t.Errorf("file1 = %s, want internal/backend/tilth.go", files[1].Path)
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
	got := RenderOutline(files)

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

func TestRenderOutlineNoSymbols(t *testing.T) {
	got := RenderOutline([]OutlineFile{{Path: "empty.rb", Language: "Ruby"}})
	if got != "# no symbols\n" {
		t.Errorf("RenderOutline(no items) = %q, want %q", got, "# no symbols\n")
	}
}
