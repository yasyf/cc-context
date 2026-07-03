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
