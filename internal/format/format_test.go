package format

import (
	"bytes"
	"context"
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
			name:      "negative zero canonicalized",
			src:       `{"n":-0}`,
			opts:      Options{Format: FormatTOON, Indent: 2, Delimiter: DelimiterComma},
			want:      "n: 0",
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
