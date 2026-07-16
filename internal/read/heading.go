package read

import (
	"fmt"
	"strings"
)

// mdHeading is one ATX heading: its 1-indexed line, its "#" level, and its
// trimmed text (leading whitespace and any trailing '\r' removed, "#"s kept).
type mdHeading struct {
	line  int
	level int
	text  string
}

// resolveHeading locates the section string among path's ATX headings — an exact
// text match, else a unique prefix match — and returns the heading's subtree: the
// heading line through the line before the next heading of equal-or-higher level.
// A miss errors listing the file's headings.
func resolveHeading(section, path string, lines []string) (start, end int, err error) {
	headings := scanHeadings(lines)
	m, ok := matchHeading(section, headings)
	if !ok {
		return 0, 0, headingMiss(section, path, headings)
	}
	end = len(lines)
	for _, h := range headings {
		if h.line > m.line && h.level <= m.level {
			end = h.line - 1
			break
		}
	}
	return m.line, end, nil
}

// scanHeadings collects the ATX headings outside fenced code blocks. It tracks
// which delimiter opened the fence, since a "```" fence closes only on "```" and
// a "~~~" fence only on "~~~".
func scanHeadings(lines []string) []mdHeading {
	var headings []mdHeading
	var fence byte // 0 outside a fence, else the '`' or '~' that opened it
	for i, line := range lines {
		text := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if marker := fenceMarker(text); marker != 0 {
			switch {
			case fence == 0:
				fence = marker
			case fence == marker:
				fence = 0
			}
			continue
		}
		if fence != 0 {
			continue
		}
		if level, ok := atxLevel(text); ok {
			headings = append(headings, mdHeading{line: i + 1, level: level, text: text})
		}
	}
	return headings
}

// fenceMarker returns '`' for a "```" line, '~' for a "~~~" line, or 0 otherwise.
func fenceMarker(text string) byte {
	switch {
	case strings.HasPrefix(text, "```"):
		return '`'
	case strings.HasPrefix(text, "~~~"):
		return '~'
	default:
		return 0
	}
}

// atxLevel returns the heading level of an already-trimmed line — the count of
// leading '#' (1-6) followed by a space, tab, or end of line. "#foo" and "#######"
// are not headings.
func atxLevel(text string) (int, bool) {
	n := 0
	for n < len(text) && text[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, false
	}
	if n == len(text) || text[n] == ' ' || text[n] == '\t' {
		return n, true
	}
	return 0, false
}

// matchHeading returns the heading whose text equals section, else the sole
// heading section prefixes; ok is false when neither resolves uniquely.
func matchHeading(section string, headings []mdHeading) (mdHeading, bool) {
	for _, h := range headings {
		if h.text == section {
			return h, true
		}
	}
	var match mdHeading
	count := 0
	for _, h := range headings {
		if strings.HasPrefix(h.text, section) {
			match = h
			count++
		}
	}
	return match, count == 1
}

// headingMiss reports that section matched no heading, listing the file's headings
// so the caller can pick one.
func headingMiss(section, path string, headings []mdHeading) error {
	if len(headings) == 0 {
		return fmt.Errorf("read %s --section %s: no markdown headings in file", path, section)
	}
	names := make([]string, len(headings))
	for i, h := range headings {
		names[i] = h.text
	}
	return fmt.Errorf("read %s --section %s: no matching heading. Headings:\n  %s", path, section, strings.Join(names, "\n  "))
}
