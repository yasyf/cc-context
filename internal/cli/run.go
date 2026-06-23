package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/astgrep"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/router"
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

// dispatchOp routes op through its backend, executes the resulting argv, and
// returns the output capped to a.Budget. The ast-grep ops emit --json=stream and
// tolerate the clean no-match exit, so they run through the shared astgrep
// orchestration; every other op runs as a plain capped CLI invocation.
func dispatchOp(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpStructural || op == backend.OpReplace || op == backend.OpStructOutline {
		return astgrep.Run(cmd.Context(), op, a)
	}
	bin, argv, err := router.For(op).CLIArgv(cmd.Context(), op, a)
	if err != nil {
		return "", err
	}
	out, err := render.RunCLI(cmd.Context(), bin, argv)
	if err != nil {
		return "", err
	}
	return render.Cap(out, a.Budget), nil
}

// runViaFacade routes op through a one-shot proxy session instead of a direct CLI
// invocation. Used for ops whose compact form is only reachable over tilth's MCP:
// outline needs signature mode, which the tilth CLI has no flag for. proxy.Call
// already caps to a.Budget.
func runViaFacade(cmd *cobra.Command, op backend.Op, a backend.Args) error {
	p := proxy.New()
	defer func() { _ = p.Close() }()
	out, err := p.Call(cmd.Context(), op, a)
	if err != nil {
		return err
	}
	cmd.Print(out)
	return nil
}
