package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newOverviewCmd() *cobra.Command {
	var a backend.Args
	return &cobra.Command{
		Use:   "overview",
		Short: "Repository overview",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runOp(cmd, backend.OpOverview, a); err != nil {
				return err
			}
			// tilth's overview is a primary-language fingerprint; append a
			// language census so multi-language repos aren't under-reported.
			if census := languageCensus(workingDir()); census != "" {
				cmd.Println("\n" + census)
			}
			return nil
		},
	}
}
