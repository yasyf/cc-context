package web

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestFormatParseCiteRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		section string
		hash    string
	}{
		{"top section", "https://example.com/guide", "1", "k7fq"},
		{"nested section", "https://go.dev/doc", "2.3.1", "ab3d"},
		{"preamble", "https://example.com/x", "0", "zzzz"},
		{"url with query", "https://example.com/a?page=2", "1.2", "m4kp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := FormatCite(tt.url, tt.section, tt.hash)
			got, err := ParseCite(s)
			if err != nil {
				t.Fatalf("ParseCite(%q): %v", s, err)
			}
			if got.URL != tt.url || got.Section != tt.section || got.Hash != tt.hash {
				t.Errorf("ParseCite(%q) = %+v, want {%q %q %q}", s, got, tt.url, tt.section, tt.hash)
			}
		})
	}
}

func TestParseCiteErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no section marker", "https://example.com/guide 1.2#k7fq"},
		{"empty hash after hash", "https://example.com/guide §1.2#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseCite(tt.in); err == nil {
				t.Errorf("ParseCite(%q) = nil error, want a parse error", tt.in)
			}
		})
	}
}

func TestParseCiteToleratesLeadingMarker(t *testing.T) {
	// The section carries a leading "§" (a hand-copied reference).
	got, err := ParseCite("https://example.com/x §§1.1#abcd")
	if err != nil {
		t.Fatalf("ParseCite: %v", err)
	}
	if got.Section != "1.1" || got.Hash != "abcd" {
		t.Errorf("ParseCite = %+v, want section 1.1 hash abcd", got)
	}
}

func citePage() *Page {
	return &Page{
		Sections: []Section{
			{ID: "0"}, {ID: "1"}, {ID: "1.1"}, {ID: "2"},
		},
		Chunks: []Chunk{
			{Index: 0, Section: "1", Hash: "aaaa"},
			{Index: 1, Section: "1.1", Hash: "bbbb"},
			{Index: 2, Section: "2", Hash: "cccc"},
		},
	}
}

func TestResolveExact(t *testing.T) {
	got, err := Resolve(citePage(), "1.1", "bbbb")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Index != 1 || got.Section != "1.1" {
		t.Errorf("Resolve = %+v, want the §1.1 chunk", got)
	}
}

func TestResolveReAnchorsMovedSection(t *testing.T) {
	// The cite claims §9, but the content (hash bbbb) now lives in §1.1.
	got, err := Resolve(citePage(), "9", "bbbb")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Section != "1.1" {
		t.Errorf("Resolve re-anchor section = %q, want %q (the chunk's actual section)", got.Section, "1.1")
	}
}

func TestResolveDrift(t *testing.T) {
	_, err := Resolve(citePage(), "1", "zzzz")
	var drift *DriftedCiteError
	if !errors.As(err, &drift) {
		t.Fatalf("Resolve err = %v, want *DriftedCiteError", err)
	}
	if drift.Nearest != "1" {
		t.Errorf("drift.Nearest = %q, want %q (the cited section still exists)", drift.Nearest, "1")
	}
}

func TestResolveAmbiguousAcrossSections(t *testing.T) {
	// The same 4-char hash "dupe" now lands in two distinct sections. A stale ref
	// that matches neither exactly must surface the ambiguity, not silently pick
	// the first match.
	page := &Page{
		Sections: []Section{{ID: "1"}, {ID: "2"}},
		Chunks: []Chunk{
			{Index: 0, Section: "1", Hash: "dupe"},
			{Index: 1, Section: "2", Hash: "dupe"},
		},
	}
	_, err := Resolve(page, "9", "dupe")
	var drift *DriftedCiteError
	if !errors.As(err, &drift) {
		t.Fatalf("Resolve err = %v, want *DriftedCiteError", err)
	}
	wantCandidates := map[string]bool{"1": true, "2": true}
	if len(drift.Candidates) != len(wantCandidates) {
		t.Fatalf("Candidates = %v, want the two distinct sections", drift.Candidates)
	}
	for _, c := range drift.Candidates {
		if !wantCandidates[c] {
			t.Errorf("Candidates = %v, has unexpected section %q", drift.Candidates, c)
		}
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err = %q, want it to say the cite is ambiguous", err)
	}
	for _, want := range []string{"§1", "§2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %s", err, want)
		}
	}
}

func TestResolveSameSectionDuplicateReAnchors(t *testing.T) {
	// Two chunks in the SAME section share a hash: one distinct section, so the
	// cite re-anchors (not ambiguous).
	page := &Page{
		Sections: []Section{{ID: "1"}},
		Chunks: []Chunk{
			{Index: 0, Section: "1", Hash: "dupe"},
			{Index: 1, Section: "1", Hash: "dupe"},
		},
	}
	got, err := Resolve(page, "9", "dupe")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Section != "1" {
		t.Errorf("Resolve section = %q, want %q", got.Section, "1")
	}
}

func TestResolvePrintedNumber(t *testing.T) {
	sections := []Section{
		{ID: "0"},
		{ID: "1", Title: "Overview"},
		{ID: "1.1", Title: "5.6.7. Date/Time Formats"},
		{ID: "1.2", Title: "5.6.8. Number Formats"},
		{ID: "2", Title: "1) Getting Started"},
		{ID: "2.1", Title: "1. Getting Started"},
	}
	tests := []struct {
		name    string
		input   string
		wantIDs []string
	}{
		{"unique", "5.6.7", []string{"1.1"}},
		{"unique trailing dot", "5.6.7.", []string{"1.1"}},
		{"unique trailing paren", "5.6.8)", []string{"1.2"}},
		{"multiple across sections", "1", []string{"2", "2.1"}},
		{"no numeric match", "9.9", nil},
		{"title without number", "Overview", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotIDs []string
			for _, s := range resolvePrintedNumber(sections, tt.input) {
				gotIDs = append(gotIDs, s.ID)
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("resolvePrintedNumber(%q) = %v, want %v", tt.input, gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestPlainTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain title unchanged", "5.6.7. Date/Time Formats", "5.6.7. Date/Time Formats"},
		{"rfc9110 linked number and bracketed text", "[5.6.7.](#section-5.6.7) [Date/Time Formats]", "5.6.7. Date/Time Formats"},
		{"bolded number", "**5.6.7.** Date/Time Formats", "5.6.7. Date/Time Formats"},
		{"inline code number", "`5.6.7.` Date/Time Formats", "5.6.7. Date/Time Formats"},
		{"collapses whitespace runs", "5.6.7.   Date/Time   Formats", "5.6.7. Date/Time Formats"},
		{"nested-paren link target unwraps whole and leaves no false number", "[*](https://e/x(y)5.6.7)", ""},
		{"escaped paren in link target unwraps whole and keeps trailing text", "[5.6.7.](https://e/a\\)) Date", "5.6.7. Date"},
		{"underscore emphasis stripped like asterisk", "__5.6.7.__ Date/Time Formats", "5.6.7. Date/Time Formats"},
		{"reference-style link unwraps to its text", "[5.6.7.][sec] Date/Time Formats", "5.6.7. Date/Time Formats"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plainTitle(tt.in); got != tt.want {
				t.Errorf("plainTitle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestResolvePrintedNumberMarkup proves a printed number at the head of a title
// resolves even when the title carries inline markdown markup — the RFC-9110 form
// links the number and brackets the text, and headings may bold the number.
func TestResolvePrintedNumberMarkup(t *testing.T) {
	sections := []Section{
		{ID: "1", Title: "[5.6.7.](#section-5.6.7) [Date/Time Formats]"},
		{ID: "2", Title: "**5.6.8.** Number Formats"},
	}
	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{"rfc linked title from bare number", "5.6.7", "1"},
		{"rfc linked title from trailing dot", "5.6.7.", "1"},
		{"bolded title", "5.6.8", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePrintedNumber(sections, tt.input)
			if len(got) != 1 || got[0].ID != tt.wantID {
				t.Errorf("resolvePrintedNumber(%q) = %v, want a single §%s", tt.input, got, tt.wantID)
			}
		})
	}
}

// TestResolvePrintedNumberTitleEdges covers the title-normalization edges: a
// nested-paren inline link must not leak a false printed number, while underscore
// emphasis and reference-style links must still resolve their leading number.
func TestResolvePrintedNumberTitleEdges(t *testing.T) {
	tests := []struct {
		name     string
		sections []Section
		input    string
		wantIDs  []string
	}{
		{
			name:     "nested-paren inline link does not falsely resolve",
			sections: []Section{{ID: "1", Title: "[*](https://e/x(y)5.6.7)"}},
			input:    "5.6.7",
			wantIDs:  nil,
		},
		{
			name:     "underscore-bolded number resolves",
			sections: []Section{{ID: "1", Title: "__5.6.7.__ Date/Time Formats"}},
			input:    "5.6.7",
			wantIDs:  []string{"1"},
		},
		{
			name:     "reference-style link number resolves",
			sections: []Section{{ID: "1", Title: "[5.6.7.][sec] Date/Time Formats"}},
			input:    "5.6.7",
			wantIDs:  []string{"1"},
		},
		{
			name:     "escaped paren in link target still resolves",
			sections: []Section{{ID: "1", Title: "[5.6.7.](https://e/a\\)) Date"}},
			input:    "5.6.7",
			wantIDs:  []string{"1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotIDs []string
			for _, s := range resolvePrintedNumber(tt.sections, tt.input) {
				gotIDs = append(gotIDs, s.ID)
			}
			if !slices.Equal(gotIDs, tt.wantIDs) {
				t.Errorf("resolvePrintedNumber(%q) = %v, want %v", tt.input, gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestLooksLikeSectionID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"2", true},
		{"2.3.1", true},
		{"0", true},
		{"5.6.7.", false},
		{"5.6.7)", false},
		{"Date/Time", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := looksLikeSectionID(tt.input); got != tt.want {
				t.Errorf("looksLikeSectionID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNearestSection(t *testing.T) {
	tests := []struct {
		name    string
		section string
		want    string
	}{
		{"exact", "1.1", "1.1"},
		{"climb to ancestor", "1.3", "1"},
		{"fall back to first", "5.6", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nearestSection(citePage(), tt.section); got != tt.want {
				t.Errorf("nearestSection(%q) = %q, want %q", tt.section, got, tt.want)
			}
		})
	}
}
