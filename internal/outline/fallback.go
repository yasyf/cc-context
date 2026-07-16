package outline

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/backend"
)

// headWindowLines caps the non-markdown head window: the fallback shows at most
// this many leading lines before pointing at ccx code read for the rest.
const headWindowLines = 40

// charsPerToken is the crude chars-per-token ratio shared with render.Cap and
// find, converting a byte length into an approximate token count.
const charsPerToken = 4

// markdownExts are the extensions Fallback outlines as an ATX heading tree; every
// other extension gets a head window.
var markdownExts = map[string]bool{".md": true, ".mdx": true, ".markdown": true}

// Fallback outlines a file ast-grep has no outline rules for. A markdown file
// (.md/.mdx/.markdown) renders as an anchored ATX heading tree; every other file
// renders an honestly-labeled head window. Binary and directory targets are gated
// upstream (BinarySkip on each surface); an empty file returns a bare empty-file
// header.
func Fallback(path string, a backend.Args) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the caller's own outline target, validated upstream
	if err != nil {
		return "", fmt.Errorf("outline fallback %s: %w", path, err)
	}
	lines := anchor.FromBytes(path, data).Lines()
	if len(lines) == 0 {
		return fmt.Sprintf("# %s — empty file\n", path), nil
	}
	if markdownExts[strings.ToLower(filepath.Ext(path))] {
		return markdownOutline(path, lines), nil
	}
	return headWindow(path, lines, len(data), a.Budget), nil
}

// markdownOutline renders each ATX heading as a content-anchored `L<line>#<hash>`
// marker aligned to a fixed column, then the heading text with its "#"s.
func markdownOutline(path string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — markdown headings\n", path)
	for _, h := range ScanHeadings(lines) {
		marker := fmt.Sprintf("L%d#%s", h.Line, anchor.Of(lines[h.Line-1]))
		fmt.Fprintf(&b, "%-10s %s\n", marker, h.Text)
	}
	return b.String()
}

// headWindow renders the file's first min(headWindowLines, budget allowance)
// lines verbatim, under a header naming the total line count, whole-file token
// estimate, and absent outline rules, plus a continuation pointer when the window
// stops short. total is the file's byte length; budget 0 leaves the window
// unbounded (render.Cap caps the total downstream).
func headWindow(path string, lines []string, total, budget int) string {
	n := len(lines)
	window := min(headWindowLines, n)
	if budget > 0 {
		window = min(window, budgetLines(lines, budget))
	}
	if window < 1 {
		window = 1
	}
	kind := filepath.Ext(path)
	if kind == "" {
		kind = filepath.Base(path)
	}
	span := anchor.FormatRange(1, window, anchor.Of(lines[0]))

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %d lines, ~%s tokens — no ast-grep outline rules for %s; head window\n",
		path, n, humanTokens(total/charsPerToken), kind)
	fmt.Fprintf(&b, "## %s:%s\n", path, span)
	b.WriteString(strings.Join(lines[:window], "\n"))
	b.WriteByte('\n')
	if window < n {
		fmt.Fprintf(&b, "… continue: ccx code read %s --section %d-%d\n", path, window+1, n)
	}
	return b.String()
}

// budgetLines returns how many leading lines fit within budget tokens, so a small
// budget shrinks the head window rather than letting render.Cap trim mid-window
// and clash with the continuation pointer. Each line costs its bytes plus the
// joining newline.
func budgetLines(lines []string, budget int) int {
	limit := budget * charsPerToken
	used := 0
	for i, line := range lines {
		used += len(line) + 1
		if used > limit {
			return i
		}
	}
	return len(lines)
}

// humanTokens renders an approximate token count, collapsing thousands to a
// one-decimal "k" ("1237" → "1.2k", "247" → "247").
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
