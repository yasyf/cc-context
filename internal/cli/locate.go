package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/locate"
)

func newLocateCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "locate <name>",
		Short: "Resolve a sibling repo, Go module, or Python package to its on-disk path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			results, err := locate.Locate(cmd.Context(), name, workspace)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				return fmt.Errorf("locate %q: %w", name, ErrNotFound)
			}
			cmd.Print(formatResults(results))
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", defaultWorkspace(), "sibling-repo root to search")
	return cmd
}

// formatResults renders each hit as a tab-separated "<kind>\t<path>[\t<version>]"
// line, omitting the version column when a resolver reports none.
func formatResults(results []locate.Result) string {
	var b strings.Builder
	for _, r := range results {
		b.WriteString(string(r.Kind))
		b.WriteByte('\t')
		b.WriteString(r.Path)
		if r.Version != "" {
			b.WriteByte('\t')
			b.WriteString(r.Version)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// defaultWorkspace is $HOME/Code, the sibling-repo root locate searches unless
// --workspace overrides it.
func defaultWorkspace() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Code")
}
