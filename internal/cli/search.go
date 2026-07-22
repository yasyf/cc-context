package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/querykind"
	"github.com/yasyf/cc-context/internal/search"
)

// engineHeaders names the routing header printed on stdout for each kind.
var engineHeaders = map[querykind.Kind]string{
	querykind.KindSemantic:   "# semantic (native)",
	querykind.KindStructural: "# structural (ast-grep)",
	querykind.KindLiteral:    "# literal (grep)",
}

func newSearchCmd() *cobra.Command {
	var (
		a                             backend.Args
		structural, semantic, literal bool
		explain                       bool
	)
	cmd := &cobra.Command{
		Use:   "search <query> [path]",
		Short: "Code search routed by query kind (semantic, structural, or literal)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			if len(args) == 2 {
				a.Path = args[1]
			}
			a.Mode = modeFlag(structural, semantic, literal)

			op, kind, err := search.Route(a)
			if err != nil {
				return err
			}
			if explain {
				cmd.PrintErrf("explain: query %q → %s (mode %q)\n", a.Query, kind, a.Mode)
			}
			if op == backend.OpStructural && a.Path != "" {
				a.Paths = []string{a.Path}
			}

			out, err := dispatchOp(cmd, op, a)
			if err != nil {
				return err
			}
			cmd.Printf("%s\n%s", engineHeaders[kind], out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&structural, "structural", false, "force structural (ast-grep) search")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "force semantic (native) search")
	cmd.Flags().BoolVar(&literal, "literal", false, "force literal (grep) search")
	cmd.MarkFlagsMutuallyExclusive("structural", "semantic", "literal")
	cmd.Flags().StringVar(&a.Lang, "lang", "", "structural: language to parse as (inferred from extension when omitted)")
	cmd.Flags().BoolVar(&explain, "explain", false, "print the routing decision to stderr")
	cmd.Flags().IntVarP(&a.K, "k", "k", 0, "max results to return")
	cmd.Flags().IntVar(&a.MaxSnippetLines, "max-snippet-lines", 10, "max lines of code per result (0 = full chunk)")
	cmd.Flags().StringVar(&a.Kind, "content", "code docs", "content types to search; several go quoted as one value: --content \"code docs\"; choices code, docs, config, all")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}

// modeFlag maps the mutually-exclusive kind flags to a backend.Args.Mode string.
// None set means auto.
func modeFlag(structural, semantic, literal bool) string {
	switch {
	case structural:
		return "structural"
	case semantic:
		return "semantic"
	case literal:
		return "literal"
	default:
		return "auto"
	}
}
