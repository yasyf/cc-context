package cli

import (
	"context"

	"github.com/yasyf/cc-context/internal/workspace"
)

// workingDir returns the project root ctx carries, falling back to the process
// working directory when ctx declares none, or "." when that cannot be read.
func workingDir(ctx context.Context) string {
	if dir, err := workspace.RootFrom(ctx); err == nil {
		return dir
	}
	return "."
}
