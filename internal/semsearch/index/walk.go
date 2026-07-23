package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// defaultIgnoredDirs is semble/index/file_walker.py _DEFAULT_IGNORED_DIRS, in
// the sorted order semble feeds them to its base GitIgnoreSpec.
var defaultIgnoredDirs = []string{
	".cache/", ".eggs/", ".git/", ".hg/", ".mypy_cache/", ".next/",
	".pytest_cache/", ".ruff_cache/", ".semble/", ".svn/", ".tox/",
	".venv/", "__pycache__/", "build/", "dist/", "node_modules/", "venv/",
}

// ignorePattern pairs a parsed gitignore pattern with whether it is a negation
// whose body carries a file-extension suffix — semble's "found" bypass, which
// re-includes a negated file even when its extension is not an indexed language.
type ignorePattern struct {
	pat               gitignore.Pattern
	negatedWithSuffix bool
}

// ignoreSpec is a set of ignore patterns anchored at a base directory, matching
// paths relative to that base — semble's IgnoreSpec.
type ignoreSpec struct {
	base     string
	patterns []ignorePattern
}

// WalkFiles enumerates indexable files under root (absolute, resolved) whose
// extension is in extensions, honoring the fixed denylist plus each directory's
// .gitignore and .sembleignore — a faithful port of semble's walk_files. It
// returns absolute paths in directory-sorted order; symlinks are skipped.
func WalkFiles(root string, extensions []string) ([]string, error) {
	extSet := make(map[string]bool, len(extensions))
	for _, e := range extensions {
		extSet[strings.ToLower(e)] = true
	}
	base := &ignoreSpec{base: root, patterns: parsePatterns(defaultIgnoredDirs)}
	var out []string
	if err := walk(root, []*ignoreSpec{base}, extSet, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walk recurses one directory, threading the inherited ignore specs and
// appending eligible files to out — semble's _walk.
func walk(dir string, inherited []*ignoreSpec, extSet map[string]bool, out *[]string) error {
	specs := inherited
	if spec := loadIgnoreForDir(dir); spec != nil {
		specs = append(append([]*ignoreSpec(nil), inherited...), spec)
	}

	entries, err := os.ReadDir(dir) // sorted by filename
	if err != nil {
		return fmt.Errorf("read dir %q: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue // never follow symlinks
		}
		full := filepath.Join(dir, entry.Name())
		isDir := entry.IsDir()
		ignored, found := isIgnored(full, isDir, specs)
		if ignored {
			continue
		}
		if isDir {
			if err := walk(full, specs, extSet, out); err != nil {
				return err
			}
			continue
		}
		if entry.Type().IsRegular() && (found || extSet[strings.ToLower(extOf(full))]) {
			*out = append(*out, full)
		}
	}
	return nil
}

// isIgnored evaluates path against every spec in order, last match wins, and
// returns whether it is ignored plus semble's extension-bypass "found" flag.
func isIgnored(path string, isDir bool, specs []*ignoreSpec) (ignored, found bool) {
	for _, spec := range specs {
		rel, ok := relComponents(spec.base, path)
		if !ok {
			continue
		}
		for _, p := range spec.patterns {
			switch p.pat.Match(rel, isDir) {
			case gitignore.Exclude:
				ignored, found = true, false
			case gitignore.Include:
				ignored, found = false, p.negatedWithSuffix
			case gitignore.NoMatch:
			}
		}
	}
	return ignored, found
}

// loadIgnoreForDir reads dir's .gitignore then .sembleignore into one spec, or
// nil when neither exists — semble's _load_ignore_for_dir.
func loadIgnoreForDir(dir string) *ignoreSpec {
	var lines []string
	for _, name := range []string{".gitignore", ".sembleignore"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil { //nolint:gosec // repo ignore file under the caller's own tree
			lines = append(lines, strings.Split(strings.ToValidUTF8(string(data), "�"), "\n")...)
		}
	}
	patterns := parsePatterns(lines)
	if len(patterns) == 0 {
		return nil
	}
	return &ignoreSpec{base: dir, patterns: patterns}
}

// parsePatterns compiles gitignore lines, dropping blanks and comments and
// pre-computing the negation-suffix bypass per pattern.
func parsePatterns(lines []string) []ignorePattern {
	var out []ignorePattern
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "\\#") {
			line = line[1:]
		}
		out = append(out, ignorePattern{
			pat:               gitignore.ParsePattern(line, nil),
			negatedWithSuffix: negatedWithSuffix(line),
		})
	}
	return out
}

// negatedWithSuffix reports whether a raw pattern line is a negation whose body
// ends in a file-extension suffix (e.g. !special.kjs, !*.py) — semble's bypass
// condition. Directory or broad-glob negations (!vendor/, !.github/*) do not.
func negatedWithSuffix(line string) bool {
	if !strings.HasPrefix(line, "!") {
		return false
	}
	body := strings.TrimRight(line[1:], " ")
	body = strings.TrimRight(body, "/")
	return extOf(body) != ""
}

// relComponents returns path's slash-split components relative to base, and
// false when path is not under base.
func relComponents(base, path string) ([]string, bool) {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, false
	}
	return strings.Split(filepath.ToSlash(rel), "/"), true
}
