package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newDiffCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "diff [source] [path...]",
		Short: "VCS-aware diff (uncommitted|staged|<ref>), of every changed file or the paths named",
		Long: `VCS-aware diff of uncommitted, staged, or <ref> changes, uncommitted by default.

The first argument is the source unless it names a file or directory in the
repository; every other argument is a path, restricting the diff as --glob does,
and one --budget covers them all. After --, every argument is a path, which is
how to name a file the diff deletes.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Source = "uncommitted"
			paths := args
			switch dash := cmd.ArgsLenAtDash(); {
			case dash > 1:
				return errors.New("diff: name one source at most before --")
			case dash == 1, dash < 0 && len(args) > 0 && !diffNamesPath(cmd.Context(), args[0]):
				a.Source, paths = args[0], args[1:]
			}
			a.Globs = append(a.Globs, paths...)
			return runOp(cmd, backend.OpDiff, a)
		},
	}
	cmd.Flags().StringArrayVarP(&a.Globs, "glob", "g", nil, globFlagUsage)
	cmd.Flags().BoolVar(&a.Full, "full", false, "inline per-file hunks")
	cmd.Flags().BoolVar(&a.Stat, "stat", false, "per-file added and removed line counts, like git diff --stat")
	cmd.MarkFlagsMutuallyExclusive("full", "stat")
	cmd.Flags().BoolVar(&a.RevealSecrets, "reveal-secrets", false, "print detected secrets raw instead of masked")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}

func diffNamesPath(ctx context.Context, arg string) bool {
	if !filepath.IsAbs(arg) {
		arg = filepath.Join(workingDir(ctx), arg)
	}
	_, err := os.Lstat(arg)
	return err == nil
}
