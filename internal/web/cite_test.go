package web

import (
	"errors"
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
