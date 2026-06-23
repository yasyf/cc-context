// Package cli builds the cobra command tree.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/version"
)

// NewRootCmd builds the root command and registers its subcommands.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "ccx",
		Short:         "Compact codebase-context tools for AI agents",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newHelloCmd(),
		newSearchCmd(),
		newRelatedCmd(),
		newOutlineCmd(),
		newReadCmd(),
		newSymbolCmd(),
		newDepsCmd(),
		newGrepCmd(),
		newReplaceCmd(),
		newFindCmd(),
		newDiffCmd(),
		newOverviewCmd(),
		newMCPCmd(),
	)
	return root
}
