package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newFindCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "find <glob>",
		Short: "List files matching a glob, with per-file token counts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Glob = args[0]
			return runOp(cmd, backend.OpFind, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "directory to scope the search to")
	return cmd
}
