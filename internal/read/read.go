// Package read serves "ccx code read": one os.ReadFile shaped into anchored,
// agent-facing output. A binary target returns the outline binary-skip row; a
// text file is split with anchor.FromBytes (so emitted anchors round-trip the
// CRLF/trailing-newline semantics of anchor resolution) and windowed to a line
// range, a markdown heading, or the whole file. Run does not cap its output —
// masking and render.Cap stay downstream in render.Finalize(OpRead).
package read

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
)

// DefaultBudget bounds read output when the caller sets none, mirroring
// find.DefaultBudget: the CLI and MCP surfaces apply it before render.Finalize,
// while Run itself emits every requested line uncapped.
const DefaultBudget = 2000

// Run reads a.Path and renders the requested window — a.Full or an empty
// a.Section for the whole file, a numeric range or markdown heading otherwise —
// as an anchored block. A binary target returns the outline binary-skip row and
// a nil error; the path is assumed pre-validated by backend.ResolvePath, so a
// file that vanished between check and read surfaces as a plain wrapped error.
func Run(a backend.Args) (string, error) {
	if skip, ok := outline.BinarySkip(a.Path); ok {
		return skip, nil
	}
	data, err := os.ReadFile(a.Path) //nolint:gosec // path is the caller's own read target, validated upstream
	if err != nil {
		return "", fmt.Errorf("read %s: %w", a.Path, err)
	}
	lines := anchor.FromBytes(a.Path, data).Lines()
	if len(lines) == 0 {
		return fmt.Sprintf("# read %s: empty file\n", a.Path), nil
	}

	whole := a.Full || a.Section == ""
	start, end := 1, len(lines)
	if !whole {
		if start, end, err = resolveSection(a.Section, a.Path, lines); err != nil {
			return "", err
		}
	}
	return render(a.Path, lines, start, end, whole), nil
}

// resolveSection maps a non-empty section string onto a 1-indexed inclusive line
// range: a numeric "A" / "A-B" clamps its end to EOF (a start past EOF is a loud
// error), and a markdown heading — only on .md/.mdx/.markdown files — spans its
// subtree. Any other section errors with the accepted forms.
func resolveSection(section, path string, lines []string) (start, end int, err error) {
	start, end, ok, err := anchor.ParseNumericRange(section)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	if ok {
		if start < 1 {
			return 0, 0, fmt.Errorf("read %s --section %s: line %d is invalid — lines are 1-indexed", path, section, start)
		}
		if start > len(lines) {
			return 0, 0, fmt.Errorf("read %s --section %s: starts past EOF (file has %d lines)", path, section, len(lines))
		}
		if end > len(lines) {
			end = len(lines)
		}
		return start, end, nil
	}
	if isMarkdown(path) {
		return resolveHeading(section, path, lines)
	}
	return 0, 0, fmt.Errorf(`read %s --section %s: --section takes a line range ("40-95"), an anchor ("15-27#k2fa"), or a markdown heading; for a symbol use ccx code symbol <name> --body`, path, section)
}

// render assembles the anchored header and the verbatim served lines. The header
// hash pins the first served line's content; a whole-file read reports "(N lines)"
// while a windowed read reports "(served of N lines)".
func render(path string, lines []string, start, end int, whole bool) string {
	served := lines[start-1 : end]
	h := anchor.Of(served[0])
	span := anchor.FormatRange(start, end, h)
	if start == end {
		span = anchor.Format(start, h)
	}
	var count string
	if whole {
		count = fmt.Sprintf("(%d lines)", len(lines))
	} else {
		count = fmt.Sprintf("(%d of %d lines)", end-start+1, len(lines))
	}
	return fmt.Sprintf("# read %s:%s %s\n%s\n", path, span, count, strings.Join(served, "\n"))
}

// mdExts are the extensions whose non-numeric sections resolve as markdown headings.
var mdExts = map[string]bool{".md": true, ".mdx": true, ".markdown": true}

// isMarkdown reports whether path's extension marks it as markdown.
func isMarkdown(path string) bool {
	return mdExts[strings.ToLower(filepath.Ext(path))]
}
