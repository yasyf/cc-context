package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newOutlineCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "outline <path>",
		Short: "Token-budgeted structural outline of a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			// Routed via the facade (one-shot tilth MCP) for signature mode —
			// the tilth CLI cannot elide bodies; see runViaFacade.
			return runViaFacade(cmd, backend.OpOutline, a)
		},
	}
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the outline")
	return cmd
}
