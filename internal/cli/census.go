package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sourceExts is the set of file extensions the language census counts — code
// plus the lightweight markup that signals a project's surface.
var sourceExts = map[string]bool{
	"go": true, "py": true, "ts": true, "tsx": true, "js": true, "jsx": true,
	"rs": true, "java": true, "kt": true, "rb": true, "php": true, "cs": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true,
	"swift": true, "scala": true, "sh": true, "lua": true,
	"md": true, "proto": true, "sql": true,
}

// censusSkipDirs are directories the census never descends into.
var censusSkipDirs = map[string]bool{
	".git": true, ".jj": true, "node_modules": true, "vendor": true,
	"target": true, "dist": true, "build": true, ".venv": true,
}

// languageCensus returns a one-line "languages: go (12), py (3), ..." summary of
// the source files under root, sorted by count then name. It complements tilth's
// overview, which reports only the primary language and under-represents
// multi-language repos. Returns "" when no source files are found.
func languageCensus(root string) string {
	counts := map[string]int{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (censusSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), ".")); sourceExts[ext] {
			counts[ext]++
		}
		return nil
	})
	if len(counts) == 0 {
		return ""
	}

	exts := make([]string, 0, len(counts))
	for e := range counts {
		exts = append(exts, e)
	}
	sort.Slice(exts, func(i, j int) bool {
		if counts[exts[i]] != counts[exts[j]] {
			return counts[exts[i]] > counts[exts[j]]
		}
		return exts[i] < exts[j]
	})

	parts := make([]string, len(exts))
	for i, e := range exts {
		parts[i] = fmt.Sprintf("%s (%d)", e, counts[e])
	}
	return "languages: " + strings.Join(parts, ", ")
}

// workingDir returns the current working directory, or "." if it cannot be read.
func workingDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
