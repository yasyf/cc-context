package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/outline"
)

// outlineHeaders names the routing header printed on stdout for each engine, so
// the answering engine is visible: ast-grep for the languages it outlines and any
// directory, the native fallback (markdown headings, head window) otherwise.
var outlineHeaders = map[backend.Op]string{
	backend.OpStructOutline: "# ast-grep",
	backend.OpOutline:       "# fallback",
}

func newOutlineCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "outline <path>",
		Short: "Token-budgeted structural outline of a file or directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			op, note, err := outline.Route(&a)
			if err != nil {
				return err
			}
			// A binary target is skipped after path resolution, so a forced --lang
			// still skips and the skip line carries no routing header.
			if line, skipped := outline.BinarySkip(a.Path); skipped {
				cmd.Printf("%s%s\n", note, line)
				return nil
			}
			if _, _, err := outline.ValidateSection(a, op); err != nil {
				return err
			}
			out, err := dispatchOp(cmd, op, a)
			if err != nil {
				return err
			}
			cmd.Printf("%s%s\n%s", note, outlineHeaders[op], out)
			return nil
		},
	}
	cmd.Flags().StringVar(&a.Section, "section", "", `restrict a single-file outline to items intersecting a line range ("40-95" or "40,95")`)
	cmd.Flags().BoolVar(&a.Deep, "deep", false, "ast-grep: include members (struct fields, class methods); default is top-level only")
	cmd.Flags().BoolVar(&a.Full, "full", false, "alias for --deep: include members")
	cmd.Flags().StringVar(&a.Items, "items", "", "ast-grep: items to include (imports|exports|structure|all)")
	cmd.Flags().StringVar(&a.Match, "match", "", "ast-grep: keep only items whose name/signature matches this regex")
	cmd.Flags().StringVar(&a.Lang, "lang", "", "ast-grep: language to parse as (inferred from extension when omitted)")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the outline")
	cmd.Flags().SetNormalizeFunc(sectionAlias)
	return cmd
}
