package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/ripgrep"
)

func newGrepCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "grep <text> [paths...]",
		Short: "Search code text (literal or regex), optionally globbed, scoped, or over explicit files",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Query = args[0]
			a.Paths = args[1:]
			if ripgrep.Handles(a) && a.Budget == 0 {
				a.Budget = ripgrep.DefaultBudget
			}
			return runOp(cmd, backend.OpGrep, a)
		},
	}
	cmd.Flags().StringVar(&a.Glob, "glob", "", "restrict to files matching this glob")
	cmd.Flags().StringVar(&a.Scope, "scope", "", "restrict to files under this directory")
	cmd.Flags().BoolVarP(&a.IgnoreCase, "ignore-case", "i", false, "case-insensitive match (ripgrep/grep engine)")
	cmd.Flags().BoolVarP(&a.Word, "word", "w", false, "match whole words only (ripgrep/grep engine)")
	cmd.Flags().BoolVarP(&a.Regex, "regex", "E", false, "treat the pattern as a regular expression (ripgrep/grep engine)")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().IntVar(&a.Expand, "expand", 0, "tilth engine inlines full source for the top N matches; rg/grep engine adds N context lines around each hit")
	return cmd
}
