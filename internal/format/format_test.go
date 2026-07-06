package format

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/toon-format/toon-go"
)

func defaultOpts() Options {
	return Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma}
}

func TestConvert(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		opts      Options
		want      string
		converted bool
	}{
		{
			name:      "tabular uniform array source-order columns",
			src:       `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "[2]{id,name}:\n  1,Ada\n  2,Lin",
			converted: true,
		},
		{
			name:      "key order preserved with non-alphabetical keys",
			src:       `{"zeta":1,"alpha":2}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "zeta: 1\nalpha: 2",
			converted: true,
		},
		{
			name:      "tabular columns keep non-alphabetical source order",
			src:       `[{"zeta":1,"alpha":2},{"zeta":3,"alpha":4}]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "[2]{zeta,alpha}:\n  1,2\n  3,4",
			converted: true,
		},
		{
			// Under the old TOON-first shootout this emitted TOON; the
			// classifier floors payloads under smallPayloadBytes to compact
			// JSON.
			name:      "auto floors small table to compact JSON",
			src:       `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`,
			opts:      defaultOpts(),
			want:      `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`,
			converted: true,
		},
		{
			name:      "auto floors deep nesting to compact JSON",
			src:       `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":1}}}}}`,
			opts:      Options{Format: FormatAuto, Indent: 4, Delimiter: DelimiterComma},
			want:      `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":1}}}}}`,
			converted: true,
		},
		{
			name:      "FormatTOON always emits TOON even when larger",
			src:       `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":1}}}}}`,
			opts:      Options{Format: FormatTOON, Indent: 4, Delimiter: DelimiterComma},
			want:      "aaa:\n    bbb:\n        ccc:\n            ddd:\n                eee: 1",
			converted: true,
		},
		{
			name:      "FormatJSON always emits compact JSON even when larger",
			src:       `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`,
			opts:      Options{Format: FormatJSON, Indent: 2, Delimiter: DelimiterComma},
			want:      `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`,
			converted: true,
		},
		{
			name:      "compact JSON does not HTML-escape",
			src:       `{"html":"<b>&amp;</b> > <"}`,
			opts:      Options{Format: FormatJSON, Indent: 2, Delimiter: DelimiterComma},
			want:      `{"html":"<b>&amp;</b> > <"}`,
			converted: true,
		},
		{
			name:      "auto compact JSON keeps angle brackets raw",
			src:       `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":"<&>"}}}}}`,
			opts:      Options{Format: FormatAuto, Indent: 4, Delimiter: DelimiterComma},
			want:      `{"aaa":{"bbb":{"ccc":{"ddd":{"eee":"<&>"}}}}}`,
			converted: true,
		},
		{
			name:      "compact JSON preserves big integer digits",
			src:       `{"n":123456789012345678901}`,
			opts:      Options{Format: FormatJSON, Indent: 2, Delimiter: DelimiterComma},
			want:      `{"n":123456789012345678901}`,
			converted: true,
		},
		{
			name:      "ndjson folds into single table",
			src:       "{\"a\":1,\"b\":2}\n{\"a\":3,\"b\":4}\n",
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "[2]{a,b}:\n  1,2\n  3,4",
			converted: true,
		},
		{
			name:      "lone top-level array stays an array",
			src:       `[1,2,3]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "[3]: 1,2,3",
			converted: true,
		},
		{
			name:      "single object",
			src:       `{"a":1}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "a: 1",
			converted: true,
		},
		{
			name:      "tab delimiter",
			src:       `[{"a":1,"b":2},{"a":3,"b":4}]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterTab},
			want:      "[2\t]{a\tb}:\n  1\t2\n  3\t4",
			converted: true,
		},
		{
			name:      "pipe delimiter",
			src:       `[{"a":1,"b":2},{"a":3,"b":4}]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterPipe},
			want:      "[2|]{a|b}:\n  1|2\n  3|4",
			converted: true,
		},
		{
			name:      "indent 4",
			src:       `{"a":{"b":1}}`,
			opts:      Options{Format: FormatTOON, Indent: 4, Delimiter: DelimiterComma},
			want:      "a:\n    b: 1",
			converted: true,
		},
		{
			name:      "null scalar",
			src:       `{"a":null}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "a: null",
			converted: true,
		},
		{
			name:      "empty array",
			src:       `[]`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "[0]:",
			converted: true,
		},
		{
			name:      "empty object",
			src:       `{}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "",
			converted: true,
		},
		{
			name:      "nested object",
			src:       `{"outer":{"inner":{"leaf":7}}}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "outer:\n  inner:\n    leaf: 7",
			converted: true,
		},
		{
			name:      "numeric-like string is quoted",
			src:       `{"a":"42"}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      `a: "42"`,
			converted: true,
		},
		{
			name:      "bool-like string is quoted",
			src:       `{"a":"true"}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      `a: "true"`,
			converted: true,
		},
		{
			name:      "leading-dash string is quoted",
			src:       `{"a":"-x"}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      `a: "-x"`,
			converted: true,
		},
		{
			name:      "plain string is unquoted",
			src:       `{"a":"hello"}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "a: hello",
			converted: true,
		},
		{
			// Just under JS MAX_SAFE_INTEGER (9007199254740991): emitted unquoted,
			// precision intact — the json.Number→float64 path would round this.
			name:      "large safe integer preserved unquoted",
			src:       `{"n":9007199254740991}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 9007199254740991",
			converted: true,
		},
		{
			// Past MAX_SAFE_INTEGER: TOON spec emits a quoted decimal string so the
			// exact value survives (a float64 round-trip would corrupt it to
			// ...680). The digits are preserved verbatim — that is the fidelity win.
			name:      "big integer past safe range preserved as quoted string",
			src:       `{"n":123456789012345678}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      `n: "123456789012345678"`,
			converted: true,
		},
		{
			// Wider than int64: numberScalar yields *big.Int and toon-go's
			// normalize applies the same safe-integer rule as its int64 case —
			// a quoted decimal string, digits intact, never a float64 coercion.
			name:      "big.Int-width integer preserved as quoted string",
			src:       `{"n":12345678901234567890123456789}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      `n: "12345678901234567890123456789"`,
			converted: true,
		},
		{
			name:      "negative zero canonicalized",
			src:       `{"n":-0}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 0",
			converted: true,
		},
		{
			// 1E2 denotes exactly 100 in float64, so toon-go's round-trip is
			// value-preserving and the lossy-number guard must let it through.
			name:      "exponent notation canonicalized when value-exact",
			src:       `{"n":1E2}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 100",
			converted: true,
		},
		{
			// 2.5e-3 is not exactly representable in float64, but toon-go's
			// canonical rendering 0.0025 denotes the same decimal value — the
			// guard must compare rendered value to source value, not the
			// float64's binary value, or it over-rejects scientific notation.
			name:      "exponent notation canonicalized when rendering is value-preserving",
			src:       `{"n":2.5e-3}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 0.0025",
			converted: true,
		},
		{
			name:      "small exponent canonicalized to plain decimal",
			src:       `{"n":1e-7}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 0.0000001",
			converted: true,
		},
		{
			// ParseInt("-0") returns 0, silently dropping the sign, so the
			// integer fast path must skip it; json.Number renders verbatim
			// through writeScalar.
			name:      "compact JSON keeps negative zero and exponent verbatim",
			src:       `{"a":-0,"b":1E2}`,
			opts:      Options{Format: FormatJSON, Indent: 2, Delimiter: DelimiterComma},
			want:      `{"a":-0,"b":1E2}`,
			converted: true,
		},
		{
			name:      "float preserved",
			src:       `{"n":1.5}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 1.5",
			converted: true,
		},
		{
			name:      "integer stays integer not float",
			src:       `{"n":1}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 1",
			converted: true,
		},
		{
			name:      "non-json passthrough verbatim",
			src:       "hello not json\n",
			opts:      defaultOpts(),
			want:      "hello not json\n",
			converted: false,
		},
		{
			name:      "empty input passthrough",
			src:       "",
			opts:      defaultOpts(),
			want:      "",
			converted: false,
		},
		{
			name:      "whitespace-only input passthrough",
			src:       "   \n  ",
			opts:      defaultOpts(),
			want:      "   \n  ",
			converted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, converted, err := Convert([]byte(tt.src), tt.opts)
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Convert() = %q, want %q", got, tt.want)
			}
			if converted != tt.converted {
				t.Errorf("Convert() converted = %v, want %v", converted, tt.converted)
			}
		})
	}
}

// TestConvertAutoRegressions pins default-auto behavior on shapes that once
// panicked or silently corrupted data: auto never fails and never drops
// fields.
func TestConvertAutoRegressions(t *testing.T) {
	dupRows := make([]string, 30)
	for i := range dupRows {
		dupRows[i] = fmt.Sprintf(`{"a":%d,"a":%d}`, i, i+100)
	}
	dupSrc := "[" + strings.Join(dupRows, ",") + "]"
	emptySrc := "[" + strings.Repeat("{},", 399) + "{}]"

	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			// The NUL-join fingerprint collided {"a\x00b","c"} with
			// {"a","b\x00c"}: the fourth element matched class A and
			// encodeTRON panicked on the missing key inside encodeAuto.
			name: "nul key collision does not panic through auto tron",
			src: `{"wrap":[{"a\u0000b":1,"c":2},{"a\u0000b":3,"c":4},{"a\u0000b":5,"c":6},{"a":7,"b\u0000c":8}],"pad":"` +
				strings.Repeat("x", 120) + `"}`,
			want: "class A: \"a\\u0000b\",c\n\n" +
				`{"wrap":[A(1,2),A(3,4),A(5,6),{"a":7,"b\u0000c":8}],"pad":"` +
				strings.Repeat("x", 120) + `"}`,
		},
		{
			// Duplicate-key rows: the tabular encoders once emitted last-wins
			// cells; they now error and auto keeps compact JSON, preserving
			// both values.
			name: "duplicate keys fall back to compact JSON",
			src:  dupSrc,
			want: dupSrc,
		},
		{
			// Zero-column rows: csvTable once accepted an empty header and
			// auto emitted 402 lines of bare |.
			name: "empty-object rows fall back to compact JSON",
			src:  emptySrc,
			want: emptySrc,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, converted, err := Convert([]byte(tt.src), defaultOpts())
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if !converted {
				t.Error("Convert() converted = false, want true")
			}
			if got != tt.want {
				t.Errorf("Convert() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestConvertAutoSkipsLossyTOON pins the default path on a null-bearing table
// whose classifier candidates lead with TOON (the hasNulls branch): toon-go's
// float64 round-trip would truncate the 26-digit decimal — output shrinks, so
// the byte-net cannot catch it — and encodeTOON must error instead, letting
// auto fall through to a verbatim encoder.
func TestConvertAutoSkipsLossyTOON(t *testing.T) {
	const pi = "3.14159265358979323846264338"
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "{\"v\":%s,\"n\":null,\"id\":%d}\n", pi, i)
	}
	got, converted, err := Convert([]byte(b.String()), defaultOpts())
	if err != nil {
		t.Fatalf("Convert() error = %v", err)
	}
	if !converted {
		t.Fatal("Convert() converted = false, want true")
	}
	if !strings.Contains(got, pi) {
		t.Errorf("Convert() lost decimal precision; %q missing from output starting %q", pi, got[:200])
	}
}

// autoNullTable builds a uniform 200-row table over the token-pressure floor
// whose first nullPads rows carry a null pad cell. TOON spends 4 bytes on
// each "null" cell that markdown leaves empty, so nullPads dials the
// TOON-vs-markdown byte gap on the hasNulls branch ([TOON, Markdown]).
func autoNullTable(t *testing.T, nullPads int) any {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i := range 200 {
		if i > 0 {
			b.WriteByte(',')
		}
		if i < nullPads {
			fmt.Fprintf(&b, `{"id":"a%03d","pad":null}`, i)
		} else {
			fmt.Fprintf(&b, `{"id":"a%03d","pad":"%s"}`, i, strings.Repeat("x", 70))
		}
	}
	b.WriteByte(']')
	v, ok, err := decodeAll([]byte(b.String()))
	if err != nil || !ok {
		t.Fatalf("decodeAll: ok=%v err=%v", ok, err)
	}
	return v
}

// TestEncodeAutoCandidateTolerance pins the tolerance pick on the null-bearing
// pressure branch (candidates [TOON, Markdown]): the earlier TOON wins when
// markdown's byte win is within candidateTolerance; a wider gap still picks
// the smaller markdown. Each case first proves its premise — markdown strictly
// smaller, gap on the intended side of the tolerance — so the fixture cannot
// silently drift.
func TestEncodeAutoCandidateTolerance(t *testing.T) {
	tests := []struct {
		name     string
		nullPads int
		want     Format
	}{
		{"markdown win within tolerance picks toon", 26, FormatTOON},
		{"markdown win past tolerance picks markdown", 120, FormatMarkdown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := autoNullTable(t, tt.nullPads)
			candidates, _ := classify(v)
			if want := []Format{FormatTOON, FormatMarkdown}; !reflect.DeepEqual(candidates, want) {
				t.Fatalf("classify() = %v, want %v", candidates, want)
			}
			toonOut, err := encodeTOON(v, defaultOpts())
			if err != nil {
				t.Fatalf("encodeTOON() error = %v", err)
			}
			mdOut, err := encodeMarkdown(v)
			if err != nil {
				t.Fatalf("encodeMarkdown() error = %v", err)
			}
			if len(mdOut) >= len(toonOut) || len(toonOut) > len(compactJSON(v)) {
				t.Fatalf("premise: markdown (%d) must beat TOON (%d) and TOON must pass the byte-net (%d)",
					len(mdOut), len(toonOut), len(compactJSON(v)))
			}
			withinTolerance := float64(len(toonOut)) <= (1+candidateTolerance)*float64(len(mdOut))
			if wantWithin := tt.want == FormatTOON; withinTolerance != wantWithin {
				t.Fatalf("premise: gap within tolerance = %v, want %v (toon %d, markdown %d)",
					withinTolerance, wantWithin, len(toonOut), len(mdOut))
			}
			want := mdOut
			if tt.want == FormatTOON {
				want = toonOut
			}
			if got := encodeAuto(v, defaultOpts()); got != want {
				t.Errorf("encodeAuto() picked %d bytes, want the %s output (%d bytes)", len(got), tt.want, len(want))
			}
		})
	}
}

// TestEncodeAutoNeverExceedsCompactJSON sweeps auto over one payload per
// classifier branch: whatever candidate wins under the tolerance rule, output
// never exceeds compact JSON by bytes.
func TestEncodeAutoNeverExceedsCompactJSON(t *testing.T) {
	prose := strings.Repeat("word ", 600)
	hetRow := func(k1, k2 string) string {
		return `{"` + k1 + `":1,"` + k2 + `":"` + strings.Repeat("y", 40) + `"}`
	}
	srcs := map[string]string{
		"tiny object":            `{"ok":true}`,
		"prose dominant":         `{"body":"` + prose + `","id":"x1"}`,
		"prose absolute":         `{"body":"` + prose + `","meta":"` + strings.Repeat("z", 3000) + `"}`,
		"uniform small markdown": classifyTestUniform(t, 10, 500),
		"uniform pressure csv":   classifyTestUniform(t, 200, 9000),
		"null pressure toon":     classifyTestNullPressure(t),
		"repeated nested tron":   classifyTestRows(classifyTestRepeatRows(`{"u":{"x":1,"y":"`+strings.Repeat("x", 60)+`"}}`, 3)...),
		"heterogeneous jsonl":    classifyTestRows(hetRow("a", "b"), hetRow("a", "b"), hetRow("c", "d"), hetRow("e", "f"), hetRow("g", "h")),
		"deep one-off json":      `{"a":{"b":{"c":{"d":"` + strings.Repeat("x", 200) + `"}}}}`,
	}
	for name, src := range srcs {
		t.Run(name, func(t *testing.T) {
			v, ok, err := decodeAll([]byte(src))
			if err != nil || !ok {
				t.Fatalf("decodeAll: ok=%v err=%v", ok, err)
			}
			out := encodeAuto(v, defaultOpts())
			if compact := compactJSON(v); len(out) > len(compact) {
				t.Errorf("encodeAuto() = %d bytes, exceeds compact JSON's %d", len(out), len(compact))
			}
		})
	}
}

// TestConvertExplicitFormats pins the dispatch: every explicit format calls
// its encoder and skips both classification and the byte-net, emitting even
// when larger than compact JSON.
func TestConvertExplicitFormats(t *testing.T) {
	rows := `[{"a":1,"b":"x"},{"a":2,"b":"y"}]`
	tests := []struct {
		name   string
		format Format
		src    string
		want   string
	}{
		{"csv", FormatCSV, rows, "a,b\n1,x\n2,y"},
		{"tsv", FormatTSV, rows, "a\tb\n1\tx\n2\ty"},
		{"markdown", FormatMarkdown, rows, "|a|b|\n|---|---|\n|1|x|\n|2|y|"},
		{"jsonl", FormatJSONL, rows, `{"a":1,"b":"x"}` + "\n" + `{"a":2,"b":"y"}`},
		{"tron mints even when larger", FormatTRON, rows, "class A: a,b\n\n[A(1,\"x\"),A(2,\"y\")]"},
		{"prose", FormatProse, `{"id":7,"body":"two words"}`, "<id>7</id>\n\ntwo words"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, converted, err := Convert([]byte(tt.src), Options{Format: tt.format, Indent: 2, Delimiter: DelimiterComma})
			if err != nil {
				t.Fatalf("Convert(%s) error = %v", tt.format, err)
			}
			if !converted {
				t.Errorf("Convert(%s) converted = false, want true", tt.format)
			}
			if got != tt.want {
				t.Errorf("Convert(%s) = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}

// TestConvertExplicitFormatShapeErrors pins the loud failure on incompatible
// shapes: an explicit format never falls back.
func TestConvertExplicitFormatShapeErrors(t *testing.T) {
	tests := []struct {
		name    string
		format  Format
		src     string
		wantErr string
	}{
		{"csv on object", FormatCSV, `{"a":1}`, "encode csv"},
		{"tsv on object", FormatTSV, `{"a":1}`, "encode tsv"},
		{"markdown on object", FormatMarkdown, `{"a":1}`, "encode markdown"},
		{"jsonl on object", FormatJSONL, `{"a":1}`, "encode jsonl"},
		{"prose on array", FormatProse, `[1,2]`, "encode prose"},
		// toon-go canonicalizes json.Number through a float64 round-trip: a
		// 26-digit decimal would silently truncate and an out-of-range
		// exponent type-flips to a quoted string, so encodeTOON errors first.
		{"toon on excess-precision decimal", FormatTOON, `[{"a":3.14159265358979323846264338}]`, "encode toon"},
		{"toon on out-of-range exponent", FormatTOON, `[{"a":1e999}]`, "encode toon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, converted, err := Convert([]byte(tt.src), Options{Format: tt.format, Indent: 2, Delimiter: DelimiterComma})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Convert(%s) error = %v, want %q", tt.format, err, tt.wantErr)
			}
			if converted {
				t.Errorf("Convert(%s) converted = true, want false", tt.format)
			}
		})
	}
}

func TestConvertUnknownFormat(t *testing.T) {
	for _, f := range []Format{"", "bogus"} {
		t.Run(string(f), func(t *testing.T) {
			_, _, err := Convert([]byte(`{"a":1}`), Options{Format: f, Indent: 2, Delimiter: DelimiterComma})
			if err == nil || !strings.Contains(err.Error(), "unknown format") {
				t.Fatalf("Convert(%q) error = %v, want unknown-format", f, err)
			}
		})
	}
}

func TestConvertStrict(t *testing.T) {
	opts := Options{Format: FormatAuto, Indent: 2, Delimiter: DelimiterComma, Strict: true}
	_, converted, err := Convert([]byte("not json"), opts)
	if err == nil {
		t.Fatal("Convert(strict) on bad JSON: want error, got nil")
	}
	if converted {
		t.Error("Convert(strict) converted = true, want false")
	}
}

func TestConvertRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"uniform array", `[{"id":1,"name":"Ada"},{"id":2,"name":"Lin"}]`},
		{"nested object", `{"outer":{"inner":{"leaf":7}}}`},
		{"non-alphabetical keys", `{"zeta":1,"alpha":2}`},
		{"mixed scalars", `{"s":"x","b":true,"n":null,"f":1.5}`},
		{"lone array", `[1,2,3]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, converted, err := Convert([]byte(tt.src), Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma})
			if err != nil {
				t.Fatalf("Convert() error = %v", err)
			}
			if !converted {
				t.Fatalf("Convert() converted = false, want true")
			}

			decoded, err := toon.DecodeString(out)
			if err != nil {
				t.Fatalf("DecodeString(%q) error = %v", out, err)
			}
			want := normalize(parseJSON(t, tt.src))
			if got := normalize(decoded); !reflect.DeepEqual(got, want) {
				t.Errorf("round-trip mismatch:\n decoded = %#v\n want    = %#v", got, want)
			}
		})
	}
}

func TestRunConvertsStdout(t *testing.T) {
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", `printf '[{"a":1},{"a":2}]'`},
		Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
		nil, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Errorf("Run() code = %d, want 0", code)
	}
	if !converted {
		t.Errorf("Run() converted = false, want true")
	}
	if want := "[2]{a}:\n  1\n  2"; out != want {
		t.Errorf("Run() out = %q, want %q", out, want)
	}
}

func TestRunNonZeroExitCapturesStderr(t *testing.T) {
	var stderr bytes.Buffer
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", `echo boom 1>&2; echo not-json; exit 3`},
		defaultOpts(),
		nil, &stderr,
	)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (command ran, just failed)", err)
	}
	if code != 3 {
		t.Errorf("Run() code = %d, want 3", code)
	}
	if converted {
		t.Errorf("Run() converted = true, want false (stdout was not JSON)")
	}
	if out != "not-json\n" {
		t.Errorf("Run() out = %q, want passthrough %q", out, "not-json\n")
	}
	if got := strings.TrimSpace(stderr.String()); got != "boom" {
		t.Errorf("stderr = %q, want %q", got, "boom")
	}
}

func TestRunSpawnFailure(t *testing.T) {
	_, _, _, err := Run(
		context.Background(),
		[]string{"this-binary-does-not-exist-xyz"},
		defaultOpts(),
		nil, &bytes.Buffer{},
	)
	if err == nil {
		t.Fatal("Run() on missing binary: want error, got nil")
	}
}

func TestRunForwardsStdin(t *testing.T) {
	in := strings.NewReader(`{"a":1}`)
	out, converted, code, err := Run(
		context.Background(),
		[]string{"sh", "-c", "cat"},
		Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
		in, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if code != 0 {
		t.Errorf("Run() code = %d, want 0", code)
	}
	if !converted {
		t.Errorf("Run() converted = false, want true")
	}
	if want := "a: 1"; out != want {
		t.Errorf("Run() out = %q, want %q (stdin forwarded and converted)", out, want)
	}
}

func TestRunEmptyArgv(t *testing.T) {
	_, _, _, err := Run(context.Background(), nil, defaultOpts(), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() with empty argv: want error, got nil")
	}
}
