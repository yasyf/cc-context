package cli

import "github.com/spf13/cobra"

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Repo-level orientation and discovery",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newOverviewCmd(),
		newFindCmd(),
		newLocateCmd(),
	)
	return cmd
}
