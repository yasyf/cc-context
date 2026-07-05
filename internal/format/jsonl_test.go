package format

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/toon-format/toon-go"
)

// jsonlIR decodes src through decodeAll so tests exercise the real IR.
func jsonlIR(t *testing.T, src string) any {
	t.Helper()
	v, ok, err := decodeAll([]byte(src))
	if err != nil {
		t.Fatalf("decodeAll(%q): %v", src, err)
	}
	if !ok {
		t.Fatalf("decodeAll(%q): no value", src)
	}
	return v
}

func TestJSONLEncode(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			"mixed shapes",
			`[{"a":1,"b":[true,null]},"txt",42,[1,{"k":"v"}]]`,
			"{\"a\":1,\"b\":[true,null]}\n\"txt\"\n42\n[1,{\"k\":\"v\"}]",
		},
		{
			"uniform objects",
			`[{"id":1,"name":"a"},{"id":2,"name":"b"},{"id":3,"name":"c"}]`,
			"{\"id\":1,\"name\":\"a\"}\n{\"id\":2,\"name\":\"b\"}\n{\"id\":3,\"name\":\"c\"}",
		},
		{
			"scalars and null",
			`[1,"two",true,null,3.5]`,
			"1\n\"two\"\ntrue\nnull\n3.5",
		},
		{
			"big integers digit-exact",
			`[123456789012345678901234567890,9223372036854775808,18014398509481985]`,
			"123456789012345678901234567890\n9223372036854775808\n18014398509481985",
		},
		{
			"no html escaping",
			`["<a href=\"x\">&</a>",{"q":"a<b>c&d"}]`,
			"\"<a href=\\\"x\\\">&</a>\"\n{\"q\":\"a<b>c&d\"}",
		},
		{
			"embedded newline stays escaped",
			`["line1\nline2","solo"]`,
			"\"line1\\nline2\"\n\"solo\"",
		},
		{
			"empty array",
			`[]`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeJSONL(jsonlIR(t, tt.src))
			if err != nil {
				t.Fatalf("encodeJSONL() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("encodeJSONL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"mixed", `[{"a":1,"b":[true,null]},"txt",42,[1,{"k":"v"}]]`},
		{"uniform", `[{"id":1},{"id":2},{"id":3}]`},
		{"bigints", `[123456789012345678901234567890,9223372036854775808]`},
		{"scalars", `[1,"two",true,null,3.5]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := jsonlIR(t, tt.src)
			out, err := encodeJSONL(in)
			if err != nil {
				t.Fatalf("encodeJSONL() error: %v", err)
			}
			for i, line := range strings.Split(out, "\n") {
				if !json.Valid([]byte(line)) {
					t.Errorf("line %d is not valid JSON: %q", i, line)
				}
			}
			got, ok, derr := decodeAll([]byte(out))
			if derr != nil || !ok {
				t.Fatalf("decodeAll(output): ok=%v err=%v", ok, derr)
			}
			if !reflect.DeepEqual(got, in) {
				t.Errorf("round trip = %#v, want %#v", got, in)
			}
		})
	}
}

func TestJSONLNotArray(t *testing.T) {
	tests := []struct {
		name string
		v    any
	}{
		{"object", toon.NewObject(toon.Field{Key: "a", Value: int64(1)})},
		{"string", "solo"},
		{"integer", int64(7)},
		{"nil", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := encodeJSONL(tt.v)
			if err == nil {
				t.Fatal("encodeJSONL() error = nil, want error")
			}
			if got, want := err.Error(), "encode jsonl: not an array"; got != want {
				t.Errorf("encodeJSONL() error = %q, want %q", got, want)
			}
			if out != "" {
				t.Errorf("encodeJSONL() = %q, want empty on error", out)
			}
		})
	}
}
