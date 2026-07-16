package read

import (
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/outline"
)

// resolveHeading locates the section string among path's ATX headings — an exact
// text match, else a unique prefix match — and returns the heading's subtree: the
// heading line through the line before the next heading of equal-or-higher level.
// A miss errors listing the file's headings.
func resolveHeading(section, path string, lines []string) (start, end int, err error) {
	headings := outline.ScanHeadings(lines)
	m, ok := matchHeading(section, headings)
	if !ok {
		return 0, 0, headingMiss(section, path, headings)
	}
	end = len(lines)
	for _, h := range headings {
		if h.Line > m.Line && h.Level <= m.Level {
			end = h.Line - 1
			break
		}
	}
	return m.Line, end, nil
}

// matchHeading returns the heading whose text equals section, else the sole
// heading section prefixes; ok is false when neither resolves uniquely.
func matchHeading(section string, headings []outline.Heading) (outline.Heading, bool) {
	for _, h := range headings {
		if h.Text == section {
			return h, true
		}
	}
	var match outline.Heading
	count := 0
	for _, h := range headings {
		if strings.HasPrefix(h.Text, section) {
			match = h
			count++
		}
	}
	return match, count == 1
}

// headingMiss reports that section matched no heading, listing the file's headings
// so the caller can pick one.
func headingMiss(section, path string, headings []outline.Heading) error {
	if len(headings) == 0 {
		return fmt.Errorf("read %s --section %s: no markdown headings in file", path, section)
	}
	names := make([]string, len(headings))
	for i, h := range headings {
		names[i] = h.Text
	}
	return fmt.Errorf("read %s --section %s: no matching heading. Headings:\n  %s", path, section, strings.Join(names, "\n  "))
}
