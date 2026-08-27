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
		newVcsInfoCmd(),
		newVcsStatusCmd(),
		newGuidelinesCmd(),
		newStackCmd(),
		newReviewsCmd(),
		newShipCmd(),
		newShowCmd(),
		newHistoryCmd(),
		newHunksCmd(),
		newWorktreeCmd(),
		newApplySelectionCmd(),
	)
	return cmd
}
