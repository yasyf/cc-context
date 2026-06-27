// Package toon converts JSON and NDJSON to TOON (Token-Oriented Object Notation),
// emitting whichever of TOON or compact JSON is smaller, and runs commands whose
// JSON stdout is converted in place.
package toon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os/exec"
	"strconv"
	"strings"

	"github.com/toon-format/toon-go"
)

// Delimiter is the character separating values inside TOON array scopes.
type Delimiter = toon.Delimiter

// The supported array delimiters.
const (
	DelimiterComma = toon.DelimiterComma
	DelimiterTab   = toon.DelimiterTab
	DelimiterPipe  = toon.DelimiterPipe
)

// Options tunes a Convert or Run call.
type Options struct {
	Indent    int
	Delimiter Delimiter
	ForceTOON bool
	Strict    bool
}

// Convert decodes JSON or NDJSON from src and returns TOON, or compact JSON when
// that is smaller (ForceTOON always returns TOON); converted reports whether a
// JSON re-encoding happened. A decode failure or empty src returns src verbatim
// with converted=false — unless opts.Strict, which returns the error. The
// passthrough is a deliberate exception to the no-defensive-coding rule: the
// wrapper must never corrupt non-JSON output.
func Convert(src []byte, opts Options) (out string, converted bool, err error) {
	model, ok, derr := decodeAll(src)
	if derr != nil {
		if opts.Strict {
			return "", false, fmt.Errorf("decode json: %w", derr)
		}
		return string(src), false, nil
	}
	if !ok {
		return string(src), false, nil
	}

	toonOut, err := toon.MarshalString(model,
		toon.WithIndent(opts.Indent),
		toon.WithArrayDelimiter(opts.Delimiter),
		toon.WithDocumentDelimiter(opts.Delimiter),
	)
	if err != nil {
		return "", false, fmt.Errorf("marshal toon: %w", err)
	}
	if opts.ForceTOON {
		return toonOut, true, nil
	}

	jsonOut := compactJSON(model)
	if len(jsonOut) < len(toonOut) {
		return jsonOut, true, nil
	}
	return toonOut, true, nil
}

// Run executes argv, capturing stdout and converting it via Convert; stderr is
// written to errOut and stdin is forwarded from in. It returns the converted
// stdout, whether a JSON re-encoding happened, and the child's exit code. A
// non-zero child exit is reported through code with a nil error (the command
// ran, it just failed); a spawn failure (binary not found, context cancelled) is
// returned as err.
func Run(ctx context.Context, argv []string, opts Options, in io.Reader, errOut io.Writer) (out string, converted bool, code int, err error) {
	if len(argv) == 0 {
		return "", false, 0, errors.New("run: empty argv")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is the wrapped command supplied by the caller
	var stdout bytes.Buffer
	cmd.Stdin = in
	cmd.Stdout = &stdout
	cmd.Stderr = errOut

	runErr := cmd.Run()

	out, converted, cerr := Convert(stdout.Bytes(), opts)
	if cerr != nil {
		return "", false, 0, fmt.Errorf("convert stdout: %w", cerr)
	}

	if runErr == nil {
		return out, converted, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return out, converted, exitErr.ExitCode(), nil
	}
	return "", false, 0, fmt.Errorf("run %s: %w", argv[0], runErr)
}

// decodeAll decodes every top-level JSON value in src into toon-go's ordered
// model. Zero values (empty or whitespace) yields ok=false; one value is used
// as-is; two or more (NDJSON) fold into a single []any so a uniform object
// stream becomes one table.
func decodeAll(src []byte) (model any, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()

	var vals []any
	for {
		var raw json.RawMessage
		if derr := dec.Decode(&raw); derr != nil {
			if errors.Is(derr, io.EOF) {
				break
			}
			return nil, false, derr
		}
		v, derr := decodeValue(raw)
		if derr != nil {
			return nil, false, derr
		}
		vals = append(vals, v)
	}

	switch len(vals) {
	case 0:
		return nil, false, nil
	case 1:
		return vals[0], true, nil
	default:
		return vals, true, nil
	}
}

// decodeValue decodes a single JSON value into the ordered model: object →
// toon.Object (fields in source order), array → []any, number → json.Number,
// and string/bool/null to their Go scalars.
func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return decodeFromToken(dec)
}

// decodeFromToken reads the next value from dec, recursing into objects and
// arrays so field order is preserved.
func decodeFromToken(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}

	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeObject(dec)
		case '[':
			return decodeArray(dec)
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", t)
		}
	case json.Number:
		return numberScalar(t), nil
	default:
		return tok, nil
	}
}

// numberScalar routes an integer-valued json.Number to a native Go integer so
// toon-go's precision-preserving integer path emits it exactly: int64 when it
// fits, *big.Int when it does not. (toon-go's json.Number path coerces through
// float64 and loses precision past 2^53.) Non-integers stay json.Number, which
// toon-go canonicalizes correctly via strconv.FormatFloat.
func numberScalar(n json.Number) any {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if bi, ok := new(big.Int).SetString(s, 10); ok {
		return bi
	}
	return n
}

// decodeObject reads fields until the closing brace, preserving source order.
func decodeObject(dec *json.Decoder) (toon.Object, error) {
	var fields []toon.Field
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return toon.Object{}, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return toon.Object{}, fmt.Errorf("object key not a string: %v", keyTok)
		}
		val, err := decodeFromToken(dec)
		if err != nil {
			return toon.Object{}, err
		}
		fields = append(fields, toon.Field{Key: key, Value: val})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return toon.Object{}, err
	}
	return toon.NewObject(fields...), nil
}

// decodeArray reads elements until the closing bracket.
func decodeArray(dec *json.Decoder) ([]any, error) {
	elems := []any{}
	for dec.More() {
		val, err := decodeFromToken(dec)
		if err != nil {
			return nil, err
		}
		elems = append(elems, val)
	}
	if _, err := dec.Token(); err != nil { // consume ']'
		return nil, err
	}
	return elems, nil
}

// compactJSON serializes the ordered model to minimal JSON. It walks the same
// model Convert encodes to TOON so the two describe the identical value (and the
// byte-length comparison is honest); json.Marshal would re-sort object keys.
func compactJSON(model any) string {
	var b strings.Builder
	writeCompact(&b, model)
	return b.String()
}

func writeCompact(b *strings.Builder, v any) {
	switch t := v.(type) {
	case toon.Object:
		b.WriteByte('{')
		for i, f := range t.Fields {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCompactString(b, f.Key)
			b.WriteByte(':')
			writeCompact(b, f.Value)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCompact(b, e)
		}
		b.WriteByte(']')
	case json.Number:
		b.WriteString(t.String())
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case *big.Int:
		b.WriteString(t.String())
	case string:
		writeCompactString(b, t)
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	default:
		panic(fmt.Sprintf("compactJSON: unexpected type %T", v))
	}
}

// writeCompactString emits a JSON string with the standard escaping.
func writeCompactString(b *strings.Builder, s string) {
	enc, _ := json.Marshal(s)
	b.Write(enc)
}
