package ripgrep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

const (
	searchStdoutLimit = 16 << 20
	searchStderrLimit = 64 << 10
)

type searchOutputWriter struct {
	buffer bytes.Buffer
	limit  int
	stream string
	cancel context.CancelFunc
	err    error
}

func execSearch(ctx context.Context, dir render.Dir, bin string, argv []string) (string, error) {
	return execSearchBounded(ctx, dir, bin, argv, searchStdoutLimit, searchStderrLimit)
}

func execSearchBounded(ctx context.Context, dir render.Dir, bin string, argv []string, stdoutLimit, stderrLimit int) (string, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := searchOutputWriter{limit: stdoutLimit, stream: "stdout", cancel: cancel}
	stderr := searchOutputWriter{limit: stderrLimit, stream: "stderr", cancel: cancel}
	err := render.RunCLIStreamSplitEnv(runCtx, dir, bin, argv, &stdout, &stderr, nil)
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s: %w", bin, ctx.Err())
	}
	if overflow := errors.Join(stdout.err, stderr.err); overflow != nil {
		return "", fmt.Errorf("%s: %w", bin, overflow)
	}
	if err == nil {
		return stdout.buffer.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == exitNoMatch && stderr.buffer.Len() == 0 {
		return stdout.buffer.String(), nil
	}
	return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.buffer.String()))
}

func (w *searchOutputWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		w.err = fmt.Errorf("grep %s exceeded %d bytes; no partial results returned; narrow the search with a subpath operand or --glob", w.stream, w.limit)
		w.cancel()
		return 0, w.err
	}
	return w.buffer.Write(p)
}
