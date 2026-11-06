package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/deps"
)

func newDepsCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "deps <path>...",
		Short: "Symbol dependencies and dependents of a file (sparse for entry points); several share one budget",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// An unset flag gets the default; an explicit --budget 0 means unlimited.
			if !cmd.Flags().Changed("budget") {
				a.Budget = deps.DefaultBudget
			}
			return runEachPath(cmd, args, a.Budget, func(path string, budget int) (string, error) {
				one := a
				one.Path, one.Budget = path, budget
				return dispatchOp(cmd, backend.OpDeps, one)
			})
		},
	}
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}
