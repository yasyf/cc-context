// Package dispatch runs the native ccx ops in-process, collapsing what the CLI
// (cli.dispatch) and the MCP proxy (proxy.call) each did per-op into one switch.
// Native reports whether an op runs here; Run executes it with the op's exact
// finalize/cap/binary-skip semantics. Only OpSearch and OpRelated — the semble
// MCP-session ops — are not native.
package dispatch

import (
	"context"
	"fmt"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/deps"
	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/edit"
	"github.com/yasyf/cc-context/internal/find"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/overview"
	"github.com/yasyf/cc-context/internal/read"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/symbol"
	"github.com/yasyf/cc-context/internal/web"
)

// Native reports whether op runs in-process here. Every op is native except
// OpSearch and OpRelated, the semble semantic ops that need the proxy's resident
// MCP session.
func Native(op backend.Op) bool {
	switch op {
	case backend.OpSearch, backend.OpRelated:
		return false
	default:
		return true
	}
}

// Run executes a native op in-process and returns its budget-capped output,
// preserving each op's finalize/cap/binary-skip semantics. It is the single
// dispatch shared by cli.dispatch and proxy.call; a non-native op fails loudly.
func Run(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpOutline || op == backend.OpStructOutline {
		// Skip a binary target before dispatch, so a forced --lang still skips.
		// BinarySkip stats internally and no-ops on a dir or text file.
		if line, skipped := outline.BinarySkip(a.Path); skipped {
			return line, nil
		}
	}

	switch op {
	case backend.OpEdit:
		return edit.Run(a)
	case backend.OpStructural, backend.OpReplace, backend.OpStructOutline:
		return astgrep.Run(ctx, op, a)
	case backend.OpWebOutline, backend.OpWebRead, backend.OpWebSearch:
		out, err := web.Run(ctx, op, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpFind:
		out, err := find.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpRead:
		out, err := read.Run(a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpOverview:
		out, err := overview.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	case backend.OpGrep:
		return ripgrep.Run(ctx, a)
	case backend.OpOutline:
		// The native fallback anchors its own output; cap only, never re-anchor.
		out, err := outline.Fallback(a.Path, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpDiff:
		// The diff renderer anchors its own output; cap only, never render.Finalize.
		out, err := diff.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpSymbol:
		// symbol self-anchors; cap only, never render.Finalize's symbol grammar.
		out, err := symbol.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	case backend.OpDeps:
		out, err := deps.Run(ctx, a)
		if err != nil {
			return "", err
		}
		return render.Cap(out, a.Budget), nil
	default:
		return "", fmt.Errorf("dispatch: op %q is not native", op)
	}
}
