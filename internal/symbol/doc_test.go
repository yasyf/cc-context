package symbol

import (
	"strings"
	"testing"
)

func TestExtractDoc(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		startLine int
		lang      string
		want      []string // expected paragraphs
	}{
		{
			name: "go line comments",
			src: `// Foo does things.
// It returns nil.
func Foo() error {`,
			startLine: 3,
			lang:      "Go",
			want:      []string{"Foo does things.\nIt returns nil."},
		},
		{
			name: "go triple-slash",
			src: `/// Bar is special.
func Bar() {`,
			startLine: 2,
			lang:      "Go",
			want:      []string{"Bar is special."},
		},
		{
			name: "go block comment",
			src: `/* Baz wraps things.
   Second line. */
func Baz() {`,
			startLine: 3,
			lang:      "Go",
			want:      []string{"Baz wraps things.\nSecond line."},
		},
		{
			name: "jsdoc block",
			src: `/** Render draws it. */
render() {`,
			startLine: 2,
			lang:      "TypeScript",
			want:      []string{"Render draws it."},
		},
		{
			name: "no doc",
			src: `x := 1
func Foo() {`,
			startLine: 2,
			lang:      "Go",
			want:      nil,
		},
		{
			name: "blank line breaks contiguity",
			src: `// Not the doc.

func Foo() {`,
			startLine: 3,
			lang:      "Go",
			want:      nil,
		},
		{
			name: "python single-line docstring",
			src: `def render(self):
    """Draw it."""
    return 1`,
			startLine: 1,
			lang:      "Python",
			want:      []string{"Draw it."},
		},
		{
			name: "python multi-paragraph docstring",
			src: `class Widget:
    """A widget.

    Second paragraph.
    """
    pass`,
			startLine: 1,
			lang:      "Python",
			want:      []string{"A widget.", "Second paragraph."},
		},
		{
			name: "python hash-comment fallback when no docstring",
			src: `# helper does work.
def helper():
    return 1`,
			startLine: 2,
			lang:      "Python",
			want:      []string{"helper does work."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDoc(strings.Split(tt.src, "\n"), tt.startLine, tt.lang)
			if !equalSlice(got, tt.want) {
				t.Errorf("extractDoc() = %q, want %q", got, tt.want)
			}
		})
	}
}
