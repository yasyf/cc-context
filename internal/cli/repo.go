package cli

import "github.com/spf13/cobra"

func newRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Repo-level orientation and discovery",
	}
	cmd.AddCommand(
		newOverviewCmd(),
		newFindCmd(),
		newLocateCmd(),
	)
	return cmd
}
