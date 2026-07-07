package codeexec

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeValue(t *testing.T) {
	big100 := new(big.Int).Lsh(big.NewInt(1), 100)
	tests := []struct {
		name string
		raw  string
		want any
	}{
		{"int", "42", int64(42)},
		{"bigint 2**100", "1267650600228229401496703205376", big100},
		{"float", "1.5", 1.5},
		{"string", `"hi"`, "hi"},
		{"bool", "true", true},
		{"null", "null", nil},
		{"tuple", `{"$tuple":[1,2]}`, []any{int64(1), int64(2)}},
		{"set", `{"$set":[1,2]}`, []any{int64(1), int64(2)}},
		{"frozenset", `{"$frozenset":[1]}`, []any{int64(1)}},
		{"bytes", `{"$bytes":[120,121]}`, []byte("xy")},
		{"dict nonstr keys", `{"$dict":[[1,"a"],[{"$tuple":[2,3]},"b"]]}`, map[string]any{"1": "a", "[2 3]": "b"}},
		{"escaped tag-shaped dict", `{"$dict":[["$bytes",[104,105]]]}`, map[string]any{"$bytes": []any{int64(104), int64(105)}}},
		{"plain object", `{"a":1}`, map[string]any{"a": int64(1)}},
		{"nested tuple in dict", `{"k":{"$tuple":[1,2]}}`, map[string]any{"k": []any{int64(1), int64(2)}}},
		{"float inf", `{"$float":"inf"}`, math.Inf(1)},
		{"float -inf", `{"$float":"-inf"}`, math.Inf(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeValue(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("decodeValue(%s) error: %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("decodeValue(%s) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDecodeValueNaN(t *testing.T) {
	got, err := decodeValue(json.RawMessage(`{"$float":"nan"}`))
	if err != nil {
		t.Fatalf("decodeValue error: %v", err)
	}
	f, ok := got.(float64)
	if !ok || !math.IsNaN(f) {
		t.Errorf("decodeValue($float nan) = %#v, want NaN", got)
	}
}

// TestDecodeDictKeyCollision proves distinct keys that stringify identically
// ({1: …, "1": …}) fail loudly instead of silently dropping a value.
func TestDecodeDictKeyCollision(t *testing.T) {
	_, err := decodeValue(json.RawMessage(`{"$dict":[[1,"int"],["1","str"]]}`))
	if err == nil {
		t.Fatal("decodeValue = nil error, want key-collision failure")
	}
	if !strings.Contains(err.Error(), "collide") {
		t.Errorf("error %q missing %q", err, "collide")
	}
}

// TestNegativeZeroRoundTrip pins current -0.0 behavior: decode keeps the sign
// ("-0.0" stays a negative-zero float64), encode emits "-0", and that
// re-decodes as int64(0).
func TestNegativeZeroRoundTrip(t *testing.T) {
	got, err := decodeValue(json.RawMessage("-0.0"))
	if err != nil {
		t.Fatalf("decodeValue(-0.0) error: %v", err)
	}
	f, ok := got.(float64)
	if !ok || f != 0 || !math.Signbit(f) {
		t.Fatalf("decodeValue(-0.0) = %#v, want negative-zero float64", got)
	}
	enc, err := encodeValue(f)
	if err != nil {
		t.Fatalf("encodeValue(-0.0) error: %v", err)
	}
	if string(enc) != "-0" {
		t.Errorf("encodeValue(-0.0) = %s, want -0", enc)
	}
	back, err := decodeValue(enc)
	if err != nil {
		t.Fatalf("decodeValue(%s) error: %v", enc, err)
	}
	if back != any(int64(0)) {
		t.Errorf("decodeValue(%s) = %#v, want int64(0)", enc, back)
	}
}

// TestWireDepthCap pins the nesting boundary on both directions: 64 levels
// pass, 65 fail loudly.
func TestWireDepthCap(t *testing.T) {
	deep := func(n int) json.RawMessage {
		return json.RawMessage(strings.Repeat("[", n) + "1" + strings.Repeat("]", n))
	}
	nest := func(n int) any {
		v := any(int64(1))
		for range n {
			v = []any{v}
		}
		return v
	}
	tests := []struct {
		name    string
		run     func() error
		wantErr bool
	}{
		{"decode 64", func() error { _, err := decodeValue(deep(64)); return err }, false},
		{"decode 65", func() error { _, err := decodeValue(deep(65)); return err }, true},
		{"encode 64", func() error { _, err := encodeValue(nest(64)); return err }, false},
		{"encode 65", func() error { _, err := encodeValue(nest(65)); return err }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("error: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "value nesting exceeds depth 64") {
				t.Errorf("error = %v, want depth-cap failure", err)
			}
		})
	}
}

// TestBigIntRendersDigitExact pins the bigint path end to end: the wire number
// decodes to *big.Int and renders through json.Marshal without losing a digit.
func TestBigIntRendersDigitExact(t *testing.T) {
	const digits = "1267650600228229401496703205376"
	val, err := decodeValue(json.RawMessage(digits))
	if err != nil {
		t.Fatalf("decodeValue error: %v", err)
	}
	if got := rendered(val, ""); got != digits {
		t.Errorf("rendered = %q, want %q", got, digits)
	}
}

// TestBytesRenderRaw pins the $bytes path: decoded []byte renders raw.
func TestBytesRenderRaw(t *testing.T) {
	val, err := decodeValue(json.RawMessage(`{"$bytes":[104,105]}`))
	if err != nil {
		t.Fatalf("decodeValue error: %v", err)
	}
	if got := rendered(val, ""); got != "hi" {
		t.Errorf("rendered = %q, want %q", got, "hi")
	}
}

func TestEncodeValue(t *testing.T) {
	big100 := new(big.Int).Lsh(big.NewInt(1), 100)
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"string", "hi", `"hi"`},
		{"int64", int64(7), "7"},
		{"bigint", big100, "1267650600228229401496703205376"},
		{"bytes", []byte("xy"), `{"$bytes":[120,121]}`},
		{"nan", math.NaN(), `{"$float":"nan"}`},
		{"inf", math.Inf(1), `{"$float":"inf"}`},
		{"-inf", math.Inf(-1), `{"$float":"-inf"}`},
		{"nil", nil, "null"},
		{"nested", map[string]any{"a": []any{int64(1), []byte("z")}}, `{"a":[1,{"$bytes":[122]}]}`},
		{"tag-shaped dict escapes", map[string]any{"$bytes": []any{int64(1)}}, `{"$dict":[["$bytes",[1]]]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeValue(tt.v)
			if err != nil {
				t.Fatalf("encodeValue(%#v) error: %v", tt.v, err)
			}
			if string(got) != tt.want {
				t.Errorf("encodeValue(%#v) = %s, want %s", tt.v, got, tt.want)
			}
		})
	}
}

// TestEncodeDecodeRoundTrip proves host returns survive the wire both ways.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	values := []any{
		"text",
		int64(42),
		new(big.Int).Lsh(big.NewInt(1), 100),
		1.5,
		true,
		nil,
		[]byte{0, 255},
		[]any{int64(1), "two", []any{int64(3)}},
		map[string]any{"k": []any{int64(1), int64(2)}, "s": "v"},
		map[string]any{"$bytes": []any{int64(104), int64(105)}},
	}
	for _, v := range values {
		enc, err := encodeValue(v)
		if err != nil {
			t.Fatalf("encodeValue(%#v) error: %v", v, err)
		}
		got, err := decodeValue(enc)
		if err != nil {
			t.Fatalf("decodeValue(%s) error: %v", enc, err)
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("round trip %#v = %#v", v, got)
		}
	}
}
