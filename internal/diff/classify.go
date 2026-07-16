// Package diff renders a native structural diff: it resolves a vcs.DiffPlan,
// computes per-file hunks (internal/hunk), outlines each before/after blob
// (ast-grep via internal/astgrep), classifies every symbol as added, removed, or
// changed, and emits an anchored, budget-friendly report. It supersedes the
// previous engine-driven diff and the regex post-processing that shaped it.
package diff

import (
	"sort"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/hunk"
)

// changeKind classifies how a symbol changed across a diff.
type changeKind int

const (
	changeAdded    changeKind = iota // present only in the after outline
	changeRemoved                    // present only in the before outline
	changeModified                   // present in both, a hunk touches it
)

// symChange is one classified symbol. Its span is in after coordinates for an
// added or modified symbol and in before coordinates for a removed one.
type symChange struct {
	kind       changeKind
	name       string // qualified, e.g. "Foo.bar"
	start, end int
	sigChanged bool   // a modified symbol whose signature line differs
	sig        string // the after signature for an added or signature-changed symbol
}

// fileClass is one file's classification: its symbol changes (position-ordered)
// and the count of changed lines that fall outside every symbol span.
type fileClass struct {
	symbols     []symChange
	miscAdded   int
	miscRemoved int
}

// classifiableMember reports whether an outline member is a symbol in its own
// right. Fields (a Go struct's members) are structural noise represented by their
// container, so they are folded into it rather than emitted as rows.
func classifiableMember(m astgrep.OutlineItem) bool {
	return m.SymbolType != "field"
}

// oneBased converts an ast-grep 0-based line number to the 1-based convention.
func oneBased(line int) int {
	return line + 1
}

// outlineSym is a flattened outline entry: its qualified name, 1-based inclusive
// span, signature, and its direct classifiable children's spans (used to carve a
// container's own region out of its members').
type outlineSym struct {
	name       string
	start, end int
	sig        string
	childSpans [][2]int
}

// flattenOutline flattens an outline into a name→entries index (a name may repeat
// across overloads or receivers). Members are qualified under their container and
// recursed into; field members are folded into the container.
func flattenOutline(files []astgrep.OutlineFile) map[string][]outlineSym {
	byName := map[string][]outlineSym{}
	for _, f := range files {
		for _, it := range f.Items {
			flattenItem(it, "", byName)
		}
	}
	return byName
}

func flattenItem(it astgrep.OutlineItem, prefix string, byName map[string][]outlineSym) {
	name := it.Name
	if prefix != "" {
		name = prefix + "." + it.Name
	}
	var childSpans [][2]int
	for _, m := range it.Members {
		if classifiableMember(m) {
			childSpans = append(childSpans, [2]int{oneBased(m.Range.Start.Line), oneBased(m.Range.End.Line)})
		}
	}
	byName[name] = append(byName[name], outlineSym{
		name:       name,
		start:      oneBased(it.Range.Start.Line),
		end:        oneBased(it.Range.End.Line),
		sig:        it.Signature,
		childSpans: childSpans,
	})
	for _, m := range it.Members {
		if classifiableMember(m) {
			flattenItem(m, name, byName)
		}
	}
}

// topSpans returns every top-level item's full span, which covers all of its
// nested members; a changed line inside any of these is inside a symbol.
func topSpans(files []astgrep.OutlineFile) [][2]int {
	var spans [][2]int
	for _, f := range files {
		for _, it := range f.Items {
			spans = append(spans, [2]int{oneBased(it.Range.Start.Line), oneBased(it.Range.End.Line)})
		}
	}
	return spans
}

// classify diffs a file's before and after outlines against its hunks: a symbol
// present in only one outline is added or removed; a symbol in both whose direct
// region (its span minus its members') a hunk touches, or whose signature line
// changed, is modified. Changed lines outside every top-level span roll up into
// the misc counts. It is pure over the parsed outlines and hunks — no subprocess.
func classify(before, after []astgrep.OutlineFile, hunks []hunk.Hunk) fileClass {
	oldByName := flattenOutline(before)
	newByName := flattenOutline(after)

	var syms []symChange
	seen := map[string]bool{}
	for _, name := range unionNames(oldByName, newByName) {
		if seen[name] {
			continue
		}
		seen[name] = true
		olds, news := oldByName[name], newByName[name]
		matched := min(len(olds), len(news))
		for i := 0; i < matched; i++ {
			o, n := olds[i], news[i]
			sigChanged := o.sig != n.sig
			if sigChanged || touchesDirect(o, n, hunks) {
				change := symChange{kind: changeModified, name: name, start: n.start, end: n.end, sigChanged: sigChanged}
				if sigChanged {
					change.sig = n.sig
				}
				syms = append(syms, change)
			}
		}
		for i := matched; i < len(news); i++ {
			n := news[i]
			syms = append(syms, symChange{kind: changeAdded, name: name, start: n.start, end: n.end, sig: n.sig})
		}
		for i := matched; i < len(olds); i++ {
			o := olds[i]
			syms = append(syms, symChange{kind: changeRemoved, name: name, start: o.start, end: o.end})
		}
	}
	sort.SliceStable(syms, func(i, j int) bool {
		if syms[i].start != syms[j].start {
			return syms[i].start < syms[j].start
		}
		if syms[i].kind != syms[j].kind {
			return syms[i].kind < syms[j].kind
		}
		return syms[i].name < syms[j].name
	})

	fc := fileClass{symbols: syms}
	fc.miscAdded, fc.miscRemoved = miscCounts(before, after, hunks)
	return fc
}

// miscCounts sums the changed lines outside every top-level symbol span: added
// lines fall outside the after spans, removed lines outside the before spans.
func miscCounts(before, after []astgrep.OutlineFile, hunks []hunk.Hunk) (added, removed int) {
	oldCover, newCover := topSpans(before), topSpans(after)
	for _, h := range hunks {
		for line := h.NewStart; line <= h.NewEnd; line++ {
			if !inSpans(line, newCover) {
				added++
			}
		}
		for line := h.OldStart; line <= h.OldEnd; line++ {
			if !inSpans(line, oldCover) {
				removed++
			}
		}
	}
	return added, removed
}

// touchesDirect reports whether a hunk touches the symbol's direct region — its
// span minus its members' — on either side, so a change confined to a member
// leaves the container unmarked while a header or inter-member change marks it.
func touchesDirect(o, n outlineSym, hunks []hunk.Hunk) bool {
	for _, h := range hunks {
		if directHit(h.OldStart, h.OldEnd, o.start, o.end, o.childSpans) {
			return true
		}
		if directHit(h.NewStart, h.NewEnd, n.start, n.end, n.childSpans) {
			return true
		}
	}
	return false
}

// directHit reports whether the hunk range [hStart,hEnd] (empty when hEnd<hStart)
// overlaps [sStart,sEnd] at a line inside no child span.
func directHit(hStart, hEnd, sStart, sEnd int, childSpans [][2]int) bool {
	if hEnd < hStart {
		return false
	}
	lo, hi := max(hStart, sStart), min(hEnd, sEnd)
	for line := lo; line <= hi; line++ {
		if !inSpans(line, childSpans) {
			return true
		}
	}
	return false
}

// inSpans reports whether line falls within any [start,end] span.
func inSpans(line int, spans [][2]int) bool {
	for _, s := range spans {
		if line >= s[0] && line <= s[1] {
			return true
		}
	}
	return false
}

// unionNames returns the sorted union of the two indexes' keys, so classification
// is deterministic before the position sort.
func unionNames(a, b map[string][]outlineSym) []string {
	set := map[string]struct{}{}
	for name := range a {
		set[name] = struct{}{}
	}
	for name := range b {
		set[name] = struct{}{}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
