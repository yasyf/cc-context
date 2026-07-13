package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/find"
)

func newFindCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "find <glob>",
		Short: "List files matching a glob, with per-file token counts",
		Long: `List files matching a glob, with per-file token counts.

Honors the gitignore chain and skips VCS stores (.git, .jj, .hg, .svn); output is
sorted and budget-capped, with a footer counting withheld rows and ignore-hidden
files. A glob anchored at a real path (.venv/**/*.py) lists files ignore rules
would hide. For whole-repo orientation use "ccx repo overview" instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Glob = args[0]
			// An unset flag gets the default; an explicit --budget 0 means unlimited.
			if !cmd.Flags().Changed("budget") {
				a.Budget = find.DefaultBudget
			}
			return runOp(cmd, backend.OpFind, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "directory to scope the search to")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}
