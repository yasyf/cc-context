package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
)

func newWebCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Outline, read, and search web pages (token-bounded)",
	}
	cmd.AddCommand(
		newWebOutlineCmd(),
		newWebReadCmd(),
		newWebSearchCmd(),
	)
	return cmd
}

func newWebOutlineCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "outline <url>",
		Short: "Heading tree of a web page with stable section refs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.URL = args[0]
			return runOp(cmd, backend.OpWebOutline, a)
		},
	}
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().BoolVar(&a.Force, "refresh", false, "bypass the cache TTL and refetch")
	return cmd
}

func newWebReadCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "read <url>",
		Short: "Read a web page: a section ref, or the whole thing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.URL = args[0]
			return runOp(cmd, backend.OpWebRead, a)
		},
	}
	cmd.Flags().StringVar(&a.Section, "section", "", "section ref echoed from ccx web outline, e.g. 2.3 or 2.3#k7fq")
	cmd.Flags().BoolVar(&a.Full, "full", false, "read the whole page")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().IntVar(&a.Offset, "offset", 0, "skip this many tokens into the section or page, to page past a --budget cap")
	cmd.Flags().BoolVar(&a.Force, "refresh", false, "bypass the cache TTL and refetch")
	return cmd
}

func newWebSearchCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "search <url> <question>",
		Short: "Ask a question of a web page: top-k relevant chunks with cites",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.URL = args[0]
			a.Query = args[1]
			return runOp(cmd, backend.OpWebSearch, a)
		},
	}
	cmd.Flags().IntVarP(&a.K, "k", "k", 0, "max results to return")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	cmd.Flags().BoolVar(&a.Force, "refresh", false, "bypass the cache TTL and refetch")
	return cmd
}
