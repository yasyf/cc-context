package cli

import "github.com/spf13/cobra"

func newVcsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vcs",
		Short: "VCS-aware commands (jj + git)",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newDiffCmd(),
		newShipCmd(),
		newShowCmd(),
		newHistoryCmd(),
		newHunksCmd(),
		newApplySelectionCmd(),
	)
	return cmd
}
