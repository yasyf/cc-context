package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/toon-format/toon-go"
)

// compactJSON serializes the ordered IR to minimal JSON. It walks the same
// model the TOON encoder receives so the two describe the identical value (and
// the byte-length comparison is honest); json.Marshal would re-sort object
// keys.
func compactJSON(v any) string {
	var b strings.Builder
	writeCompact(&b, v)
	return b.String()
}

func writeCompact(b *strings.Builder, v any) {
	switch t := v.(type) {
	case toon.Object:
		b.WriteByte('{')
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteByte(',')
			}
			writeScalar(b, f.Key)
			b.WriteByte(':')
			writeCompact(b, f.Value)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCompact(b, e)
		}
		b.WriteByte(']')
	default:
		writeScalar(b, v)
	}
}

// writeScalar emits one IR scalar with JSON semantics: strings JSON-escaped
// without HTML escaping, integers (int64, *big.Int) and json.Number as
// verbatim decimal text, bool and nil as their JSON literals. Every encoder
// renders scalars through it — any float64 path corrupts integers past 2^53.
func writeScalar(b *strings.Builder, v any) {
	switch t := v.(type) {
	case json.Number:
		b.WriteString(t.String())
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case *big.Int:
		b.WriteString(t.String())
	case string:
		writeJSONString(b, t)
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("writeScalar: unexpected type %T", v))
	}
}

// writeJSONString emits a JSON string with standard escaping but without
// encoding/json's default HTML escaping, so <, >, and & pass through raw.
func writeJSONString(b *strings.Builder, s string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(fmt.Sprintf("writeJSONString: %v", err))
	}
	b.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}
