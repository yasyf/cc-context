// Package find lists the files a glob selects with a native Go walk: it honors the
// gitignore chain, skips VCS stores, marks binaries without a token estimate, and
// bounds its output by a token budget rather than a fixed row cap.
package find

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/sniff"
)

// DefaultBudget bounds find output when the caller sets none, mirroring
// ripgrep.DefaultBudget: the CLI and MCP surfaces apply it, while the codeexec path
// leaves Budget zero so a sandboxed script filters the full listing itself.
const DefaultBudget = 2000

// VCSStoreDirs are the version-control metadata directories the default walk always
// skips; the language census skips them too. The anchored-glob escape hatch clears
// them, since an explicit path is explicit intent.
var VCSStoreDirs = []string{".git", ".jj", ".hg", ".svn"}

// bytesPerToken is the chars-per-token ratio shared with render.Cap, converting the
// token budget into the byte budget the walk fills.
const bytesPerToken = 4

// footerReserveBytes holds space for the overflow and ignore-disclosure footers so
// the rendered listing stays within budget and render.Cap never trims it.
const footerReserveBytes = 512

// maxTrailerBytes upper-bounds a row's non-path text (the widest binary trailer plus
// the newline) so the budget cutoff is chosen before any file is probed.
const maxTrailerBytes = 64

// maxBudget clamps the effective token budget so budget*bytesPerToken cannot
// overflow (a math.MaxInt64 budget would wrap negative in the cutoff multiply).
const maxBudget = 1 << 31

// Run lists the files matching a.Glob under a.Scope (or the cwd), rendered to a
// budgeted listing. A zero a.Budget renders every row uncapped.
func Run(ctx context.Context, a backend.Args) (string, error) {
	root := a.Scope
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("find: resolve cwd: %w", err)
		}
		root = cwd
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("find: resolve root %q: %w", root, err)
	}

	walkRoot, matchGlob, escaped, excludeVCS := resolveAnchor(absRoot, a.Glob)
	cfg := walkConfig{escaped: escaped, excludeVCS: excludeVCS}
	if !escaped {
		cfg.gitRoot = gitRootOf(walkRoot)
		cfg.matcher = ancestorMatcher(cfg.gitRoot, walkRoot)
	}
	matches, seenExts, err := collect(ctx, absRoot, walkRoot, matchGlob, cfg)
	if err != nil {
		return "", err
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].rel < matches[j].rel })

	hidden := 0
	if !escaped {
		if hidden, err = countHidden(ctx, absRoot, matchGlob, len(matches)); err != nil {
			return "", err
		}
	}

	budget := a.Budget
	if budget > maxBudget {
		budget = maxBudget
	}
	return render(a.Glob, absRoot, matches, seenExts, hidden, budget), nil
}

// render assembles the header, the rows that fit the budget, and the overflow and
// ignore-disclosure footers.
func render(glob, displayRoot string, matches []match, seenExts map[string]bool, hidden, budget int) string {
	displayRoot = filepath.ToSlash(displayRoot)
	total := len(matches)
	var b strings.Builder
	fmt.Fprintf(&b, "# Glob: %q in %s — %s files\n", glob, displayRoot, humanComma(total))

	if total == 0 {
		// A zero match still discloses hidden files when there are any — that hint is
		// more actionable than the extensions list, so it wins over it.
		if hidden > 0 {
			b.WriteString(disclosureLine(hidden))
		} else if hint := zeroHint(displayRoot, seenExts); hint != "" {
			b.WriteString(hint + "\n")
		}
		return b.String()
	}

	cutoff := budgetCutoff(b.Len(), matches, budget)
	for _, m := range matches[:cutoff] {
		b.WriteString(formatRow(m))
	}

	if cutoff < total {
		var withheld int64
		for _, m := range matches[cutoff:] {
			withheld += m.size
		}
		fmt.Fprintf(&b, "… and %s more files (~%s tokens) — raise --budget, or narrow the glob / --scope. Orienting the repo? `ccx repo overview`.\n",
			humanComma(total-cutoff), humanTokens(int(withheld)/bytesPerToken))
	}

	if hidden > 0 {
		b.WriteString(disclosureLine(hidden))
	}

	return b.String()
}

// disclosureLine names how many ignore-filtered files matched the glob and points
// at the anchored-glob escape hatch.
func disclosureLine(hidden int) string {
	return fmt.Sprintf("%s ignored files hidden — anchor the glob at a real path (e.g. .venv/**/*.py) to include them.\n", humanComma(hidden))
}

// budgetCutoff returns how many leading rows fit the byte budget, using a per-row
// upper bound so the cutoff is decided without probing a single file. A non-positive
// budget keeps every row.
func budgetCutoff(used int, matches []match, budget int) int {
	if budget <= 0 {
		return len(matches)
	}
	limit := budget*bytesPerToken - footerReserveBytes
	for i, m := range matches {
		used += len(m.rel) + maxTrailerBytes
		if used > limit {
			return i
		}
	}
	return len(matches)
}

// formatRow renders one file row, probing it so a binary file reports its size and
// media type instead of a misleading token estimate.
func formatRow(m match) string {
	if mime, binary := sniff.Detect(m.abs); binary {
		return fmt.Sprintf("%s  (binary, %s, %s)\n", m.rel, humanBytes(m.size), mime)
	}
	return fmt.Sprintf("%s  (~%d tokens)\n", m.rel, (m.size+bytesPerToken-1)/bytesPerToken)
}

// zeroHint offers the extensions present under root when the glob matched nothing,
// or "" when the walk saw no extensions.
func zeroHint(displayRoot string, seenExts map[string]bool) string {
	if len(seenExts) == 0 {
		return ""
	}
	exts := make([]string, 0, len(seenExts))
	for e := range seenExts {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	const maxExts = 12
	if len(exts) > maxExts {
		exts = exts[:maxExts]
	}
	return "no files match — extensions under " + displayRoot + ": " + strings.Join(exts, ", ")
}

// humanComma groups an integer with thousands separators ("1204" → "1,204").
func humanComma(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// humanTokens renders an approximate token count, collapsing thousands to "k"
// ("52000" → "52k").
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return strconv.Itoa((n+500)/1000) + "k"
}

// humanBytes renders a byte size with a binary-unit suffix ("48128" → "47KB").
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dKB", (n+512)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(1024*1024*1024))
	}
}
