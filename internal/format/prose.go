package format

import (
	"errors"
	"fmt"
	"strings"

	"github.com/toon-format/toon-go"
)

// encodeProse unwraps a prose-dominant payload into plain text. A bare string
// returns verbatim — real newlines, never JSON-escaped. An object emits every
// non-prose field first as a lightweight <key>value</key> tag line, then a
// blank line, then the dominant prose field's body raw; nothing is dropped.
// The dominant field is the largest prose-like (multi-word) string field by
// byte size; an object without one errors. String tag values stay raw even
// when they contain newlines or angle brackets — the tags are a visual
// grouping for the model, not parseable XML, and escaping would reintroduce
// the JSON-escape cost this branch exists to avoid. Non-scalar residuals
// render as compact JSON inside their tag. The output is deliberately not
// JSON: Strict/round-trip machinery must exempt the prose branch (the
// integration phase wires that). No trailing newline is appended; a body
// whose source data ends in one keeps it.
func encodeProse(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case toon.Object:
		return proseObject(t)
	default:
		return "", fmt.Errorf("encode prose: cannot unwrap %T", v)
	}
}

func proseObject(o toon.Object) (string, error) {
	dominant := proseDominantIndex(o)
	if dominant < 0 {
		return "", errors.New("encode prose: no dominant prose field")
	}

	var b strings.Builder
	for i, f := range o.Fields {
		if i == dominant {
			continue
		}
		proseWriteTag(&b, f.Key, f.Value)
	}
	body := o.Fields[dominant].Value.(string)
	if b.Len() == 0 {
		return body, nil
	}
	b.WriteByte('\n')
	b.WriteString(body)
	return b.String(), nil
}

// proseDominantIndex finds the largest prose-like string field by byte size,
// or -1 when none qualifies. Prose-like means multi-word: at least two
// whitespace-separated words, so a single token (an ID, a base64 blob) never
// counts no matter how large. Ties keep the earlier field.
func proseDominantIndex(o toon.Object) int {
	best, bestLen := -1, -1
	for i, f := range o.Fields {
		s, isString := f.Value.(string)
		if !isString || len(strings.Fields(s)) < 2 {
			continue
		}
		if len(s) > bestLen {
			best, bestLen = i, len(s)
		}
	}
	return best
}

func proseWriteTag(b *strings.Builder, key string, v any) {
	b.WriteByte('<')
	b.WriteString(key)
	b.WriteByte('>')
	switch t := v.(type) {
	case string:
		b.WriteString(t)
	case toon.Object, []any:
		b.WriteString(compactJSON(t))
	default:
		writeScalar(b, v)
	}
	b.WriteString("</")
	b.WriteString(key)
	b.WriteString(">\n")
}
