package deps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

// scanSpec is a used-by search: the ripgrep query for the family's importable
// needle, whether it is a regex, an optional shape a matched line's trimmed text
// must satisfy to count as a real import (dropping prose and comments), and whether
// to drop matches from files outside the family's own extensions. A zero query
// means the family has no derivable needle (e.g. a Go file outside any module),
// which yields no dependents.
type scanSpec struct {
	query    string
	regex    bool
	shape    *regexp.Regexp
	scopeFam bool
}

// findDependents scans the repo for files importing path, taking the first
// import-shaped match line per file, sorting by path, and enriching each with the
// target symbols it references. It returns a note in place of results when the
// family's imports cannot be soundly scanned (Rust, C#); the caller renders it.
func findDependents(ctx context.Context, path string, fam family, cc classCtx) ([]dependent, string, error) {
	spec, note := usedByScan(path, fam, cc)
	if note != "" {
		return nil, note, nil
	}
	if spec.query == "" {
		return nil, "", nil
	}
	matches, err := ripgrep.Matches(ctx, backend.Args{Query: spec.query, Regex: spec.regex})
	if err != nil {
		return nil, "", err
	}
	var deps []dependent
	for _, fm := range matches {
		if spec.scopeFam && !inFamily(fm.Path, fam) {
			continue
		}
		for _, ml := range fm.Lines {
			if !ml.IsMatch {
				continue
			}
			if spec.shape != nil && !spec.shape.MatchString(strings.TrimSpace(ml.Text)) {
				continue
			}
			deps = append(deps, dependent{path: fm.Path, line: ml.Num})
			break
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].path < deps[j].path })
	if err := enrich(ctx, deps, path, fam, cc); err != nil {
		return nil, "", err
	}
	return deps, "", nil
}

// usedByScan builds the used-by needle for path's family: Go matches its exact
// quoted import path, Python its dotted module in import/from position, JS/TS its
// path-anchored basename in a quoted specifier, and every other family delegates to
// defaultScan.
func usedByScan(path string, fam family, cc classCtx) (scanSpec, string) {
	switch fam {
	case familyGo:
		ip := goImportPath(path, cc.mod)
		if ip == "" {
			return scanSpec{}, ""
		}
		shape := regexp.MustCompile(`^(?:import\s+)?(?:[A-Za-z_]\w*\s+|\.\s+|_\s+)?"` + regexp.QuoteMeta(ip) + `"`)
		return scanSpec{query: ip, shape: shape}, ""
	case familyPython:
		return pythonScan(path, cc.root), ""
	case familyJS:
		base := baseName(path)
		if base == "" {
			return scanSpec{}, ""
		}
		return scanSpec{
			query: `/` + regexp.QuoteMeta(base) + `(?:\.\w+)?['"]`,
			regex: true,
			shape: regexp.MustCompile(`\b(?:import|export|require|from)\b`),
		}, ""
	default:
		return defaultScan(path, fam)
	}
}

// defaultScan builds the used-by spec for a family the Go/Python/JS branches do not
// cover. Every sweep is scoped to the family's own extensions (scopeFam) and its
// query is anchored to a real import line, so cross-language comments and markdown
// prose never register. A family whose dependents a filename or basename needle
// cannot soundly express — Rust module paths, C# namespaces — returns the honest
// not-scanned note instead of guessing.
func defaultScan(path string, fam family) (scanSpec, string) {
	stem := regexp.QuoteMeta(baseName(path))
	file := regexp.QuoteMeta(filepath.Base(path))
	switch fam {
	case familyRuby:
		return scanSpec{query: `^\s*require(?:_relative)?\s+['"][^'"]*\b` + stem + `['"]`, regex: true, scopeFam: true}, ""
	case familyJava:
		return scanSpec{query: `^\s*import\s+(?:static\s+)?[\w.]*\b` + stem + `\s*;`, regex: true, scopeFam: true}, ""
	case familyKotlin:
		return scanSpec{query: `^\s*import\s+[\w.]*\b` + stem + `\b`, regex: true, scopeFam: true}, ""
	case familyC:
		return scanSpec{query: `^\s*#\s*include\s*[<"][^>"]*` + file + `[>"]`, regex: true, scopeFam: true}, ""
	case familyShell:
		return scanSpec{query: `^\s*(?:source|\.)\s+['"]?[^'"\n]*` + file + `(?:['"\s]|$)`, regex: true, scopeFam: true}, ""
	case familyPHP:
		return scanSpec{query: `^\s*(?:use\s+[\\\w]*\b` + stem + `\b|(?:require|require_once|include|include_once)\b[^'"]*['"][^'"]*` + file + `['"])`, regex: true, scopeFam: true}, ""
	case familyRust:
		return scanSpec{}, notScanned(path, ".rs", "module-path imports")
	case familyCSharp:
		return scanSpec{}, notScanned(path, ".cs", "namespace imports")
	default:
		return scanSpec{}, notScanned(path, filepath.Ext(path), "no import rule")
	}
}

// notScanned renders the honest used-by note for a family whose dependents a
// syntactic needle cannot soundly find, pointing at a grep the caller can run.
func notScanned(path, ext, why string) string {
	return fmt.Sprintf("not scanned for %s (%s) — try ccx code grep -w %s", ext, why, baseName(path))
}

// inFamily reports whether path's extension belongs to fam, so a used-by sweep
// drops a cross-language file (and any non-source prose) before shape-matching.
func inFamily(path string, fam family) bool {
	f, ok := familyForExt(path)
	return ok && f == fam
}

// pythonScan builds the Python used-by needle: the dotted module imported directly
// or via `from`, plus the `from <parent> import <mod>` form when the module is
// nested.
func pythonScan(path, root string) scanSpec {
	dotted, parent, modname := pythonDotted(path, root)
	if dotted == "" {
		return scanSpec{}
	}
	qd := regexp.QuoteMeta(dotted)
	branches := []string{
		`import\s+` + qd + `(?:\s|,|$)`,
		`from\s+` + qd + `\s+import\b`,
	}
	if parent != "" {
		branches = append(branches, `from\s+`+regexp.QuoteMeta(parent)+`\s+import\b[^#\n]*\b`+regexp.QuoteMeta(modname)+`\b`)
	}
	return scanSpec{query: `^\s*(?:` + strings.Join(branches, "|") + `)`, regex: true}
}

// goImportPath returns the package import path of path's directory —
// <module>/<dir relative to module root>, or the bare module path at its root — or
// "" when path lies outside a module.
func goImportPath(path string, mod goModule) string {
	if mod.path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(mod.root, filepath.Dir(abs))
	if err != nil {
		return ""
	}
	if rel = filepath.ToSlash(rel); rel == "." {
		return mod.path
	}
	return mod.path + "/" + rel
}

// pythonDotted maps path to its dotted module (relative to root, ".py"/"__init__"
// stripped), plus its parent package and bare module name.
func pythonDotted(path, root string) (dotted, parent, modname string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		rel = filepath.Base(abs)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	rel = strings.TrimSuffix(filepath.ToSlash(rel), "/__init__")
	dotted = strings.ReplaceAll(rel, "/", ".")
	if i := strings.LastIndexByte(dotted, '.'); i >= 0 {
		return dotted, dotted[:i], dotted[i+1:]
	}
	return dotted, "", dotted
}

// baseName returns path's filename without its extension.
func baseName(path string) string {
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// enrich attaches the target symbols each dependent references, for the
// qualified-access families only: Go and Python resolve one target-derived
// package needle across every dependent, JS/TS resolves each dependent's own
// namespace alias. Default- and named-import families get nothing.
func enrich(ctx context.Context, deps []dependent, path string, fam family, cc classCtx) error {
	switch fam {
	case familyGo:
		return enrichUniform(ctx, deps, goPkgName(path, cc.mod))
	case familyPython:
		_, _, modname := pythonDotted(path, cc.root)
		return enrichUniform(ctx, deps, modname)
	case familyJS:
		enrichNamespace(deps, path)
		return nil
	default:
		return nil
	}
}

// enrichUniform scans every dependent for `<pkg>.<Symbol>` accesses with a single
// ripgrep pass, assigning the unique symbols found to each dependent. A blank pkg
// or an empty dependent set is a no-op.
func enrichUniform(ctx context.Context, deps []dependent, pkg string) error {
	if pkg == "" || len(deps) == 0 {
		return nil
	}
	reStr := `\b` + regexp.QuoteMeta(pkg) + `\.[A-Za-z_]\w*`
	re := regexp.MustCompile(reStr)
	paths := make([]string, len(deps))
	byPath := make(map[string]*dependent, len(deps))
	for i := range deps {
		paths[i] = deps[i].path
		byPath[deps[i].path] = &deps[i]
	}
	matches, err := ripgrep.Matches(ctx, backend.Args{Query: reStr, Regex: true, Paths: paths})
	if err != nil {
		return err
	}
	for _, fm := range matches {
		d, ok := byPath[fm.Path]
		if !ok {
			continue
		}
		d.symbols = uniqueMatches(re, fm.Lines)
	}
	return nil
}

// enrichNamespace resolves, per dependent, the alias it binds the target module to
// via `import * as <ns> from '…<base>…'`, then collects that dependent's
// `<ns>.<Symbol>` accesses. A dependent using a default or named import binds no
// namespace and contributes no symbols.
func enrichNamespace(deps []dependent, path string) {
	base := baseName(path)
	if base == "" {
		return
	}
	nsRe := regexp.MustCompile(`import\s*\*\s*as\s+([A-Za-z_$][\w$]*)\s+from\s+['"][^'"]*\b` + regexp.QuoteMeta(base) + `(?:\.\w+)?['"]`)
	for i := range deps {
		data, err := os.ReadFile(deps[i].path) //nolint:gosec // deps[i].path is a repo file the scan already matched
		if err != nil {
			continue
		}
		content := string(data)
		m := nsRe.FindStringSubmatch(content)
		if m == nil {
			continue
		}
		symRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(m[1]) + `\.[A-Za-z_$][\w$]*`)
		deps[i].symbols = uniqueStrings(symRe.FindAllString(content, -1))
	}
}

// goPkgName approximates the target's package name as the last segment of its
// import path, the accessor a dependent qualifies its symbols with.
func goPkgName(path string, mod goModule) string {
	ip := goImportPath(path, mod)
	if i := strings.LastIndexByte(ip, '/'); i >= 0 {
		return ip[i+1:]
	}
	return ip
}

// uniqueMatches extracts re's matches from every match line, deduplicated in
// first-appearance order.
func uniqueMatches(re *regexp.Regexp, lines []ripgrep.MatchLine) []string {
	var found []string
	for _, ml := range lines {
		if ml.IsMatch {
			found = append(found, re.FindAllString(ml.Text, -1)...)
		}
	}
	return uniqueStrings(found)
}

// uniqueStrings returns in in first-appearance order with duplicates removed, or
// nil when empty.
func uniqueStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
