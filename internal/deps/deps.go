// Package deps serves `ccx code deps`: it lists a file's imports (classified
// local/std/external) and the files that import it. Imports come from ast-grep
// for the languages it outlines and a per-family import-line regex otherwise;
// dependents from a ripgrep import-line scan. Both are syntactic, not a build
// graph, which the output's method footer discloses.
package deps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

// The classification labels a use carries. "unresolved" is the honest fallback
// when no family rule fires — the classifier never guesses std vs external.
const (
	classLocal      = "local"
	classStd        = "std"
	classExternal   = "external"
	classUnresolved = "unresolved"
)

// DefaultBudget bounds deps output on the flood-prone MCP surface when the caller
// sets none, mirroring read.DefaultBudget; the codeexec path leaves it zero.
const DefaultBudget = 2000

// useItem is one import of the target file: its normalized module identifier, the
// 1-based source line it sits on, its classification, and the text shown in the
// `## uses` row (a local Go import shows its repo-relative path, everything else
// its full identifier).
type useItem struct {
	name    string
	line    int
	class   string
	display string
}

// dependent is one file that imports the target: its cwd-relative path, the
// 1-based import line the scan matched, and — for a qualified-access language —
// the target symbols it references.
type dependent struct {
	path    string
	line    int
	symbols []string
}

// classCtx carries the filesystem context classification and used-by scanning
// consult: root is the cwd every dependent path and repo-dir probe resolves
// against, and mod is the resolved Go module (zero value for a non-Go file or a
// file outside any module).
type classCtx struct {
	root string
	mod  goModule
}

// Run lists a.Path's imports and dependents. It reads the file first so a read
// failure surfaces as a plain wrapped error rather than an engine diagnostic,
// selects the family from the extension, extracts and classifies the imports,
// scans the repo for dependents, and renders the anchored report. Output is
// uncapped — the caller Caps it.
func Run(ctx context.Context, a backend.Args) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("deps: resolve cwd: %w", err)
	}
	data, err := os.ReadFile(a.Path) //nolint:gosec // a.Path is validated pre-dispatch; deps reads the caller's own target
	if err != nil {
		return "", fmt.Errorf("deps: read %s: %w", a.Path, err)
	}
	fam, ok := familyForExt(a.Path)
	if !ok {
		return "", fmt.Errorf("deps: no import rules for %q", filepath.Ext(a.Path))
	}

	cc := classCtx{root: cwd}
	if fam == familyGo {
		cc.mod, _ = resolveGoModule(a.Path)
	}

	uses, method, err := extractImports(ctx, a.Path, string(data), fam)
	if err != nil {
		return "", err
	}
	for i := range uses {
		uses[i].class, uses[i].display = classify(fam, uses[i].name, cc)
	}

	deps, usedByNote, err := findDependents(ctx, a.Path, fam, cc)
	if err != nil {
		return "", err
	}

	files := anchor.NewFiles(cwd)
	return render(a.Path, uses, deps, method, usedByNote, files), nil
}

// render assembles the report: a header line summarizing the use breakdown and
// dependent count, the `## uses` rows (each anchored to its import line), the
// `## used by` rows (each a path:line#hash cite, with an optional "→ symbols"
// trailer) or the not-scanned note when usedByNote is set, and a method footer.
func render(path string, uses []useItem, deps []dependent, method, usedByNote string, files *anchor.Files) string {
	var b strings.Builder
	if usedByNote != "" {
		fmt.Fprintf(&b, "# deps %s — %d uses%s, dependents not scanned\n", path, len(uses), useBreakdown(uses))
	} else {
		fmt.Fprintf(&b, "# deps %s — %d uses%s, %d dependents\n", path, len(uses), useBreakdown(uses), len(deps))
	}

	b.WriteString("## uses\n")
	for _, u := range uses {
		fmt.Fprintf(&b, "L%d%s   %s (%s)\n", u.line, anchorSuffix(files, path, u.line), u.display, u.class)
	}

	b.WriteString("## used by\n")
	if usedByNote != "" {
		b.WriteString(usedByNote + "\n")
	}
	for _, d := range deps {
		loc := fmt.Sprintf("%s:%d%s", d.path, d.line, anchorSuffix(files, d.path, d.line))
		if len(d.symbols) > 0 {
			fmt.Fprintf(&b, "%s   → %s\n", loc, strings.Join(d.symbols, ", "))
		} else {
			fmt.Fprintf(&b, "%s\n", loc)
		}
	}

	fmt.Fprintf(&b, "# method: imports via %s; dependents via ripgrep import-line scan — syntactic, not a build graph\n", method)
	return b.String()
}

// useBreakdown renders the " (2 local · 1 std · …)" summary that follows the use
// count, listing only the non-zero categories in a fixed order; it returns "" for
// a file with no imports so the header reads a bare "0 uses".
func useBreakdown(uses []useItem) string {
	var local, std, external, unresolved int
	for _, u := range uses {
		switch u.class {
		case classLocal:
			local++
		case classStd:
			std++
		case classExternal:
			external++
		default:
			unresolved++
		}
	}
	var parts []string
	for _, c := range []struct {
		n     int
		label string
	}{{local, classLocal}, {std, classStd}, {external, classExternal}, {unresolved, classUnresolved}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.label))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " · ") + ")"
}

// anchorSuffix returns the "#<hash>" content anchor for path's 1-based line, or ""
// when the cache cannot resolve it (unreadable file or out-of-range line), so the
// span degrades to a bare line number rather than a hash over unverifiable text.
func anchorSuffix(files *anchor.Files, path string, line int) string {
	if text, ok := files.LineAt(path, line); ok {
		return "#" + anchor.Of(text).String()
	}
	return ""
}

// oneBased converts an ast-grep 0-based line number to the 1-based convention.
func oneBased(line int) int {
	return line + 1
}
