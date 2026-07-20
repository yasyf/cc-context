package edit

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// runMatch replaces (or deletes) the literal bytes of a.Match — over the whole file
// or the a.Section span — splicing every match in one pass and writing once. The
// replacement is written verbatim (no logicalLines), so a trailing newline survives.
// Every error path returns before cache.Store, leaving the file byte-identical.
func runMatch(a backend.Args, resolved string, data []byte, f *anchor.File) (string, error) {
	lines := f.Lines()
	crlf := len(lines) > 0 && strings.HasSuffix(lines[0], "\r")
	needle := normalizeEOL(a.Match, crlf)
	if needle == "" {
		return "", fmt.Errorf("edit %s: empty --match", a.Path)
	}
	replacement := normalizeEOL(a.Content, crlf)
	offsets := lineOffsets(lines)

	scanStart, scanEnd := 0, len(data)
	var move *anchor.Move
	scoped := a.Section != ""
	var spanA, spanB int
	if scoped {
		oldA, oldB, mv, err := resolve(f, a.Section)
		if err != nil {
			return "", fmt.Errorf("edit %s: %w", a.Path, err)
		}
		move, spanA, spanB = mv, oldA, oldB
		scanStart = offsets[oldA-1]
		scanEnd = lineEnd(offsets, len(data), oldB, len(lines))
	}

	matches := scan(string(data[scanStart:scanEnd]), needle, scanStart)

	switch {
	case len(matches) == 0 && scoped:
		return "", fmt.Errorf("edit %s: --match not found in %s", a.Path, formatRef(spanA, spanB, anchor.Of(lines[spanA-1])))
	case len(matches) == 0:
		return "", fmt.Errorf("edit %s: --match not found", a.Path)
	case len(matches) > 1 && !a.All:
		cands := make([]string, len(matches))
		for i, ms := range matches {
			ln := lineAt(offsets, ms)
			cands[i] = anchor.Format(ln, anchor.Of(lines[ln-1]))
		}
		return "", fmt.Errorf("edit %s: --match found %d matches (%s); scope with --at, extend the match, or pass --all", a.Path, len(matches), strings.Join(cands, ", "))
	}
	if !a.Delete && replacement == needle {
		return "", fmt.Errorf("edit %s: nothing to change: content equals --match", a.Path)
	}

	final := splice(data, matches, needle, replacement)

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}
	if err := cache.Store(resolved, final, info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}

	var noteHash anchor.Hash
	if scoped {
		noteHash = anchor.Of(lines[spanA-1])
	}
	return matchReport(a, final, lines, offsets, matches, needle, replacement, move, noteHash), nil
}

// normalizeEOL forces s onto the file's line ending: on a CRLF file every "\r\n"
// and bare "\n" becomes "\r\n" while a standalone "\r" is preserved; on an LF file
// s is unchanged. Collapsing "\r\n" first keeps it idempotent.
func normalizeEOL(s string, crlf bool) string {
	if !crlf {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// lineOffsets returns the byte offset at which each 1-indexed line begins; the
// trailing "\r" of a CRLF line rides inside the line, so a single "\n" separator
// keeps the arithmetic correct for both line endings.
func lineOffsets(lines []string) []int {
	offsets := make([]int, len(lines))
	pos := 0
	for i, l := range lines {
		offsets[i] = pos
		pos += len(l) + 1
	}
	return offsets
}

// lineEnd returns the byte offset just past line's trailing newline: the start of
// the next line, or the end of the file for the last line.
func lineEnd(offsets []int, dataLen, line, numLines int) int {
	if line < numLines {
		return offsets[line]
	}
	return dataLen
}

// lineAt returns the 1-indexed line containing byte off.
func lineAt(offsets []int, off int) int {
	return sort.Search(len(offsets), func(i int) bool { return offsets[i] > off })
}

// scan returns the absolute byte offsets of every non-overlapping left-to-right
// occurrence of needle in region, whose first byte sits at base in the whole file.
func scan(region, needle string, base int) []int {
	var matches []int
	for i := 0; ; {
		rel := strings.Index(region[i:], needle)
		if rel < 0 {
			return matches
		}
		matches = append(matches, base+i+rel)
		i += rel + len(needle)
	}
}

// splice rewrites data with replacement substituted for every match.
func splice(data []byte, matches []int, needle, replacement string) []byte {
	var b strings.Builder
	prev := 0
	for _, ms := range matches {
		b.WriteString(string(data[prev:ms]))
		b.WriteString(replacement)
		prev = ms + len(needle)
	}
	b.WriteString(string(data[prev:]))
	return []byte(b.String())
}

// matchReport renders the change budget-capped once: a moved scoped anchor leads
// with a relocation note, --all always prints a pluralized count header, then one
// blank-line-separated stanza per match. Every post-edit coordinate reads the
// final file's own line structure, so a match that fused or split lines renders
// panic-free.
func matchReport(a backend.Args, final []byte, lines []string, offsets, matches []int, needle, replacement string, move *anchor.Move, noteHash anchor.Hash) string {
	finalLines := anchor.FromBytes(a.Path, final).Lines()
	finalOffsets := lineOffsets(finalLines)
	d := len(replacement) - len(needle)

	var b strings.Builder
	b.WriteString(anchor.MoveNote(noteHash, move))
	if a.All {
		noun := "occurrences"
		if len(matches) == 1 {
			noun = "occurrence"
		}
		fmt.Fprintf(&b, "# %d %s replaced\n", len(matches), noun)
	}
	for i, ms := range matches {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(stanza(a, lines, offsets, finalLines, finalOffsets, i, ms, needle, replacement, d))
	}
	return render.Cap(b.String(), a.Budget)
}

// stanza renders one match: the arrow header pinning the whole pre-edit lines to
// the whole post-edit lines, then the removed and inserted lines. finalStart is
// where match i's replacement begins in the final file — its own offset plus the
// net length change of the i earlier splices — so the inserted lines and their
// anchors read from the final bytes and a fused or split neighbour still renders.
func stanza(a backend.Args, lines []string, offsets []int, finalLines []string, finalOffsets []int, i, ms int, needle, replacement string, d int) string {
	oldStart := lineAt(offsets, ms)
	oldEnd := lineAt(offsets, ms+len(needle)-1)
	oldRef := formatRef(oldStart, oldEnd, anchor.Of(lines[oldStart-1]))

	finalStart := ms + i*d
	var newSide string
	var inserted []string
	if replacement == "" {
		newSide = matchDeleteSide(a, finalLines, finalOffsets, finalStart)
	} else {
		newStart := lineAt(finalOffsets, finalStart)
		newEnd := lineAt(finalOffsets, finalStart+len(replacement)-1)
		inserted = finalLines[newStart-1 : newEnd]
		newSide = a.Path + ":" + formatRef(newStart, newEnd, anchor.Of(finalLines[newStart-1]))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s:%s → %s\n", a.Path, oldRef, newSide)
	for _, l := range lines[oldStart-1 : oldEnd] {
		fmt.Fprintf(&b, "- %s\n", strings.TrimSuffix(l, "\r"))
	}
	for _, l := range inserted {
		fmt.Fprintf(&b, "+ %s\n", strings.TrimSuffix(l, "\r"))
	}
	return b.String()
}

// matchDeleteSide renders a deletion's post-edit anchor: the final line now at the
// splice point, or "(empty)" when the file is now empty — mirroring the at-mode
// delete report.
func matchDeleteSide(a backend.Args, finalLines []string, finalOffsets []int, finalStart int) string {
	if len(finalLines) == 0 {
		return "(empty)"
	}
	pos := lineAt(finalOffsets, finalStart)
	return a.Path + ":" + formatRef(pos, pos, anchor.Of(finalLines[pos-1]))
}
