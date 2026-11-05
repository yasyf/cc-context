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
		newVcsAuthCmd(),
		newVcsGhCmd(),
		newVcsJobLogCmd(),
		newVcsStatusCmd(),
		newVcsPRCmd(),
		newVcsMergeCmd(),
		newGuidelinesCmd(),
		newStackCmd(),
		newPruneCmd(),
		newReviewsCmd(),
		newShipCmd(),
		newVcsPushCmd(),
		newVcsFetchCmd(),
		newVcsRebaseCmd(),
		newShowCmd(),
		newHistoryCmd(),
		newHunksCmd(),
		newWorktreeCmd(),
		newCleanupCmd(),
		newApplySelectionCmd(),
	)
	return cmd
}
