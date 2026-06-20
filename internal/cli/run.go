package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
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
