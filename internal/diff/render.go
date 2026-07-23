package diff

import (
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/secrets"
)

// classifyCap bounds how many covered files undergo full symbol classification;
// files beyond it are disclosed with hunk counts only.
const classifyCap = 30

const (
	sigilAdded    = "+"
	sigilModified = "~"
	sigilRemoved  = "−" // MINUS SIGN, distinct from the ASCII hyphen in code
	emDash        = "—"
	midDot        = "·"
	ellipsis      = "…"
	arrow         = "→" // rename header separator: "old → new"
)

// fileKind selects how one changed file renders.
type fileKind int

const (
	fileKindSymbols  fileKind = iota // ast-grep covered: classified symbol rows
	fileKindRawHunks                 // uncovered language: raw hunks from hunk.Compute
	fileKindRawText                  // non-symbolic plan: jj diff --git text
	fileKindBinary                   // binary blob
	fileKindCapped                   // beyond the classification cap: hunk count only
	fileKindRenamed                  // a rename with no content change
)

// fileReport is one changed file's render payload; which fields are set depends
// on kind.
type fileReport struct {
	path        string
	renamedFrom string // non-empty for a rename: the pre-image path
	kind        fileKind
	class       fileClass   // fileKindSymbols
	hunks       []hunk.Hunk // fileKindSymbols (--full), fileKindRawHunks, fileKindCapped
	before      []byte      // fileKindSymbols (--full context), fileKindRawHunks
	after       []byte      // fileKindSymbols (anchor source)
	raw         string      // fileKindRawText
	ext         string      // fileKindRawHunks
}

// fileHeader renders a file report's "## " heading path, showing "old → new" for
// a detected rename.
func fileHeader(r fileReport) string {
	if r.renamedFrom != "" {
		return r.renamedFrom + " " + arrow + " " + r.path
	}
	return r.path
}

// diffModel is the assembled, render-ready diff: the source label, the symbol
// totals across classified files, and the per-file reports in VCS order.
type diffModel struct {
	label                   string
	added, changed, removed int
	files                   []fileReport
}

// render emits the final diff shape from an assembled model — a header line, one
// section per file, and (in terse mode, when any file classified) a trailer
// pointing at --full — masking each file's section in that file's path context
// (a renamed file's under the pre-image path too) unless reveal is set, and
// returning the fired rule ids in file order for the caller's footer. In full
// mode each classified file also inlines its 3-context hunks; the trailer is
// dropped.
func render(m diffModel, full, reveal bool) (string, []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "# diff %s %s %d files %s +%d ~%d %s%d symbols\n",
		m.label, emDash, len(m.files), midDot, m.added, m.changed, sigilRemoved, m.removed)

	capped := 0
	for _, r := range m.files {
		if r.kind == fileKindCapped {
			capped++
		}
	}

	var ids []string
	disclosed, hasSymbols := false, false
	for _, r := range m.files {
		if r.kind == fileKindCapped && !disclosed {
			fmt.Fprintf(&b, "# %s %d files beyond the %d-file classification cap %s hunk counts only:\n",
				ellipsis, capped, classifyCap, emDash)
			disclosed = true
		}
		var fb strings.Builder
		switch r.kind {
		case fileKindSymbols:
			renderSymbolFile(&fb, r, full)
			hasSymbols = true
		case fileKindRawHunks:
			renderRawHunks(&fb, r, full)
		case fileKindRawText:
			renderRawText(&fb, r)
		case fileKindBinary:
			fmt.Fprintf(&fb, "## %s %s binary\n", fileHeader(r), emDash)
		case fileKindCapped:
			renderCapped(&fb, r)
		case fileKindRenamed:
			fmt.Fprintf(&fb, "## %s %s renamed, no content change\n", fileHeader(r), emDash)
		}
		section := fb.String()
		if !reveal {
			var fired []string
			section, fired = secrets.Mask(section, r.path)
			ids = append(ids, fired...)
			if r.renamedFrom != "" {
				// A renamed file's hunks mix old- and new-image content; a second
				// pass under the pre-image path fires the rules gated on it (.env),
				// masking the union of both paths' findings.
				section, fired = secrets.Mask(section, r.renamedFrom)
				ids = append(ids, fired...)
			}
		}
		b.WriteString(section)
	}
	if !full && hasSymbols {
		fmt.Fprintf(&b, "hunks hidden %s --full inlines per-file hunks\n", emDash)
	}
	return b.String(), ids
}

// symRow is one rendered symbol line before column alignment.
type symRow struct {
	sigil, name, loc, detail string
}

// renderSymbolFile writes a covered file's header, its aligned symbol rows, its
// misc rollup, and — in full mode — its 3-context hunks.
func renderSymbolFile(b *strings.Builder, r fileReport, full bool) {
	fc := r.class
	added, changed, removed := countKinds(fc.symbols)
	b.WriteString("## " + fileHeader(r))
	if parts := countParts(added, changed, removed); parts != "" {
		b.WriteString(" (" + parts + ")")
	}
	b.WriteByte('\n')

	lines := anchor.FromBytes(r.path, r.after).Lines()
	rows := make([]symRow, 0, len(fc.symbols))
	nameW, locW := 0, 0
	for _, s := range fc.symbols {
		row := symRow{sigil: sigilFor(s.kind), name: s.name}
		if s.kind == changeRemoved {
			row.loc = "(was " + spanText(s.start, s.end) + ")"
		} else {
			row.loc = locWithAnchor(lines, s.start, s.end)
			if s.kind == changeAdded || s.sigChanged {
				row.detail = s.sig
			} else {
				row.detail = "body"
			}
		}
		if len(s.name) > nameW {
			nameW = len(s.name)
		}
		if row.detail != "" && len(row.loc) > locW {
			locW = len(row.loc)
		}
		rows = append(rows, row)
	}
	for _, row := range rows {
		if row.detail == "" {
			fmt.Fprintf(b, "[%s] %-*s  %s\n", row.sigil, nameW, row.name, row.loc)
		} else {
			fmt.Fprintf(b, "[%s] %-*s  %-*s   %s\n", row.sigil, nameW, row.name, locW, row.loc, row.detail)
		}
	}
	if fc.miscAdded > 0 || fc.miscRemoved > 0 {
		fmt.Fprintf(b, "%s +%d/%s%d lines outside symbols\n", ellipsis, fc.miscAdded, sigilRemoved, fc.miscRemoved)
	}
	if full {
		writeHunks(b, r.hunks, anchor.FromBytes(r.path, r.before).Lines(), 3)
	}
}

// renderRawHunks writes an uncovered-language file's header note and its hunks
// (zero context terse, 3-context full).
func renderRawHunks(b *strings.Builder, r fileReport, full bool) {
	fmt.Fprintf(b, "## %s %s %s\n", fileHeader(r), emDash, noRulesNote(r.ext))
	ctx := 0
	if full {
		ctx = 3
	}
	writeHunks(b, r.hunks, anchor.FromBytes(r.path, r.before).Lines(), ctx)
}

// renderRawText writes a non-symbolic file's raw `jj diff --git` body.
func renderRawText(b *strings.Builder, r fileReport) {
	b.WriteString("## " + r.path + "\n")
	if text := strings.TrimRight(r.raw, "\n"); text != "" {
		b.WriteString(text)
		b.WriteByte('\n')
	}
}

// renderCapped writes a hunk-count-only line for a file beyond the cap.
func renderCapped(b *strings.Builder, r fileReport) {
	adds, dels := hunkLineCounts(r.hunks)
	fmt.Fprintf(b, "## %s %s %d hunks (+%d/%s%d)\n", fileHeader(r), emDash, len(r.hunks), adds, sigilRemoved, dels)
}

// writeHunks writes each hunk as a unified block with ctx context lines drawn
// from the before-image lines.
func writeHunks(b *strings.Builder, hunks []hunk.Hunk, before []string, ctx int) {
	for _, h := range hunks {
		b.WriteString(renderHunkUnified(h, before, ctx))
		b.WriteByte('\n')
	}
}

// renderHunkUnified renders one hunk as a unified block: an `@@ -o,c +n,c @@`
// header then ctx common lines, the deletions, the additions, and ctx trailing
// common lines. Context is taken from before (the pre-image), whose lines the
// hunk's coordinates index.
func renderHunkUnified(h hunk.Hunk, before []string, ctx int) string {
	preStart := h.OldStart - ctx
	if preStart < 1 {
		preStart = 1
	}
	pre := before[preStart-1 : h.OldStart-1]
	postStart, postEnd := h.OldEnd+1, h.OldEnd+ctx
	if postEnd > len(before) {
		postEnd = len(before)
	}
	var post []string
	if postStart <= postEnd {
		post = before[postStart-1 : postEnd]
	}
	oldCount := len(pre) + len(h.Old) + len(post)
	newCount := len(pre) + len(h.New) + len(post)

	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", preStart, oldCount, h.NewStart-len(pre), newCount)
	for _, c := range pre {
		b.WriteString(" " + c + "\n")
	}
	for _, o := range h.Old {
		b.WriteString("-" + o + "\n")
	}
	for _, n := range h.New {
		b.WriteString("+" + n + "\n")
	}
	for _, c := range post {
		b.WriteString(" " + c + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// countKinds tallies a file's added, changed, and removed symbol rows.
func countKinds(syms []symChange) (added, changed, removed int) {
	for _, s := range syms {
		switch s.kind {
		case changeAdded:
			added++
		case changeModified:
			changed++
		case changeRemoved:
			removed++
		}
	}
	return added, changed, removed
}

// countParts renders the nonzero symbol counts for a file header, e.g. "+1 ~2".
func countParts(added, changed, removed int) string {
	var parts []string
	if added > 0 {
		parts = append(parts, fmt.Sprintf("+%d", added))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("~%d", changed))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%s%d", sigilRemoved, removed))
	}
	return strings.Join(parts, " ")
}

// sigilFor maps a change kind to its row sigil.
func sigilFor(k changeKind) string {
	switch k {
	case changeAdded:
		return sigilAdded
	case changeRemoved:
		return sigilRemoved
	default:
		return sigilModified
	}
}

// spanText renders a symbol's line span, collapsing a single line to "L<n>".
func spanText(start, end int) string {
	if start == end {
		return fmt.Sprintf("L%d", start)
	}
	return fmt.Sprintf("L%d-%d", start, end)
}

// locWithAnchor renders a symbol's after-side span with a content anchor hashed
// from its start line, falling back to a bare span when the line is out of range.
func locWithAnchor(lines []string, start, end int) string {
	span := spanText(start, end)
	if start >= 1 && start <= len(lines) {
		return span + "#" + anchor.Of(lines[start-1]).String()
	}
	return span
}

// noRulesNote renders the uncovered-language header note for an extension.
func noRulesNote(ext string) string {
	if ext == "" {
		return "no ast-grep rules; raw hunks"
	}
	return "no ast-grep rules for " + ext + "; raw hunks"
}

// hunkLineCounts sums a file's added and deleted line counts across its hunks.
func hunkLineCounts(hunks []hunk.Hunk) (added, deleted int) {
	for _, h := range hunks {
		added += len(h.New)
		deleted += len(h.Old)
	}
	return added, deleted
}
