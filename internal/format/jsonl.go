package format

import (
	"errors"
	"strings"
)

// encodeJSONL renders an IR array as JSON Lines: one compact JSON document per
// element, newline-separated, with no trailing newline. Any non-array root is
// an error — NDJSON multi-document payloads arrive pre-folded into one []any.
func encodeJSONL(v any) (string, error) {
	arr, ok := v.([]any)
	if !ok {
		return "", errors.New("encode jsonl: not an array")
	}
	var b strings.Builder
	for i, e := range arr {
		if i > 0 {
			b.WriteByte('\n')
		}
		writeCompact(&b, e)
	}
	return b.String(), nil
}
