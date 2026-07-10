package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/edit"
	"github.com/yasyf/cc-context/internal/grep"
	"github.com/yasyf/cc-context/internal/grok"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/ripgrep"
	"github.com/yasyf/cc-context/internal/router"
	"github.com/yasyf/cc-context/internal/web"
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
// note is never truncated away.
func dispatchOp(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	a, note, err := anchor.RewriteArgs(op, a)
	if err != nil {
		return "", err
	}
	out, err := dispatch(cmd, op, a)
	if err != nil {
		return "", err
	}
	return note + out, nil
}

// dispatch routes op through its backend, executes the resulting argv, and
// returns the output capped to a.Budget. The ast-grep ops emit --json=stream and
// tolerate the clean no-match exit, so they run through the shared astgrep
// orchestration; every other op runs as a plain capped CLI invocation.
func dispatch(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpEdit {
		return edit.Run(a)
	}
	if op == backend.OpStructural || op == backend.OpReplace || op == backend.OpStructOutline {
		return astgrep.Run(cmd.Context(), op, a)
	}
	if op == backend.OpWebOutline || op == backend.OpWebRead || op == backend.OpWebSearch {
		out, err := web.Run(cmd.Context(), op, a)
		if err != nil {
			return "", err
		}
		return render.Finalize(op, out, a)
	}
	if op == backend.OpGrep && (a.IgnoreCase || a.Word) {
		return ripgrep.Run(cmd.Context(), a)
	}
	bin, argv, err := router.For(op).CLIArgv(cmd.Context(), op, a)
	if err != nil {
		return "", err
	}
	if op == backend.OpDiff {
		return render.RunDiffCLI(cmd.Context(), bin, argv, a.Source, a.Budget)
	}
	if op == backend.OpSymbol {
		return grok.Run(cmd.Context(), bin, argv, a)
	}
	if op == backend.OpGrep {
		return grep.Run(cmd.Context(), bin, argv, a)
	}
	out, err := render.RunCLI(cmd.Context(), bin, argv)
	if err != nil {
		return "", err
	}
	return render.Finalize(op, out, a)
}
