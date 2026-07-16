package vendor

import (
	"context"

	"github.com/yasyf/cc-context/internal/lookpath"
)

// Resolve returns the path to a tool's binary, preferring (in order) the
// configured bin, a system install on PATH, then the pinned runtime download. The
// PATH lookup is keyed on t.Name (e.g. "tilth"), so a Homebrew or hand-installed
// binary wins before any download.
func Resolve(ctx context.Context, t Tool, configuredBin string) (string, error) {
	if configuredBin != "" {
		return configuredBin, nil
	}
	if p := lookpath.Find(t.Name); p != "" {
		return p, nil
	}
	return Ensure(ctx, t)
}
