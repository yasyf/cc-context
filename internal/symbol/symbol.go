// Package symbol serves `ccx code symbol`: it resolves a symbol to a terse
// locate card — signature, path:line anchor, and doc — with body, callers,
// callees, siblings, and tests behind expansion flags. The definition index is
// a native ast-grep outline over the scope (internal/astgrep), the reference
// scan is ripgrep (internal/ripgrep), and every span is content-anchored
// (internal/anchor) so a cite survives edits. Output is uncapped; the caller
// budget-caps it.
package symbol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
)

// DefaultBudget bounds symbol output when the caller sets none: the CLI and MCP
// surfaces apply it, while codeexec leaves it zero.
const DefaultBudget = 2000

// ErrNotFound marks a query that resolved neither a structural definition nor
// definition-shaped text. The CLI maps it to exit 3.
var ErrNotFound = errors.New("symbol not found")

// separators splits a qualified query into (qualifier, name) at the last one:
// "Recv.Method", "Class::method", "Class#method". Longest first so "::" is not
// mistaken for a single ":".
var separators = []string{"::", "#", "."}

// Run resolves a.Query, returning the rendered card and the secret-masking rule
// ids that fired while rendering it (the caller appends the shared footer after
// its cap). It runs a full outline over the scope (a.Scope, else "."), filters
// flattened items to the query name including members, ranks the hits, and
// renders the top hit's card with the requested expansions — the signature,
// doc, body, callees, and sibling rows masked in the defining file's path
// context, and each reference or degraded-match row in its own file's, unless
// a.RevealSecrets. A miss degrades exact → case-insensitive → definition-
// keyword text before failing with ErrNotFound. The output is not capped.
func Run(ctx context.Context, a backend.Args) (string, []string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, fmt.Errorf("symbol: resolve cwd: %w", err)
	}
	qualifier, name := parseQuery(a.Query)
	scope := a.Scope
	if scope == "" {
		scope = "."
	}
	r := &resolver{
		ctx:       ctx,
		a:         a,
		scope:     scope,
		refScope:  a.Scope,
		qualifier: qualifier,
		name:      name,
		files:     anchor.NewFiles(cwd),
		outlines:  map[string][]astgrep.OutlineFile{},
		lineCache: map[string][]string{},
	}
	out, err := r.run()
	if err != nil {
		return "", nil, err
	}
	return out, r.maskedIDs, nil
}

// resolver holds one Run's state: the parsed query, the scope, the per-response
// anchor cache, and memoized outlines and file line tables. It is never shared
// across calls — a reused line table would resolve anchors against stale content.
type resolver struct {
	ctx       context.Context
	a         backend.Args
	scope     string // outline scope path ("." for the whole tree)
	refScope  string // ripgrep scope ("" for the whole tree)
	qualifier string
	name      string
	files     *anchor.Files
	scopeSet  []astgrep.OutlineFile
	outlines  map[string][]astgrep.OutlineFile
	lineCache map[string][]string
	maskedIDs []string
}

// run drives the miss ladder: an exact-name hit renders the card, else a
// case-insensitive hit renders it with a disclosure, else definition-shaped text
// degrades, else ErrNotFound.
func (r *resolver) run() (string, error) {
	files, err := r.outline([]string{r.scope})
	if err != nil {
		return "", err
	}
	r.scopeSet = files

	for _, fold := range []bool{false, true} {
		cands := r.candidates(files, fold)
		if len(cands) == 0 {
			continue
		}
		rank(cands, r.name)
		c, err := r.buildCard(cands, fold)
		if err != nil {
			return "", err
		}
		return renderCard(c), nil
	}

	degraded, err := r.degraded()
	if err != nil {
		return "", err
	}
	if degraded != "" {
		return degraded, nil
	}
	return "", fmt.Errorf("symbol %q: no definition and no definition-shaped text: %w", r.a.Query, ErrNotFound)
}

// parseQuery splits a query into its qualifier and bare name at the last
// separator. A query with no separator, or one whose separator leaves an empty
// name (a trailing "."), is the bare name with no qualifier.
func parseQuery(query string) (qualifier, name string) {
	best, sepLen := -1, 0
	for _, sep := range separators {
		if i := strings.LastIndex(query, sep); i > best {
			best, sepLen = i, len(sep)
		}
	}
	if best < 0 {
		return "", query
	}
	name = query[best+sepLen:]
	if name == "" {
		return "", query
	}
	return query[:best], name
}

// outline runs a memoized `ast-grep outline` over paths. Both the scope index and
// the per-reference-set attribution outline flow through it, so a repeated path
// set never re-spawns ast-grep within one Run.
func (r *resolver) outline(paths []string) ([]astgrep.OutlineFile, error) {
	key := strings.Join(paths, "\x00")
	if f, ok := r.outlines[key]; ok {
		return f, nil
	}
	f, err := astgrep.OutlinePaths(r.ctx, paths, astgrep.OutlineOpts{})
	if err != nil {
		return nil, fmt.Errorf("symbol: outline %v: %w", paths, err)
	}
	r.outlines[key] = f
	return f, nil
}

// normPath strips the "./" prefix ast-grep prints for a whole-tree outline so a
// path matches the bare form ripgrep reports.
func normPath(p string) string {
	return strings.TrimPrefix(p, "./")
}

// spanText renders a line span, collapsing a single line to "A".
func spanText(start, end int) string {
	if start >= end {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

// isTestFile classifies a path as a test file across the languages symbol
// resolves: Go (*_test.go), Python (test_*.py, *_test.py), and the JS/TS spec
// and test conventions (*.spec.*, *.test.*).
func isTestFile(path string) bool {
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, "_test.go"):
		return true
	case strings.HasSuffix(base, "_test.py"):
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"):
		return true
	case strings.Contains(base, ".spec."):
		return true
	case strings.Contains(base, ".test."):
		return true
	default:
		return false
	}
}

// fileLines returns path's lines (trailing CR kept, as anchor.Load splits them),
// caching the read for this Run.
func (r *resolver) fileLines(path string) []string {
	if lines, ok := r.lineCache[path]; ok {
		return lines
	}
	f, err := anchor.Load(path)
	if err != nil {
		r.lineCache[path] = nil
		return nil
	}
	lines := f.Lines()
	r.lineCache[path] = lines
	return lines
}

// anchoredLine renders "line#hash" for path's line, falling back to the bare line
// number when the cache cannot resolve it.
func (r *resolver) anchoredLine(path string, line int) string {
	span := fmt.Sprintf("%d", line)
	if text, ok := r.files.LineAt(path, line); ok {
		span += "#" + anchor.Of(text).String()
	}
	return span
}
