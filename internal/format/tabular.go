package format

import (
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/toon-format/toon-go"
)

// encodeCSV renders a []any of same-keyed toon.Object rows as RFC 4180 CSV via
// encoding/csv: header from the first row's key order, one record per object,
// nil cells as empty fields, non-string scalars through writeScalar.
func encodeCSV(v any) (string, error) {
	return csvEncode("csv", ',', v)
}

// encodeTSV renders the same tabular shape as encodeCSV with a tab delimiter.
// encoding/csv RFC-quotes any field containing a tab, quote, or newline —
// strict tab-only TSV consumers must tolerate that quoting.
func encodeTSV(v any) (string, error) {
	return csvEncode("tsv", '\t', v)
}

func csvEncode(name string, comma rune, v any) (string, error) {
	header, rows, err := csvTable(name, v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Comma = comma
	if err := w.WriteAll(append([][]string{header}, rows...)); err != nil {
		return "", fmt.Errorf("encode %s: %w", name, err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// encodeMarkdown renders the same tabular shape as encodeCSV as a compact
// GitHub-style markdown table: |a|b| header, |---|---| separator, one row per
// object. Cells stay single-line and content-preserving: backslashes escape
// to \\, pipes to \|, and embedded newlines (\r\n, \n, \r) become <br>; nil
// is an empty cell.
func encodeMarkdown(v any) (string, error) {
	header, rows, err := csvTable("markdown", v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	mdRow(&b, header)
	b.WriteString("\n|")
	for range header {
		b.WriteString("---|")
	}
	for _, row := range rows {
		b.WriteByte('\n')
		mdRow(&b, row)
	}
	return b.String(), nil
}

func mdRow(b *strings.Builder, cells []string) {
	b.WriteByte('|')
	for _, c := range cells {
		b.WriteString(mdCell(c))
		b.WriteByte('|')
	}
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return strings.ReplaceAll(s, "\r", "<br>")
}

// csvTable validates the tabular IR shape shared by the CSV, TSV, and
// markdown encoders and renders it to a header plus cell strings: v must be a
// non-empty []any of toon.Object rows over one non-empty, duplicate-free key
// set, scalar cells only. The header is the first row's key order; later rows
// may reorder keys but not add or drop them. A duplicate key errors — one
// cell per header slot cannot carry two values.
func csvTable(name string, v any) (header []string, rows [][]string, err error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("encode %s: %T is not an array of objects", name, v)
	}
	if len(arr) == 0 {
		return nil, nil, fmt.Errorf("encode %s: empty array has no header", name)
	}
	first, ok := arr[0].(toon.Object)
	if !ok {
		return nil, nil, fmt.Errorf("encode %s: row 0 is %T, not an object", name, arr[0])
	}
	if len(first.Fields) == 0 {
		return nil, nil, fmt.Errorf("encode %s: row 0 has no keys", name)
	}
	header = make([]string, len(first.Fields))
	for i, f := range first.Fields {
		header[i] = f.Key
	}

	rows = make([][]string, len(arr))
	for i, e := range arr {
		obj, ok := e.(toon.Object)
		if !ok {
			return nil, nil, fmt.Errorf("encode %s: row %d is %T, not an object", name, i, e)
		}
		if len(obj.Fields) != len(header) {
			return nil, nil, fmt.Errorf("encode %s: row %d has %d keys, header has %d", name, i, len(obj.Fields), len(header))
		}
		cells := make(map[string]any, len(obj.Fields))
		for _, f := range obj.Fields {
			if _, dup := cells[f.Key]; dup {
				return nil, nil, fmt.Errorf("encode %s: row %d duplicates key %q", name, i, f.Key)
			}
			cells[f.Key] = f.Value
		}
		row := make([]string, len(header))
		for j, key := range header {
			cell, present := cells[key]
			if !present {
				return nil, nil, fmt.Errorf("encode %s: row %d is missing key %q", name, i, key)
			}
			s, ok := csvCell(cell)
			if !ok {
				return nil, nil, fmt.Errorf("encode %s: row %d key %q holds %T, not a scalar", name, i, key, cell)
			}
			row[j] = s
		}
		rows[i] = row
	}
	return header, rows, nil
}

// csvCell renders one scalar cell as raw text: nil empties, strings pass
// through verbatim, every other scalar goes through writeScalar so integer
// precision survives. ok=false flags a non-scalar cell.
func csvCell(v any) (s string, ok bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, true
	case toon.Object, []any:
		return "", false
	default:
		var b strings.Builder
		writeScalar(&b, t)
		return b.String(), true
	}
}
