package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newSearchCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "search <query> [path]",
		Short: "Semantic code search (natural-language or symbol)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			if len(args) == 2 {
				a.Path = args[1]
			}
			return runOp(cmd, backend.OpSearch, a)
		},
	}
	cmd.Flags().IntVarP(&a.K, "k", "k", 0, "max results to return")
	cmd.Flags().IntVar(&a.MaxSnippetLines, "max-snippet-lines", 0, "max lines per result snippet")
	cmd.Flags().StringVar(&a.Kind, "content", "", "content filter: code|docs|config|all")
	return cmd
}
