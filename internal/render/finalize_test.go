package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

var update = flag.Bool("update", false, "regenerate golden fixtures under testdata/")

// readFixture reads a testdata fixture by name.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// checkGolden compares got against the named golden file, or rewrites it under -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch\n got = %q\nwant = %q", name, got, string(want))
	}
}

func TestAnnotateGolden(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		golden string
		fn     func(string, *anchor.Files) string
	}{
		{"grep", "grep_input.txt", "grep_golden.txt", annotateGrep},
		{"symbol", "symbol_input.txt", "symbol_golden.txt", annotateSymbol},
		{"deps", "deps_input.txt", "deps_golden.txt", annotateDeps},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn(readFixture(t, tt.input), anchor.NewFiles("testdata"))
			checkGolden(t, tt.golden, got)
		})
	}
}

func TestAnnotateGrepInvariants(t *testing.T) {
	got := annotateGrep(readFixture(t, "grep_input.txt"), anchor.NewFiles("testdata"))
	tests := []struct {
		name string
		line string
	}{
		{"section header untouched", "### example.go:14,19 [2 usages in function Greeter]"},
		{"open range untouched", "  [4-]   imports: ("},
		{"fence header untouched", "```example.go:18-20"},
		{"gutter untouched", "  19 │ func (g *Greeter) Greet(name string) string {"},
		{"missing-file frame stays bare", "→ [8]   func gone() {}"},
		{"resolved frame anchored", "  [9-11#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(got, tt.line) {
				t.Errorf("want output to contain %q\n---\n%s", tt.line, got)
			}
		})
	}
}

func TestAnnotateSymbolInvariants(t *testing.T) {
	got := annotateSymbol(readFixture(t, "symbol_input.txt"), anchor.NewFiles("testdata"))
	tests := []struct {
		name string
		line string
	}{
		{"body bracket line stays bare", "  [14-16]   body bracket line, must stay bare"},
		{"missing-file caller stays bare", "    [40]   in ghost()"},
		{"trailer untouched", "... and 0 more"},
		{"grok header anchored", "# grok: Greet [example.go:19#"},
		{"caller anchored", "    [25#"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(got, tt.line) {
				t.Errorf("want output to contain %q\n---\n%s", tt.line, got)
			}
		})
	}
}

// TestAnnotateGrepCRLF proves a CRLF-terminated grep section header still sets
// the active file, so the following frame anchors against it and keeps its "\r".
func TestAnnotateGrepCRLF(t *testing.T) {
	hash := string(anchor.Of("func (g *Greeter) Greet(name string) string {"))
	in := "### example.go:19\r\n" +
		"  [19]   func (g *Greeter) Greet(name string) string {\r\n"
	want := "### example.go:19\r\n" +
		"  [19#" + hash + "]   func (g *Greeter) Greet(name string) string {\r\n"
	if got := annotateGrep(in, anchor.NewFiles("testdata")); got != want {
		t.Errorf("annotateGrep()\n got:\n%q\nwant:\n%q", got, want)
	}
}

// TestAnnotateSymbolCRLF proves a CRLF-terminated "## siblings (path)" header
// still enters the siblings section, so the row anchors and keeps its "\r".
func TestAnnotateSymbolCRLF(t *testing.T) {
	hash := string(anchor.Of("func NewGreeter(prefix string) *Greeter {"))
	in := "## siblings (example.go)\r\n" +
		"NewGreeter               [14-16]   func NewGreeter(prefix string) *Greeter\r\n"
	want := "## siblings (example.go)\r\n" +
		"NewGreeter               [14-16#" + hash + "]   func NewGreeter(prefix string) *Greeter\r\n"
	if got := annotateSymbol(in, anchor.NewFiles("testdata")); got != want {
		t.Errorf("annotateSymbol()\n got:\n%q\nwant:\n%q", got, want)
	}
}

func TestAnnotateDepsInvariants(t *testing.T) {
	in := readFixture(t, "deps_input.txt")
	got := annotateDeps(in, anchor.NewFiles("testdata"))

	// Lines outside the "## Used by" block survive byte-identical: the "# Deps:"
	// header, the whole "## Uses (local)" block, the tail, and the footer. Pulling
	// them straight from the input sidesteps hard-coding their column padding.
	var passthrough []string
	for _, ln := range strings.Split(in, "\n") {
		if strings.HasPrefix(ln, "# Deps:") ||
			strings.HasPrefix(ln, "## Uses (local)") ||
			strings.HasPrefix(ln, "example_helper.go") ||
			strings.HasPrefix(ln, "... and ") ||
			strings.HasPrefix(ln, "[~") {
			passthrough = append(passthrough, ln)
		}
	}
	if len(passthrough) != 5 {
		t.Fatalf("fixture drift: want 5 passthrough lines, found %d", len(passthrough))
	}
	tests := []struct {
		name string
		want string
	}{
		{"first-appearance group heading", "### example.go"},
		{"second group heading", "### missing.go"},
		{"anchored row 14", "L14#"},
		{"anchored row 19", "L19#"},
		{"anchored row 24", "L24#"},
		{"out-of-range line stays bare", "L999 phantom"},
		{"missing-file row stays bare", "L40 ghost"},
		{"duplicate symbols preserved", "→ Greet, Greet"},
	}
	for _, ln := range passthrough {
		if !strings.Contains(got, ln) {
			t.Errorf("passthrough line %q missing from output\n---\n%s", ln, got)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(got, tt.want) {
				t.Errorf("want output to contain %q\n---\n%s", tt.want, got)
			}
		})
	}
	// The path column is dropped: no row keeps its raw "path:line" prefix.
	if strings.Contains(got, "example.go:14") || strings.Contains(got, "missing.go:40") {
		t.Errorf("raw path:line prefix survived regrouping\n---\n%s", got)
	}
	// A bare span never grows a "#" hash.
	if strings.Contains(got, "L999#") || strings.Contains(got, "L40#") {
		t.Errorf("unresolvable row was anchored\n---\n%s", got)
	}
}

// TestAnnotateDepsCRLF proves a CRLF "## Used by" row anchors, keeps its "\r", and
// carries "\r" onto the inserted "### path" heading.
func TestAnnotateDepsCRLF(t *testing.T) {
	hash := string(anchor.Of("func (g *Greeter) Greet(name string) string {"))
	in := "## Used by\r\n" +
		"example.go:19  callGreet             → Greet\r\n"
	got := annotateDeps(in, anchor.NewFiles("testdata"))
	wantRow := "L19#" + hash + " callGreet             → Greet\r"
	if !strings.Contains(got, wantRow) {
		t.Errorf("CRLF row not anchored/preserved\n got: %q\nwant substr: %q", got, wantRow)
	}
	if !strings.Contains(got, "### example.go\r") {
		t.Errorf("CRLF heading dropped its \\r\n got: %q", got)
	}
}

// TestAnnotateDepsNoUsedBySection proves output with no "## Used by" block passes
// through byte-identical.
func TestAnnotateDepsNoUsedBySection(t *testing.T) {
	in := "# Deps: example.go — 0 local, 0 external, 0 dependents\n" +
		"\n" +
		"## Uses (local)\n" +
		"example.go                     Greet\n" +
		"\n" +
		"[~12 tokens]\n"
	if got := annotateDeps(in, anchor.NewFiles("testdata")); got != in {
		t.Errorf("annotateDeps changed output with no \"## Used by\"\n got: %q\nwant: %q", got, in)
	}
}

// TestAnnotateDepsGroupsNonConsecutive proves a later row for an already-seen file
// joins that file's earlier group under a single heading, in first-appearance order.
func TestAnnotateDepsGroupsNonConsecutive(t *testing.T) {
	in := "## Used by\n" +
		"example.go:14  a → x\n" +
		"missing.go:40  b → y\n" +
		"example.go:19  c → z\n" +
		"[~9 tokens]\n"
	got := annotateDeps(in, anchor.NewFiles("testdata"))
	if n := strings.Count(got, "### example.go"); n != 1 {
		t.Fatalf("want exactly one example.go heading, got %d\n---\n%s", n, got)
	}
	order := []string{"### example.go", "→ x", "→ z", "### missing.go", "→ y"}
	prev := -1
	for _, tok := range order {
		i := strings.Index(got, tok)
		if i <= prev {
			t.Fatalf("token %q out of order (idx %d after %d)\n---\n%s", tok, i, prev, got)
		}
		prev = i
	}
}

// TestAnnotateDepsProseSplitsGroup proves an unmatched prose line between two rows
// of the same file stays in its original position and splits the file into two
// groups, so the "### path" heading is emitted twice.
func TestAnnotateDepsProseSplitsGroup(t *testing.T) {
	files := anchor.NewFiles("testdata")
	l14, _ := files.LineAt("example.go", 14)
	l19, _ := files.LineAt("example.go", 19)
	in := "## Used by\n" +
		"example.go:14  a → x\n" +
		"interrupting prose\n" +
		"example.go:19  c → z\n" +
		"[~9 tokens]\n"
	want := "## Used by\n" +
		"### example.go\n" +
		"L14#" + string(anchor.Of(l14)) + " a → x\n" +
		"interrupting prose\n" +
		"### example.go\n" +
		"L19#" + string(anchor.Of(l19)) + " c → z\n" +
		"[~9 tokens]\n"
	got := annotateDeps(in, files)
	if got != want {
		t.Errorf("annotateDeps()\n got: %q\nwant: %q", got, want)
	}
	if n := strings.Count(got, "### example.go"); n != 2 {
		t.Errorf("want the group split into 2 headings, got %d\n---\n%s", n, got)
	}
}

// TestAnnotateDepsSpacePathPassthrough proves a row whose path holds a space fails
// the row grammar and passes through verbatim at its original position, un-anchored.
func TestAnnotateDepsSpacePathPassthrough(t *testing.T) {
	files := anchor.NewFiles("testdata")
	l14, _ := files.LineAt("example.go", 14)
	in := "## Used by\n" +
		"example.go:14  a → x\n" +
		"my file.go:20  spaced → y\n" +
		"[~9 tokens]\n"
	want := "## Used by\n" +
		"### example.go\n" +
		"L14#" + string(anchor.Of(l14)) + " a → x\n" +
		"my file.go:20  spaced → y\n" +
		"[~9 tokens]\n"
	got := annotateDeps(in, files)
	if got != want {
		t.Errorf("annotateDeps()\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "L20") {
		t.Errorf("space-path row was anchored\n---\n%s", got)
	}
}

// TestAnnotateDepsLineCountInvariant proves the walker never loses or duplicates a
// line: output line count equals input line count plus the "### " headings inserted.
func TestAnnotateDepsLineCountInvariant(t *testing.T) {
	files := anchor.NewFiles("testdata")
	cases := []struct {
		name string
		in   string
	}{
		{"real fixture", readFixture(t, "deps_input.txt")},
		{"prose split", "## Used by\nexample.go:14  a → x\nprose\nexample.go:19  c → z\n[~9 tokens]\n"},
		{"space path", "## Used by\nexample.go:14  a → x\nmy file.go:20  s → y\n[~9 tokens]\n"},
		{"non-consecutive", "## Used by\nexample.go:14  a → x\nmissing.go:40  b → y\nexample.go:19  c → z\n[~9 tokens]\n"},
		{"no used-by", "# Deps: x\n\n## Uses (local)\nx  Greet\n\n[~12 tokens]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inN := len(strings.Split(tc.in, "\n"))
			outLines := strings.Split(annotateDeps(tc.in, files), "\n")
			headings := 0
			for _, ln := range outLines {
				if strings.HasPrefix(ln, "### ") {
					headings++
				}
			}
			if got := len(outLines) - headings; got != inN {
				t.Errorf("line count drift: out=%d headings=%d out-headings=%d want inN=%d",
					len(outLines), headings, got, inN)
			}
		})
	}
}

func TestFinalizeDefaultOpPassesThrough(t *testing.T) {
	// A non-anchoring op just caps; the payload is byte-identical below budget.
	in := "line one\nline two\n"
	got, err := Finalize(backend.OpFind, in, 0)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got != in {
		t.Errorf("Finalize(OpFind) = %q, want %q", got, in)
	}
}
