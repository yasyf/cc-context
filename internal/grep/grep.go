// Package grep orchestrates the CLI grep op on the tilth engine, pre-flighting the
// scope, normalizing tilth's no-match path-fallback into the house no-match output,
// and re-verifying every tilth zero through a live ripgrep recheck so a stale index
// never reports a confident "0 matches" for content that exists on disk.
package grep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/grok"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

// Run executes the tilth grep invocation, pre-flighting a non-empty scope so a
// typo fails loudly, and normalizing tilth's no-match path-fallback — a "not
// found:" error byte-identical to its nonexistent-scope error — into the house
// no-match output. Any other error propagates. A tilth zero — clean or via the
// path-fallback — is re-verified through a live ripgrep recheck (Recheck): the
// recheck's output replaces the zero only when it finds matches the stale index
// missed, so a genuine zero stays byte-identical to today.
func Run(ctx context.Context, bin string, argv []string, a backend.Args) (string, error) {
	if a.Scope != "" {
		if _, err := os.Stat(a.Scope); err != nil {
			return "", fmt.Errorf("grep: scope %q does not exist: %w", a.Scope, err)
		}
	}
	out, err := render.RunCLI(ctx, bin, argv)
	switch {
	case err == nil:
		if ZeroMatches(out) {
			rechecked, ok, rerr := recheckOverride(ctx, a)
			if rerr != nil {
				return "", rerr
			}
			if ok {
				return rechecked, nil
			}
		}
		return render.Finalize(backend.OpGrep, out, a)
	case grok.IsNotFoundText(err.Error()):
		rechecked, ok, rerr := recheckOverride(ctx, a)
		if rerr != nil {
			return "", rerr
		}
		if ok {
			return rechecked, nil
		}
		return render.Finalize(backend.OpGrep, ripgrep.NoMatch(a.Query), a)
	default:
		return "", err
	}
}

// ZeroMatches reports whether out is one of tilth's clean zero results — its first
// line ending in the "— 0 matches" suffix, identical on the CLI and MCP surfaces
// across plain/--glob/--scope/--expand searches (verified against tilth v0.9.0). A
// matched header ends in "— N matches" plus a parenthetical, never this suffix, and
// the check is first-line-only so the trailing token footer never sways it.
func ZeroMatches(out string) bool {
	first, _, _ := strings.Cut(out, "\n")
	return strings.HasSuffix(strings.TrimRight(first, "\r"), "— 0 matches")
}

// Recheck re-runs a's grep through the live ripgrep engine to verify a tilth zero
// against the working tree rather than the possibly-stale index, reporting
// found-ness structurally (ripgrep.RunFound) so budget capping can never distort
// the verdict. It value-copies a and defaults an unset Budget to
// ripgrep.DefaultBudget so the recheck is capped on every lane — codeexec
// included, whose own greps run uncapped — because a verification pass must never
// flood the surface. Both engines share backend.AnchorGrepArgs' glob/scope
// peeling, so they search the same file set.
//
// Search-space asymmetries are left as-is, never patched: tilth indexes hidden
// dirs while rg skips them by default, so a hidden-only stale zero is not rescued
// (output identical to today); the system-grep fallback ignores .gitignore (its
// engine note already discloses this). The override only ever flips zero→matches,
// so every asymmetry is safe in that direction.
func Recheck(ctx context.Context, a backend.Args) (out string, found bool, err error) {
	if a.Budget == 0 {
		a.Budget = ripgrep.DefaultBudget
	}
	return ripgrep.RunFound(ctx, a)
}

// recheckOverride runs a live recheck of a tilth zero and returns its output only
// when the working tree holds matches the stale index missed. ok is false when the
// zero stands: the recheck also found nothing, or it failed with the request
// context still live (e.g. no engine on PATH — degrade to today's output). A
// recheck failure under a dead context propagates ctx.Err() as err instead — not
// the recheck's own error, which under cancellation is incidental — so a dying
// request is never misreported as a verified zero and errors.Is sees the
// cancellation.
func recheckOverride(ctx context.Context, a backend.Args) (out string, ok bool, err error) {
	slog.Debug("grep zero recheck", "query", a.Query)
	rechecked, found, err := Recheck(ctx, a)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", false, fmt.Errorf("grep: zero recheck aborted: %w", ctx.Err())
	case err != nil, !found:
		return "", false, nil
	}
	slog.Debug("grep zero recheck overrides stale index", "query", a.Query)
	return rechecked, true, nil
}
