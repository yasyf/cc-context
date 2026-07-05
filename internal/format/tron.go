package format

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/toon-format/toon-go"
)

// tronClass is one minted TRON class: the key-set fingerprint, the assigned
// name, and the declaration keys in first-seen order.
type tronClass struct {
	fp   string
	name string
	keys []string
}

var tronIdentRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// encodeTRON renders the IR as TRON (tron-format.github.io): a JSON superset
// that mints a class for every object key-set with two or more properties
// occurring two or more times, declares each as a "class NAME: k1,k2" header
// line, and emits matching objects as NAME(v1,v2,…) with values reordered to
// declaration order. Everything else is compact JSON.
func encodeTRON(v any) (string, error) {
	classes, order := tronDiscover(v)

	var b strings.Builder
	for _, cls := range order {
		b.WriteString("class ")
		b.WriteString(cls.name)
		b.WriteString(": ")
		for i, k := range cls.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			tronWriteHeaderKey(&b, k)
		}
		b.WriteByte('\n')
	}
	if len(order) > 0 {
		b.WriteByte('\n')
	}
	tronWrite(&b, v, classes)
	return b.String(), nil
}

// tronDiscover walks v in DFS pre-order fingerprinting every non-empty object
// (each object counted before its children), then mints the qualifying
// key-sets — at least two keys and at least two occurrences — assigning names
// sequentially in discovery order. It returns the minted classes keyed by
// fingerprint and in order.
func tronDiscover(v any) (map[string]*tronClass, []*tronClass) {
	counts := make(map[string]int)
	seen := make(map[string]*tronClass)
	var discovered []*tronClass

	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case toon.Object:
			if len(t.Fields) == 0 {
				return
			}
			fp := tronFingerprint(t)
			counts[fp]++
			if _, ok := seen[fp]; !ok {
				keys := make([]string, len(t.Fields))
				for i, f := range t.Fields {
					keys[i] = f.Key
				}
				cls := &tronClass{fp: fp, keys: keys}
				seen[fp] = cls
				discovered = append(discovered, cls)
			}
			for _, f := range t.Fields {
				walk(f.Value)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)

	minted := make(map[string]*tronClass)
	var order []*tronClass
	for _, cls := range discovered {
		if len(cls.keys) < 2 || counts[cls.fp] < 2 {
			continue
		}
		cls.name = tronClassName(len(order))
		minted[cls.fp] = cls
		order = append(order, cls)
	}
	return minted, order
}

// tronFingerprint is the order-insensitive key-set identity of o: sorted keys
// joined with NUL. The JS reference joins with "," which collides for
// comma-containing keys ({"a,b","c"} vs {"a","b,c"}) and corrupts the losing
// shape's data; the NUL join is a deliberate divergence that keeps them
// distinct.
func tronFingerprint(o toon.Object) string {
	keys := make([]string, len(o.Fields))
	for i, f := range o.Fields {
		keys[i] = f.Key
	}
	slices.Sort(keys)
	return strings.Join(keys, "\x00")
}

// tronClassName assigns the nth class name: A-Z, then A1-Z1, A2-Z2, ….
func tronClassName(index int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	cycle, pos := index/26, index%26
	if cycle == 0 {
		return string(letters[pos])
	}
	return string(letters[pos]) + strconv.Itoa(cycle)
}

// tronWriteHeaderKey emits a declaration key: raw when it is an identifier,
// JSON-quoted otherwise.
func tronWriteHeaderKey(b *strings.Builder, key string) {
	if tronIdentRE.MatchString(key) {
		b.WriteString(key)
		return
	}
	writeScalar(b, key)
}

// tronWrite serializes v compactly: minted objects as NAME(values in
// declaration order), other objects as JSON with every key quoted, arrays as
// JSON, scalars via writeScalar.
func tronWrite(b *strings.Builder, v any, classes map[string]*tronClass) {
	switch t := v.(type) {
	case toon.Object:
		if len(t.Fields) == 0 {
			b.WriteString("{}")
			return
		}
		if cls, ok := classes[tronFingerprint(t)]; ok {
			b.WriteString(cls.name)
			b.WriteByte('(')
			for i, k := range cls.keys {
				if i > 0 {
					b.WriteByte(',')
				}
				tronWrite(b, tronFieldValue(t, k), classes)
			}
			b.WriteByte(')')
			return
		}
		b.WriteByte('{')
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteByte(',')
			}
			writeScalar(b, f.Key)
			b.WriteByte(':')
			tronWrite(b, f.Value, classes)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			tronWrite(b, e, classes)
		}
		b.WriteByte(']')
	default:
		writeScalar(b, v)
	}
}

// tronFieldValue looks up key in o; a fingerprint match guarantees presence.
func tronFieldValue(o toon.Object, key string) any {
	for _, f := range o.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	panic("tronFieldValue: key " + strconv.Quote(key) + " missing from fingerprint-matched object")
}
