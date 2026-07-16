package ripgrep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

// namedEngine pairs a resolved engine with a label for subtest naming.
type namedEngine struct {
	name string
	eng  engine
	bin  string
}

// presentEngines returns every grep engine on PATH (rg preferred, then system
// grep), skipping the whole test when neither is installed. Matches drives them
// through searchGroups+toFileMatches so both backends are covered on one fixture.
func presentEngines(t *testing.T) []namedEngine {
	t.Helper()
	var out []namedEngine
	if bin, err := exec.LookPath("rg"); err == nil {
		out = append(out, namedEngine{"ripgrep", engineRipgrep, bin})
	}
	if bin, err := exec.LookPath("grep"); err == nil {
		out = append(out, namedEngine{"grep", engineGrep, bin})
	}
	if len(out) == 0 {
		t.Skip("need rg or grep on PATH")
	}
	return out
}

// matchesVia returns Matches-equivalent output for one specific engine, so a test
// asserts identical hits from rg and system grep over the same fixture.
func matchesVia(t *testing.T, e namedEngine, a backend.Args) []FileMatch {
	t.Helper()
	groups, err := searchGroups(context.Background(), e.eng, e.bin, a, execEngine)
	if err != nil {
		t.Fatalf("searchGroups(%s): %v", e.name, err)
	}
	return toFileMatches(groups)
}

// writeFixture writes one file into a fresh temp dir, chdirs into it, and returns
// the dir — the on-disk operand searchGroups searches.
func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	t.Chdir(dir)
	return dir
}

// TestMatches_SingleFileBothEngines proves Matches returns strict per-file,
// per-line hits identically from rg and system grep — match lines, an -A/-B/-C
// context line, and a clean no-match yielding zero FileMatches.
func TestMatches_SingleFileBothEngines(t *testing.T) {
	engines := presentEngines(t)
	writeFixture(t, "a.go", "package a\n\nconst x = 1\nvar needle = 1\n// needle note\n")

	tests := []struct {
		name string
		args backend.Args
		want []FileMatch
	}{
		{
			name: "plain literal matches",
			args: backend.Args{Query: "needle", Paths: []string{"a.go"}},
			want: []FileMatch{{Path: "a.go", Lines: []MatchLine{
				{Num: 4, Text: "var needle = 1", IsMatch: true},
				{Num: 5, Text: "// needle note", IsMatch: true},
			}}},
		},
		{
			name: "before-context line rides as a non-match",
			args: backend.Args{Query: "needle", Paths: []string{"a.go"}, Before: 1},
			want: []FileMatch{{Path: "a.go", Lines: []MatchLine{
				{Num: 3, Text: "const x = 1", IsMatch: false},
				{Num: 4, Text: "var needle = 1", IsMatch: true},
				{Num: 5, Text: "// needle note", IsMatch: true},
			}}},
		},
		{
			name: "clean no-match yields zero files",
			args: backend.Args{Query: "zzz", Paths: []string{"a.go"}},
			want: []FileMatch{},
		},
	}
	for _, e := range engines {
		for _, tt := range tests {
			t.Run(e.name+"/"+tt.name, func(t *testing.T) {
				if got := matchesVia(t, e, tt.args); !reflect.DeepEqual(got, tt.want) {
					t.Errorf("Matches()\n got = %+v\nwant = %+v", got, tt.want)
				}
			})
		}
	}
}

// TestMatches_Public drives the exported Matches entry point end to end (through
// resolveEngine), asserting the per-file hits across two files. File order is
// engine-dependent, so hits are indexed by path before the strict compare.
func TestMatches_Public(t *testing.T) {
	presentEngines(t) // skip when neither engine is installed
	writeFixture(t, "unused", "")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"a.go": "package a\n\nvar needle = 1\n",
		"b.go": "package a\n\nfunc use() int { return needle }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Matches(context.Background(), backend.Args{Query: "needle", Paths: []string{"a.go", "b.go"}})
	if err != nil {
		t.Fatalf("Matches() err = %v", err)
	}
	byPath := map[string][]MatchLine{}
	paths := make([]string, 0, len(got))
	for _, fm := range got {
		byPath[fm.Path] = fm.Lines
		paths = append(paths, fm.Path)
	}
	sort.Strings(paths)
	if want := []string{"a.go", "b.go"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("Matches() paths = %v, want %v", paths, want)
	}
	want := map[string][]MatchLine{
		"a.go": {{Num: 3, Text: "var needle = 1", IsMatch: true}},
		"b.go": {{Num: 3, Text: "func use() int { return needle }", IsMatch: true}},
	}
	for path, lines := range want {
		if !reflect.DeepEqual(byPath[path], lines) {
			t.Errorf("Matches()[%s]\n got = %+v\nwant = %+v", path, byPath[path], lines)
		}
	}
}

// TestMatches_ValidatesContext proves Matches rejects an out-of-range context
// request before touching an engine, exactly as Run does.
func TestMatches_ValidatesContext(t *testing.T) {
	if _, err := Matches(context.Background(), backend.Args{Query: "foo", Context: maxContext + 1}); err == nil {
		t.Fatal("Matches() err = nil, want context-cap error")
	}
}
