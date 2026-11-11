package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
)

func newVcsQuotaCmd() *cobra.Command {
	var byPath, asJSON bool
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show who spent each open GitHub quota pool on this machine, from its ledger and with no request to GitHub",
		Long: `Show who spent each open GitHub quota pool on this machine, from its ledger
and with no request to GitHub: one line per pool and caller, heaviest first,
summing the caller's requests over every open window of the pool.

--json prints the same rows as one array of
{"pool", "caller", "path", "requests"}, path only with --by-path, so a
script reads the spend without parsing the ledger file, whose shape is ccx's own.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spends, err := ghapi.LedgerSpend(byPath)
			if err != nil {
				return fmt.Errorf("vcs quota: %w", err)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(spends)
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
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the rows as a JSON array")
	return cmd
}
