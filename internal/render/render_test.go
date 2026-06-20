package render

import (
	"strings"
	"testing"
)

func TestCap(t *testing.T) {
	tests := []struct {
		id     string
		s      string
		budget int
		want   string
	}{
		{
			id:     "non-positive budget returns input unchanged",
			s:      "line one\nline two\nline three\n",
			budget: 0,
			want:   "line one\nline two\nline three\n",
		},
		{
			id:     "negative budget returns input unchanged",
			s:      "anything at all here",
			budget: -10,
			want:   "anything at all here",
		},
		{
			id:     "under budget passes through",
			s:      "short\n",
			budget: 100,
			want:   "short\n",
		},
		{
			id:     "exactly at budget passes through",
			s:      "abcd",
			budget: 1, // limit = 1*4 = 4 == len
			want:   "abcd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := Cap(tt.s, tt.budget); got != tt.want {
				t.Errorf("Cap(%q, %d) = %q, want %q", tt.s, tt.budget, got, tt.want)
			}
		})
	}
}

func TestCapOverflowCutsAtLineBoundary(t *testing.T) {
	// Six 8-char lines (incl. newline) = 48 chars. budget 4 => limit 16 chars,
	// which lands mid-third-line; the cut must back up to the last newline so
	// only whole lines are kept.
	s := "1234567\n2234567\n3234567\n4234567\n5234567\n6234567\n"
	got := Cap(s, 4)

	if !strings.HasPrefix(got, "1234567\n2234567\n") {
		t.Fatalf("kept prefix not on a line boundary: %q", got)
	}
	if strings.Contains(got, "3234567") {
		t.Errorf("partial line leaked past the cut: %q", got)
	}
	if !strings.Contains(got, "omitted — re-run with a larger --budget") {
		t.Errorf("missing explicit footer: %q", got)
	}
}

func TestCapFooterText(t *testing.T) {
	// Limit lands mid-line so the cut backs up to after "aaaa\n" (5 chars),
	// keeping exactly the first line and omitting the remaining four.
	s := "aaaa\nbbbb\ncccc\ndddd\neeee\n"
	got := Cap(s, 2) // limit = 8 chars

	want := "aaaa\n… +4 lines, ~5 tokens omitted — re-run with a larger --budget\n"
	if got != want {
		t.Errorf("Cap footer\n got = %q\nwant = %q", got, want)
	}
}

func TestCapNoNewlineFallsBackToHardCut(t *testing.T) {
	// No newline before the limit: LastIndexByte returns -1, so the cut falls
	// back to the raw char limit and the footer still appends.
	s := "abcdefghijklmnop" // 16 chars, no newlines
	got := Cap(s, 2)        // limit = 8 chars

	if !strings.HasPrefix(got, "abcdefgh") {
		t.Fatalf("hard cut prefix wrong: %q", got)
	}
	if !strings.Contains(got, "omitted — re-run with a larger --budget") {
		t.Errorf("missing footer on hard cut: %q", got)
	}
}
