package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/dispatch"
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
// through internal/dispatch in-process (every op, including the semantic
// search/related ops), and prepends the anchor-move note after capping so the
// note is never truncated away.
func dispatchOp(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	a, note, err := anchor.RewriteArgs(op, a)
	if err != nil {
		return "", err
	}
	out, err := dispatch.Run(cmd.Context(), op, a)
	if err != nil {
		return "", err
	}
	return note + out, nil
}
