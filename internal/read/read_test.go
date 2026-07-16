package read

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
)

// write drops content at name under a fresh temp dir and returns the path.
func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestRunOutput(t *testing.T) {
	const goFile = "alpha\nbravo\ncharlie\ndelta\n"
	const mdFile = "# Title\nintro\n\n## Foo\nfoo body\nline\n\n## Bar\nbar body\n"

	tests := []struct {
		name    string
		file    string // written under a temp dir with this basename
		content string
		args    backend.Args
		want    string // exact expected output; %H is replaced by anchor.Of(firstServed)
		first   string // first served line, hashed to fill %H
	}{
		{
			name:    "full read",
			file:    "a.go",
			content: goFile,
			args:    backend.Args{Full: true},
			first:   "alpha",
			want:    "# read %P:1-4#%H (4 lines)\nalpha\nbravo\ncharlie\ndelta\n",
		},
		{
			name:    "no section is whole file",
			file:    "a.go",
			content: goFile,
			args:    backend.Args{},
			first:   "alpha",
			want:    "# read %P:1-4#%H (4 lines)\nalpha\nbravo\ncharlie\ndelta\n",
		},
		{
			name:    "numeric section",
			file:    "a.go",
			content: goFile,
			args:    backend.Args{Section: "2-3"},
			first:   "bravo",
			want:    "# read %P:2-3#%H (2 of 4 lines)\nbravo\ncharlie\n",
		},
		{
			name:    "single-line section",
			file:    "a.go",
			content: goFile,
			args:    backend.Args{Section: "3"},
			first:   "charlie",
			want:    "# read %P:3#%H (1 of 4 lines)\ncharlie\n",
		},
		{
			name:    "end clamped to EOF",
			file:    "a.go",
			content: goFile,
			args:    backend.Args{Section: "3-999"},
			first:   "charlie",
			want:    "# read %P:3-4#%H (2 of 4 lines)\ncharlie\ndelta\n",
		},
		{
			name:    "markdown heading hit",
			file:    "d.md",
			content: mdFile,
			args:    backend.Args{Section: "## Foo"},
			first:   "## Foo",
			want:    "# read %P:4-7#%H (4 of 9 lines)\n## Foo\nfoo body\nline\n\n",
		},
		{
			name:    "markdown heading to EOF",
			file:    "d.md",
			content: mdFile,
			args:    backend.Args{Section: "## Bar"},
			first:   "## Bar",
			want:    "# read %P:8-9#%H (2 of 9 lines)\n## Bar\nbar body\n",
		},
		{
			name:    "markdown heading unique prefix",
			file:    "d.md",
			content: mdFile,
			args:    backend.Args{Section: "## Ba"},
			first:   "## Bar",
			want:    "# read %P:8-9#%H (2 of 9 lines)\n## Bar\nbar body\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, tt.file, tt.content)
			tt.args.Path = path
			got, err := Run(tt.args)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			want := strings.ReplaceAll(tt.want, "%P", path)
			want = strings.ReplaceAll(want, "%H", string(anchor.Of(tt.first)))
			if got != want {
				t.Errorf("Run() =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	const goFile = "alpha\nbravo\ncharlie\n"
	const mdFile = "# Title\n\n## Foo\nbody\n"

	tests := []struct {
		name    string
		file    string
		content string
		section string
		wantSub string
	}{
		{
			name:    "start past EOF",
			file:    "a.go",
			content: goFile,
			section: "500-520",
			wantSub: "starts past EOF (file has 3 lines)",
		},
		{
			name:    "reversed range",
			file:    "a.go",
			content: goFile,
			section: "3-1",
			wantSub: "reversed",
		},
		{
			name:    "heading on non-markdown redirects",
			file:    "a.go",
			content: goFile,
			section: "## Foo",
			wantSub: "ccx code symbol <name> --body",
		},
		{
			name:    "heading miss lists headings",
			file:    "d.md",
			content: mdFile,
			section: "## Nope",
			wantSub: "Headings:\n  # Title\n  ## Foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, tt.file, tt.content)
			_, err := Run(backend.Args{Path: path, Section: tt.section})
			if err == nil {
				t.Fatalf("Run() error = nil, want error containing %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Run() error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestFencedHeadingSkipped proves a "#"-prefixed line inside a fenced code block
// is not a heading: requesting it misses, while the real heading around it resolves.
func TestFencedHeadingSkipped(t *testing.T) {
	const doc = "# Doc\n```\n# not a heading\n```\n## Section\nbody\n"
	path := write(t, "d.md", doc)

	if _, err := Run(backend.Args{Path: path, Section: "# not a heading"}); err == nil {
		t.Fatal("Run() on fenced pseudo-heading = nil error, want miss")
	} else if !strings.Contains(err.Error(), "no matching heading") {
		t.Errorf("Run() error = %q, want a heading miss", err.Error())
	}

	got, err := Run(backend.Args{Path: path, Section: "## Section"})
	if err != nil {
		t.Fatalf("Run(## Section) error = %v", err)
	}
	want := "# read " + path + ":5-6#" + string(anchor.Of("## Section")) + " (2 of 6 lines)\n## Section\nbody\n"
	if got != want {
		t.Errorf("Run(## Section) =\n%q\nwant\n%q", got, want)
	}
}

// TestTildeFenceHeadingSkipped proves a "~~~" CommonMark fence hides its inner
// "#"-headings from resolution and the miss list, just as a "```" fence does.
func TestTildeFenceHeadingSkipped(t *testing.T) {
	const doc = "## Real\nreal body\n~~~\n## fake\n~~~\nmore real\n"
	path := write(t, "d.md", doc)

	// "## Real" spans the whole doc: "## fake" inside the ~~~ fence is invisible.
	got, err := Run(backend.Args{Path: path, Section: "## Real"})
	if err != nil {
		t.Fatalf("Run(## Real) error = %v", err)
	}
	want := "# read " + path + ":1-6#" + string(anchor.Of("## Real")) + " (6 of 6 lines)\n" + doc
	if got != want {
		t.Errorf("Run(## Real) =\n%q\nwant\n%q", got, want)
	}

	// "## fake" lives inside the fence: it neither resolves nor appears in the list.
	_, err = Run(backend.Args{Path: path, Section: "## fake"})
	if err == nil {
		t.Fatal("Run(## fake) = nil error, want a heading miss")
	}
	if !strings.Contains(err.Error(), "no matching heading") {
		t.Errorf("Run(## fake) error = %q, want a heading miss", err.Error())
	}
	if _, list, _ := strings.Cut(err.Error(), "Headings:"); strings.Contains(list, "fake") {
		t.Errorf("miss list must not contain the fenced heading: %q", list)
	}
}

// TestBinarySkip proves a binary file returns outline.BinarySkip's row verbatim.
func TestBinarySkip(t *testing.T) {
	path := write(t, "blob.bin", "PK\x03\x04\x00\x00\x00\x00binary\x00payload")
	want, ok := outline.BinarySkip(path)
	if !ok {
		t.Fatalf("BinarySkip(%s) did not classify the fixture as binary", path)
	}
	got, err := Run(backend.Args{Path: path, Full: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}

// TestCRLFRoundTrip proves CRLF line endings survive verbatim and the header hash
// matches anchor.FromBytes semantics: anchor.Of trims the trailing '\r', so the
// emitted anchor resolves back to the same start line.
func TestCRLFRoundTrip(t *testing.T) {
	path := write(t, "crlf.go", "line1\r\nline2\r\nline3\r\n")
	got, err := Run(backend.Args{Full: true, Path: path})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantHash := string(anchor.Of("line1\r"))
	want := "# read " + path + ":1-3#" + wantHash + " (3 lines)\nline1\r\nline2\r\nline3\r\n"
	if got != want {
		t.Fatalf("Run() =\n%q\nwant\n%q", got, want)
	}
	// The emitted anchor round-trips through the resolver back to line 1.
	ref, ok, err := anchor.Parse("1-3#" + wantHash)
	if err != nil || !ok {
		t.Fatalf("Parse(emitted anchor) ok=%v err=%v", ok, err)
	}
	f, err := anchor.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	rng, move, err := f.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if move != nil || rng.Start != 1 || rng.End != 3 {
		t.Errorf("Resolve() = %+v move=%v, want {1 3} no-move", rng, move)
	}
}

// TestEmptyFile proves a zero-byte file returns the empty-file header, not a panic.
func TestEmptyFile(t *testing.T) {
	path := write(t, "empty.go", "")
	got, err := Run(backend.Args{Full: true, Path: path})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := "# read " + path + ": empty file\n"; got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}
