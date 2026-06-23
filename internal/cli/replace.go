package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newReplaceCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "replace <pattern> <rewrite> [paths...]",
		Short: "Structural find-replace without reading the file (preview by default)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Pattern = args[0]
			a.Rewrite = args[1]
			a.Paths = args[2:]
			return runOp(cmd, backend.OpReplace, a)
		},
	}
	cmd.Flags().StringVar(&a.Lang, "lang", "", "language to parse as (inferred from extension when omitted)")
	cmd.Flags().StringVar(&a.Glob, "glob", "", "gitignore-style include/exclude (! to exclude)")
	cmd.Flags().BoolVar(&a.Apply, "apply", false, "write the changes (default: preview only)")
	cmd.Flags().BoolVar(&a.Force, "force", false, "bypass the apply file-count cap")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the preview diff")
	return cmd
}
