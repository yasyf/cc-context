package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newReadCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "read <path>",
		Short: "Read a file: a section, a heading, or the whole thing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Path = args[0]
			return runOp(cmd, backend.OpRead, a)
		},
	}
	cmd.Flags().StringVar(&a.Section, "section", "", `range ("A-B") or heading ("## Heading")`)
	cmd.Flags().BoolVar(&a.Full, "full", false, "read the whole file")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}
