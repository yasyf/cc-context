package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newSymbolCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:     "symbol <name>",
		Aliases: []string{"grok"},
		Short:   "Grok a symbol: def, doc, callers, callees, siblings, tests",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			return runOp(cmd, backend.OpSymbol, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "directory to scope the lookup to")
	cmd.Flags().BoolVar(&a.Full, "full", false, "include full bodies")
	return cmd
}
