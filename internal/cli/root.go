// Package cli builds the cobra command tree.
package cli

import (
	"os"

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
	// cobra's Print family targets OutOrStderr; without an explicit out stream
	// every command's result lands on stderr.
	root.SetOut(os.Stdout)
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newVcsCmd(),
		newCodeCmd(),
		newRepoCmd(),
		newExecCmd(),
		newToonCmd(),
		newMCPCmd(),
	)
	return root
}
