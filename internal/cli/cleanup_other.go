//go:build !darwin

package cli

import (
	"context"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const cleanupDaemonized = false

func reachCleanup(context.Context) (cleanup.Service, error) {
	return nil, cleanup.ErrUnsupported
}

func previewCleanup(context.Context, cleanup.Request) (cleanup.Job, error) {
	return cleanup.Job{}, cleanup.ErrUnsupported
}

func runCleanupServe(context.Context) error {
	return cleanup.ErrUnsupported
}
