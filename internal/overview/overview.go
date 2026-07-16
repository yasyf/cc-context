// Package overview builds the native "ccx repo overview" fingerprint: a token-lean,
// most-orienting-first digest of a repository (module headline, languages, directory
// layout, entry points, manifests, tests, and git state) assembled from one file walk
// plus git subprocesses. The caller budget-caps the output via render.Finalize.
package overview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
)

// maxEntries caps how many entry-point paths the entry section lists.
const maxEntries = 4

// Run assembles the repo overview for a.Scope (or the cwd), ordered most-orienting
// first so a budget cap trims the git/churn tail gracefully. It is not itself capped;
// the caller finalizes it through render.
func Run(ctx context.Context, a backend.Args) (string, error) {
	root, err := resolveRoot(a)
	if err != nil {
		return "", err
	}
	census, err := walkRepo(ctx, root)
	if err != nil {
		return "", err
	}
	manifests := probeManifests(root)

	var b strings.Builder
	b.WriteString(headerLine(root, manifests))
	writeLine(&b, languagesLine(census.exts))
	writeLine(&b, dirsLine(census.tree))
	writeLine(&b, entryLine(root))
	writeLine(&b, manifestsLine(manifests))
	writeLine(&b, testsLine(census.tests))
	if gitAnswers(ctx, root) {
		writeLine(&b, gitSection(ctx, root))
		writeLine(&b, hotLine(ctx, root))
	}
	return b.String(), nil
}

// resolveRoot resolves a.Scope (or the cwd) to an absolute path.
func resolveRoot(a backend.Args) (string, error) {
	root := a.Scope
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("overview: resolve cwd: %w", err)
		}
		root = cwd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("overview: resolve root %q: %w", root, err)
	}
	return abs, nil
}

// writeLine appends a non-empty section line and its newline.
func writeLine(b *strings.Builder, line string) {
	if line != "" {
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// headerLine renders "# <repo> — <headline>" from the repo directory name and the
// primary manifest's headline, dropping the headline when no manifest supplies one.
func headerLine(root string, ms []manifest) string {
	name := filepath.Base(root)
	if len(ms) > 0 && ms[0].headline != "" {
		return fmt.Sprintf("# %s — %s\n", name, ms[0].headline)
	}
	return fmt.Sprintf("# %s\n", name)
}

// entryLine renders "entry: cmd/ccx/main.go" from the first language whose entry-point
// heuristic matches (Go, then TS/JS, then Python, then bin/), or "" when none do.
func entryLine(root string) string {
	for _, g := range [][]string{goEntries(root), tsEntries(root), pyEntries(root), binEntries(root)} {
		if len(g) == 0 {
			continue
		}
		if len(g) > maxEntries {
			g = g[:maxEntries]
		}
		return "entry: " + strings.Join(g, " · ")
	}
	return ""
}

func goEntries(root string) []string {
	out := globRel(root, filepath.Join("cmd", "*", "main.go"))
	if fileExists(filepath.Join(root, "main.go")) {
		out = append(out, "main.go")
	}
	return out
}

func tsEntries(root string) []string {
	var out []string
	for _, p := range []string{"src/index.ts", "src/index.js", "index.ts", "index.js"} {
		if fileExists(filepath.Join(root, filepath.FromSlash(p))) {
			out = append(out, p)
		}
	}
	return out
}

func pyEntries(root string) []string {
	var out []string
	for _, p := range []string{"main.py", "__main__.py"} {
		if fileExists(filepath.Join(root, p)) {
			out = append(out, p)
		}
	}
	return append(out, globRel(root, filepath.Join("src", "*", "__main__.py"))...)
}

func binEntries(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, "bin/"+e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// globRel resolves a filepath.Glob pattern under root and returns the matches as
// sorted slash-relative paths.
func globRel(root, pattern string) []string {
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if rel, err := filepath.Rel(root, m); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	sort.Strings(out)
	return out
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
