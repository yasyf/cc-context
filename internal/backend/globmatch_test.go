package backend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var globFixture = []string{
	"internal/a.go",
	"internal/cli/b.go",
	"internal/cli/c.md",
	"internal/sub/d.go",
	"keep/cli/h.go",
	"keep/g.go",
	"other/e.go",
	"other/f.md",
	"top.go",
	"top.md",
}

// Every want below is real rg's verdict over a tree of globFixture's paths.
func TestMatchGlobs(t *testing.T) {
	tests := []struct {
		name  string
		globs []string
		want  []string
	}{
		{"empty list selects everything", nil, globFixture},
		{"single include", []string{"*.go"}, []string{"internal/a.go", "internal/cli/b.go", "internal/sub/d.go", "keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"several includes", []string{"*.md", "*.go"}, globFixture},
		{"single exclusion", []string{"!*.go"}, []string{"internal/cli/c.md", "other/f.md", "top.md"}},
		{"several exclusions", []string{"!*.md", "!internal"}, []string{"keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"include then exclusion cancels", []string{"*.go", "!*.go"}, nil},
		{"exclusion then include restores", []string{"!*.go", "*.go"}, []string{"internal/a.go", "internal/cli/b.go", "internal/sub/d.go", "keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"include then dir exclusion", []string{"*.go", "!internal"}, []string{"keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"dir exclusion then include", []string{"!internal", "*.go"}, []string{"keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"exclusion only, dir form", []string{"!internal/"}, []string{"keep/cli/h.go", "keep/g.go", "other/e.go", "other/f.md", "top.go", "top.md"}},
		{"slash-less matches basename at depth", []string{"b.go"}, []string{"internal/cli/b.go"}},
		{"slashed matches the whole path", []string{"internal/cli/b.go"}, []string{"internal/cli/b.go"}},
		{"slashed is never operand-relative", []string{"cli/b.go"}, nil},
		{"brace alternation", []string{"{*.go,*.md}"}, globFixture},
		{"bare directory selects no file", []string{"internal"}, nil},
		{"bare directory with slash selects no file", []string{"internal/"}, nil},
		{"ancestor prune beats a later include", []string{"!cli", "*.go"}, []string{"internal/a.go", "internal/sub/d.go", "keep/g.go", "other/e.go", "top.go"}},
		{"doublestar prunes subdirs, not the root", []string{"!internal/**", "*.go"}, []string{"internal/a.go", "keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"later include rescues a pruned directory", []string{"!internal", "internal", "*.go"}, []string{"internal/a.go", "internal/cli/b.go", "internal/sub/d.go", "keep/cli/h.go", "keep/g.go", "other/e.go", "top.go"}},
		{"subtree include then extension exclusion", []string{"internal/**", "!*.md"}, []string{"internal/a.go", "internal/cli/b.go", "internal/sub/d.go"}},
		{"trailing doublestar reaches depth one", []string{"internal/**"}, []string{"internal/a.go", "internal/cli/b.go", "internal/cli/c.md", "internal/sub/d.go"}},
		{"doublestar spans zero segments", []string{"internal/**/a.go"}, []string{"internal/a.go"}},
		{"doublestar spans zero or more segments", []string{"internal/**/*.go"}, []string{"internal/a.go", "internal/cli/b.go", "internal/sub/d.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectGlobs(t, globFixture, tt.globs)
			if !slices.Equal(got, tt.want) {
				t.Errorf("MatchGlobs(%q) selected %q, want %q", tt.globs, got, tt.want)
			}
		})
	}
}

func TestMatchGlobsBadPattern(t *testing.T) {
	if _, err := MatchGlobs("a.go", []string{"["}); err == nil {
		t.Fatal("MatchGlobs([) = nil error, want a bad-pattern error")
	}
}

func TestSharedGlobAnchor(t *testing.T) {
	tests := []struct {
		name  string
		globs []string
		want  string
	}{
		{"empty set", nil, ""},
		{"slash-less include", []string{"*.go"}, ""},
		{"single anchored include", []string{"internal/cli/*.go"}, "internal/cli"},
		{"same anchor twice", []string{"internal/*.go", "internal/*.md"}, "internal"},
		{"differing anchors", []string{"internal/*.go", "other/*.md"}, ""},
		{"nested anchors are not shared", []string{"internal/*.go", "internal/cli/*.md"}, ""},
		{"one unanchored include cancels", []string{"internal/*.go", "*.md"}, ""},
		{"exclusions are skipped", []string{"internal/*.go", "!*_test.go"}, "internal"},
		{"exclusion-only set never anchors", []string{"!internal/*.go"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SharedGlobAnchor(tt.globs); got != tt.want {
				t.Errorf("SharedGlobAnchor(%q) = %q, want %q", tt.globs, got, tt.want)
			}
		})
	}
}

// globPathsFixture writes a.go, b.txt, and a sub/ directory into a fresh temp dir
// and chdirs into it — the on-disk operands FilterGlobPaths classifies with os.Stat.
func globPathsFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"a.go", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	t.Chdir(dir)
}

func TestFilterGlobPaths(t *testing.T) {
	globPathsFixture(t)
	tests := []struct {
		name    string
		paths   []string
		globs   []string
		want    []string
		wantErr bool
	}{
		{"file matching glob kept", []string{"a.go"}, []string{"*.go"}, []string{"a.go"}, false},
		{"file not matching glob dropped, none left → error", []string{"b.txt"}, []string{"*.go"}, nil, true},
		{"directory passes through to native filtering", []string{"sub"}, []string{"*.go"}, []string{"sub"}, false},
		{"nonexistent operand passes through unchanged", []string{"missing.go"}, []string{"*.go"}, []string{"missing.go"}, false},
		{"mixed: keep matching file, drop other, pass dir", []string{"a.go", "b.txt", "sub"}, []string{"*.go"}, []string{"a.go", "sub"}, false},
		{"slashed glob matches whole path", []string{"a.go"}, []string{"**/*.go"}, []string{"a.go"}, false},
		{"negated glob drops the file it excludes", []string{"a.go", "b.txt"}, []string{"!*.go"}, []string{"b.txt"}, false},
		{"negated glob keeps the file it does not exclude", []string{"b.txt"}, []string{"!*.go"}, []string{"b.txt"}, false},
		{"negated mixed: drop excluded file, keep other, pass dir", []string{"a.go", "b.txt", "sub"}, []string{"!*.go"}, []string{"b.txt", "sub"}, false},
		{"negated slashed glob inverts the whole-path match", []string{"a.go"}, []string{"!**/*.go"}, nil, true},
		{"negation excluding every file operand → error", []string{"a.go"}, []string{"!*.go"}, nil, true},
		{"several includes union the operands", []string{"a.go", "b.txt"}, []string{"*.go", "*.txt"}, []string{"a.go", "b.txt"}, false},
		{"include then exclusion drops the excluded operand", []string{"a.go", "b.txt"}, []string{"*.go", "*.txt", "!*.txt"}, []string{"a.go"}, false},
		{"last match wins across the list", []string{"a.go"}, []string{"!*.go", "*.go"}, []string{"a.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterGlobPaths(tt.paths, tt.globs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("FilterGlobPaths() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), "no paths match") {
					t.Errorf("FilterGlobPaths() err = %v, want no-paths-match message", err)
				}
				return
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("FilterGlobPaths() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSingleGlob(t *testing.T) {
	if got := SingleGlob(""); got != nil {
		t.Errorf("SingleGlob(\"\") = %q, want nil", got)
	}
	if got := SingleGlob("*.go"); !slices.Equal(got, []string{"*.go"}) {
		t.Errorf("SingleGlob(*.go) = %q, want [*.go]", got)
	}
}

// TestMatchGlobsParity compares MatchGlobs' selection over the repo file list
// against real rg's for each glob set. It skips when rg is absent.
func TestMatchGlobsParity(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	universe := rgFiles(t, root, nil)
	if len(universe) < 100 {
		t.Fatalf("repo file list has %d entries, too few to prove parity", len(universe))
	}
	globSets := [][]string{
		nil,
		{"*.go"},
		{"*.go", "*.md"},
		{"!*.go"},
		{"!*.go", "!*.md"},
		{"*.go", "!internal"},
		{"!internal", "*.go"},
		{"!internal/"},
		{"cli/*.go"},
		{"internal/cli/*.go"},
		{"{*.go,*.md}"},
		{"internal"},
		{"internal/"},
		{"!cli", "*.go"},
		{"!internal/**", "*.go"},
		{"!internal", "internal", "*.go"},
		{"internal/**", "!*.md"},
		{"internal/**/*.go"},
		{"!internal/cli/**", "*.go"},
	}
	for _, globs := range globSets {
		t.Run(strings.Join(globs, " "), func(t *testing.T) {
			want := intersect(universe, rgFiles(t, root, globs))
			got := selectGlobs(t, universe, globs)
			if slices.Equal(got, want) {
				return
			}
			t.Errorf("MatchGlobs(%q) diverged from rg: selected %d, rg %d\n  only MatchGlobs: %q\n  only rg: %q",
				globs, len(got), len(want), sample(diff(got, want)), sample(diff(want, got)))
		})
	}
}

func selectGlobs(t *testing.T, paths, globs []string) []string {
	t.Helper()
	var out []string
	for _, p := range paths {
		ok, err := MatchGlobs(p, globs)
		if err != nil {
			t.Fatalf("MatchGlobs(%q, %q): %v", p, globs, err)
		}
		if ok {
			out = append(out, p)
		}
	}
	return out
}

// rgFiles returns rg's file list for globs, rooted at dir. Exit status 1 is rg
// reporting zero matches, not a failure.
func rgFiles(t *testing.T, dir string, globs []string) []string {
	t.Helper()
	args := make([]string, 0, 3+2*len(globs))
	args = append(args, "--files", "--sort", "path")
	for _, g := range globs {
		args = append(args, "-g", g)
	}
	cmd := exec.Command("rg", args...) //nolint:gosec // fixed rg argv; dir is the repo root and globs are test literals
	cmd.Dir = dir
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		t.Fatalf("rg %q: %v", args, err)
	}
	listing := strings.TrimSpace(string(out))
	if listing == "" {
		return nil
	}
	return strings.Split(listing, "\n")
}

// intersect keeps base's entries present in other, in base's order — dropping
// the ignored files an include glob whitelists into rg's listing alone.
func intersect(base, other []string) []string {
	set := make(map[string]struct{}, len(other))
	for _, p := range other {
		set[p] = struct{}{}
	}
	var out []string
	for _, p := range base {
		if _, ok := set[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

func diff(from, minus []string) []string {
	set := make(map[string]struct{}, len(minus))
	for _, p := range minus {
		set[p] = struct{}{}
	}
	var out []string
	for _, p := range from {
		if _, ok := set[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

func sample(paths []string) []string {
	if len(paths) > 5 {
		return paths[:5]
	}
	return paths
}
