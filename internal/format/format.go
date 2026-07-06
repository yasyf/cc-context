// Package format converts JSON and NDJSON tool output into token-lean
// encodings — TOON, TRON, CSV/TSV, markdown tables, JSONL, prose unwrap, or
// compact JSON — and runs commands whose JSON stdout is converted in place.
// FormatAuto classifies the payload's shape and emits its preferred candidate
// encoding — the earliest within candidateTolerance of the leanest — never
// exceeding compact JSON by bytes.
package format

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Format names an output encoding.
type Format string

// The supported output formats. FormatAuto classifies the payload and picks
// the earliest candidate encoding within candidateTolerance of the smallest
// that passes the byte-net invariant; every other constant forces its
// encoder, emitting even when larger.
const (
	FormatAuto     Format = "auto"
	FormatTOON     Format = "toon"
	FormatTRON     Format = "tron"
	FormatCSV      Format = "csv"
	FormatTSV      Format = "tsv"
	FormatMarkdown Format = "markdown"
	FormatJSONL    Format = "jsonl"
	FormatProse    Format = "prose"
	FormatJSON     Format = "json"
)

// # Encoder contract
//
// Later phases add one encoder per file — tron.go, tabular.go (CSV/TSV +
// markdown), jsonl.go, prose.go — plus classify.go, each written against this
// contract alone: five agents must be able to implement the five files
// without coordinating.
//
// ## The IR
//
// Every encoder consumes the value produced by decodeAll (decode.go), never
// raw JSON, and nothing re-decodes. The IR value is exactly one of:
//
//   - toon.Object (github.com/toon-format/toon-go) — an object with source
//     key order preserved; iterate o.Fields ([]toon.Field{Key string;
//     Value any}) in order. Objects are NEVER map[string]any.
//   - []any — an array. An NDJSON payload of two or more top-level documents
//     arrives pre-folded into a single []any of those documents (a lone
//     document arrives as itself, unwrapped); encoders cannot and must not
//     distinguish a folded stream from a literal top-level array.
//   - Scalars: int64 (integers that fit), *big.Int (integers that do not),
//     json.Number (non-integer numbers, verbatim decimal text), string,
//     bool, and untyped nil. float64 NEVER appears in the IR — routing any
//     number through float64 corrupts integers beyond 2^53.
//
// No other type ever occurs; panic on anything else (writeScalar already
// does).
//
// ## Encoder functions
//
// Each encoder is one unexported function with this exact shape:
//
//	func encodeTRON(v any) (string, error)
//	func encodeCSV(v any) (string, error)
//	func encodeTSV(v any) (string, error)
//	func encodeMarkdown(v any) (string, error)
//	func encodeJSONL(v any) (string, error)
//	func encodeProse(v any) (string, error)
//
// v is the IR root. Only encodeTOON deviates — encodeTOON(v any, opts
// Options) — because Indent and Delimiter are TOON-only knobs. The returned
// string carries no trailing newline (trim what your library appends;
// encoding/csv's Writer adds one). An encoder that cannot represent v (e.g.
// CSV on a non-tabular shape) returns a descriptive error prefixed with its
// name ("encode csv: …"); it never falls back to another format — the encode
// dispatch below owns fallback policy. Wire-up is one arm each: the
// integration phase replaces each format's not-implemented arm in encode with
// a single call to its encoder — encoder files themselves never edit this
// file.
//
// ## Scalar rendering
//
// Every scalar emitted in a JSON-quoted position goes through writeScalar
// (json.go): strings JSON-escaped WITHOUT HTML escaping (<, >, & stay raw),
// int64/*big.Int/json.Number as verbatim decimal text, bool as true/false,
// nil as null, panic on any other type. Encoders whose output positions take
// raw unquoted text — CSV/TSV cells, markdown cells, prose bodies — handle
// the string case themselves and route every non-string scalar through
// writeScalar into a strings.Builder so integer precision survives.
//
// ## Naming
//
// All files share package format, so every unexported helper in an encoder
// file carries its encoder's prefix: tronX in tron.go, csvX in tabular.go
// (mdX for its markdown half), jsonlX in jsonl.go, proseX in prose.go.
//
// ## classify
//
// classify.go provides analyze(v any) analysis and classify(v any)
// ([]Format, analysis) — candidate formats in priority order for the
// FormatAuto arm, which encodes each candidate and keeps the byte-net
// invariant len(chosen) <= len(compactJSON(v)); compact JSON is always the
// implicit last contender.

// Options tunes a Convert or Run call. Indent and Delimiter apply only to the
// TOON encoder.
type Options struct {
	Format    Format
	Indent    int
	Delimiter Delimiter
	Strict    bool
}

// Convert decodes JSON or NDJSON from src and re-encodes it per opts.Format:
// FormatAuto classifies the shape and emits its preferred candidate encoding
// — the earliest within candidateTolerance of the smallest — never exceeding
// compact JSON by bytes; an explicit format always emits
// its encoding, even when larger, and errors loudly on an incompatible shape;
// converted reports whether a re-encoding happened. A decode failure or empty
// src returns src verbatim
// with converted=false — unless opts.Strict, which returns the error. The
// passthrough is a deliberate exception to the no-defensive-coding rule: the
// wrapper must never corrupt non-JSON output.
func Convert(src []byte, opts Options) (out string, converted bool, err error) {
	v, ok, derr := decodeAll(src)
	if derr != nil {
		if opts.Strict {
			return "", false, fmt.Errorf("decode json: %w", derr)
		}
		return string(src), false, nil
	}
	if !ok {
		return string(src), false, nil
	}

	out, err = encode(v, opts)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// ParseFormat resolves a format name, defaulting empty to FormatAuto.
func ParseFormat(name string) (Format, error) {
	if name == "" {
		return FormatAuto, nil
	}
	switch f := Format(name); f {
	case FormatAuto, FormatTOON, FormatTRON, FormatCSV, FormatTSV, FormatMarkdown, FormatJSONL, FormatProse, FormatJSON:
		return f, nil
	default:
		return "", fmt.Errorf("invalid format %q: want auto|toon|tron|csv|tsv|markdown|jsonl|prose|json", name)
	}
}

// encode renders the IR in the requested format. An explicit format emits even
// when larger than compact JSON and errors loudly on an incompatible shape;
// FormatAuto owns fallback policy.
func encode(v any, opts Options) (string, error) {
	switch opts.Format {
	case FormatAuto:
		return encodeAuto(v, opts), nil
	case FormatTOON:
		return encodeTOON(v, opts)
	case FormatTRON:
		return encodeTRON(v)
	case FormatCSV:
		return encodeCSV(v)
	case FormatTSV:
		return encodeTSV(v)
	case FormatMarkdown:
		return encodeMarkdown(v)
	case FormatJSONL:
		return encodeJSONL(v)
	case FormatProse:
		return encodeProse(v)
	case FormatJSON:
		return compactJSON(v), nil
	default:
		return "", fmt.Errorf("unknown format %q", opts.Format)
	}
}

// candidateTolerance is the relative size slack within which classifier order
// outranks a byte win: an earlier candidate beats a smaller later one unless
// the later one is more than 5% smaller.
const candidateTolerance = 0.05 // heuristic

// encodeAuto classifies v and returns the earliest candidate encoding whose
// size is within candidateTolerance of the smallest, among candidates that
// pass the byte-net invariant len(out) <= len(compactJSON(v)) — classifier
// order is a preference ranking, so a near-tie goes to the preferred format.
// Candidates that error or exceed the net are skipped, and compact JSON is
// the implicit last contender, so auto mode never fails.
func encodeAuto(v any, opts Options) string {
	candidates, _ := classify(v)
	jsonOut := compactJSON(v)
	var outs []string
	minLen := 0
	for _, f := range candidates {
		o := opts
		o.Format = f
		out, err := encode(v, o)
		if err != nil || len(out) > len(jsonOut) {
			continue
		}
		if len(outs) == 0 || len(out) < minLen {
			minLen = len(out)
		}
		outs = append(outs, out)
	}
	for _, out := range outs {
		if float64(len(out)) <= (1+candidateTolerance)*float64(minLen) {
			return out
		}
	}
	return jsonOut
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
