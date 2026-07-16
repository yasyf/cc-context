package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newEditCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "edit <file> (--at <range-or-anchor> | --match <text> | both) (--content <text> | --content - | --delete) [--all]",
		Short: "Replace or delete an anchored or positional line range, writing the file immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			hasContent := cmd.Flags().Changed("content")
			if hasContent == a.Delete {
				return fmt.Errorf("provide exactly one of --content or --delete")
			}
			if cmd.Flags().Changed("match") && a.Match == "" {
				return fmt.Errorf("--match must be non-empty")
			}
			if a.All && a.Match == "" {
				return fmt.Errorf("--all requires --match")
			}
			if a.Section == "" && a.Match == "" {
				return fmt.Errorf("provide --at, --match, or both")
			}
			if hasContent {
				content, _ := cmd.Flags().GetString("content")
				if content == "-" {
					data, err := io.ReadAll(cmd.InOrStdin())
					if err != nil {
						return fmt.Errorf("read --content from stdin: %w", err)
					}
					content = string(data)
				}
				a.Content = content
			}
			return runOp(cmd, backend.OpEdit, a)
		},
	}
	cmd.Flags().StringVar(&a.Section, "at", "", "line range (\"40-95\"), single line, or anchor (\"15-27#k2fa\", bare \"k2fa\")")
	cmd.Flags().StringVar(&a.Match, "match", "", "exact text to find and replace; byte-exact, multi-line ok; combine with --at to scope; needle and content newlines normalize to the file's EOL (a standalone \\r is preserved)")
	cmd.Flags().String("content", "", "replacement text; \"-\" reads all of stdin")
	cmd.Flags().BoolVar(&a.Delete, "delete", false, "delete the range instead of replacing it")
	cmd.Flags().BoolVar(&a.All, "all", false, "with --match, replace every occurrence instead of failing on more than one")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the report")
	return cmd
}
