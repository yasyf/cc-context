package cli

import "github.com/spf13/cobra"

func newCodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "code",
		Short: "Read, search, and transform code (token-bounded)",
	}
	cmd.AddCommand(
		newReadCmd(),
		newOutlineCmd(),
		newSearchCmd(),
		newGrepCmd(),
		newSymbolCmd(),
		newDepsCmd(),
		newRelatedCmd(),
		newReplaceCmd(),
	)
	return cmd
}
