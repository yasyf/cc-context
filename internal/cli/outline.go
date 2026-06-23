package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
)

// outlineHeaders names the routing header printed on stdout for each engine, so
// the answering engine is visible (ast-grep for supported languages and dirs,
// tilth signature mode otherwise).
var outlineHeaders = map[backend.Op]string{
	backend.OpStructOutline: "# ast-grep",
	backend.OpOutline:       "# tilth",
}

func newOutlineCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "outline <path>",
		Short: "Token-budgeted structural outline of a file or directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			op, err := outline.Route(a)
			if err != nil {
				return err
			}
			if op == backend.OpStructOutline {
				// ast-grep outline is a stateless CLI op, like search/replace.
				return runHeadedOp(cmd, op, a)
			}
			// tilth signature mode is reachable only over its MCP (the tilth CLI
			// cannot elide bodies); see runViaFacade.
			return runHeadedFacade(cmd, op, a)
		},
	}
	cmd.Flags().StringVar(&a.Items, "items", "", "ast-grep: items to include (imports|exports|structure|all)")
	cmd.Flags().StringVar(&a.Match, "match", "", "ast-grep: keep only items whose name/signature matches this regex")
	cmd.Flags().StringVar(&a.Lang, "lang", "", "ast-grep: language to parse as (inferred from extension when omitted)")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the outline")
	return cmd
}

// runHeadedOp dispatches op through the CLI path and prints the routing header
// above the budget-capped output.
func runHeadedOp(cmd *cobra.Command, op backend.Op, a backend.Args) error {
	out, err := dispatchOp(cmd, op, a)
	if err != nil {
		return err
	}
	cmd.Printf("%s\n%s", outlineHeaders[op], out)
	return nil
}

// runHeadedFacade dispatches op through the one-shot facade session and prints
// the routing header above the budget-capped output.
func runHeadedFacade(cmd *cobra.Command, op backend.Op, a backend.Args) error {
	cmd.Printf("%s\n", outlineHeaders[op])
	return runViaFacade(cmd, op, a)
}
