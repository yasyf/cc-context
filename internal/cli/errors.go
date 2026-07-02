package cli

import (
	"errors"
	"strconv"
)

// ErrNotFound reports that a lookup resolved to nothing; ExitCode maps it to 3.
var ErrNotFound = errors.New("not found")

// ExitError carries a process exit code up to main without a printable message;
// it lets the toon wrapper propagate a child command's exit code intact.
type ExitError struct{ Code int }

// Error reports the exit code; the wrapper has already mirrored the child's
// stderr, so the message stays minimal.
func (e *ExitError) Error() string { return "exit code " + strconv.Itoa(e.Code) }

// ExitCode maps err to a process exit code: 0 for nil, an *ExitError's Code, 3
// for a wrapped ErrNotFound, and 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if errors.Is(err, ErrNotFound) {
		return 3
	}
	return 1
}
