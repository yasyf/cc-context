package render

import (
	"bytes"
	"context"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRunCLIStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to script output")
	}
	tests := []struct {
		name    string
		script  string // sh -c body
		want    string // combined stdout+stderr written to w
		wantErr bool
	}{
		{
			"stdout and stderr both flow to w",
			"printf out; printf err 1>&2",
			"outerr",
			false,
		},
		{
			"nonzero exit is reported, output already streamed",
			"printf partial; exit 3",
			"partial",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w bytes.Buffer
			err := RunCLIStream(context.Background(), "/bin/sh", []string{"-c", tt.script}, &w)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIStream err = %v, wantErr %v", err, tt.wantErr)
			}
			if w.String() != tt.want {
				t.Errorf("streamed output = %q, want %q", w.String(), tt.want)
			}
		})
	}
}

func TestRunCLIAllowExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to script exit codes")
	}
	tests := []struct {
		name    string
		script  string // sh -c body
		okCodes []int
		want    string
		wantErr bool
	}{
		{
			"exit 0 returns stdout",
			"printf hello",
			[]int{1},
			"hello",
			false,
		},
		{
			"tolerated exit 1, empty stderr → stdout (ast-grep clean no-match)",
			"exit 1",
			[]int{1},
			"",
			false,
		},
		{
			"tolerated exit 1 with stdout returns it",
			"printf 'match'; exit 1",
			[]int{1},
			"match",
			false,
		},
		{
			"tolerated exit 1 WITH stderr → error (real failure)",
			"echo boom 1>&2; exit 1",
			[]int{1},
			"",
			true,
		},
		{
			"non-listed nonzero exit → error",
			"echo usage 1>&2; exit 2",
			[]int{1},
			"",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RunCLIAllowExit(context.Background(), "/bin/sh", []string{"-c", tt.script}, tt.okCodes...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIAllowExit err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("RunCLIAllowExit = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestCapContinuation(t *testing.T) {
	tests := []struct {
		id     string
		span   string
		offset int
		budget int
		want   string
	}{
		{
			id:     "empty span serves empty", // empty HTTP 200 markdown body + full read
			span:   "",
			offset: 0,
			budget: 5,
			want:   "",
		},
		{
			id:     "non-positive budget serves from the offset to the end uncapped",
			span:   "abcdefgh",
			offset: 1, // startRaw 4, rune-aligned
			budget: 0,
			want:   "efgh",
		},
		{
			id:     "window reaching the end passes through from the offset",
			span:   "short\n",
			offset: 0,
			budget: 100,
			want:   "short\n",
		},
		{
			id:     "over budget from offset 0 names offset+budget as the next offset",
			span:   "aaaa\nbbbb\ncccc\ndddd\neeee\n",
			offset: 0,
			budget: 2, // window [0,8); snapped end 8 lands on a rune start
			want:   "aaaa\nbbb\n… +4 lines, ~4 tokens omitted — re-run with --offset 2 to continue, or a larger --budget\n",
		},
		{
			id:     "next offset is offset+budget regardless of the requested offset",
			span:   "aaaa\nbbbb\ncccc\ndddd\neeee\n",
			offset: 1,
			budget: 2, // window [4,12); serves span[4:12]
			want:   "\nbbbb\ncc\n… +3 lines, ~3 tokens omitted — re-run with --offset 3 to continue, or a larger --budget\n",
		},
		{
			id:     "boundary snaps backward off a multi-byte rune so it is never split",
			span:   "ab😀cd", // emoji occupies bytes 2..5; window [0,4) lands mid-rune
			offset: 0,
			budget: 1,
			// remainder "😀cd" has no trailing newline, so its unterminated line counts.
			want: "ab\n… +1 lines, ~1 tokens omitted — re-run with --offset 1 to continue, or a larger --budget\n",
		},
		{
			id:     "unterminated final line is counted in the footer",
			span:   "abcdef", // window [0,4) keeps "abcd"; remainder "ef" has no newline
			offset: 0,
			budget: 1,
			want:   "abcd\n… +1 lines, ~0 tokens omitted — re-run with --offset 1 to continue, or a larger --budget\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := CapContinuation(tt.span, tt.offset, tt.budget); got != tt.want {
				t.Errorf("CapContinuation(%q, %d, %d)\n got = %q\nwant = %q", tt.span, tt.offset, tt.budget, got, tt.want)
			}
		})
	}
}

// contFooterRe matches a whole continuation footer so a paged read's served content
// can be recovered by stripping it (an unfootered final page is left unchanged).
var contFooterRe = regexp.MustCompile(`\n… \+\d+ lines, ~\d+ tokens omitted — re-run with --offset \d+ to continue, or a larger --budget\n\z`)

// TestCapContinuationInvalidUTF8Bounded drives an adversarial run of bare UTF-8
// continuation bytes: an unbounded backward snap to the nearest rune start would
// collapse whole windows to empty pages then dump one giant page. The bounded
// walk-back caps every page's content at budget*charsPerToken+utf8.UTFMax-1 bytes,
// keeps paging monotonic and terminating, and still joins back to the exact span.
func TestCapContinuationInvalidUTF8Bounded(t *testing.T) {
	const budget = 1
	const maxContent = budget*charsPerToken + utf8.UTFMax - 1
	span := "A" + strings.Repeat("\x80", 64) + "Z"

	var sb strings.Builder
	offset := 0
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatalf("paging did not terminate within 1000 pages")
		}
		out := CapContinuation(span, offset, budget)
		content := contFooterRe.ReplaceAllString(out, "")
		if len(content) > maxContent {
			t.Fatalf("page at offset %d served %d bytes, want <= %d:\n%q", offset, len(content), maxContent, content)
		}
		sb.WriteString(content)
		if content == out { // nothing stripped: the final, unfootered page
			break
		}
		offset += budget // the footer names offset+budget as the next offset
	}
	if got := sb.String(); got != span {
		t.Errorf("reconstruction mismatch:\n got = %q\nwant = %q", got, span)
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
