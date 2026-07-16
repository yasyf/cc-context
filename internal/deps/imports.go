package deps

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/outline"
)

// family is the language family a file belongs to, keyed off its extension. It
// drives import extraction, classification, the used-by needle, and symbol
// enrichment.
type family int

const (
	familyGo family = iota
	familyPython
	familyJS // JavaScript and TypeScript, incl. the jsx/tsx and .mjs/.cjs variants
	familyRust
	familyJava
	familyKotlin
	familyRuby
	familyC // C and C++
	familyCSharp
	familyPHP
	familyShell
)

// familyByExt maps a lowercase extension to its family. An unlisted extension has
// no import rules and Run rejects it.
var familyByExt = map[string]family{
	".go":   familyGo,
	".py":   familyPython,
	".pyi":  familyPython,
	".ts":   familyJS,
	".mts":  familyJS,
	".cts":  familyJS,
	".tsx":  familyJS,
	".js":   familyJS,
	".mjs":  familyJS,
	".cjs":  familyJS,
	".jsx":  familyJS,
	".rs":   familyRust,
	".java": familyJava,
	".kt":   familyKotlin,
	".kts":  familyKotlin,
	".rb":   familyRuby,
	".c":    familyC,
	".h":    familyC,
	".cc":   familyC,
	".cpp":  familyC,
	".cxx":  familyC,
	".hpp":  familyC,
	".hh":   familyC,
	".hxx":  familyC,
	".cs":   familyCSharp,
	".php":  familyPHP,
	".sh":   familyShell,
	".bash": familyShell,
}

// familyForExt returns the family for path's extension, ok=false when unlisted.
func familyForExt(path string) (family, bool) {
	f, ok := familyByExt[strings.ToLower(filepath.Ext(path))]
	return f, ok
}

// extractImports returns the file's imports and the method label for the footer.
// A language ast-grep outlines is scanned with `--items imports` (method
// "ast-grep"); every other family is scanned with a per-family import-line regex
// over content (method "regex").
func extractImports(ctx context.Context, path, content string, fam family) ([]useItem, string, error) {
	if _, ok := outline.LangForExt(path); ok {
		files, err := astgrep.OutlinePaths(ctx, []string{path}, astgrep.OutlineOpts{Items: "imports"})
		if err != nil {
			return nil, "", err
		}
		return usesFromOutline(fam, files), "ast-grep", nil
	}
	return extractRegex(fam, content), "regex", nil
}

// usesFromOutline projects an `--items imports` outline into normalized uses in
// source order, dropping any import whose name normalizes to empty.
func usesFromOutline(fam family, files []astgrep.OutlineFile) []useItem {
	var uses []useItem
	for _, f := range files {
		for _, it := range f.Items {
			name := normalizeName(fam, it.Name)
			if name == "" {
				continue
			}
			uses = append(uses, useItem{name: name, line: oneBased(it.Range.Start.Line)})
		}
	}
	return uses
}

// normalizeName cleans ast-grep's raw import name into a bare module identifier:
// Python drops an "as <alias>" tail, JS/TS strips the quotes ast-grep keeps around
// a module specifier, and the rest pass through trimmed.
func normalizeName(fam family, raw string) string {
	raw = strings.TrimSpace(raw)
	switch fam {
	case familyPython:
		if i := strings.Index(raw, " as "); i >= 0 {
			raw = raw[:i]
		}
		return strings.TrimSpace(raw)
	case familyJS:
		return strings.Trim(raw, "'\"`")
	default:
		return raw
	}
}

// regexFamilyPatterns holds, per regex-fallback family, the compiled patterns
// whose first capture group is the imported module. A line may yield several (a
// PHP file mixes use and require), so every pattern is tried on every line.
var regexFamilyPatterns = map[family][]*regexp.Regexp{
	familyRuby:   {regexp.MustCompile(`^\s*require(?:_relative)?\s+['"]([^'"]+)['"]`)},
	familyC:      {regexp.MustCompile(`^\s*#\s*include\s+[<"]([^>"]+)[>"]`)},
	familyCSharp: {regexp.MustCompile(`^\s*using\s+(?:static\s+)?([A-Za-z_][\w.]*)\s*;`)},
	familyPHP: {
		regexp.MustCompile(`^\s*use\s+([\\A-Za-z_][\w\\]*)`),
		regexp.MustCompile(`^\s*(?:require|require_once|include|include_once)\b[^'"]*['"]([^'"]+)['"]`),
	},
	familyShell: {regexp.MustCompile(`^\s*(?:source|\.)\s+['"]?([^'"\s]+)`)},
}

// extractRegex scans content line by line with the family's import patterns,
// returning one useItem per captured module in source order.
func extractRegex(fam family, content string) []useItem {
	pats := regexFamilyPatterns[fam]
	var uses []useItem
	for i, line := range strings.Split(content, "\n") {
		for _, re := range pats {
			if m := re.FindStringSubmatch(line); m != nil {
				uses = append(uses, useItem{name: m[1], line: i + 1})
			}
		}
	}
	return uses
}

// goModule is a resolved Go module: the absolute directory holding its go.mod and
// its module path. The zero value marks "no module" (a file outside any module).
type goModule struct {
	root string
	path string
}

// resolveGoModule walks up from file's directory to the nearest go.mod, returning
// the module path and its directory. ok is false when no go.mod is found or its
// module line is missing.
func resolveGoModule(file string) (goModule, bool) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return goModule{}, false
	}
	dir := filepath.Dir(abs)
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil { //nolint:gosec // reads the module's own go.mod, a fixed filename walked up from the target
			if p := moduleLine(data); p != "" {
				return goModule{root: dir, path: p}, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return goModule{}, false
		}
		dir = parent
	}
}

// moduleLine returns the module path from a go.mod body, or "" when absent.
func moduleLine(data []byte) string {
	for _, ln := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
