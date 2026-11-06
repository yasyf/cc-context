package cli

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/read"
)

// sectionAlias maps the --lines flag name onto --section so a caller who guesses
// "--lines A-B" hits the canonical section flag; only --section shows in help.
func sectionAlias(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if name == "lines" {
		name = "section"
	}
	return pflag.NormalizedName(name)
}

func newReadCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "read <path>...",
		Short: "Read a file: a section, a heading, or the whole thing; several files share one budget",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.Section != "" && len(args) > 1 {
				return errors.New("read: --section names lines of one file; pass one path with it")
			}
			if a.Budget == 0 {
				a.Budget = read.DefaultBudget
			}
			return runEachPath(cmd, args, a.Budget, func(path string, budget int) (string, error) {
				one := a
				one.Path, one.Budget = path, budget
				return dispatchOp(cmd, backend.OpRead, one)
			})
		},
	}
	cmd.Flags().StringVar(&a.Section, "section", "", `range ("40-95"), heading ("## Heading"), or anchor ("15-27#k2fa" or bare "k2fa") echoed from a producer command`)
	cmd.Flags().BoolVar(&a.Full, "full", false, "read the whole file")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().BoolVar(&a.RevealSecrets, "reveal-secrets", false, "print detected secrets raw instead of masked")
	cmd.Flags().SetNormalizeFunc(sectionAlias)
	return cmd
}
