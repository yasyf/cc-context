package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"exit error", &ExitError{Code: 3}, 3},
		{"exit error zero", &ExitError{Code: 0}, 0},
		{"plain error", errors.New("boom"), 1},
		{"wrapped exit error", fmt.Errorf("ran: %w", &ExitError{Code: 7}), 7},
		{"not found", ErrNotFound, 3},
		{"wrapped not found", fmt.Errorf("locate %q: %w", "nope", ErrNotFound), 3},
		{"path not found", backend.ErrPathNotFound, 3},
		{"wrapped path not found", fmt.Errorf("read: %w", backend.ErrPathNotFound), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.want {
				t.Errorf("ExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExitErrorMessage(t *testing.T) {
	if got := (&ExitError{Code: 5}).Error(); got != "exit code 5" {
		t.Errorf("Error() = %q, want %q", got, "exit code 5")
	}
}
