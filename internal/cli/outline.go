package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
	"github.com/yasyf/cc-context/internal/proxy"
)

// outlineHeaders names the routing header printed on stdout for each engine, so
// the answering engine is visible: ast-grep for the languages it outlines and
// any directory, tilth signature mode otherwise.
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
			if _, _, err := outline.ValidateSection(a, op); err != nil {
				return err
			}
			out, err := outlineFor(cmd, op, a)
			if err != nil {
				return err
			}
			cmd.Printf("%s\n%s", outlineHeaders[op], out)
			return nil
		},
	}
	cmd.Flags().StringVar(&a.Section, "section", "", `restrict a single-file outline to items intersecting a line range ("40-95" or "40,95")`)
	cmd.Flags().StringVar(&a.Items, "items", "", "ast-grep: items to include (imports|exports|structure|all)")
	cmd.Flags().StringVar(&a.Match, "match", "", "ast-grep: keep only items whose name/signature matches this regex")
	cmd.Flags().StringVar(&a.Lang, "lang", "", "ast-grep: language to parse as (inferred from extension when omitted)")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the outline")
	cmd.Flags().SetNormalizeFunc(sectionAlias)
	return cmd
}

// outlineFor returns op's budget-capped outline output: ast-grep's structural
// outline through the direct CLI dispatch, or tilth signature mode through a
// one-shot facade session — the tilth CLI cannot elide bodies, so its compact
// form is reachable only over MCP.
func outlineFor(cmd *cobra.Command, op backend.Op, a backend.Args) (string, error) {
	if op == backend.OpStructOutline {
		return dispatchOp(cmd, op, a)
	}
	p := proxy.New()
	defer func() { _ = p.Close() }()
	return p.Call(cmd.Context(), op, a)
}
