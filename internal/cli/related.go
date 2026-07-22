package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newRelatedCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "related <file:line> [path]",
		Short: "Find code related to a file:line, or an anchored f.go:12#a3fk",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			if len(args) == 2 {
				a.Path = args[1]
			}
			return runOp(cmd, backend.OpRelated, a)
		},
	}
	cmd.Flags().StringVar(&a.Kind, "content", "code docs", "content types to search; several go quoted as one value: --content \"code docs\"; choices code, docs, config, all")
	return cmd
}
