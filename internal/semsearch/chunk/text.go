package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isPythonSpace reports whether r is stripped by Python's str.strip(): Go's
// Unicode White_Space set plus the C0 information separators U+001C–U+001F,
// which Python's str.isspace() treats as whitespace and Go's unicode does not.
func isPythonSpace(r rune) bool {
	if r >= 0x1c && r <= 0x1f {
		return true
	}
	return unicode.IsSpace(r)
}

// pythonStrip trims leading and trailing Python whitespace, matching str.strip().
func pythonStrip(s string) string {
	return strings.TrimFunc(s, isPythonSpace)
}

// decodeReplace decodes raw file bytes as UTF-8, replacing every maximal invalid
// subsequence with U+FFFD, mirroring Python's bytes.decode("utf-8", "replace")
// used by semble's read_file_text. Valid UTF-8 is returned unchanged.
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(utf8.RuneError)
			i++
			continue
		}
		sb.Write(b[i : i+size])
		i += size
	}
	return sb.String()
}

// lineBreaks holds the single-rune line boundaries of Python's str.splitlines.
// U+000D is handled separately so CRLF collapses into one break.
var lineBreaks = map[rune]struct{}{
	'\n': {}, '\v': {}, '\f': {}, 0x1c: {}, 0x1d: {}, 0x1e: {}, 0x85: {}, 0x2028: {}, 0x2029: {},
}

// splitLineSpans returns the byte span of every line in src including its
// terminator, matching str.splitlines(keepends=True): breaks on
// \n \r \r\n \v \f \x1c \x1d \x1e \x85    , with a trailing
// unterminated line kept as its own span. src must be valid UTF-8.
func splitLineSpans(src []byte) []boundary {
	var spans []boundary
	start := 0
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == '\r' {
			end := i + size
			if end < len(src) && src[end] == '\n' {
				end++
			}
			spans = append(spans, boundary{start, end})
			i = end
			start = end
			continue
		}
		if _, ok := lineBreaks[r]; ok {
			end := i + size
			spans = append(spans, boundary{start, end})
			i = end
			start = end
			continue
		}
		i += size
	}
	if start < len(src) {
		spans = append(spans, boundary{start, len(src)})
	}
	return spans
}

// countNewlines counts '\n' bytes in src[:end], the metric semble uses for line
// numbers (only U+000A, never the other line breaks).
func countNewlines(src []byte, end int) int {
	n := 0
	for _, b := range src[:end] {
		if b == '\n' {
			n++
		}
	}
	return n
}
