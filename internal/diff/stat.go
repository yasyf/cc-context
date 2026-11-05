package diff

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/vcs"
)

type statRow struct {
	name           string
	added, removed int
	binary         bool
}

func runStat(plan vcs.DiffPlan, files []string) (string, error) {
	var rows []statRow
	for _, path := range files {
		row, changed, err := statFile(plan, path)
		if err != nil {
			return "", err
		}
		if changed {
			rows = append(rows, row)
		}
	}
	return renderStat(plan.Label, rows), nil
}

func statFile(plan vcs.DiffPlan, path string) (statRow, bool, error) {
	if !plan.Symbolic {
		raw, err := plan.Raw(path)
		if err != nil {
			return statRow{}, false, err
		}
		added, removed := rawLineCounts(raw)
		return statRow{name: path, added: added, removed: removed}, true, nil
	}
	renamedFrom := plan.Renames[path]
	before, after, err := blobs(plan, path)
	if err != nil {
		return statRow{}, false, err
	}
	row := statRow{name: fileHeader(fileReport{path: path, renamedFrom: renamedFrom})}
	if blobBinary(before, after) {
		row.binary = true
		return row, true, nil
	}
	row.added, row.removed = hunkLineCounts(hunk.Compute(before, after))
	return row, row.added+row.removed > 0 || renamedFrom != "", nil
}

func rawLineCounts(raw string) (added, removed int) {
	inHunk := false
	for line := range strings.Lines(raw) {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

func renderStat(label string, rows []statRow) string {
	width, added, removed := 0, 0, 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.name))
		added, removed = added+r.added, removed+r.removed
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# diff %s %s %d files %s +%d/%s%d lines\n", label, emDash, len(rows), midDot, added, sigilRemoved, removed)
	for _, r := range rows {
		if r.binary {
			fmt.Fprintf(&b, "%-*s | binary\n", width, r.name)
			continue
		}
		fmt.Fprintf(&b, "%-*s | +%d/%s%d\n", width, r.name, r.added, sigilRemoved, r.removed)
	}
	return b.String()
}
