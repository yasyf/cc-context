package format

import (
	"strings"
	"testing"
)

func proseIR(t *testing.T, src string) any {
	t.Helper()
	v, ok, err := decodeAll([]byte(src))
	if err != nil || !ok {
		t.Fatalf("decodeAll(%q): ok=%v err=%v", src, ok, err)
	}
	return v
}

func TestProseEncode(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "bare string with escaped newlines comes out raw",
			src:  `"# Title\n\nFirst paragraph with several words.\nSecond line, still prose."`,
			want: "# Title\n\nFirst paragraph with several words.\nSecond line, still prose.",
		},
		{
			name: "github issue metadata tags then body",
			src: `{"title":"Panic when config missing","number":1347,"state":"open",` +
				`"locked":false,"assignee":null,"labels":["bug","p1"],` +
				`"body":"Running ccx with no config panics.\n\nSteps to reproduce:\n1. rm ~/.ccx.toml\n2. ccx repo overview\n\nExpected an error, got a panic."}`,
			want: "<title>Panic when config missing</title>\n" +
				"<number>1347</number>\n" +
				"<state>open</state>\n" +
				"<locked>false</locked>\n" +
				"<assignee>null</assignee>\n" +
				`<labels>["bug","p1"]</labels>` + "\n" +
				"\n" +
				"Running ccx with no config panics.\n\nSteps to reproduce:\n1. rm ~/.ccx.toml\n2. ccx repo overview\n\nExpected an error, got a panic.",
		},
		{
			name: "nested object residual as compact JSON in its tag",
			src:  `{"user":{"login":"yasyf","id":9223372036854775807},"body":"A multi word prose body that dominates this payload."}`,
			want: `<user>{"login":"yasyf","id":9223372036854775807}</user>` + "\n" +
				"\n" +
				"A multi word prose body that dominates this payload.",
		},
		{
			name: "bigint metadata field digit-exact",
			src:  `{"id":98765432109876543210987654321098765432,"body":"big integers ride along untouched in their tag"}`,
			want: "<id>98765432109876543210987654321098765432</id>\n" +
				"\n" +
				"big integers ride along untouched in their tag",
		},
		{
			name: "angle brackets in prose body pass through raw",
			src:  `{"note":"see below","body":"Render <b>bold</b> & <i>italic</i> — raw angle brackets and ampersands survive unescaped."}`,
			want: "<note>see below</note>\n" +
				"\n" +
				"Render <b>bold</b> & <i>italic</i> — raw angle brackets and ampersands survive unescaped.",
		},
		{
			name: "largest multi-word string wins over single-token blob",
			src:  `{"blob":"` + strings.Repeat("A", 200) + `","summary":"a short multi word summary is still the only prose candidate"}`,
			want: "<blob>" + strings.Repeat("A", 200) + "</blob>\n" +
				"\n" +
				"a short multi word summary is still the only prose candidate",
		},
		{
			name: "smaller prose field rides along as a tag",
			src:  `{"tldr":"short prose here","body":"the substantially longer prose body wins the dominance contest outright"}`,
			want: "<tldr>short prose here</tldr>\n" +
				"\n" +
				"the substantially longer prose body wins the dominance contest outright",
		},
		{
			name: "string metadata with newline stays raw inside its tag",
			src:  `{"sig":"line one\nline two","body":"the actual prose body is comfortably larger than the signature"}`,
			want: "<sig>line one\nline two</sig>\n" +
				"\n" +
				"the actual prose body is comfortably larger than the signature",
		},
		{
			name: "only-prose object emits body alone with no blank line",
			src:  `{"body":"just the prose body and nothing else at all"}`,
			want: "just the prose body and nothing else at all",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodeProse(proseIR(t, tt.src))
			if err != nil {
				t.Fatalf("encodeProse() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("encodeProse() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProseErrors(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "object with no multi-word string field",
			v:    proseIRHelper(`{"a":"single-token","b":42}`),
			want: "encode prose: no dominant prose field",
		},
		{
			name: "object with only non-string fields",
			v:    proseIRHelper(`{"n":1,"ok":true}`),
			want: "encode prose: no dominant prose field",
		},
		{
			name: "array root",
			v:    []any{"one two three", "four five six"},
			want: "encode prose: cannot unwrap []interface {}",
		},
		{
			name: "integer root",
			v:    int64(42),
			want: "encode prose: cannot unwrap int64",
		},
		{
			name: "nil root",
			v:    nil,
			want: "encode prose: cannot unwrap <nil>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := encodeProse(tt.v)
			if err == nil {
				t.Fatalf("encodeProse() = %q, want error %q", out, tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("encodeProse() error = %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

// proseIRHelper decodes src outside a test body so error-table literals can
// build IR values inline; it panics on malformed fixtures.
func proseIRHelper(src string) any {
	v, ok, err := decodeAll([]byte(src))
	if err != nil || !ok {
		panic("proseIRHelper: bad fixture " + src)
	}
	return v
}
