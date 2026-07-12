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
		Short:   "Grok a symbol: signature, path:line, doc — body/callers/callees/siblings/tests behind flags",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			return runOp(cmd, backend.OpSymbol, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "directory to scope the lookup to")
	cmd.Flags().BoolVar(&a.Full, "full", false, "the full rich output: body, callers, callees, siblings, tests")
	cmd.Flags().BoolVar(&a.Body, "body", false, "include the definition body")
	cmd.Flags().BoolVar(&a.Callers, "callers", false, "include the callers list")
	cmd.Flags().BoolVar(&a.Callees, "callees", false, "include the callees list")
	cmd.Flags().BoolVar(&a.Siblings, "siblings", false, "include the siblings list")
	cmd.Flags().BoolVar(&a.Tests, "tests", false, "include the tests list")
	return cmd
}
