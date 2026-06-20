package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newDiffCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "diff [source]",
		Short: "VCS-aware diff (uncommitted|staged|<ref>)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Source = "uncommitted"
			if len(args) == 1 {
				a.Source = args[0]
			}
			return runOp(cmd, backend.OpDiff, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "path to scope the diff to")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}
