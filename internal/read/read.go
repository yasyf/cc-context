// Package read serves anchored file windows without capping downstream output.
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
	whole := a.Full || a.Section == ""
	if !whole {
		start, end, numeric, rangeErr := anchor.ParseNumericRange(a.Section)
		if numeric || rangeErr != nil {
			return readNumeric(a.Path, a.Section, start, end, rangeErr)
		}
	}
	data, err := os.ReadFile(a.Path) //nolint:gosec // path is the caller's own read target, validated upstream
	if err != nil {
		return "", fmt.Errorf("read %s: %w", a.Path, err)
	}
	lines := anchor.FromBytes(a.Path, data).Lines()
	if len(lines) == 0 {
		return fmt.Sprintf("# read %s: empty file\n", a.Path), nil
	}

	start, end := 1, len(lines)
	if !whole {
		if start, end, err = resolveSection(a.Section, a.Path, lines); err != nil {
			return "", err
		}
	}
	return render(a.Path, lines[start-1:end], start, end, len(lines), whole), nil
}

func resolveSection(section, path string, lines []string) (start, end int, err error) {
	start, end, ok, err := anchor.ParseNumericRange(section)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	if ok {
		return numericBounds(section, path, start, end, len(lines))
	}
	if isMarkdown(path) {
		return resolveHeading(section, path, lines)
	}
	return 0, 0, fmt.Errorf(`read %s --section %s: --section takes a line range ("40-95"), an anchor ("15-27#k2fa"), or a markdown heading; for a symbol use ccx code symbol <name> --body`, path, section)
}

func render(path string, served []string, start, end, total int, whole bool) string {
	h := anchor.Of(served[0])
	span := anchor.FormatRange(start, end, h)
	if start == end {
		span = anchor.Format(start, h)
	}
	var count string
	if whole {
		count = fmt.Sprintf("(%d lines)", total)
	} else {
		count = fmt.Sprintf("(%d of %d lines)", end-start+1, total)
	}
	return fmt.Sprintf("# read %s:%s %s\n%s\n", path, span, count, strings.Join(served, "\n"))
}

var mdExts = map[string]bool{".md": true, ".mdx": true, ".markdown": true}

func isMarkdown(path string) bool {
	return mdExts[strings.ToLower(filepath.Ext(path))]
}
