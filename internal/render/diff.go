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
	// the working-side path and symbol count. The optional "x/… w/" prefix covers
	// working-tree output (c/ and i/ index variants); a bare path covers
	// commit-range output, where tilth emits no c//w/ prefix.
	diffFileHeader = regexp.MustCompile(`^## (?:[a-z]/\S+ w/)?(\S+) \((\d+) symbols\)$`)
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

// RunDiffCLI runs the tilth diff, supplements any empty-hunk section with its
// raw jj-aware hunk, shortens the header SHAs, collapses each file's redundant
// preamble, anchors the changed-symbol rows, and caps to budget.
func RunDiffCLI(ctx context.Context, bin string, argv []string, source string, budget int) (string, error) {
	out, err := RunCLI(ctx, bin, argv)
	if err != nil {
		return "", err
	}
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

// shortenDiffSHAs trims every full 40-hex object id on the first "# Diff:" header
// line to its first 10 characters. Only that line is touched, so a 40-hex string
// living in the hunk body is left byte-identical.
func shortenDiffSHAs(out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "# Diff:") {
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
		if m := diffFileHeader.FindStringSubmatch(line); m != nil {
			heading = m[1]
			collapsible = false
			kept = append(kept, line)
			continue
		}
		if heading != "" {
			if m := diffGitPreamble.FindStringSubmatch(line); m != nil {
				collapsible = stripDiffPrefix(m[1]) == heading && stripDiffPrefix(m[2]) == heading
				if collapsible {
					continue
				}
				kept = append(kept, line)
				continue
			}
			if strings.HasPrefix(line, "@@") {
				collapsible = false
				kept = append(kept, line)
				continue
			}
			if collapsible && droppablePreamble(line, heading) {
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
	for i, line := range lines {
		if m := diffFileHeader.FindStringSubmatch(line); m != nil {
			heading = m[1]
			continue
		}
		if heading == "" {
			continue
		}
		m := diffSymbolRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		text, ok := files.LineAt(heading, n)
		if !ok {
			continue
		}
		lines[i] = m[1] + "L" + m[2] + "#" + string(anchor.Of(text)) + m[3]
	}
	return strings.Join(lines, "\n")
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

		m := diffFileHeader.FindStringSubmatch(line)
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
// "## " file header or EOF, joined as the section body.
func sectionBody(lines []string, start int) string {
	next := start
	for next < len(lines) && !strings.HasPrefix(lines[next], "## ") {
		next++
	}
	return strings.Join(lines[start:next], "\n")
}
