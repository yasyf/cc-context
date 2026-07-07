package codeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// maxWireDepth caps wire-value nesting on both directions (mirrored in
// driver.py): breadth is bounded by the driver's arg ceiling, depth is not.
const maxWireDepth = 64

// Call carries one sandbox call's positional and keyword arguments as plain
// Go values.
type Call struct {
	Args   []any
	Kwargs map[string]any
}

// HostFunc does the blocking work of one host call: it reads the Python
// call's args and returns a single value (or an error surfaced to the script
// as a RuntimeError). The wire pump dispatches each call on its own goroutine
// so independent awaits in one script run concurrently.
type HostFunc func(ctx context.Context, call Call) (any, error)

// decodeCall maps a call frame's encoded args and kwargs into plain Go values.
func decodeCall(f driverFrame) (Call, error) {
	var call Call
	if len(f.Args) > 0 {
		call.Args = make([]any, len(f.Args))
		for i, raw := range f.Args {
			v, err := decodeValue(raw)
			if err != nil {
				return Call{}, err
			}
			call.Args[i] = v
		}
	}
	if len(f.Kwargs) > 0 {
		call.Kwargs = make(map[string]any, len(f.Kwargs))
		for k, raw := range f.Kwargs {
			v, err := decodeValue(raw)
			if err != nil {
				return Call{}, err
			}
			call.Kwargs[k] = v
		}
	}
	return call, nil
}

// decodeValue maps one wire-encoded value into plain Go: bare numbers to
// int64/*big.Int/float64, the $-tagged forms ($tuple/$set/$frozenset to
// []any, $bytes to []byte, $dict to a string-keyed map, $float to NaN/±Inf),
// and containers recursively.
func decodeValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return fromWire(v, 0)
}

func fromWire(v any, depth int) (any, error) {
	if depth > maxWireDepth {
		return nil, fmt.Errorf("value nesting exceeds depth %d", maxWireDepth)
	}
	switch x := v.(type) {
	case json.Number:
		return numberScalar(x), nil
	case []any:
		return fromWireSlice(x, depth+1)
	case map[string]any:
		if len(x) == 1 {
			for tag, val := range x {
				if out, ok, err := fromTag(tag, val, depth+1); ok || err != nil {
					return out, err
				}
			}
		}
		m := make(map[string]any, len(x))
		for k, val := range x {
			conv, err := fromWire(val, depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = conv
		}
		return m, nil
	default:
		return v, nil
	}
}

func fromWireSlice(items []any, depth int) ([]any, error) {
	out := make([]any, len(items))
	for i, item := range items {
		conv, err := fromWire(item, depth)
		if err != nil {
			return nil, err
		}
		out[i] = conv
	}
	return out, nil
}

// fromTag decodes one $-tagged wire form; ok is false when tag is not one.
func fromTag(tag string, val any, depth int) (out any, ok bool, err error) {
	switch tag {
	case "$tuple", "$set", "$frozenset":
		items, isArr := val.([]any)
		if !isArr {
			return nil, true, fmt.Errorf("%s payload is %T, want array", tag, val)
		}
		out, err := fromWireSlice(items, depth)
		return out, true, err
	case "$bytes":
		items, isArr := val.([]any)
		if !isArr {
			return nil, true, fmt.Errorf("$bytes payload is %T, want array", val)
		}
		b := make([]byte, len(items))
		for i, item := range items {
			n, isNum := item.(json.Number)
			if !isNum {
				return nil, true, fmt.Errorf("$bytes element %d is %T, want number", i, item)
			}
			u, err := strconv.ParseUint(n.String(), 10, 8)
			if err != nil {
				return nil, true, fmt.Errorf("$bytes element %d: %w", i, err)
			}
			b[i] = byte(u)
		}
		return b, true, nil
	case "$float":
		s, isStr := val.(string)
		if !isStr {
			return nil, true, fmt.Errorf("$float payload is %T, want string", val)
		}
		switch s {
		case "nan":
			return math.NaN(), true, nil
		case "inf":
			return math.Inf(1), true, nil
		case "-inf":
			return math.Inf(-1), true, nil
		}
		return nil, true, fmt.Errorf("unknown $float %q", s)
	case "$dict":
		pairs, isArr := val.([]any)
		if !isArr {
			return nil, true, fmt.Errorf("$dict payload is %T, want array", val)
		}
		m := make(map[string]any, len(pairs))
		for _, p := range pairs {
			kv, isPair := p.([]any)
			if !isPair || len(kv) != 2 {
				return nil, true, fmt.Errorf("$dict pair is %T, want [key, value]", p)
			}
			k, err := fromWire(kv[0], depth)
			if err != nil {
				return nil, true, err
			}
			v, err := fromWire(kv[1], depth)
			if err != nil {
				return nil, true, err
			}
			// Keys collapse to strings for JSON rendering; a collision after
			// stringification ({1: …, "1": …}) would silently drop a value.
			key := fmt.Sprint(k)
			if _, dup := m[key]; dup {
				return nil, true, fmt.Errorf("$dict keys collide at %q after string conversion", key)
			}
			m[key] = v
		}
		return m, true, nil
	}
	return nil, false, nil
}

// numberScalar mirrors internal/format's integer routing: int64 when it fits,
// *big.Int beyond that so 2**100 survives digit-exact, float64 otherwise.
func numberScalar(n json.Number) any {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return i
	}
	if bi, ok := new(big.Int).SetString(n.String(), 10); ok {
		return bi
	}
	f, _ := n.Float64()
	return f
}

// encodeValue maps a host function's plain Go return into the wire grammar:
// []byte becomes $bytes and non-finite floats $float, everything else is
// ordinary JSON (json.Marshal keeps *big.Int digit-exact).
func encodeValue(v any) (json.RawMessage, error) {
	wired, err := toWire(v, 0)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(wired)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func toWire(v any, depth int) (any, error) {
	if depth > maxWireDepth {
		return nil, fmt.Errorf("value nesting exceeds depth %d", maxWireDepth)
	}
	switch x := v.(type) {
	case []byte:
		ints := make([]int, len(x))
		for i, b := range x {
			ints[i] = int(b)
		}
		return map[string]any{"$bytes": ints}, nil
	case float64:
		switch {
		case math.IsNaN(x):
			return map[string]any{"$float": "nan"}, nil
		case math.IsInf(x, 1):
			return map[string]any{"$float": "inf"}, nil
		case math.IsInf(x, -1):
			return map[string]any{"$float": "-inf"}, nil
		}
		return x, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			conv, err := toWire(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = conv
		}
		return out, nil
	case map[string]any:
		// A lone $-prefixed key is tag-shaped in-band data: escape it as
		// $dict so the decoders round-trip it as a dict, not a tag.
		if len(x) == 1 {
			for k, item := range x {
				if strings.HasPrefix(k, "$") {
					conv, err := toWire(item, depth+1)
					if err != nil {
						return nil, err
					}
					return map[string]any{"$dict": []any{[]any{k, conv}}}, nil
				}
			}
		}
		out := make(map[string]any, len(x))
		for k, item := range x {
			conv, err := toWire(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = conv
		}
		return out, nil
	default:
		return v, nil
	}
}

// native recursively converts a decoded value into JSON-marshalable Go:
// []byte to string, containers walked.
func native(v any) any {
	switch x := v.(type) {
	case []byte:
		// json.Marshal maps invalid UTF-8 here to U+FFFD — accepted collapse
		// for bytes nested inside a container (bare bytes render raw).
		return string(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = native(item)
		}
		return out
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, item := range x {
			m[k] = native(item)
		}
		return m
	default:
		return v
	}
}

// kindOf names a decoded value's Python-side kind for argument-error wording.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "None"
	case bool:
		return "bool"
	case int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []byte:
		return "bytes"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return fmt.Sprintf("%T", v)
	}
}
