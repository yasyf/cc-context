package outline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

// row is a fixed-width (98-byte) generator line so a fixture's byte length — and
// thus its token estimate — is deterministic. anchor.Of(row(1)) is "geq8".
func row(i int) string {
	return fmt.Sprintf("row %03d ", i) + strings.Repeat("x", 90)
}

// gen builds n row lines, each terminated by '\n'. gen(w) also equals the head
// window's verbatim body for a window of w lines.
func gen(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(row(i))
		b.WriteByte('\n')
	}
	return b.String()
}

const mdContent = "# cc-context\n" +
	"\n" +
	"Intro prose.\n" +
	"\n" +
	"## Install\n" +
	"\n" +
	"```sh\n" +
	"# not a heading (backtick fence)\n" +
	"```\n" +
	"\n" +
	"### Homebrew\n" +
	"\n" +
	"~~~\n" +
	"## also not a heading (tilde fence)\n" +
	"~~~\n" +
	"\n" +
	"## Usage\n"

func mdHeadings(path string) string {
	return "# " + path + " — markdown headings\n" +
		"L1#kf1j    # cc-context\n" +
		"L5#a72w    ## Install\n" +
		"L11#hcr3   ### Homebrew\n" +
		"L17#a8ec   ## Usage\n"
}

func TestFallback(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		budget  int
		want    func(path string) string
	}{
		{
			name:    "markdown nested levels, fences skipped",
			file:    "README.md",
			content: mdContent,
			want:    mdHeadings,
		},
		{
			name:    "markdown CRLF hashes identically",
			file:    "readme_crlf.md",
			content: strings.ReplaceAll(mdContent, "\n", "\r\n"),
			want:    mdHeadings,
		},
		{
			name:    "markdown with no headings is header only",
			file:    "prose.md",
			content: "Just some prose.\nMore text here.\n",
			want: func(path string) string {
				return "# " + path + " — markdown headings\n"
			},
		},
		{
			name:    "head window over 40 lines with continuation",
			file:    "big.rb",
			content: gen(50),
			want: func(path string) string {
				return "# " + path + " — 50 lines, ~1.2k tokens — no ast-grep outline rules for .rb; head window\n" +
					"## " + path + ":1-40#geq8\n" +
					gen(40) +
					"… continue: ccx code read " + path + " --section 41-50\n"
			},
		},
		{
			name:    "file shorter than window has no continuation",
			file:    "small.rb",
			content: gen(10),
			want: func(path string) string {
				return "# " + path + " — 10 lines, ~247 tokens — no ast-grep outline rules for .rb; head window\n" +
					"## " + path + ":1-10#geq8\n" +
					gen(10)
			},
		},
		{
			name:    "extensionless file labels by basename",
			file:    "Dockerfile",
			content: gen(5),
			want: func(path string) string {
				return "# " + path + " — 5 lines, ~123 tokens — no ast-grep outline rules for Dockerfile; head window\n" +
					"## " + path + ":1-5#geq8\n" +
					gen(5)
			},
		},
		{
			name:    "small budget shrinks the window below 40",
			file:    "budget.rb",
			content: gen(50),
			budget:  125,
			want: func(path string) string {
				return "# " + path + " — 50 lines, ~1.2k tokens — no ast-grep outline rules for .rb; head window\n" +
					"## " + path + ":1-5#geq8\n" +
					gen(5) +
					"… continue: ccx code read " + path + " --section 6-50\n"
			},
		},
		{
			name:    "empty non-markdown file",
			file:    "empty.rb",
			content: "",
			want: func(path string) string {
				return "# " + path + " — empty file\n"
			},
		},
		{
			name:    "empty markdown file",
			file:    "empty.md",
			content: "",
			want: func(path string) string {
				return "# " + path + " — empty file\n"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.file)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			got, _, err := Fallback(path, backend.Args{Budget: tt.budget})
			if err != nil {
				t.Fatalf("Fallback: %v", err)
			}
			if want := tt.want(path); got != want {
				t.Errorf("Fallback mismatch\n got:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestFallbackMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.rb")
	if _, _, err := Fallback(path, backend.Args{}); err == nil {
		t.Fatal("Fallback on a missing file: want error, got nil")
	}
}
