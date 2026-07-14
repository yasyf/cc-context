// Package cli builds the cobra command tree.
package cli

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
		newWebCmd(),
		newExecCmd(),
		newFormatCmd(),
		newMCPCmd(),
	)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if hint := flagErrorHint(cmd, err); hint != "" {
			return fmt.Errorf("%w (%s)", err, hint)
		}
		return err
	})
	return root
}

// groupHelp is the RunE for a parent command that has no action of its own: the
// bare invocation prints help, while pairing it with cobra.NoArgs turns an unknown
// subcommand into an error naming the bad token. A non-runnable parent instead
// short-circuits to help with exit 0 before args are ever validated.
func groupHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

// flagHint names a known invented/misplaced-flag confusion mined from agent
// unknown-flag errors: cmd is the leaf command name the flag was wrongly
// given to ("" matches any command), flag is the offending flag's name
// without its dashes, and hint computes the message to show.
type flagHint struct {
	cmd, flag string
	hint      func(cmd *cobra.Command) string
}

func staticHint(s string) func(*cobra.Command) string {
	return func(*cobra.Command) string { return s }
}

// flagHints is the curated cross-command confusion table: entries for flags a
// Levenshtein suggestion can't catch because the intended flag lives on a
// different command entirely. Keep it short — new entries earn their place
// from corpus-mined confusions, not speculation.
var flagHints = []flagHint{
	{"grep", "section", staticHint("grep takes -A/-B/-C for context; --section belongs to read/outline")},
	{"grep", "full", staticHint("read/symbol take --full")},
	{"read", "glob", staticHint("grep takes --glob")},
	{"outline", "glob", staticHint("grep takes --glob")},
	{"", "budget", budgetHint},
}

var (
	reUnknownFlag      = regexp.MustCompile(`^unknown flag: --(\S+)`)
	reUnknownShorthand = regexp.MustCompile(`^unknown shorthand flag: '(.)' in`)
)

// flagErrorHint returns a parenthetical suggestion for a flag-parse error on
// cmd: the nearest real flag on cmd by Levenshtein distance when one is
// close, else a curated cross-command hint for a known confusion, else "".
func flagErrorHint(cmd *cobra.Command, err error) string {
	msg := err.Error()
	if m := reUnknownFlag.FindStringSubmatch(msg); m != nil {
		name := m[1]
		if best, dist := nearestFlag(cmd, name); best != "" && dist <= 2 {
			return fmt.Sprintf("did you mean --%s?", best)
		}
		for _, h := range flagHints {
			if h.flag == name && (h.cmd == "" || h.cmd == cmd.Name()) {
				return h.hint(cmd)
			}
		}
		return ""
	}
	if m := reUnknownShorthand.FindStringSubmatch(msg); m != nil {
		if owner := shorthandOwner(cmd, m[1]); owner != "" {
			return fmt.Sprintf("-%s belongs to %s", m[1], owner)
		}
	}
	return ""
}

// nearestFlag returns cmd's own flag closest to name by Levenshtein distance,
// and that distance; ("", 0) when cmd defines no flags.
func nearestFlag(cmd *cobra.Command, name string) (string, int) {
	best, bestDist := "", -1
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		d := levenshtein(name, f.Name)
		if bestDist == -1 || d < bestDist {
			best, bestDist = f.Name, d
		}
	})
	return best, bestDist
}

// shorthandOwner walks the command tree rooted at cmd's root for another
// command defining the given single-character shorthand, returning its
// path relative to the root and the long flag name it backs (e.g. "code
// grep (--after-context)"), or "" when no other command defines it.
func shorthandOwner(cmd *cobra.Command, letter string) string {
	prefix := cmd.Root().Name() + " "
	var found string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if found == "" && c != cmd && c.Runnable() {
			if f := c.Flags().ShorthandLookup(letter); f != nil {
				found = fmt.Sprintf("%s (--%s)", strings.TrimPrefix(c.CommandPath(), prefix), f.Name)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd.Root())
	return found
}

// budgetHint lists every other command that defines --budget, for a
// --budget error on a command that doesn't.
func budgetHint(cmd *cobra.Command) string {
	prefix := cmd.Root().Name() + " "
	var names []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c != cmd && c.Runnable() && c.Flags().Lookup("budget") != nil {
			names = append(names, strings.TrimPrefix(c.CommandPath(), prefix))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd.Root())
	sort.Strings(names)
	return "commands with --budget: " + strings.Join(names, ", ")
}

// levenshtein returns the edit distance between a and b.
func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
