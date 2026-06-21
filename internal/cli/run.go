package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/proxy"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/router"
)

// runOp routes op through its backend, executes the resulting argv, caps the
// output to a.Budget, and prints it on the command's stdout.
func runOp(cmd *cobra.Command, op backend.Op, a backend.Args) error {
	bin, argv, err := router.For(op).CLIArgv(cmd.Context(), op, a)
	if err != nil {
		return err
	}
	out, err := render.RunCLI(cmd.Context(), bin, argv)
	if err != nil {
		return err
	}
	cmd.Print(render.Cap(out, a.Budget))
	return nil
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
