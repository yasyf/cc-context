// Package render runs backend invocations and shapes their output to a budget.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// charsPerToken is the crude chars-per-token ratio used to estimate budgets.
const charsPerToken = 4

// RunCLI executes bin with argv, returning its stdout. A nonzero exit wraps the
// child's stderr in the returned error.
func RunCLI(ctx context.Context, bin string, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunCLIEnv is RunCLI with extraEnv appended to the process environment, for
// callers that must set an env-only variable the flag surface cannot express
// (e.g. GIT_INDEX_FILE). extraEnv extends os.Environ(), so a "KEY=value" element
// overrides any inherited KEY per exec's last-wins rule.
func RunCLIEnv(ctx context.Context, bin string, argv, extraEnv []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunCLIStdin is RunCLI with stdin fed from the given bytes, for a command that
// reads its payload from stdin (e.g. git hash-object --stdin).
func RunCLIStdin(ctx context.Context, bin string, argv []string, stdin []byte) (string, error) {
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// RunCLIStream executes bin with argv, wiring the child's stdout and stderr to w
// as they are produced. It does not buffer output; the returned error carries the
// exit status only (any stderr already flowed to w).
func RunCLIStream(ctx context.Context, bin string, argv []string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", bin, err)
	}
	return nil
}

// RunCLIAllowExit is RunCLI but tolerates the listed nonzero exit codes: when the
// child exits with one of okCodes and writes nothing to stderr, its stdout is
// returned without error (the caller interprets an empty stdout — e.g. ast-grep
// `run` exits 1 with empty output on a clean no-match). A tolerated exit that
// still wrote to stderr is treated as a real failure and wrapped, as is any
// non-listed nonzero exit. The exit code is read from the process error via
// errors.As, never by string-matching.
func RunCLIAllowExit(ctx context.Context, bin string, argv []string, okCodes ...int) (string, error) {
	cmd := exec.CommandContext(ctx, bin, argv...) //nolint:gosec // bin/argv come from trusted backend translation, not user free-text
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && tolerated(exitErr.ExitCode(), okCodes) && stderr.Len() == 0 {
		return stdout.String(), nil
	}
	return "", fmt.Errorf("%s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
}

// tolerated reports whether code is one of the allowed exit codes.
func tolerated(code int, okCodes []int) bool {
	for _, ok := range okCodes {
		if code == ok {
			return true
		}
	}
	return false
}

// Cap trims s to budgetTokens (estimated as len/4). When it trims, it cuts at a
// line boundary and appends an explicit footer naming the omitted volume; it
// never silently truncates. A non-positive budget returns s unchanged.
func Cap(s string, budgetTokens int) string {
	kept, omitted, trimmed := capTrim(s, budgetTokens)
	if !trimmed {
		return kept
	}
	omittedLines := strings.Count(strings.Trim(omitted, "\n"), "\n") + 1
	omittedTokens := len(omitted) / charsPerToken

	return fmt.Sprintf(
		"%s\n… +%d lines, ~%d tokens omitted — re-run with a larger --budget\n",
		kept, omittedLines, omittedTokens,
	)
}

// CapContinuation serves a fixed-stride page of span for a paged web read: the
// byte window [offset*charsPerToken, (offset+budget)*charsPerToken), each bound
// snapped backward to a UTF-8 rune start so page N's end equals page N+1's start
// and consecutive pages join exactly. A window that stops short of span's end
// appends a footer naming the next --offset (offset+budget); a non-positive budget
// or a window reaching the end serves to span's end with no footer. An empty span
// serves empty; otherwise offset must be a valid page start
// (offset*charsPerToken < len(span)), which serveSpan enforces.
func CapContinuation(span string, offset, budget int) string {
	if span == "" {
		return span
	}
	startRaw := offset * charsPerToken
	start := snapRuneStart(span, startRaw)
	// Divide, don't multiply: budget can be MaxInt and budget*charsPerToken overflow.
	if budget <= 0 || budget > (len(span)-startRaw-1)/charsPerToken {
		return span[start:]
	}
	end := snapRuneStart(span, startRaw+budget*charsPerToken)
	remainder := span[end:]
	omittedLines := strings.Count(remainder, "\n")
	if !strings.HasSuffix(remainder, "\n") {
		omittedLines++ // count the unterminated final line
	}
	omittedTokens := len(remainder) / charsPerToken
	next := offset + budget

	return fmt.Sprintf(
		"%s\n… +%d lines, ~%d tokens omitted — re-run with --offset %d to continue, or a larger --budget\n",
		span[start:end], omittedLines, omittedTokens, next,
	)
}

// snapRuneStart moves i backward to the first byte of the UTF-8 rune it lands in so
// a stride boundary never splits a multi-byte rune, bounding the walk-back to
// utf8.UTFMax-1 bytes so a run of malformed continuation bytes cannot drag the cut
// arbitrarily far — beyond the bound, i is returned unchanged and invalid UTF-8 is
// split as-is. A pure function of (s, i): page N's snapped end equals page N+1's
// snapped start, so consecutive pages still join exactly. i must index into s.
func snapRuneStart(s string, i int) int {
	for j := i; j >= 0 && i-j < utf8.UTFMax; j-- {
		if utf8.RuneStart(s[j]) {
			return j
		}
	}
	return i
}

// capTrim splits s at the last line boundary within budgetTokens, returning the
// kept prefix, the omitted suffix, and whether a trim was needed. A non-positive
// budget or an s already within budget returns s whole with trimmed false.
func capTrim(s string, budgetTokens int) (kept, omitted string, trimmed bool) {
	if budgetTokens <= 0 {
		return s, "", false
	}
	limit := budgetTokens * charsPerToken
	// Guard the multiply: a math.MaxInt64 budget wraps negative, so an overflow (or
	// any budget wide enough to hold s) keeps everything uncut.
	if limit/charsPerToken != budgetTokens || len(s) <= limit {
		return s, "", false
	}
	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut < 0 {
		cut = limit
	}
	return s[:cut], s[cut:], true
}
