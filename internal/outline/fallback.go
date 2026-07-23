package outline

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/secrets"
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

// Fallback outlines a file ast-grep has no outline rules for, returning the
// rendered outline and the secret-masking rule ids that fired over it (the
// caller appends the shared footer after its cap). A markdown file
// (.md/.mdx/.markdown) renders as an anchored ATX heading tree, masked in
// path's rule context unless a.RevealSecrets; every other file renders an
// honestly-labeled head window whose full source is masked before windowing —
// so a multiline secret the window truncates cannot leak its visible prefix.
// Binary and directory targets are gated upstream (BinarySkip on each
// surface); an empty file returns a bare empty-file header.
func Fallback(path string, a backend.Args) (string, []string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the caller's own outline target, validated upstream
	if err != nil {
		return "", nil, fmt.Errorf("outline fallback %s: %w", path, err)
	}
	lines := anchor.FromBytes(path, data).Lines()
	switch {
	case len(lines) == 0:
		return fmt.Sprintf("# %s — empty file\n", path), nil, nil
	case markdownExts[strings.ToLower(filepath.Ext(path))]:
		out := markdownOutline(path, lines)
		if a.RevealSecrets {
			return out, nil, nil
		}
		masked, ids := secrets.Mask(out, path)
		return masked, ids, nil
	default:
		out, ids := headWindow(path, lines, len(data), a.Budget, a.RevealSecrets)
		return out, ids, nil
	}
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
// lines, under a header naming the total (raw) line count, whole-file token
// estimate, and absent outline rules, plus a continuation pointer when the
// window stops short. Unless reveal, the FULL source is masked in path's rule
// context before the window is cut, so a multiline secret whose tail sits past
// the window still masks instead of leaking its visible prefix; the returned
// ids are the rules whose masked spans begin inside the window. Anchors, the
// line count, and the continuation span stay in raw-line coordinates. total is
// the file's byte length; budget 0 leaves the window unbounded (render.Cap
// caps the total downstream).
func headWindow(path string, lines []string, total, budget int, reveal bool) (string, []string) {
	display := lines
	var masked []secrets.MaskedLine
	if !reveal {
		masked = secrets.MaskLines(lines, path)
		display = make([]string, len(masked))
		for i, ml := range masked {
			display[i] = ml.Text
		}
	}
	window := min(headWindowLines, len(display))
	if budget > 0 {
		window = min(window, budgetLines(display, budget))
	}
	if window < 1 {
		window = 1
	}
	n := len(lines)
	rawEnd := n // 1-based raw line the window covers through
	if window < len(display) {
		rawEnd = window
		if masked != nil {
			rawEnd = masked[window].Src
		}
	}
	var ids []string
	for _, ml := range masked[:min(window, len(masked))] {
		ids = append(ids, ml.Rules...)
	}
	kind := filepath.Ext(path)
	if kind == "" {
		kind = filepath.Base(path)
	}
	span := anchor.FormatRange(1, rawEnd, anchor.Of(lines[0]))

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %d lines, ~%s tokens — no ast-grep outline rules for %s; head window\n",
		path, n, humanTokens(total/charsPerToken), kind)
	fmt.Fprintf(&b, "## %s:%s\n", path, span)
	b.WriteString(strings.Join(display[:window], "\n"))
	b.WriteByte('\n')
	if rawEnd < n {
		fmt.Fprintf(&b, "… continue: ccx code read %s --section %d-%d\n", path, rawEnd+1, n)
	}
	return b.String(), ids
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
