package render

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
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
			err := RunCLIStream(context.Background(), Ambient, "/bin/sh", []string{"-c", tt.script}, &w)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIStream err = %v, wantErr %v", err, tt.wantErr)
			}
			if w.String() != tt.want {
				t.Errorf("streamed output = %q, want %q", w.String(), tt.want)
			}
		})
	}
}

// TestRunCLIRunTimeout drives the runaway guard through runTimeout in
// milliseconds: a child that would block for 30s must come back killed and named
// as the deadline, not as an opaque signal exit; a caller's own deadline wins in
// either direction — a tighter one is never misreported as the guard firing, and
// a longer one lets the child run past runTimeout, which is the whole point of
// ship's CI watch outliving any default bound.
func TestRunCLIRunTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to block")
	}
	tests := []struct {
		name    string
		timeout time.Duration
		parent  time.Duration // caller's own deadline; 0 leaves it unbounded
		script  string        // sh -c body
		wantOut string        // stdout of a child the bound let finish; "" expects a failure
		want    string        // substring the error must carry
		wantNot string        // substring the error must not carry
	}{
		{
			name:    "a blocked child is killed and reported as the deadline",
			timeout: 50 * time.Millisecond,
			script:  "sleep 30",
			want:    "/bin/sh did not finish within 50ms and was killed",
		},
		{
			name:    "a caller's tighter deadline wins and is not read as the guard",
			timeout: time.Minute,
			parent:  50 * time.Millisecond,
			script:  "sleep 30",
			wantNot: "did not finish within",
		},
		{
			name:    "a caller's longer deadline wins and the child outlives runTimeout",
			timeout: 50 * time.Millisecond,
			parent:  30 * time.Second,
			script:  "sleep 1; printf concluded",
			wantOut: "concluded",
		},
		{
			name:    "a real failure still wraps the child's stderr",
			timeout: time.Minute,
			script:  "echo boom 1>&2; exit 3",
			want:    "boom",
			wantNot: "did not finish within",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := runTimeout
			runTimeout = tt.timeout
			t.Cleanup(func() { runTimeout = restore })

			ctx := context.Background()
			if tt.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.parent)
				defer cancel()
			}

			start := time.Now()
			out, err := RunCLI(ctx, Ambient, "/bin/sh", []string{"-c", tt.script})
			elapsed := time.Since(start)
			if tt.wantOut != "" {
				if err != nil {
					t.Fatalf("RunCLI err = %v, want the caller's %s deadline to let the child finish", err, tt.parent)
				}
				if out != tt.wantOut {
					t.Errorf("RunCLI = %q, want %q", out, tt.wantOut)
				}
				return
			}
			if err == nil {
				t.Fatalf("RunCLI = %q, want a failure", out)
			}
			if elapsed > 10*time.Second {
				t.Errorf("RunCLI returned after %s; the child outlived its bound", elapsed)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("RunCLI err = %v, want it to carry %q", err, tt.want)
			}
			if tt.wantNot != "" && strings.Contains(err.Error(), tt.wantNot) {
				t.Errorf("RunCLI err = %v, want it not to carry %q", err, tt.wantNot)
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
			got, err := RunCLIAllowExit(context.Background(), Ambient, "/bin/sh", []string{"-c", tt.script}, tt.okCodes...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIAllowExit err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("RunCLIAllowExit = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunCLIEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to read env")
	}
	tests := []struct {
		name     string
		script   string
		extraEnv []string
		want     string
		wantErr  bool
	}{
		{"extra var reaches the child", `printf '%s' "$CCX_TEST_VAR"`, []string{"CCX_TEST_VAR=idxfile"}, "idxfile", false},
		{"last value wins over an inherited one", `printf '%s' "$PATH_TEST"`, []string{"PATH_TEST=a", "PATH_TEST=b"}, "b", false},
		{"os.Environ is still inherited", `printf '%s' "${HOME:+has-home}"`, nil, "has-home", false},
		{"nonzero exit wraps stderr", "echo boom 1>&2; exit 1", nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RunCLIEnv(context.Background(), Ambient, "/bin/sh", []string{"-c", tt.script}, tt.extraEnv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIEnv err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("RunCLIEnv = %q, want %q", got, tt.want)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "boom") {
				t.Errorf("RunCLIEnv err = %v, want it to carry the child stderr", err)
			}
		})
	}
}

func TestRunCLIStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to echo stdin")
	}
	tests := []struct {
		name    string
		script  string
		stdin   []byte
		want    string
		wantErr bool
	}{
		{"stdin is fed to the child", "cat", []byte("selected\ncontent\n"), "selected\ncontent\n", false},
		{"empty stdin", "cat", nil, "", false},
		{"child that ignores stdin still succeeds", "printf oid", []byte("ignored payload"), "oid", false},
		{"nonzero exit wraps stderr", "echo boom 1>&2; exit 1", []byte("x"), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RunCLIStdin(context.Background(), Ambient, "/bin/sh", []string{"-c", tt.script}, tt.stdin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunCLIStdin err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("RunCLIStdin = %q, want %q", got, tt.want)
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
		{
			// No newline to cut at, so the fallback cut (limit=8) must snap back
			// to the rune boundary at byte 6, not split the third 世 mid-rune.
			id:     "no newline snaps the cut to a rune boundary",
			s:      "世世世世世",
			budget: 2,
			want:   "世世\n… +1 lines, ~2 tokens omitted — re-run with a larger --budget\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := Cap(tt.s, tt.budget)
			if got != tt.want {
				t.Errorf("Cap(%q, %d) = %q, want %q", tt.s, tt.budget, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Cap(%q, %d) = %q, not valid UTF-8", tt.s, tt.budget, got)
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

func TestPinnedRunDropsGitLocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh to read the environment")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	t.Setenv("GIT_DIR", "/tmp/wrong/.git")
	t.Setenv("GIT_WORK_TREE", "/tmp/wrong")
	t.Setenv("GIT_INDEX_FILE", "/tmp/wrong/index")

	script := `printf 'GIT_DIR=%s GIT_WORK_TREE=%s GIT_INDEX_FILE=%s cwd=%s' "${GIT_DIR-<unset>}" "${GIT_WORK_TREE-<unset>}" "${GIT_INDEX_FILE-<unset>}" "$(pwd)"`
	got, err := RunCLI(context.Background(), Dir(dir), "/bin/sh", []string{"-c", script})
	if err != nil {
		t.Fatalf("RunCLI err = %v", err)
	}
	want := "GIT_DIR=<unset> GIT_WORK_TREE=<unset> GIT_INDEX_FILE=/tmp/wrong/index cwd=" + dir
	if got != want {
		t.Errorf("pinned child env = %q, want %q", got, want)
	}

	got, err = RunCLI(context.Background(), Ambient, "/bin/sh", []string{"-c", script})
	if err != nil {
		t.Fatalf("RunCLI err = %v", err)
	}
	if !strings.HasPrefix(got, "GIT_DIR=/tmp/wrong/.git GIT_WORK_TREE=/tmp/wrong ") {
		t.Errorf("ambient child env = %q, want the inherited GIT_DIR and GIT_WORK_TREE", got)
	}
}
