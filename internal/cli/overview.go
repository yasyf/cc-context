package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newOverviewCmd() *cobra.Command {
	var a backend.Args
	return &cobra.Command{
		Use:   "overview",
		Short: "Repository overview",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOp(cmd, backend.OpOverview, a)
		},
	}
}
