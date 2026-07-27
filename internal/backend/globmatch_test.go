package backend

import (
	"errors"
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
