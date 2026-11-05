package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
)

func newVcsQuotaCmd() *cobra.Command {
	var byPath bool
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show who spent each open GitHub quota pool on this machine, from its ledger and with no request to GitHub",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spends, err := ghapi.LedgerSpend(byPath)
			if err != nil {
				return fmt.Errorf("vcs quota: %w", err)
			}
			for _, s := range spends {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%6d  %s  %s  %s\n", s.Requests, s.Pool, s.Caller, s.Path); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&byPath, "by-path", false, "split each caller's requests by the path they asked for")
	return cmd
}
