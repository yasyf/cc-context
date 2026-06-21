package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newDepsCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "deps <path>",
		Short: "Symbol dependencies and dependents of a file (sparse for entry points)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			return runOp(cmd, backend.OpDeps, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "directory to scope the analysis to")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}
