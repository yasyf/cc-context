// Package render runs backend invocations and shapes their output to a budget.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
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
	if budgetTokens <= 0 {
		return s
	}
	limit := budgetTokens * charsPerToken
	if len(s) <= limit {
		return s
	}

	cut := strings.LastIndexByte(s[:limit], '\n')
	if cut < 0 {
		cut = limit
	}
	kept, omitted := s[:cut], s[cut:]
	omittedLines := strings.Count(strings.Trim(omitted, "\n"), "\n") + 1
	omittedTokens := len(omitted) / charsPerToken

	return fmt.Sprintf(
		"%s\n… +%d lines, ~%d tokens omitted — re-run with a larger --budget\n",
		kept, omittedLines, omittedTokens,
	)
}
