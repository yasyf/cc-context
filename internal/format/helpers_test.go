package format

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseJSON decodes src into the standard JSON model (map[string]any, []any,
// float64, string, bool, nil) for round-trip comparison.
func parseJSON(t *testing.T, src string) any {
	t.Helper()
	var v any
	if err := json.NewDecoder(strings.NewReader(src)).Decode(&v); err != nil {
		t.Fatalf("parseJSON(%q): %v", src, err)
	}
	return v
}

// normalize coerces every number in v to float64 so a value decoded from TOON
// (which yields float64) compares equal to one parsed from JSON regardless of the
// concrete numeric type either side produced.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	case json.Number:
		f, _ := t.Float64()
		return f
	case int64:
		return float64(t)
	default:
		return v
	}
}
