package astgrep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return b
}

func TestParseAndDistinctFiles(t *testing.T) {
	matches, err := Parse(readFixture(t, "search_stream.jsonl"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(matches) != 9 {
		t.Fatalf("parsed %d matches, want 9", len(matches))
	}
	if got := DistinctFiles(matches); got != 2 {
		t.Errorf("DistinctFiles = %d, want 2", got)
	}
	// First record is render.go:24, drawn straight from the captured stream.
	if matches[0].File != "internal/render/render.go" || matches[0].Range.Start.Line != 24 {
		t.Errorf("first match = %s:%d, want internal/render/render.go:24", matches[0].File, matches[0].Range.Start.Line)
	}
}

func TestParseEmptyStream(t *testing.T) {
	// ast-grep's clean no-match output is empty; it must parse to zero matches.
	matches, err := Parse([]byte("\n  \n"))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("parsed %d matches from blank stream, want 0", len(matches))
	}
}

func TestParseMalformed(t *testing.T) {
	if _, err := Parse([]byte("{not json}")); err == nil {
		t.Fatal("Parse: want error for malformed json line")
	}
}

func TestRenderSearch(t *testing.T) {
	matches, err := Parse(readFixture(t, "search_stream.jsonl"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := RenderSearch(matches)

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("rendered %d lines, want 9:\n%s", len(lines), got)
	}
	// Single-line matches collapse to file:Lstart#hash; the preview is the trimmed
	// match text (no leading tab, no body). ast-grep's 0-based line 24 renders as
	// the 1-based L25, anchored by Of("\treturn stdout.String(), nil") = mqc1.
	want := "internal/render/render.go:L25#mqc1  return stdout.String(), nil"
	if lines[0] != want {
		t.Errorf("first line = %q, want %q", lines[0], want)
	}
	if strings.Contains(got, "\t") {
		t.Errorf("rendered search must not carry raw tabs:\n%s", got)
	}
}

func TestRenderPreview(t *testing.T) {
	matches, err := Parse(readFixture(t, "rewrite_stream.jsonl"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := RenderPreview(matches)

	header := "# 9 matches across 2 files\n"
	if !strings.HasPrefix(got, header) {
		t.Errorf("preview header = %q, want prefix %q", got[:len(header)], header)
	}
	// The first hit's anchor + diff lines come from the captured stream; ast-grep's
	// 0-based line 24 renders as the 1-based 25, anchored by
	// Of("\treturn stdout.String(), nil") = mqc1.
	wantBlock := "internal/render/render.go:25#mqc1\n" +
		"- return stdout.String(), nil\n" +
		"+ return stdout.String(), nil /* ok */\n"
	if !strings.Contains(got, wantBlock) {
		t.Errorf("preview missing first-hit block %q in:\n%s", wantBlock, got)
	}
}
