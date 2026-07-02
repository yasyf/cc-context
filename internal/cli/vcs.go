package cli

import "github.com/spf13/cobra"

func newVcsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vcs",
		Short: "VCS-aware commands (jj + git)",
	}
	cmd.AddCommand(
		newDiffCmd(),
		newShipCmd(),
		newShowCmd(),
		newHistoryCmd(),
	)
	return cmd
}
