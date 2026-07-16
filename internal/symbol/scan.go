package symbol

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

const (
	rowsPerFile = 3
	filesShown  = 10
)

// ref is one word-boundary reference to the symbol: its file, 1-based line, and
// source text.
type ref struct {
	path string
	line int
	text string
}

// refScan word-searches the scope for the symbol name and returns the references,
// dropping the definition's own line and import-shaped lines. It is one ripgrep
// spawn, run even for a terse card's counts.
func (r *resolver) refScan(top candidate) ([]ref, error) {
	fms, err := ripgrep.Matches(r.ctx, backend.Args{Query: r.name, Word: true, Scope: r.refScope})
	if err != nil {
		return nil, fmt.Errorf("symbol: ref scan %q: %w", r.name, err)
	}
	var refs []ref
	for _, fm := range fms {
		for _, l := range fm.Lines {
			if !l.IsMatch {
				continue
			}
			if fm.Path == top.path && l.Num == top.start {
				continue
			}
			if isImportShaped(l.Text) {
				continue
			}
			refs = append(refs, ref{path: fm.Path, line: l.Num, text: l.Text})
		}
	}
	return refs, nil
}

// importShapedRe matches a lone quoted string on its own line — a Go import spec
// or a bare module path — which a word search hits inside an import block.
var importShapedRe = regexp.MustCompile(`^"[^"]*",?$`)

// isImportShaped reports whether a line is an import statement or a bare quoted
// import spec, so it is dropped from a reference set.
func isImportShaped(text string) bool {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "import "):
		return true
	case strings.HasPrefix(t, "from ") && strings.Contains(t, " import "):
		return true
	case importShapedRe.MatchString(t):
		return true
	default:
		return false
	}
}

// filterTestRefs keeps only the references in test files.
func filterTestRefs(refs []ref) []ref {
	var out []ref
	for _, rf := range refs {
		if isTestFile(rf.path) {
			out = append(out, rf)
		}
	}
	return out
}

// refBlock assembles a rendered reference block (callers or tests): it groups the
// refs per file — defining-directory files first, then by descending count — caps
// each file to rowsPerFile rows and the block to filesShown files, and batches one
// outline over only the files that render for enclosing-item attribution, disclosing
// the omitted remainder.
func (r *resolver) refBlock(label string, top candidate, refs []ref) (refBlock, error) {
	grouped := groupRefs(refs)
	order := orderRefFiles(grouped, filepath.Dir(top.path))

	shownFiles := order
	if len(shownFiles) > filesShown {
		shownFiles = shownFiles[:filesShown]
	}
	encl := map[string][]astgrep.FlatItem{}
	if len(shownFiles) > 0 {
		ofs, err := r.outline(shownFiles)
		if err != nil {
			return refBlock{}, err
		}
		for _, f := range ofs {
			encl[normPath(f.Path)] = f.Flatten()
		}
	}

	blk := refBlock{label: label, word: r.name, total: len(refs), files: len(grouped)}
	shown := 0
	for _, p := range shownFiles {
		g := refGroup{path: p}
		for j, rf := range grouped[p] {
			if j >= rowsPerFile {
				break
			}
			row := fmt.Sprintf("[%s] %s", r.anchoredLine(p, rf.line), strings.TrimSpace(rf.text))
			if name := enclosingName(encl[p], rf.line); name != "" {
				row += "   in " + name
			}
			g.rows = append(g.rows, row)
			shown++
		}
		blk.groups = append(blk.groups, g)
	}
	blk.omitted = len(refs) - shown
	return blk, nil
}

// enclosingName returns the qualified name of the innermost outline item whose
// span contains line — the smallest containing span — or empty when a ref sits at
// file scope.
func enclosingName(items []astgrep.FlatItem, line int) string {
	best, bestSpan := -1, 1<<30
	for i, it := range items {
		if line < it.StartLine || line > it.EndLine {
			continue
		}
		if span := it.EndLine - it.StartLine; span < bestSpan {
			best, bestSpan = i, span
		}
	}
	if best < 0 {
		return ""
	}
	return items[best].Qualified
}

// groupRefs buckets references by file, preserving each file's line order.
func groupRefs(refs []ref) map[string][]ref {
	byFile := map[string][]ref{}
	for _, rf := range refs {
		byFile[rf.path] = append(byFile[rf.path], rf)
	}
	return byFile
}

// orderRefFiles orders reference files defining-directory-first, then by
// descending reference count, then by path.
func orderRefFiles(byFile map[string][]ref, defDir string) []string {
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := filepath.Dir(paths[i]) == defDir, filepath.Dir(paths[j]) == defDir
		if di != dj {
			return di
		}
		if len(byFile[paths[i]]) != len(byFile[paths[j]]) {
			return len(byFile[paths[i]]) > len(byFile[paths[j]])
		}
		return paths[i] < paths[j]
	})
	return paths
}

// bodyLines returns the definition's source lines verbatim.
func (r *resolver) bodyLines(top candidate) []string {
	lines := r.fileLines(top.path)
	if top.start < 1 || top.end > len(lines) {
		return nil
	}
	out := make([]string, 0, top.end-top.start+1)
	for _, l := range lines[top.start-1 : top.end] {
		out = append(out, stripCR(l))
	}
	return out
}

// siblingRows renders the defining file's other top-level items as
// "[A-B#hash] signature" rows.
func (r *resolver) siblingRows(top candidate) []string {
	items := r.siblingItems(top)
	rows := make([]string, 0, len(items))
	for _, it := range items {
		span := spanText(it.StartLine, it.EndLine)
		if text, ok := r.files.LineAt(top.path, it.StartLine); ok {
			span += "#" + anchor.Of(text).String()
		}
		sig := strings.TrimSuffix(it.Signature, " {")
		if sig == "" {
			sig = it.Name
		}
		rows = append(rows, fmt.Sprintf("[%s] %s", span, sig))
	}
	return rows
}

// calleeRe captures a call-shaped token: an identifier immediately followed by an
// open parenthesis.
var calleeRe = regexp.MustCompile(`([A-Za-z_]\w*)\s*\(`)

// defLineRe matches a line that opens a definition — its own name would read as a
// call — so a container's members are excluded from its callee scan.
var defLineRe = regexp.MustCompile(`^\s*(?:pub\s+|async\s+|static\s+|export\s+|default\s+)*(?:func|function|def|fn|class|struct|interface|type|impl|trait|module|mod)\b`)

// callees scans the definition body for call-shaped identifiers, dropping
// definition-shaped lines (so nested members are not read as calls), the symbol's
// own name, and language keywords/builtins, deduped in first-appearance order. It
// is a purely syntactic approximation.
func (r *resolver) callees(top candidate) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range r.bodyLines(top) {
		if defLineRe.MatchString(line) {
			continue
		}
		for _, m := range calleeRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if name == r.name || calleeSkip[name] || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// calleeSkip is the set of keywords, control-flow words, and common built-ins and
// type conversions that a call-shaped token match must not report as a callee.
var calleeSkip = toSet(
	// control flow / declarations across Go, Python, JS/TS, Rust, Java
	"if", "for", "while", "switch", "case", "return", "func", "fn", "def",
	"class", "struct", "interface", "impl", "trait", "module", "mod", "type",
	"const", "var", "let", "go", "defer", "select", "range", "map", "chan",
	"package", "import", "from", "as", "in", "is", "and", "or", "not", "elif",
	"else", "try", "except", "finally", "with", "del", "yield", "await",
	"async", "lambda", "pass", "raise", "new", "catch", "throw", "throws",
	"extends", "implements", "public", "private", "protected", "static",
	"void", "this", "super", "match", "use", "pub", "where",
	// common built-ins / conversions
	"make", "len", "cap", "append", "copy", "delete", "close", "panic",
	"recover", "print", "println", "string", "int", "int64", "bool", "float",
	"float64", "byte", "rune", "error", "self",
)

// toSet builds a lookup set from names.
func toSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
