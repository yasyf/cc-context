// Package grok orchestrates the symbol op, normalizing tilth's "not found" miss and falling back to an ast-grep type lookup.
package grok

import (
	"context"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
)

// notFoundSentinel is the free-text marker tilth prints (both surfaces) when grok resolves no symbol.
const notFoundSentinel = "not found:"

// Run executes the tilth grok invocation, falling back to an ast-grep type lookup on a "not found" miss, and caps to budget.
func Run(ctx context.Context, bin string, argv []string, a backend.Args) (string, error) {
	out, err := render.RunCLI(ctx, bin, argv)
	switch {
	case err == nil && !strings.Contains(out, notFoundSentinel):
		return render.Cap(out, a.Budget), nil
	case err == nil:
		// tilth exited 0 but printed the sentinel on stdout; normalize that miss to the error path.
		return FallbackTypeDecl(ctx, a, fmt.Errorf("tilth grok: %s %s", notFoundSentinel, a.Query))
	case IsNotFoundText(err.Error()):
		return FallbackTypeDecl(ctx, a, err)
	default:
		return "", err
	}
}

// IsNotFoundText reports whether text carries tilth's grok "not found" sentinel.
func IsNotFoundText(text string) bool {
	return strings.Contains(text, notFoundSentinel)
}

// FallbackTypeDecl resolves a.Query as a Go top-level type decl via ast-grep, wrapping miss in a not-found error if that also fails.
func FallbackTypeDecl(ctx context.Context, a backend.Args, miss error) (string, error) {
	resolved, err := resolveTypeDecl(ctx, a)
	if err != nil {
		return "", fmt.Errorf("grok ast-grep fallback for %q: %w", a.Query, err)
	}
	if resolved == "" {
		return "", fmt.Errorf("grok: symbol %q not found: %w", a.Query, miss)
	}
	return render.Cap(resolved, a.Budget), nil
}

// resolveTypeDecl runs the ast-grep `type <Name> $TY` search for a Go top-level type decl, returning "" on no match.
func resolveTypeDecl(ctx context.Context, a backend.Args) (string, error) {
	fa := backend.Args{
		Query:  fmt.Sprintf("type %s $TY", a.Query),
		Lang:   "go",
		Budget: a.Budget,
	}
	if a.Scope != "" {
		// OpStructural scopes by path positional, so thread the grok scope through Paths or it is dropped.
		fa.Paths = []string{a.Scope}
	}
	out, err := astgrep.Run(ctx, backend.OpStructural, fa)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", nil
	}
	return fmt.Sprintf("# grok: %s (ast-grep type fallback)\n%s", a.Query, out), nil
}
