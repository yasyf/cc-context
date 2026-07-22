package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/dispatch"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semble"
)

// runOp dispatches op through its backend and prints the budget-capped output on
// the command's stdout.
func runOp(cmd *cobra.Command, op backend.Op, a backend.Args) error {
	out, err := dispatchOp(cmd, op, a)
	if err != nil {
		return err
	}
	cmd.Print(out)
	return nil
}

// dispatchOp resolves content anchors in a to plain line numbers, dispatches op
// through its backend, and prepends the anchor-move note after capping so the
// note is never truncated away. Native ops run in-process via internal/dispatch;
// the semantic ops (search, related) run the one-shot semble CLI lane.
func dispatchOp(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	a, note, err := anchor.RewriteArgs(op, a)
	if err != nil {
		return "", err
	}
	var out string
	if dispatch.Native(op) {
		out, err = dispatch.Run(cmd.Context(), op, a)
	} else {
		out, err = runSemble(cmd.Context(), op, a)
	}
	if err != nil {
		return "", err
	}
	return note + out, nil
}

// runSemble runs the one-shot semble CLI lane for the semantic ops (search,
// related): the facade drives semble as a subprocess here rather than the proxy's
// resident MCP session, capping and anchoring the reshaped result via Finalize.
func runSemble(ctx context.Context, op backend.Op, a backend.Args) (string, error) {
	bin, argv, err := semble.CLIArgv(ctx, op, a)
	if err != nil {
		return "", err
	}
	start := time.Now()
	out, err := render.RunCLI(ctx, bin, argv)
	elapsed := time.Since(start)
	if err != nil {
		return "", err
	}
	out, err = render.Finalize(op, out, a)
	if err != nil {
		return "", err
	}
	return render.WithSlowSearchNote(out, elapsed), nil
}
