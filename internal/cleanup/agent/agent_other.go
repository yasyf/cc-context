//go:build !darwin

package agent

import (
	"context"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Connect reports cleanup.ErrUnsupported: the daemon runs on macOS only, and
// removal elsewhere stays inline.
func Connect(context.Context, Options) (cleanup.Control, error) {
	return nil, cleanup.ErrUnsupported
}
