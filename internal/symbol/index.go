package symbol

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/yasyf/cc-context/internal/astgrep"
)

// candidate is one definition-index hit: a flattened outline item, its file, and
// whether its name reads as exported.
type candidate struct {
	path       string
	name       string
	qualified  string
	kind       string
	signature  string
	start, end int // 1-based inclusive span
	exported   bool
}

// candidates flattens every outline file and keeps the items whose name matches
// the query — exactly, or case-insensitively when fold is set — including members
// (a class's methods, a struct's fields). A qualifier further restricts the hits
// to those whose container or receiver equals it.
func (r *resolver) candidates(files []astgrep.OutlineFile, fold bool) []candidate {
	var out []candidate
	for _, f := range files {
		path := normPath(f.Path)
		for _, it := range f.Flatten() {
			if !nameMatches(it.Name, r.name, fold) {
				continue
			}
			if r.qualifier != "" && !qualifierMatches(r.qualifier, it.Qualified, it.Signature) {
				continue
			}
			out = append(out, candidate{
				path:      path,
				name:      it.Name,
				qualified: it.Qualified,
				kind:      it.SymbolType,
				signature: it.Signature,
				start:     it.StartLine,
				end:       it.EndLine,
				exported:  exportedName(it.Name),
			})
		}
	}
	return out
}

// nameMatches reports whether an item name equals the query name, folding case
// when fold is set.
func nameMatches(itemName, query string, fold bool) bool {
	if fold {
		return strings.EqualFold(itemName, query)
	}
	return itemName == query
}

// goReceiverRe captures the receiver type of a Go method signature —
// "func (w Widget) …", "func (w *Widget) …", "func (Widget) …" — so a Recv.Method
// query resolves a method whose outline carries no container qualification.
var goReceiverRe = regexp.MustCompile(`^func\s*\(\s*(?:\w+\s+)?\*?(\w+)`)

// qualifierMatches reports whether qualifier names the item's container: the
// immediate parent segment of a member's qualified name (Class.method → Class),
// or the receiver type parsed from a Go method signature.
func qualifierMatches(qualifier, qualified, signature string) bool {
	if segs := strings.Split(qualified, "."); len(segs) >= 2 && segs[len(segs)-2] == qualifier {
		return true
	}
	if m := goReceiverRe.FindStringSubmatch(signature); m != nil && m[1] == qualifier {
		return true
	}
	return false
}

// exportedName reports whether a name reads as exported: its first rune is an
// uppercase letter. It is Go-exact and a reasonable public-symbol proxy
// elsewhere; ast-grep's own isExported flag is all-true for Go and so unusable.
func exportedName(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// rank orders candidates best-first: an exact-case name over a folded one, an
// exported name over an unexported one, a non-test file over a test file, a
// shorter path over a longer one, then path and start line for determinism.
func rank(cands []candidate, name string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if x, y := a.name == name, b.name == name; x != y {
			return x
		}
		if a.exported != b.exported {
			return a.exported
		}
		if x, y := isTestFile(a.path), isTestFile(b.path); x != y {
			return y
		}
		if len(a.path) != len(b.path) {
			return len(a.path) < len(b.path)
		}
		if a.path != b.path {
			return a.path < b.path
		}
		return a.start < b.start
	})
}

// defFile returns the scope outline's record for path, if the scope covered it.
func (r *resolver) defFile(path string) (astgrep.OutlineFile, bool) {
	for _, f := range r.scopeSet {
		if normPath(f.Path) == path {
			return f, true
		}
	}
	return astgrep.OutlineFile{}, false
}

// siblingItems returns the defining file's top-level items excluding the resolved
// symbol itself, reusing the already-fetched scope outline.
func (r *resolver) siblingItems(top candidate) []astgrep.FlatItem {
	f, ok := r.defFile(top.path)
	if !ok {
		return nil
	}
	var out []astgrep.FlatItem
	for _, it := range f.Flatten() {
		if it.Depth != 0 {
			continue
		}
		if it.Name == top.name && it.StartLine == top.start {
			continue
		}
		out = append(out, it)
	}
	return out
}
