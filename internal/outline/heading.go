package outline

import "strings"

// Heading is one ATX heading: its 1-indexed line, its "#" level, and its trimmed
// text (leading whitespace and any trailing '\r' removed, "#"s kept).
type Heading struct {
	Line  int
	Level int
	Text  string
}

// ScanHeadings collects the ATX headings outside fenced code blocks. It tracks
// which delimiter opened the fence, since a "```" fence closes only on "```" and
// a "~~~" fence only on "~~~".
func ScanHeadings(lines []string) []Heading {
	var headings []Heading
	var fence byte // 0 outside a fence, else the '`' or '~' that opened it
	for i, line := range lines {
		text := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if marker := fenceMarker(text); marker != 0 {
			switch fence {
			case 0:
				fence = marker
			case marker:
				fence = 0
			}
			continue
		}
		if fence != 0 {
			continue
		}
		if level, ok := atxLevel(text); ok {
			headings = append(headings, Heading{Line: i + 1, Level: level, Text: text})
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
