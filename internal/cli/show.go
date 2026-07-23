package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/secrets"
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
	cmd.Flags().BoolVar(&a.RevealSecrets, "reveal-secrets", false, "print detected secrets raw instead of masked")
	cmd.Flags().IntVar(&a.Budget, "budget", 0, "token budget for the output")
	return cmd
}

// runShow resolves the commit header and its parent..commit range, renders that
// range through the shared diff pipeline (which masks each file's section), and
// caps the header+diff to budget — show's own render/cap point, so the
// masked-secrets footer lands after the cap here, not in dispatch. The commit
// header (subject, body, author) is masked pathlessly, its fired rules joining
// the footer.
func runShow(cmd *cobra.Command, ref string, a backend.Args) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	commit, err := vcs.Show(cmd.Context(), cwd, ref)
	if err != nil {
		return err
	}
	a.Source = commit.Range
	a, note, err := anchor.RewriteArgs(backend.OpDiff, a)
	if err != nil {
		return err
	}
	out, ids, err := diff.Run(cmd.Context(), a)
	if err != nil {
		return err
	}
	header := showHeader(commit)
	if !a.RevealSecrets {
		masked, fired := secrets.Mask(header, "")
		header = masked
		ids = append(fired, ids...)
	}
	cmd.Print(note + render.WithSecretsFooter(render.Cap(header+out, a.Budget), ids))
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
