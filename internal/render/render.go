// Package render runs backend invocations and shapes their output to a budget.
package render

import (
	"bytes"
	"context"
	"fmt"
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
