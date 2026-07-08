package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yasyf/cc-context/internal/anchor"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// sectionKey projects a Section onto its deterministic, offset-free fields.
type sectionKey struct {
	ID     string
	Level  int
	Title  string
	Parent string
}

func keys(sections []Section) []sectionKey {
	out := make([]sectionKey, len(sections))
	for i, s := range sections {
		out[i] = sectionKey{s.ID, s.Level, s.Title, s.Parent}
	}
	return out
}

// breadcrumbs maps each section ID to the breadcrumb its chunks carry.
func breadcrumbs(chunks []Chunk) map[string]string {
	out := map[string]string{}
	for _, c := range chunks {
		out[c.Section] = c.Breadcrumb
	}
	return out
}

// assertInvariant proves the load-bearing chunking guarantees for one document.
func assertInvariant(t *testing.T, markdown string, sections []Section, chunks []Chunk) {
	t.Helper()

	ids := map[string]Section{}
	for _, s := range sections {
		ids[s.ID] = s
	}

	// Sections partition the document.
	if len(sections) > 0 {
		if sections[0].Start != 0 {
			t.Errorf("first section starts at %d, want 0", sections[0].Start)
		}
		for i := 1; i < len(sections); i++ {
			if sections[i].Start != sections[i-1].End {
				t.Errorf("section %s starts at %d, previous ends at %d", sections[i].ID, sections[i].Start, sections[i-1].End)
			}
		}
		if last := sections[len(sections)-1].End; last != len(markdown) {
			t.Errorf("last section ends at %d, want %d", last, len(markdown))
		}
	}

	// Chunks tile the document exactly, in order, with no gaps or overlaps.
	var rebuilt strings.Builder
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d has index %d", i, c.Index)
		}
		if c.Start >= c.End {
			t.Errorf("chunk %d is empty: [%d:%d]", i, c.Start, c.End)
		}
		if i == 0 && c.Start != 0 {
			t.Errorf("first chunk starts at %d, want 0", c.Start)
		}
		if i > 0 && c.Start != chunks[i-1].End {
			t.Errorf("chunk %d starts at %d, previous ends at %d", i, c.Start, chunks[i-1].End)
		}
		span := markdown[c.Start:c.End]
		if !utf8.ValidString(span) {
			t.Errorf("chunk %d [%d:%d] is not valid UTF-8 (a rune was split)", i, c.Start, c.End)
		}
		if got := estimateTokens(span); got > maxChunkTokens {
			t.Errorf("chunk %d estimates %d tokens, over ceiling %d", i, got, maxChunkTokens)
		}
		if _, ok := ids[c.Section]; !ok {
			t.Errorf("chunk %d references unknown section %q", i, c.Section)
		}
		if want := anchor.Of(span).String(); c.Hash != want {
			t.Errorf("chunk %d hash %q, want %q", i, c.Hash, want)
		}
		if len(c.Hash) != 4 {
			t.Errorf("chunk %d hash %q is not 4 chars", i, c.Hash)
		}
		rebuilt.WriteString(span)
	}
	if len(chunks) > 0 {
		if last := chunks[len(chunks)-1].End; last != len(markdown) {
			t.Errorf("last chunk ends at %d, want %d", last, len(markdown))
		}
	}
	if rebuilt.String() != markdown {
		t.Errorf("concatenated chunks do not reproduce the markdown")
	}

	// The first chunk of every section begins at the section (heading) start.
	seen := map[string]bool{}
	for _, c := range chunks {
		if seen[c.Section] {
			continue
		}
		seen[c.Section] = true
		if c.Start != ids[c.Section].Start {
			t.Errorf("section %s first chunk starts at %d, want section start %d", c.Section, c.Start, ids[c.Section].Start)
		}
	}
}

func TestChunkInvariant(t *testing.T) {
	fixtures := []string{
		"atx_setext.md",
		"deep_nesting.md",
		"hash_in_fence.md",
		"tables.md",
		"oversized_fence.md",
		"oversized_paragraph.md",
	}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			md := loadFixture(t, name)
			sections, chunks := ChunkPage(md)
			assertInvariant(t, md, sections, chunks)
		})
	}

	inline := []struct {
		name     string
		markdown string
	}{
		{"empty", ""},
		{"no_heading", "just a paragraph of text\n\nand another\n"},
		{"heading_at_start", "# Title\n\nbody\n"},
		{"only_blank", "\n\n\n"},
		{"trailing_no_newline", "# H\n\nno trailing newline"},
	}
	for _, tt := range inline {
		t.Run(tt.name, func(t *testing.T) {
			sections, chunks := ChunkPage(tt.markdown)
			assertInvariant(t, tt.markdown, sections, chunks)
		})
	}
}

func TestSectionsATXSetext(t *testing.T) {
	md := loadFixture(t, "atx_setext.md")
	sections, chunks := ChunkPage(md)

	want := []sectionKey{
		{"0", 0, "", ""},
		{"1", 1, "Getting Started", ""},
		{"1.1", 2, "Install", "1"},
		{"2", 1, "Setext Section", ""},
		{"2.1", 3, "Deep", "2"},
	}
	got := keys(sections)
	if len(got) != len(want) {
		t.Fatalf("got %d sections, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	wantCrumbs := map[string]string{
		"0":   "",
		"1":   "Getting Started",
		"1.1": "Getting Started > Install",
		"2":   "Setext Section",
		"2.1": "Setext Section > Deep",
	}
	for id, crumb := range breadcrumbs(chunks) {
		if wantCrumbs[id] != crumb {
			t.Errorf("section %s breadcrumb = %q, want %q", id, crumb, wantCrumbs[id])
		}
	}

	// Each small section is a single chunk whose span equals the section span.
	if len(chunks) != len(sections) {
		t.Fatalf("got %d chunks, want %d (one per small section)", len(chunks), len(sections))
	}
	for i, c := range chunks {
		if c.Section != sections[i].ID {
			t.Errorf("chunk %d section = %q, want %q", i, c.Section, sections[i].ID)
		}
		if c.Start != sections[i].Start || c.End != sections[i].End {
			t.Errorf("chunk %d span [%d:%d], want section span [%d:%d]", i, c.Start, c.End, sections[i].Start, sections[i].End)
		}
	}
}

func TestSectionsDeepNesting(t *testing.T) {
	md := loadFixture(t, "deep_nesting.md")
	sections, chunks := ChunkPage(md)

	want := []sectionKey{
		{"1", 1, "A", ""},
		{"1.1", 2, "B", "1"},
		{"1.1.1", 3, "C", "1.1"},
		{"1.1.1.1", 4, "D", "1.1.1"},
		{"1.2", 2, "E", "1"},
		{"2", 1, "F", ""},
	}
	got := keys(sections)
	if len(got) != len(want) {
		t.Fatalf("got %d sections, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	wantCrumbs := map[string]string{
		"1.1.1.1": "A > B > C > D",
		"1.2":     "A > E",
		"2":       "F",
	}
	crumbs := breadcrumbs(chunks)
	for id, want := range wantCrumbs {
		if crumbs[id] != want {
			t.Errorf("section %s breadcrumb = %q, want %q", id, crumbs[id], want)
		}
	}
}

func TestHashInFenceIsNotHeading(t *testing.T) {
	md := loadFixture(t, "hash_in_fence.md")
	sections, chunks := ChunkPage(md)

	want := []sectionKey{
		{"1", 1, "Real Heading", ""},
		{"1.1", 2, "After Fence", "1"},
	}
	got := keys(sections)
	if len(got) != len(want) {
		t.Fatalf("got %d sections, want %d — a '#' inside the fence leaked as a heading: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The fence — including its interior blank line and its '#' comment — stays
	// whole in section 1's single chunk.
	var sec1 []Chunk
	for _, c := range chunks {
		if c.Section == "1" {
			sec1 = append(sec1, c)
		}
	}
	if len(sec1) != 1 {
		t.Fatalf("section 1 split into %d chunks; the fitting fence must not split", len(sec1))
	}
	body := md[sec1[0].Start:sec1[0].End]
	for _, want := range []string{"# This is a comment, not a heading", "def f():", "    pass"} {
		if !strings.Contains(body, want) {
			t.Errorf("section 1 chunk missing %q; the fence was broken", want)
		}
	}
}

func TestOversizedFenceSplitsAtLines(t *testing.T) {
	md := loadFixture(t, "oversized_fence.md")
	sections, chunks := ChunkPage(md)
	assertInvariant(t, md, sections, chunks)

	var fenceChunks int
	for _, c := range chunks {
		if c.Section == "1" {
			fenceChunks++
		}
	}
	if fenceChunks < 2 {
		t.Errorf("oversized fence produced %d chunks, want it split into ≥2", fenceChunks)
	}
	// A line-split keeps chunk boundaries on newlines (no rune-splitting needed).
	for _, c := range chunks {
		if c.End < len(md) && c.End > 0 && md[c.End-1] != '\n' {
			t.Errorf("chunk ending at %d does not end on a line boundary", c.End)
		}
	}
}

func TestOversizedParagraphSplitsAtRunes(t *testing.T) {
	md := loadFixture(t, "oversized_paragraph.md")
	sections, chunks := ChunkPage(md)
	assertInvariant(t, md, sections, chunks)

	var paraChunks int
	for _, c := range chunks {
		if c.Section == "1" {
			paraChunks++
		}
	}
	if paraChunks < 2 {
		t.Errorf("oversized paragraph produced %d chunks, want it split into ≥2", paraChunks)
	}
	// assertInvariant already checks each chunk is valid UTF-8; confirm the
	// multibyte content actually survived the round-trip.
	if !strings.Contains(md, "café") {
		t.Fatal("fixture lost its multibyte marker")
	}
}

func TestTableSurvivesAsOneBlock(t *testing.T) {
	md := loadFixture(t, "tables.md")
	_, chunks := ChunkPage(md)

	var tableChunk string
	for _, c := range chunks {
		span := md[c.Start:c.End]
		if strings.Contains(span, "| Col A | Col B |") {
			tableChunk = span
		}
	}
	if tableChunk == "" {
		t.Fatal("no chunk contains the table header")
	}
	for _, row := range []string{"| Col A | Col B |", "|-------|-------|", "| 1     | 2     |", "| 3     | 4     |"} {
		if !strings.Contains(tableChunk, row) {
			t.Errorf("table row %q landed in a different chunk; the table was split", row)
		}
	}
}

func TestSetextMultilineHeadingTitle(t *testing.T) {
	md := loadFixture(t, "setext_multiline.md")
	sections, chunks := ChunkPage(md)
	assertInvariant(t, md, sections, chunks)

	var h1 string
	for _, s := range sections {
		if s.Level == 1 {
			h1 = s.Title
		}
	}
	const want = "Introduction to the Advanced Widget API"
	if h1 != want {
		t.Errorf("setext H1 title = %q, want %q", h1, want)
	}
	if strings.ContainsRune(h1, '\n') {
		t.Errorf("title %q retains an interior newline", h1)
	}
	// Every breadcrumb built from the title must stay single-line too.
	for _, c := range chunks {
		if strings.ContainsRune(c.Breadcrumb, '\n') {
			t.Errorf("chunk %d breadcrumb %q spans multiple lines", c.Index, c.Breadcrumb)
		}
	}
}

func TestPreamblePresence(t *testing.T) {
	tests := []struct {
		name        string
		markdown    string
		wantPreLen  bool // expect a section "0"
		wantFirstID string
	}{
		{"intro_before_heading", "intro\n\n# H\n\nbody\n", true, "0"},
		{"heading_at_start", "# H\n\nbody\n", false, "1"},
		{"no_heading", "just text\n", true, "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sections, _ := ChunkPage(tt.markdown)
			if len(sections) == 0 {
				t.Fatal("no sections produced")
			}
			hasPre := sections[0].ID == "0" && sections[0].Level == 0
			if hasPre != tt.wantPreLen {
				t.Errorf("preamble present = %v, want %v", hasPre, tt.wantPreLen)
			}
			if sections[0].ID != tt.wantFirstID {
				t.Errorf("first section ID = %q, want %q", sections[0].ID, tt.wantFirstID)
			}
		})
	}

	// A document with no headings is a single preamble section spanning it all.
	sections, chunks := ChunkPage("only body, no headings\n")
	if len(sections) != 1 || sections[0].ID != "0" {
		t.Fatalf("no-heading doc sections = %+v, want one section \"0\"", sections)
	}
	if sections[0].End != len("only body, no headings\n") {
		t.Errorf("preamble ends at %d, want end of document", sections[0].End)
	}
	if len(chunks) != 1 || chunks[0].Section != "0" {
		t.Errorf("no-heading doc chunks = %+v, want one chunk in section 0", chunks)
	}
}
