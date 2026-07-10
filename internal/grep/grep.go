// Package grep orchestrates the CLI grep op on the tilth engine, pre-flighting the scope and normalizing tilth's no-match path-fallback into the house no-match output.
package grep

import (
	"context"
	"fmt"
	"os"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/grok"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

// Run executes the tilth grep invocation, pre-flighting a non-empty scope so a
// typo fails loudly, and normalizing tilth's no-match path-fallback — a "not
// found:" error byte-identical to its nonexistent-scope error — into the house
// no-match output. Any other error propagates.
func Run(ctx context.Context, bin string, argv []string, a backend.Args) (string, error) {
	if a.Scope != "" {
		if _, err := os.Stat(a.Scope); err != nil {
			return "", fmt.Errorf("grep: scope %q does not exist: %w", a.Scope, err)
		}
	}
	out, err := render.RunCLI(ctx, bin, argv)
	switch {
	case err == nil:
		return render.Finalize(backend.OpGrep, out, a.Budget)
	case grok.IsNotFoundText(err.Error()):
		return render.Finalize(backend.OpGrep, ripgrep.NoMatch(a.Query), a.Budget)
	default:
		return "", err
	}
}
