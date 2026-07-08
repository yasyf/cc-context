package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newGrepCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "grep <text>",
		Short: "Search code text, optionally globbed and budgeted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			return runOp(cmd, backend.OpGrep, a)
		},
	}
	cmd.Flags().StringVar(&a.Glob, "glob", "", "restrict to files matching this glob")
	cmd.Flags().StringVar(&a.Scope, "scope", "", "restrict to files under this directory")
	cmd.Flags().BoolVarP(&a.IgnoreCase, "ignore-case", "i", false, "case-insensitive match (ripgrep/grep engine)")
	cmd.Flags().BoolVarP(&a.Word, "word", "w", false, "match whole words only (ripgrep/grep engine)")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().IntVar(&a.Expand, "expand", 0, "tilth engine inlines full source for the top N matches; rg/grep engine adds N context lines around each hit")
	return cmd
}
