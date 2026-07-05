package format

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	"github.com/toon-format/toon-go"
)

func csvRow(t *testing.T, pairs ...any) toon.Object {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("csvRow: odd pair count %d", len(pairs))
	}
	fields := make([]toon.Field, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			t.Fatalf("csvRow: key %v is not a string", pairs[i])
		}
		fields = append(fields, toon.Field{Key: key, Value: pairs[i+1]})
	}
	return toon.NewObject(fields...)
}

func csvBig(t *testing.T, s string) *big.Int {
	t.Helper()
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("csvBig: bad integer %q", s)
	}
	return bi
}

func csvGoldenRows(t *testing.T) []any {
	t.Helper()
	return []any{
		csvRow(t, "name", "ada", "id", int64(1), "active", true, "note", nil, "score", json.Number("2.5")),
		csvRow(t, "name", "bob", "id", int64(2), "active", false, "note", "x", "score", json.Number("0.125")),
	}
}

func TestCSVGolden(t *testing.T) {
	got, err := encodeCSV(csvGoldenRows(t))
	if err != nil {
		t.Fatalf("encodeCSV() error: %v", err)
	}
	want := "name,id,active,note,score\nada,1,true,,2.5\nbob,2,false,x,0.125"
	if got != want {
		t.Errorf("encodeCSV() = %q, want %q", got, want)
	}
}

func TestCSVQuoting(t *testing.T) {
	v := []any{csvRow(t, "a", "x,y", "b", `say "hi"`, "c", "l1\nl2")}
	got, err := encodeCSV(v)
	if err != nil {
		t.Fatalf("encodeCSV() error: %v", err)
	}
	want := "a,b,c\n\"x,y\",\"say \"\"hi\"\"\",\"l1\nl2\""
	if got != want {
		t.Errorf("encodeCSV() = %q, want %q", got, want)
	}
}

func TestCSVKeyReorder(t *testing.T) {
	v := []any{
		csvRow(t, "a", int64(1), "b", int64(2)),
		csvRow(t, "b", int64(4), "a", int64(3)),
	}
	got, err := encodeCSV(v)
	if err != nil {
		t.Fatalf("encodeCSV() error: %v", err)
	}
	want := "a,b\n1,2\n3,4"
	if got != want {
		t.Errorf("encodeCSV() = %q, want %q", got, want)
	}
}

func TestCSVBigInt(t *testing.T) {
	v := []any{csvRow(t,
		"big", csvBig(t, "12345678901234567890123456789"),
		"max", int64(9223372036854775807),
		"neg", csvBig(t, "-98765432109876543210"),
	)}
	got, err := encodeCSV(v)
	if err != nil {
		t.Fatalf("encodeCSV() error: %v", err)
	}
	want := "big,max,neg\n12345678901234567890123456789,9223372036854775807,-98765432109876543210"
	if got != want {
		t.Errorf("encodeCSV() = %q, want %q", got, want)
	}
}

func TestCSVErrors(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"non-array root", "nope", "encode csv: string is not an array of objects"},
		{
			"object root", csvRow(t, "a", int64(1)),
			fmt.Sprintf("encode csv: %T is not an array of objects", toon.NewObject()),
		},
		{"empty array", []any{}, "encode csv: empty array has no header"},
		{"zero columns", []any{csvRow(t)}, "encode csv: row 0 has no keys"},
		{"non-object first row", []any{int64(5)}, "encode csv: row 0 is int64, not an object"},
		{
			// A duplicate key must error, not silently keep one of the values.
			"duplicate key in first row",
			[]any{csvRow(t, "a", int64(1), "a", int64(101))},
			`encode csv: row 0 duplicates key "a"`,
		},
		{
			"duplicate key in later row",
			[]any{csvRow(t, "a", int64(1), "b", int64(2)), csvRow(t, "a", int64(3), "a", int64(4))},
			`encode csv: row 1 duplicates key "a"`,
		},
		{"non-object later row", []any{csvRow(t, "a", int64(1)), "x"}, "encode csv: row 1 is string, not an object"},
		{
			"row adds key",
			[]any{csvRow(t, "a", int64(1)), csvRow(t, "a", int64(2), "b", int64(3))},
			"encode csv: row 1 has 2 keys, header has 1",
		},
		{
			"row drops key",
			[]any{csvRow(t, "a", int64(1), "b", int64(2)), csvRow(t, "a", int64(3))},
			"encode csv: row 1 has 1 keys, header has 2",
		},
		{
			"row renames key",
			[]any{csvRow(t, "a", int64(1), "b", int64(2)), csvRow(t, "a", int64(3), "c", int64(4))},
			`encode csv: row 1 is missing key "b"`,
		},
		{
			"array cell",
			[]any{csvRow(t, "a", []any{int64(1)})},
			`encode csv: row 0 key "a" holds []interface {}, not a scalar`,
		},
		{
			"object cell",
			[]any{csvRow(t, "a", toon.NewObject())},
			fmt.Sprintf(`encode csv: row 0 key "a" holds %T, not a scalar`, toon.NewObject()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeCSV(tt.v)
			if err == nil {
				t.Fatal("encodeCSV() error = nil, want error")
			}
			if err.Error() != tt.want {
				t.Errorf("encodeCSV() error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestTSVGolden(t *testing.T) {
	got, err := encodeTSV(csvGoldenRows(t))
	if err != nil {
		t.Fatalf("encodeTSV() error: %v", err)
	}
	want := "name\tid\tactive\tnote\tscore\nada\t1\ttrue\t\t2.5\nbob\t2\tfalse\tx\t0.125"
	if got != want {
		t.Errorf("encodeTSV() = %q, want %q", got, want)
	}
}

func TestTSVQuoting(t *testing.T) {
	v := []any{csvRow(t, "a", "x\ty", "b", "l1\nl2", "c", "plain")}
	got, err := encodeTSV(v)
	if err != nil {
		t.Fatalf("encodeTSV() error: %v", err)
	}
	want := "a\tb\tc\n\"x\ty\"\t\"l1\nl2\"\tplain"
	if got != want {
		t.Errorf("encodeTSV() = %q, want %q", got, want)
	}
}

func TestTSVBigInt(t *testing.T) {
	v := []any{csvRow(t, "n", csvBig(t, "12345678901234567890123456789"))}
	got, err := encodeTSV(v)
	if err != nil {
		t.Fatalf("encodeTSV() error: %v", err)
	}
	want := "n\n12345678901234567890123456789"
	if got != want {
		t.Errorf("encodeTSV() = %q, want %q", got, want)
	}
}

func TestTSVErrors(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"non-array root", "nope", "encode tsv: string is not an array of objects"},
		{
			"array cell",
			[]any{csvRow(t, "a", []any{})},
			`encode tsv: row 0 key "a" holds []interface {}, not a scalar`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeTSV(tt.v)
			if err == nil {
				t.Fatal("encodeTSV() error = nil, want error")
			}
			if err.Error() != tt.want {
				t.Errorf("encodeTSV() error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestMarkdownGolden(t *testing.T) {
	got, err := encodeMarkdown(csvGoldenRows(t))
	if err != nil {
		t.Fatalf("encodeMarkdown() error: %v", err)
	}
	want := "|name|id|active|note|score|\n|---|---|---|---|---|\n|ada|1|true||2.5|\n|bob|2|false|x|0.125|"
	if got != want {
		t.Errorf("encodeMarkdown() = %q, want %q", got, want)
	}
}

func TestMarkdownEscaping(t *testing.T) {
	v := []any{csvRow(t, "a", "x|y", "b", "l1\nl2", "c", "crlf\r\nend")}
	got, err := encodeMarkdown(v)
	if err != nil {
		t.Fatalf("encodeMarkdown() error: %v", err)
	}
	want := "|a|b|c|\n|---|---|---|\n|x\\|y|l1<br>l2|crlf<br>end|"
	if got != want {
		t.Errorf("encodeMarkdown() = %q, want %q", got, want)
	}
}

// TestMarkdownBackslashEscaping pins the backslash escape: a cell ending in
// \ would otherwise emit \| — an escaped literal pipe to a GFM parser — and
// destroy the column boundary.
func TestMarkdownBackslashEscaping(t *testing.T) {
	v := []any{csvRow(t, "p", `C:\`, "q", `x\|y`)}
	got, err := encodeMarkdown(v)
	if err != nil {
		t.Fatalf("encodeMarkdown() error: %v", err)
	}
	want := "|p|q|\n|---|---|\n" + `|C:\\|x\\\|y|`
	if got != want {
		t.Errorf("encodeMarkdown() = %q, want %q", got, want)
	}
}

func TestMarkdownBigInt(t *testing.T) {
	v := []any{csvRow(t, "n", csvBig(t, "12345678901234567890123456789"))}
	got, err := encodeMarkdown(v)
	if err != nil {
		t.Fatalf("encodeMarkdown() error: %v", err)
	}
	want := "|n|\n|---|\n|12345678901234567890123456789|"
	if got != want {
		t.Errorf("encodeMarkdown() = %q, want %q", got, want)
	}
}

func TestMarkdownErrors(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"non-array root", int64(7), "encode markdown: int64 is not an array of objects"},
		{
			"array cell",
			[]any{csvRow(t, "a", []any{})},
			`encode markdown: row 0 key "a" holds []interface {}, not a scalar`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeMarkdown(tt.v)
			if err == nil {
				t.Fatal("encodeMarkdown() error = nil, want error")
			}
			if err.Error() != tt.want {
				t.Errorf("encodeMarkdown() error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}
