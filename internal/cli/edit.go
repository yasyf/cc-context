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
		Use:   "edit <file> --at <range-or-anchor> (--content <text> | --content - | --delete)",
		Short: "Replace or delete an anchored or positional line range, writing the file immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			hasContent := cmd.Flags().Changed("content")
			if hasContent == a.Delete {
				return fmt.Errorf("provide exactly one of --content or --delete")
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
	cmd.Flags().String("content", "", "replacement text; \"-\" reads all of stdin")
	cmd.Flags().BoolVar(&a.Delete, "delete", false, "delete the range instead of replacing it")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the report")
	_ = cmd.MarkFlagRequired("at")
	return cmd
}
