// Package format converts JSON and NDJSON tool output into token-lean
// encodings — TOON, TRON, CSV/TSV, markdown tables, JSONL, prose unwrap, or
// compact JSON — and runs commands whose JSON stdout is converted in place.
// The conversion engine is format-core compiled to WASM (internal/format/engine.go);
// this file holds the exported Go surface and the strict/passthrough policy.
// FormatAuto classifies the payload's shape and emits its preferred candidate
// encoding — the earliest within tolerance of the leanest — never exceeding
// compact JSON by bytes.
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

// The supported output formats. FormatAuto classifies the payload and picks the
// leanest encoding that passes the byte-net invariant; every other constant
// forces its encoder, emitting even when larger.
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

// Delimiter is the character separating values inside TOON array scopes.
type Delimiter uint8

// The supported array delimiters.
const (
	DelimiterComma Delimiter = iota
	DelimiterTab
	DelimiterPipe
)

// char renders the delimiter as the literal the WASM request envelope expects.
func (d Delimiter) char() string {
	switch d {
	case DelimiterComma:
		return ","
	case DelimiterTab:
		return "\t"
	case DelimiterPipe:
		return "|"
	default:
		panic(fmt.Sprintf("format: unknown delimiter %d", d))
	}
}

// Options tunes a Convert or Run call. Indent and Delimiter apply only to the
// TOON encoder.
type Options struct {
	Format    Format
	Indent    int
	Delimiter Delimiter
	Strict    bool
}

// Convert decodes JSON or NDJSON from src and re-encodes it per opts.Format:
// FormatAuto classifies the shape and emits its preferred candidate encoding —
// the earliest within tolerance of the smallest — never exceeding compact JSON
// by bytes; an explicit format always emits its encoding, even when larger, and
// errors loudly on an incompatible shape; converted reports whether a
// re-encoding happened. A decode failure or empty src returns src verbatim with
// converted=false — unless opts.Strict, which returns the error. The passthrough
// is a deliberate exception to the no-defensive-coding rule: the wrapper must
// never corrupt non-JSON output.
func Convert(src []byte, opts Options) (out string, converted bool, err error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return string(src), false, nil
	}

	res, err := runEngine(src, opts)
	if err != nil {
		if errors.Is(err, errEngineUnavailable) {
			return "", false, err
		}
		return convertHostError(src, opts, err)
	}

	switch res.errKind {
	case "":
		return res.text, true, nil
	case "not_json":
		if opts.Strict {
			return "", false, errors.New(res.errMsg)
		}
		return string(src), false, nil
	default:
		return "", false, errors.New(res.errMsg)
	}
}

// convertHostError applies the strict/passthrough policy to a WASM host failure
// (trap/timeout/limit): auto mode passes src through untouched, strict or forced
// mode surfaces the error.
func convertHostError(src []byte, opts Options, err error) (string, bool, error) {
	if opts.Format == FormatAuto && !opts.Strict {
		return string(src), false, nil
	}
	return "", false, fmt.Errorf("format engine: %w", err)
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

	out, converted, cerr := Convert(stdout.Bytes(), opts) //nolint:contextcheck // the engine pins its own init/call deadlines, deliberately decoupled from caller cancellation (see loadEngine)
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
