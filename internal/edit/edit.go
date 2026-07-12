// Package edit applies an in-place splice — a content replacement or a deletion
// — to a single file, addressed by a content anchor or a plain line range. It
// resolves and rewrites from one atomic snapshot and never dispatches to an
// engine.
package edit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// Run resolves a.Section against a fresh snapshot of a.Path, splices in a.Content
// (or deletes the range when a.Delete), writes the result atomically, and returns
// a budget-capped report of the change. An anchored section whose content vanished
// or resolves ambiguously errors before any write; a plain numeric range is spliced
// unverified but bounds-checked. Every error path leaves the file byte-identical.
func Run(a backend.Args) (string, error) {
	if a.Delete && a.Content != "" {
		return "", fmt.Errorf("edit %s: --delete takes no content", a.Path)
	}

	// Resolve symlinks once up front so the atomic temp+rename in cache.Store
	// writes through to the real target instead of replacing the symlink inode.
	resolved, err := filepath.EvalSymlinks(a.Path)
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // the path is the caller's own edit target, not untrusted input
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}
	f := anchor.FromBytes(a.Path, data)
	lines := f.Lines()

	oldA, oldB, move, err := resolve(f, a.Section)
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}

	crlf := len(lines) > 0 && strings.HasSuffix(lines[0], "\r")
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'

	logical := logicalLines(a)
	inserted := logical
	if crlf {
		inserted = make([]string, len(logical))
		for i, l := range logical {
			inserted[i] = l + "\r"
		}
	}

	newLines := make([]string, 0, len(lines)-(oldB-oldA+1)+len(inserted))
	newLines = append(newLines, lines[:oldA-1]...)
	newLines = append(newLines, inserted...)
	newLines = append(newLines, lines[oldB:]...)

	newContent := strings.Join(newLines, "\n")
	if trailingNewline && len(newLines) > 0 {
		newContent += "\n"
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}
	if err := cache.Store(resolved, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("edit %s: %w", a.Path, err)
	}

	return report(a, lines, newLines, oldA, oldB, len(inserted), move), nil
}

// resolve maps a.Section to a resolved 1-indexed inclusive [start,end] range plus
// any relocation. An anchored ref re-resolves by content (vanished or ambiguous
// errors); a plain numeric range is bounds-checked but unverified.
func resolve(f *anchor.File, section string) (start, end int, move *anchor.Move, err error) {
	ref, ok, err := anchor.Parse(section)
	if err != nil {
		return 0, 0, nil, err
	}
	if ok {
		rng, mv, err := f.Resolve(ref)
		if err != nil {
			return 0, 0, nil, err
		}
		return rng.Start, rng.End, mv, nil
	}
	start, end, numeric := parseNumeric(section)
	if !numeric {
		return 0, 0, nil, fmt.Errorf("section %q is not a line range (%q or %q), a single line, or an anchor", section, "A-B", "A,B")
	}
	if start < 1 || start > end || end > len(f.Lines()) {
		return 0, 0, nil, fmt.Errorf("line range %s out of bounds: file has %d lines", section, len(f.Lines()))
	}
	return start, end, nil, nil
}

// parseNumeric parses a line range — a plain "A", a dash "A-B", or the comma
// alias "A,B" — into 1-indexed bounds; ok is false for anything else (a heading,
// an empty section, garbage).
func parseNumeric(section string) (start, end int, ok bool) {
	section = anchor.NormalizeRange(section)
	dash := strings.IndexByte(section, '-')
	if dash < 0 {
		n, err := strconv.Atoi(section)
		if err != nil {
			return 0, 0, false
		}
		return n, n, true
	}
	start, err := strconv.Atoi(section[:dash])
	if err != nil {
		return 0, 0, false
	}
	end, err = strconv.Atoi(section[dash+1:])
	if err != nil {
		return 0, 0, false
	}
	return start, end, true
}

// logicalLines splits the replacement content into EOL-normalized lines, or nil
// for a deletion. A single trailing newline terminates the last line rather than
// adding an empty one; an explicitly empty content yields one empty line.
func logicalLines(a backend.Args) []string {
	if a.Delete {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(a.Content, "\n"), "\n")
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	return parts
}

// report renders the change: an optional relocation note, a header pinning the
// pre- and post-edit anchored ranges, and a mini-diff of removed and inserted
// lines. Output is self-capped to a.Budget.
func report(a backend.Args, oldLines, newLines []string, oldA, oldB, insertedCount int, move *anchor.Move) string {
	var b strings.Builder
	b.WriteString(anchor.MoveNote(anchor.Of(oldLines[oldA-1]), move))

	oldRef := formatRef(oldA, oldB, anchor.Of(oldLines[oldA-1]))
	fmt.Fprintf(&b, "%s:%s → %s\n", a.Path, oldRef, newSide(a, newLines, oldA, insertedCount))

	for _, l := range oldLines[oldA-1 : oldB] {
		fmt.Fprintf(&b, "- %s\n", strings.TrimSuffix(l, "\r"))
	}
	for _, l := range logicalLines(a) {
		fmt.Fprintf(&b, "+ %s\n", l)
	}

	return render.Cap(b.String(), a.Budget)
}

// newSide renders the post-edit anchored range: the inserted span for a content
// edit, the line now at the splice point for a deletion (the new last line when
// the splice point fell past EOF), or "(empty)" when the file is now empty.
func newSide(a backend.Args, newLines []string, oldA, insertedCount int) string {
	if a.Delete {
		if len(newLines) == 0 {
			return "(empty)"
		}
		pos := min(oldA, len(newLines))
		return a.Path + ":" + formatRef(pos, pos, anchor.Of(newLines[pos-1]))
	}
	start, end := oldA, oldA+insertedCount-1
	return a.Path + ":" + formatRef(start, end, anchor.Of(newLines[start-1]))
}

// formatRef renders a resolved range as a single-line or range anchor.
func formatRef(start, end int, h anchor.Hash) string {
	if start == end {
		return anchor.Format(start, h)
	}
	return anchor.FormatRange(start, end, h)
}
