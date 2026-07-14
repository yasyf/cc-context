package cli

import (
	"errors"
	"strconv"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/web"
)

// ErrNotFound reports that a lookup resolved to nothing; ExitCode maps it to 3.
var ErrNotFound = errors.New("not found")

// ExitError carries a process exit code up to main without a printable message;
// it lets the format wrapper propagate a child command's exit code intact.
type ExitError struct{ Code int }

// Error reports the exit code; the wrapper has already mirrored the child's
// stderr, so the message stays minimal.
func (e *ExitError) Error() string { return "exit code " + strconv.Itoa(e.Code) }

// ExitCode maps err to a process exit code: 0 for nil, an *ExitError's Code, 3
// for a wrapped ErrNotFound, backend.ErrPathNotFound, or web.ErrGone (the
// not-found category), and 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, backend.ErrPathNotFound) || errors.Is(err, web.ErrGone) {
		return 3
	}
	return 1
}
