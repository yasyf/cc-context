package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newRelatedCmd() *cobra.Command {
	var a backend.Args
	return &cobra.Command{
		Use:   "related <file:line>",
		Short: "Find code related to a file:line, or an anchored f.go:12#a3fk",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			return runOp(cmd, backend.OpRelated, a)
		},
	}
}
