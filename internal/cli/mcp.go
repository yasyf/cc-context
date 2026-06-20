package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/mcpserver"
)

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Serve the ccx_* tools over the MCP stdio transport",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return mcpserver.Serve(cmd.Context())
		},
	}
}
