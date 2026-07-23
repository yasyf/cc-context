// Fixtures: ast-grep run -p 'return $A, nil' --json=stream internal/ripgrep > internal/astgrep/testdata/search_stream.jsonl; ast-grep run -p 'return $A, nil' -r 'return $A, nil /* ok */' --json=stream internal/ripgrep > internal/astgrep/testdata/rewrite_stream.jsonl
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
	if len(matches) != 14 {
		t.Fatalf("parsed %d matches, want 14", len(matches))
	}
	if got := DistinctFiles(matches); got != 2 {
		t.Errorf("DistinctFiles = %d, want 2", got)
	}
	// First record is ripgrep.go:161, drawn straight from the captured stream.
	if matches[0].File != "internal/ripgrep/ripgrep.go" || matches[0].Range.Start.Line != 161 {
		t.Errorf("first match = %s:%d, want internal/ripgrep/ripgrep.go:161", matches[0].File, matches[0].Range.Start.Line)
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
	if len(lines) != 14 {
		t.Fatalf("rendered %d lines, want 14:\n%s", len(lines), got)
	}
	// Single-line matches collapse to file:Lstart#hash; the preview is the trimmed
	// match text (no leading tab, no body). ast-grep's 0-based line 161 renders as
	// the 1-based L162, anchored by Of("\treturn toFileMatches(groups), nil") = ag4k.
	want := "internal/ripgrep/ripgrep.go:L162#ag4k  return toFileMatches(groups), nil"
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

	header := "# 14 matches across 2 files\n"
	if !strings.HasPrefix(got, header) {
		t.Errorf("preview header = %q, want prefix %q", got[:len(header)], header)
	}
	// The first hit's anchor + diff lines come from the captured stream; ast-grep's
	// 0-based line 161 renders as the 1-based 162, anchored by
	// Of("\treturn toFileMatches(groups), nil") = ag4k.
	wantBlock := "internal/ripgrep/ripgrep.go:162#ag4k\n" +
		"- return toFileMatches(groups), nil\n" +
		"+ return toFileMatches(groups), nil /* ok */\n"
	if !strings.Contains(got, wantBlock) {
		t.Errorf("preview missing first-hit block %q in:\n%s", wantBlock, got)
	}
}
