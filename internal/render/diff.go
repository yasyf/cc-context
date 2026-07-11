package render

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/vcs"
)

var (
	// diffFileHeader matches a tilth structural-diff per-file header, capturing
	// the working-side path (which may contain spaces) and symbol count. The
	// optional "x/… w/" prefix covers working-tree output (c/ and i/ index
	// variants); a bare path covers commit-range output, where tilth emits no
	// c//w/ prefix.
	diffFileHeader = regexp.MustCompile(`^## (?:[a-z]/.+? w/)?(.+?) \((\d+) symbols\)$`)
	// collapsedDiffHeader matches tilth's one-line header for a scoped diff with no
	// symbol changes, capturing the whole path region (which may contain spaces)
	// and the symbol count; collapsedDiffWorkingPath derives the working-side path
	// from it. The em dash is U+2014 and the minus is U+2212 (or ASCII '-'), copied
	// from live output.
	collapsedDiffHeader = regexp.MustCompile(`^# Diff: (.+?) — (\d+) symbols touched, \+\d+/[−-]\d+ lines$`)
	// diffSHARe matches a full 40-hex object id as a standalone token; it is only
	// ever applied to the "# Diff:" header line, never the hunk body, where a
	// 40-hex string can be legitimate content.
	diffSHARe = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	// diffGitPreamble matches a "diff --git X/left Y/right" line, capturing both
	// mnemonic-prefixed sides so the collapse can test them against the heading.
	diffGitPreamble = regexp.MustCompile(`^diff --git (\S+) (\S+)$`)
	// diffPathPrefix strips git's single-letter mnemonic prefix (a/ b/ c/ i/ w/ o/)
	// so a preamble path can be compared to the unprefixed heading path.
	diffPathPrefix = regexp.MustCompile(`^[a-z]/`)
	// diffSymbolRow matches a supplemented changed-symbol row "  [~] Name  L11  (…)",
	// capturing the run up to the "L", the line number, and the trailing "(…)".
	// The required "\s+\(" tail pins the match to the real line locator, never an
	// "L<digits>" that happens to sit inside a symbol name.
	diffSymbolRow = regexp.MustCompile(`^(\s*\[.\].*?)L(\d+)(\s+\(.*)$`)
)

// RunDiffCLI runs the tilth diff, expands a collapsed scoped-diff header into a
// supplementable section, supplements any empty-hunk section with its raw
// jj-aware hunk, shortens the header SHAs, collapses each file's redundant
// preamble, anchors the changed-symbol rows, and caps to budget.
func RunDiffCLI(ctx context.Context, bin string, argv []string, source, scope string, budget int) (string, error) {
	out, err := RunCLI(ctx, bin, argv)
	if err != nil {
		return "", err
	}
	out = expandCollapsedDiffHeader(out, scope)
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	tilthSource, useTilth, _, err := vcs.ResolveDiffSource(ctx, cwd, source, "")
	if err != nil {
		return "", fmt.Errorf("resolve diff source: %w", err)
	}
	fetch := func(workPath string) (string, error) {
		hunkArgv := vcs.RawHunkArgvFor(cwd, source, tilthSource, useTilth, workPath)
		return RunCLI(ctx, hunkArgv[0], hunkArgv[1:])
	}
	supplemented, err := SupplementDiff(out, fetch)
	if err != nil {
		return "", err
	}
	piped := shortenDiffSHAs(supplemented)
	piped = collapsePreambles(piped)
	piped = annotateDiffSymbols(piped, anchor.NewFiles(cwd))
	return Cap(piped, budget), nil
}

// expandCollapsedDiffHeader rewrites tilth's collapsed scoped-diff one-liner into
// the two-tier header the pipeline needs. A --scope diff with no symbol changes
// yields only the "# Diff: <path> — 0 symbols touched, +0/−0 lines" line and no
// "## <path> (0 symbols)" section, so SupplementDiff never splices the raw hunk
// and the whole diff is dropped. Under a triple gate — a non-empty scope, a
// 0-symbol collapsed line, no existing per-file header, and the derived working
// path equal to the scope — it appends the synthesized "## <path> (0 symbols)" header so the
// rest of the pipeline runs unchanged. Every other shape passes through untouched,
// so the scoped >0-symbol per-symbol format is never disturbed.
func expandCollapsedDiffHeader(out, scope string) string {
	if scope == "" {
		return out
	}
	lines := strings.Split(out, "\n")
	collapsedIdx := -1
	var path string
	for i, line := range lines {
		trimmed := strings.TrimSuffix(line, "\r")
		if diffFileHeader.MatchString(trimmed) {
			return out
		}
		if m := collapsedDiffHeader.FindStringSubmatch(trimmed); m != nil && m[2] == "0" {
			collapsedIdx = i
			path = collapsedDiffWorkingPath(m[1])
		}
	}
	if collapsedIdx < 0 || path != scope {
		return out
	}
	expanded := make([]string, 0, len(lines)+1)
	expanded = append(expanded, lines[:collapsedIdx+1]...)
	expanded = append(expanded, "## "+path+" (0 symbols)")
	expanded = append(expanded, lines[collapsedIdx+1:]...)
	return strings.Join(expanded, "\n")
}

// collapsedDiffWorkingPath derives the working-side path from a collapsed-header
// capture. tilth emits "c/<X> w/<Y>" for a working-tree diff (X==Y) and a bare
// path for a commit-range diff; a path may hold spaces, so the capture is the
// whole middle. Split on the last " w/": when the left side carries a mnemonic
// prefix and strips to the working side, return that side, else the capture
// unchanged. A path that itself contains " w/" is best-effort.
func collapsedDiffWorkingPath(capture string) string {
	i := strings.LastIndex(capture, " w/")
	if i < 0 {
		return capture
	}
	left, right := capture[:i], capture[i+len(" w/"):]
	if diffPathPrefix.MatchString(left) && stripDiffPrefix(left) == right {
		return right
	}
	return capture
}

// shortenDiffSHAs trims every full 40-hex object id on the first "# Diff:" header
// line to its first 10 characters. Only that line is touched, so a 40-hex string
// living in the hunk body is left byte-identical.
func shortenDiffSHAs(out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSuffix(line, "\r"), "# Diff:") {
			lines[i] = diffSHARe.ReplaceAllStringFunc(line, func(sha string) string { return sha[:10] })
			break
		}
	}
	return strings.Join(lines, "\n")
}

// collapsePreambles drops the lines a supplemented raw hunk repeats from its "## P"
// heading: the "diff --git X/P Y/P" line and its "index …", "--- P|/dev/null", and
// "+++ P|/dev/null" preamble, but only while both sides of the "diff --git" resolve
// to the heading path P. A rename or copy names two different paths, so its whole
// preamble — "rename from/to", "similarity index", "diff --git" — is kept; mode and
// binary lines and every "@@" hunk header are always kept.
func collapsePreambles(out string) string {
	lines := strings.Split(out, "\n")
	kept := make([]string, 0, len(lines))
	var heading string
	collapsible := false
	for _, line := range lines {
		match := strings.TrimSuffix(line, "\r")
		if m := diffFileHeader.FindStringSubmatch(match); m != nil {
			heading = m[1]
			collapsible = false
			kept = append(kept, line)
			continue
		}
		if heading != "" {
			if m := diffGitPreamble.FindStringSubmatch(match); m != nil {
				collapsible = stripDiffPrefix(m[1]) == heading && stripDiffPrefix(m[2]) == heading
				if collapsible {
					continue
				}
				kept = append(kept, line)
				continue
			}
			if strings.HasPrefix(match, "@@") {
				collapsible = false
				kept = append(kept, line)
				continue
			}
			if collapsible && droppablePreamble(match, heading) {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// stripDiffPrefix removes git's single-letter mnemonic prefix from a preamble path.
func stripDiffPrefix(path string) string {
	return diffPathPrefix.ReplaceAllString(path, "")
}

// droppablePreamble reports whether line is an "index …", "--- P|/dev/null", or
// "+++ P|/dev/null" preamble line the enclosing "## P" heading already conveys.
func droppablePreamble(line, heading string) bool {
	if strings.HasPrefix(line, "index ") {
		return true
	}
	if rest, ok := strings.CutPrefix(line, "--- "); ok {
		return rest == "/dev/null" || stripDiffPrefix(rest) == heading
	}
	if rest, ok := strings.CutPrefix(line, "+++ "); ok {
		return rest == "/dev/null" || stripDiffPrefix(rest) == heading
	}
	return false
}

// annotateDiffSymbols appends a content anchor to each changed-symbol row's line
// locator ("L11" → "L11#b3dk"), hashing the current working file named by the
// enclosing "## P" heading. A row whose file or line the cache cannot resolve — an
// old ref, a deleted file, a drifted line — keeps its bare locator.
func annotateDiffSymbols(out string, files *anchor.Files) string {
	lines := strings.Split(out, "\n")
	var heading string
	inHunk := false
	for i, line := range lines {
		match := strings.TrimSuffix(line, "\r")
		if m := diffFileHeader.FindStringSubmatch(match); m != nil {
			heading = m[1]
			inHunk = false
			continue
		}
		if heading == "" {
			continue
		}
		if diffHunkContent(match) {
			inHunk = true
		}
		if inHunk {
			continue
		}
		m := diffSymbolRow.FindStringSubmatch(match)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		text, ok := files.LineAt(heading, n)
		if !ok {
			continue
		}
		lines[i] = m[1] + "L" + m[2] + "#" + string(anchor.Of(text)) + m[3] + line[len(match):]
	}
	return strings.Join(lines, "\n")
}

// diffHunkPrefixes are the line prefixes that mark where a supplemented hunk's
// content begins under a "## P" heading: the "@@" hunk header and every raw-diff
// preamble line collapsePreambles may keep. Symbol rows sit directly after the
// heading, before any of these, so annotateDiffSymbols stops anchoring once one
// appears — a hunk context line shaped like a symbol row must never be rewritten.
var diffHunkPrefixes = []string{
	"@@", "diff --git", "new file mode", "deleted file mode",
	"rename ", "similarity ", "copy ", "Binary files",
	"index ", "--- ", "+++ ",
}

// diffHunkContent reports whether line begins a supplemented hunk's content.
func diffHunkContent(line string) bool {
	for _, p := range diffHunkPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// SupplementDiff appends a raw textual hunk (via fetch) to each empty "(0 symbols)" file section in tilth's diff output.
func SupplementDiff(out string, fetch func(workPath string) (string, error)) (string, error) {
	lines := strings.Split(out, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}

		m := diffFileHeader.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		if m[2] != "0" || strings.TrimSpace(sectionBody(lines, i+1)) != "" {
			continue
		}

		hunk, err := fetch(m[1])
		if err != nil {
			return "", fmt.Errorf("supplement diff for %q: %w", m[1], err)
		}
		hunk = strings.TrimRight(hunk, "\n")
		if hunk == "" {
			continue
		}
		b.WriteString("\n")
		b.WriteString(hunk)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// sectionBody returns the lines from start up to (but not including) the next
// per-file header or EOF, joined as the section body. The boundary is a full
// diffFileHeader match, not a bare "## " prefix, so body content that opens with
// a markdown-style heading does not make a non-empty section look empty.
func sectionBody(lines []string, start int) string {
	next := start
	for next < len(lines) && !diffFileHeader.MatchString(strings.TrimSuffix(lines[next], "\r")) {
		next++
	}
	return strings.Join(lines[start:next], "\n")
}
