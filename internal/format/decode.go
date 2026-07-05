package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"

	"github.com/toon-format/toon-go"
)

// decodeAll decodes every top-level JSON value in src into toon-go's ordered
// model. Zero values (empty or whitespace) yields ok=false; one value is used
// as-is; two or more (NDJSON) fold into a single []any so a uniform object
// stream becomes one table.
func decodeAll(src []byte) (model any, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()

	var vals []any
	for {
		var raw json.RawMessage
		if derr := dec.Decode(&raw); derr != nil {
			if errors.Is(derr, io.EOF) {
				break
			}
			return nil, false, derr
		}
		v, derr := decodeValue(raw)
		if derr != nil {
			return nil, false, derr
		}
		vals = append(vals, v)
	}

	switch len(vals) {
	case 0:
		return nil, false, nil
	case 1:
		return vals[0], true, nil
	default:
		return vals, true, nil
	}
}

// decodeValue decodes a single JSON value into the ordered model: object →
// toon.Object (fields in source order), array → []any, number → json.Number,
// and string/bool/null to their Go scalars.
func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return decodeFromToken(dec)
}

// decodeFromToken reads the next value from dec, recursing into objects and
// arrays so field order is preserved.
func decodeFromToken(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}

	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObject(dec)
		case '[':
			return decodeArray(dec)
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", t)
		}
	case json.Number:
		return numberScalar(t), nil
	default:
		return tok, nil
	}
}

// numberScalar routes an integer-valued json.Number to a native Go integer so
// toon-go's precision-preserving integer path emits it exactly: int64 when it
// fits, *big.Int when it does not. (toon-go's json.Number path coerces through
// float64 and loses precision past 2^53.) Non-integers stay json.Number, which
// toon-go canonicalizes correctly via strconv.FormatFloat.
func numberScalar(n json.Number) any {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if bi, ok := new(big.Int).SetString(s, 10); ok {
		return bi
	}
	return n
}

// decodeObject reads fields until the closing brace, preserving source order.
func decodeObject(dec *json.Decoder) (toon.Object, error) {
	var fields []toon.Field
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return toon.Object{}, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return toon.Object{}, fmt.Errorf("object key not a string: %v", keyTok)
		}
		val, err := decodeFromToken(dec)
		if err != nil {
			return toon.Object{}, err
		}
		fields = append(fields, toon.Field{Key: key, Value: val})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return toon.Object{}, err
	}
	return toon.NewObject(fields...), nil
}

// decodeArray reads elements until the closing bracket.
func decodeArray(dec *json.Decoder) ([]any, error) {
	elems := []any{}
	for dec.More() {
		val, err := decodeFromToken(dec)
		if err != nil {
			return nil, err
		}
		elems = append(elems, val)
	}
	if _, err := dec.Token(); err != nil { // consume ']'
		return nil, err
	}
	return elems, nil
}
