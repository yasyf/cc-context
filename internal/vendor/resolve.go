package vendor

import (
	"context"
	"os/exec"
)

// LookPath wraps exec.LookPath, returning "" when the binary is absent. It is a
// package var so tests can stub PATH resolution.
var LookPath = func(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Resolve returns the path to a tool's binary, preferring (in order) the
// configured bin, a system install on PATH, then the pinned runtime download. The
// PATH lookup is keyed on t.Name (e.g. "ast-grep", never "sg"), so a Homebrew or
// hand-installed binary wins before any download.
func Resolve(ctx context.Context, t Tool, configuredBin string) (string, error) {
	if configuredBin != "" {
		return configuredBin, nil
	}
	if p := LookPath(t.Name); p != "" {
		return p, nil
	}
	return Ensure(ctx, t)
}
