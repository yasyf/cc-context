package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// newShowCmd builds the `show` command: a single commit's message plus the
// structural, token-bounded diff of exactly that commit's change.
func newShowCmd() *cobra.Command {
	var a backend.Args
	cmd := &cobra.Command{
		Use:   "show [ref]",
		Short: "Commit message + structural per-file summary of a single commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runShow(cmd, ref, a)
		},
	}
	cmd.Flags().StringVar(&a.Scope, "scope", "", "path to scope the diff to")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}

// runShow resolves the commit header and its parent..commit range, renders that
// range through the shared diff pipeline, and caps the header+diff to budget.
func runShow(cmd *cobra.Command, ref string, a backend.Args) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	commit, err := vcs.Show(cmd.Context(), cwd, ref)
	if err != nil {
		return err
	}
	budget := a.Budget
	a.Source = commit.Range
	a.Budget = 0 // render the full diff; the header+diff total is capped below
	diff, err := dispatchOp(cmd, backend.OpDiff, a)
	if err != nil {
		return err
	}
	cmd.Print(render.Cap(showHeader(commit)+diff, budget))
	return nil
}

// showHeader renders the compact commit header — short id, author, date,
// subject, and body when present — terminated by a blank line before the diff.
func showHeader(c vcs.Commit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\n", c.ShortID)
	fmt.Fprintf(&b, "Author: %s <%s>\n", c.Author, c.Email)
	fmt.Fprintf(&b, "Date:   %s\n\n", c.Date)
	b.WriteString(c.Subject)
	b.WriteString("\n")
	if c.Body != "" {
		b.WriteString("\n")
		b.WriteString(c.Body)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}
