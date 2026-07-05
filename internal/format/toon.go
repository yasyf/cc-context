package format

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/toon-format/toon-go"
)

// Delimiter is the character separating values inside TOON array scopes.
type Delimiter = toon.Delimiter

// The supported array delimiters.
const (
	DelimiterComma = toon.DelimiterComma
	DelimiterTab   = toon.DelimiterTab
	DelimiterPipe  = toon.DelimiterPipe
)

// encodeTOON marshals the IR to TOON with the options' indent and delimiter.
// toon-go canonicalizes json.Number through a float64 round-trip (ParseFloat +
// FormatFloat), so encodeTOON first rejects any number that round-trip would
// corrupt: decimals past float64 precision silently truncate and out-of-range
// exponents type-flip to quoted strings — and the auto byte-net cannot catch
// truncation, which only shrinks output.
func encodeTOON(v any, opts Options) (string, error) {
	if n, lossy := toonLossyNumber(v); lossy {
		return "", fmt.Errorf("encode toon: number %s does not survive toon-go's float64 round-trip", n)
	}
	out, err := toon.MarshalString(v,
		toon.WithIndent(opts.Indent),
		toon.WithArrayDelimiter(opts.Delimiter),
		toon.WithDocumentDelimiter(opts.Delimiter),
	)
	if err != nil {
		return "", fmt.Errorf("marshal toon: %w", err)
	}
	return out, nil
}

// toonLossyNumber walks the IR for the first json.Number toon-go's float64
// canonicalization would corrupt. Integer-valued numbers arrive as
// int64/*big.Int (decode.go numberScalar), so only non-integer forms and "-0"
// reach this check.
func toonLossyNumber(v any) (json.Number, bool) {
	switch t := v.(type) {
	case toon.Object:
		for _, f := range t.Fields {
			if n, lossy := toonLossyNumber(f.Value); lossy {
				return n, true
			}
		}
	case []any:
		for _, e := range t {
			if n, lossy := toonLossyNumber(e); lossy {
				return n, true
			}
		}
	case json.Number:
		if !toonNumberSafe(t) {
			return t, true
		}
	}
	return "", false
}

// toonNumberSafe reports whether toon-go's number canonicalization (ParseFloat
// then FormatFloat 'f' -1) preserves n's value: either the round-trip
// reproduces the text verbatim ("3.14"), or the canonical rendering denotes
// exactly the value the source text does ("1E2" → 100, "2.5e-3" → 0.0025). An
// out-of-range exponent ("1e999") fails ParseFloat and type-flips to a quoted
// string; excess-precision decimals render to a different value and would
// silently truncate.
func toonNumberSafe(n json.Number) bool {
	s := n.String()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	rendered := strconv.FormatFloat(f, 'f', -1, 64)
	if rendered == s {
		return true
	}
	src, ok := new(big.Rat).SetString(s)
	if !ok {
		return false
	}
	out, ok := new(big.Rat).SetString(rendered)
	return ok && src.Cmp(out) == 0
}
