package format

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// classifyTestProse builds prose-like text (whitespace-separated words) of
// exactly n bytes with no JSON-escaped characters.
func classifyTestProse(t *testing.T, n int) string {
	t.Helper()
	s := strings.Repeat("word ", n/5) + strings.Repeat("x", n%5)
	if len(s) != n || !strings.Contains(s, " ") {
		t.Fatalf("classifyTestProse(%d): built %d bytes", n, len(s))
	}
	return s
}

// classifyTestUniform builds a uniform all-scalar array of rows shaped
// {"id":"NNN","pad":"xxx…"} whose compact JSON is exactly target bytes, with
// the pad column averaging ≤ proseCellChars so the prose-column arm stays
// quiet.
func classifyTestUniform(t *testing.T, rows, target int) string {
	t.Helper()
	sumPads := target - 22*rows - 1 // per row: 21 structural chars + pad; plus rows-1 commas + 2 brackets
	if sumPads < 0 || float64(sumPads)/float64(rows) > proseCellChars {
		t.Fatalf("classifyTestUniform(%d, %d): pad avg out of range", rows, target)
	}
	base, rem := sumPads/rows, sumPads%rows
	var b strings.Builder
	b.WriteByte('[')
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		pad := base
		if i < rem {
			pad++
		}
		fmt.Fprintf(&b, `{"id":"%03d","pad":"%s"}`, i, strings.Repeat("x", pad))
	}
	b.WriteByte(']')
	if b.Len() != target {
		t.Fatalf("classifyTestUniform(%d, %d): built %d bytes", rows, target, b.Len())
	}
	return b.String()
}

// classifyTestNullPressure builds a uniform array over the token-pressure
// threshold (≥ 8000 compact bytes) with one null pad cell.
func classifyTestNullPressure(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i := range 90 {
		fmt.Fprintf(&b, `{"id":"%03d","pad":"%s"},`, i, strings.Repeat("x", 70))
	}
	b.WriteString(`{"id":"090","pad":null}]`)
	if b.Len() < tableTokenPressure*4 { // 4 chars/token — must land in the pressure arm
		t.Fatalf("classifyTestNullPressure: built %d bytes, want >= %d", b.Len(), tableTokenPressure*4)
	}
	return b.String()
}

func classifyTestRows(rows ...string) string {
	return "[" + strings.Join(rows, ",") + "]"
}

func classifyTestRepeatRows(row string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = row
	}
	return out
}

func TestClassify(t *testing.T) {
	x := func(n int) string { return strings.Repeat("x", n) }
	rowAB := `{"a":1,"b":"` + x(10) + `"}`
	rowZQ := `{"z":1,"q":"` + x(10) + `"}`
	het := func(k1, k2 string) string { return `{"` + k1 + `":1,"` + k2 + `":"` + x(40) + `"}` }
	nested3 := `{"a":{"x":1,"y":"` + x(50) + `"},"b":{"x":2,"y":"` + x(50) + `"},"c":{"x":3,"y":"` + x(50) + `"}}`

	tests := []struct {
		name  string
		src   string
		want  []Format
		check func(t *testing.T, a analysis)
	}{
		// Branch 1 — pre-chart size floor.
		{
			name: "floor tiny object",
			src:  `{"ok":true}`,
			want: []Format{FormatJSON},
		},
		// Boundary smallPayloadBytes: 199 bytes floors, 200 exits the floor.
		{
			name: "floor boundary 199 bytes",
			src:  `"` + x(197) + `"`,
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.compactBytes != 199 {
					t.Errorf("compactBytes = %d, want 199", a.compactBytes)
				}
			},
		},
		{
			name: "floor boundary 200 bytes exits to prose",
			src:  `"` + x(198) + `"`,
			want: []Format{FormatProse},
			check: func(t *testing.T, a analysis) {
				if a.compactBytes != 200 || !a.singleString {
					t.Errorf("compactBytes = %d singleString = %v, want 200/true", a.compactBytes, a.singleString)
				}
			},
		},
		// Branch 2 — chart step 2: prose-dominant payloads unwrap.
		{
			name: "single long string",
			src:  `"` + classifyTestProse(t, 300) + `"`,
			want: []Format{FormatProse},
		},
		{
			name: "dominant prose field with metadata",
			src:  `{"body":"` + classifyTestProse(t, 800) + `","id":"x1"}`,
			want: []Format{FormatProse},
			check: func(t *testing.T, a analysis) {
				if a.proseField != "body" {
					t.Errorf("proseField = %q, want body", a.proseField)
				}
			},
		},
		// Boundary proseShare: 660/1000 = 0.66 qualifies, 660/1001 does not.
		{
			name: "prose share boundary at 0.66",
			src:  `{"body":"` + classifyTestProse(t, 660) + `","meta":"` + x(319) + `"}`,
			want: []Format{FormatProse},
			check: func(t *testing.T, a analysis) {
				if a.compactBytes != 1000 || a.proseFieldShare < proseShare {
					t.Errorf("compactBytes = %d share = %v, want 1000 and >= %v", a.compactBytes, a.proseFieldShare, proseShare)
				}
			},
		},
		{
			name: "prose share boundary below 0.66",
			src:  `{"body":"` + classifyTestProse(t, 660) + `","meta":"` + x(320) + `"}`,
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.proseFieldShare >= proseShare {
					t.Errorf("share = %v, want < %v", a.proseFieldShare, proseShare)
				}
			},
		},
		// Boundary proseMinBytes: a 511-byte field never unwraps, 512 does.
		{
			name: "prose min bytes boundary 511",
			src:  `{"body":"` + classifyTestProse(t, 511) + `"}`,
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.proseFieldBytes != 511 {
					t.Errorf("proseFieldBytes = %d, want 511", a.proseFieldBytes)
				}
			},
		},
		{
			name: "prose min bytes boundary 512",
			src:  `{"body":"` + classifyTestProse(t, 512) + `"}`,
			want: []Format{FormatProse},
		},
		// Branch 3 — chart steps 3+4: uniform array, small → markdown.
		{
			name: "uniform small table markdown",
			src:  classifyTestUniform(t, 10, 500),
			want: []Format{FormatMarkdown},
		},
		// Boundary uniformShare: modal 9/10 = 0.9 is uniform, 8/10 is not.
		{
			name: "uniform share boundary 0.9",
			src:  classifyTestRows(append(classifyTestRepeatRows(rowAB, 9), rowZQ)...),
			want: []Format{FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if !a.uniform || a.modalShare < uniformShare {
					t.Errorf("uniform = %v modalShare = %v, want true and >= %v", a.uniform, a.modalShare, uniformShare)
				}
			},
		},
		{
			name: "uniform share boundary 0.8 falls to JSON",
			src:  classifyTestRows(append(classifyTestRepeatRows(rowAB, 8), rowZQ, rowZQ)...),
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.uniform || a.hetero {
					t.Errorf("uniform = %v hetero = %v, want false/false", a.uniform, a.hetero)
				}
			},
		},
		// Boundary proseCellChars: avg exactly 80 stays tabular, 81 reads prose.
		{
			name: "prose cell boundary avg 80 stays tabular",
			src:  classifyTestRows(classifyTestRepeatRows(`{"text":"`+x(80)+`"}`, 3)...),
			want: []Format{FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if a.proseColumn {
					t.Error("proseColumn = true, want false at avg exactly 80")
				}
			},
		},
		{
			name: "prose cell boundary avg 81 goes record-shaped",
			src:  classifyTestRows(classifyTestRepeatRows(`{"text":"`+x(81)+`"}`, 3)...),
			want: []Format{FormatJSONL, FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if !a.proseColumn {
					t.Error("proseColumn = false, want true at avg 81")
				}
			},
		},
		// Boundary embedded newline: any newline cell forces the prose column arm.
		{
			name: "embedded newline forces prose column",
			src: classifyTestRows(
				`{"id":"1","note":"line one\nline two","pad":"`+x(70)+`"}`,
				`{"id":"2","note":"short","pad":"`+x(70)+`"}`,
			),
			want: []Format{FormatJSONL, FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if a.compactBytes < smallPayloadBytes || !a.proseColumn {
					t.Errorf("compactBytes = %d proseColumn = %v, want >= %d and true", a.compactBytes, a.proseColumn, smallPayloadBytes)
				}
			},
		},
		// Boundary tableTokenPressure: 1999 estimated tokens → markdown, 2000 → CSV shootout.
		{
			name: "token pressure boundary 1999 tokens markdown",
			src:  classifyTestUniform(t, 90, 7999),
			want: []Format{FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if a.estTokens != 1999 {
					t.Errorf("estTokens = %d, want 1999", a.estTokens)
				}
			},
		},
		{
			name: "token pressure boundary 2000 tokens csv",
			src:  classifyTestUniform(t, 90, 8000),
			want: []Format{FormatCSV, FormatTSV},
			check: func(t *testing.T, a analysis) {
				if a.estTokens != 2000 {
					t.Errorf("estTokens = %d, want 2000", a.estTokens)
				}
			},
		},
		// Boundary toonMinRows: TOON joins the pressure shootout at 100 rows, not 99.
		{
			name: "toon rows boundary 99",
			src:  classifyTestUniform(t, 99, 8000),
			want: []Format{FormatCSV, FormatTSV},
		},
		{
			name: "toon rows boundary 100",
			src:  classifyTestUniform(t, 100, 8000),
			want: []Format{FormatCSV, FormatTSV, FormatTOON},
		},
		// Boundary nulls: under token pressure null cells reroute to TOON
		// (chart steps 3+4; CSV cannot distinguish null from empty string).
		{
			name: "nulls under pressure pick toon",
			src:  classifyTestNullPressure(t),
			want: []Format{FormatTOON, FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if !a.hasNulls || a.estTokens < tableTokenPressure {
					t.Errorf("hasNulls = %v estTokens = %d, want true and >= %d", a.hasNulls, a.estTokens, tableTokenPressure)
				}
			},
		},
		{
			name: "nulls in small table still markdown",
			src: classifyTestRows(
				`{"a":1,"b":"`+x(60)+`"}`,
				`{"a":null,"b":"`+x(60)+`"}`,
				`{"a":3,"b":"`+x(60)+`"}`,
			),
			want: []Format{FormatMarkdown},
			check: func(t *testing.T, a analysis) {
				if !a.hasNulls {
					t.Error("hasNulls = false, want true")
				}
			},
		},
		// Branch 4 — chart step 5: repeated nested shapes → TRON.
		{
			name: "repeated nested shapes tron",
			src:  classifyTestRows(classifyTestRepeatRows(`{"u":{"x":1,"y":"`+x(60)+`"}}`, 3)...),
			want: []Format{FormatTRON},
			check: func(t *testing.T, a analysis) {
				if a.maxRepeat != 3 {
					t.Errorf("maxRepeat = %d, want 3", a.maxRepeat)
				}
			},
		},
		// Boundary tronMinRepeat: a shape repeating twice does not amortize.
		{
			name: "tron repeat boundary 2",
			src:  classifyTestRows(classifyTestRepeatRows(`{"u":{"x":1,"y":"`+x(100)+`"}}`, 2)...),
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.maxRepeat != 2 || a.compactBytes < smallPayloadBytes {
					t.Errorf("maxRepeat = %d compactBytes = %d, want 2 and >= %d", a.maxRepeat, a.compactBytes, smallPayloadBytes)
				}
			},
		},
		// Boundary nestedDepthMin: the same repeating shape at depth 1 does not
		// count; wrapped one level deeper it does.
		{
			name: "tron depth boundary 1",
			src:  nested3,
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.maxRepeat != 0 || a.maxDepth != 1 {
					t.Errorf("maxRepeat = %d maxDepth = %d, want 0 and 1", a.maxRepeat, a.maxDepth)
				}
			},
		},
		{
			name: "tron depth boundary 2",
			src:  `{"data":` + nested3 + `}`,
			want: []Format{FormatTRON},
			check: func(t *testing.T, a analysis) {
				if a.maxRepeat != 3 {
					t.Errorf("maxRepeat = %d, want 3", a.maxRepeat)
				}
			},
		},
		// Branch 5 — heterogeneous array → JSONL.
		{
			name: "heterogeneous object shapes",
			src:  classifyTestRows(het("a", "b"), het("a", "b"), het("c", "d"), het("e", "f"), het("g", "h")),
			want: []Format{FormatJSONL},
			check: func(t *testing.T, a analysis) {
				if !a.hetero || a.modalShare >= heteroShare {
					t.Errorf("hetero = %v modalShare = %v, want true and < %v", a.hetero, a.modalShare, heteroShare)
				}
			},
		},
		// Boundary heteroShare: modal exactly 0.5 is not heterogeneous.
		{
			name: "hetero share boundary 0.5 falls to JSON",
			src:  classifyTestRows(het("a", "b"), het("a", "b"), het("c", "d"), het("e", "f")),
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.hetero {
					t.Error("hetero = true, want false at modal share exactly 0.5")
				}
			},
		},
		// Branch 5 — a folded NDJSON-like stream of mixed kinds → JSONL.
		{
			name: "mixed kind stream jsonl",
			src:  classifyTestRows(`{"a":1,"b":2}`, `"`+x(180)+`"`, `123`, `[1,2]`),
			want: []Format{FormatJSONL},
		},
		// Branch 6 — else: deep one-off structure stays minified JSON.
		{
			name: "deep one-off structure json",
			src:  `{"a":{"b":{"c":{"d":"` + x(200) + `"}}}}`,
			want: []Format{FormatJSON},
			check: func(t *testing.T, a analysis) {
				if a.maxDepth != 3 || a.maxRepeat != 0 {
					t.Errorf("maxDepth = %d maxRepeat = %d, want 3 and 0", a.maxDepth, a.maxRepeat)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok, err := decodeAll([]byte(tt.src))
			if err != nil || !ok {
				t.Fatalf("decodeAll: ok=%v err=%v", ok, err)
			}
			got, a := classify(v)
			if !slices.Equal(got, tt.want) {
				t.Errorf("classify() = %v, want %v", got, tt.want)
			}
			if tt.check != nil {
				tt.check(t, a)
			}
		})
	}
}
