package cli

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vendor"
)

func newHistoryCmd() *cobra.Command {
	var (
		number int
		budget int
	)
	cmd := &cobra.Command{
		Use:   "history <path>",
		Short: "Per-commit summary of a file's changed symbols (replaces git log -p)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := runHistory(cmd.Context(), args[0], number, budget)
			if err != nil {
				return err
			}
			cmd.Print(out)
			return nil
		},
	}
	cmd.Flags().IntVarP(&number, "number", "n", 10, "max commits to summarize")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the output")
	return cmd
}

// historyCommit is one entry from `git log --follow --name-status`: the
// abbreviated hash, the authored date (--date=short), the subject line, and the
// file's path as of that commit — the rename destination on a rename commit, the
// then-current name otherwise. path is the scope handed to that commit's diff.
type historyCommit struct {
	short   string
	date    string
	subject string
	path    string
}

// runHistory enumerates up to n commits touching path (newest first, following
// renames), summarizes each commit's changed symbols via the tilth structural
// diff, and returns the budget-capped report.
func runHistory(ctx context.Context, path string, n, budget int) (string, error) {
	commits, err := logCommits(ctx, path, n)
	if err != nil {
		return "", err
	}
	tilthBin, err := vendor.Resolve(ctx, vendor.Tilth, "")
	if err != nil {
		return "", fmt.Errorf("resolve tilth: %w", err)
	}

	var b strings.Builder
	for _, c := range commits {
		summary, err := commitSummary(ctx, tilthBin, c.short, c.path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s %s %s\n    %s\n", c.short, c.date, c.subject, summary)
	}
	return render.Cap(b.String(), budget), nil
}

// logCommits runs the pinned `git log --follow --name-status` enumeration over
// path and parses its records into per-commit hash, date, subject, and the file's
// then-current name (following renames across the file's history).
func logCommits(ctx context.Context, path string, n int) ([]historyCommit, error) {
	out, err := render.RunCLI(ctx, "git", []string{
		"log", "--follow",
		"--format=%h%x00%ad%x00%s",
		"--date=short",
		"-n", strconv.Itoa(n),
		"--name-status",
		"--", path,
	})
	if err != nil {
		return nil, fmt.Errorf("git log %q: %w", path, err)
	}
	return parseCommits(out), nil
}

// parseCommits decodes `git log --follow --name-status` output. Each commit is a
// header line — three NUL-separated fields (%h, %ad, %s) — followed, after a blank
// line, by one name-status line naming the file at that commit. Header lines are
// identified by their NUL separators; the name-status line (tab-separated, no NUL)
// sets the preceding commit's path.
func parseCommits(out string) []historyCommit {
	var commits []historyCommit
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.IndexByte(line, 0) >= 0:
			f := strings.SplitN(line, "\x00", 3)
			commits = append(commits, historyCommit{short: f[0], date: f[1], subject: f[2]})
		case line == "":
			continue
		case len(commits) > 0:
			commits[len(commits)-1].path = statusPath(line)
		}
	}
	return commits
}

// statusPath returns the file's path from a `--name-status` line, which is its last
// tab-separated field: the destination on a rename or copy (`R<score>\told\tnew`) or
// the sole path on an add, modify, or delete (`A\tpath`).
func statusPath(line string) string {
	f := strings.Split(line, "\t")
	return f[len(f)-1]
}

// commitSummary returns the indented symbol line for one commit: the changed
// symbols from the tilth structural diff; the degraded (+added/-deleted) numstat
// when tilth extracts no symbols (non-structural files, comment-only edits); or
// "(added)" for a root commit with no parent to diff against.
func commitSummary(ctx context.Context, tilthBin, sha, path string) (string, error) {
	parents, added, deleted, err := commitStat(ctx, sha, path)
	if err != nil {
		return "", err
	}
	if len(parents) == 0 {
		return "(added)", nil
	}
	diff, err := render.RunCLI(ctx, tilthBin, []string{"diff", sha + "^.." + sha, "--scope", path})
	if err != nil {
		return "", fmt.Errorf("tilth diff %s: %w", sha, err)
	}
	if syms := changedSymbols(diff); len(syms) > 0 {
		return strings.Join(syms, ", "), nil
	}
	return fmt.Sprintf("(+%d/-%d)", added, deleted), nil
}

// commitStat returns sha's parent hashes and the file's added/deleted line counts
// via a single `git show --numstat --format=%P`. Empty parents marks a root commit.
func commitStat(ctx context.Context, sha, path string) (parents []string, added, deleted int, err error) {
	out, err := render.RunCLI(ctx, "git", []string{"show", "--numstat", "--format=%P", sha, "--", path})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git show %s: %w", sha, err)
	}
	parents, added, deleted = parseNumstat(out)
	return parents, added, deleted, nil
}

// parseNumstat decodes `git show --numstat --format=%P` output: the first line is
// the space-separated parent hashes (empty for a root commit), followed by
// "<added>\t<deleted>\t<path>" numstat rows. Binary files report "-" counts,
// which decode to zero.
func parseNumstat(out string) (parents []string, added, deleted int) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	parents = strings.Fields(lines[0])
	for _, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		a, _ := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		added += a
		deleted += d
	}
	return parents, added, deleted
}

// symbolHeader matches a tilth structural-diff per-symbol section header, e.g.
// "## [~] dispatchOp — body changed (L28-47)". The status flag is ' ' (unchanged),
// '~' (body changed), '+' (added), or '-' (deleted); the name runs up to the
// em-dash change note or the "(L…" location, so names containing parentheses
// (Go's "import (") survive intact.
var symbolHeader = regexp.MustCompile(`^## \[([ ~+-])\] (.+?)(?: —| \(L\d)`)

// changedSymbols extracts the changed symbols from tilth diff output, tagging each
// with its change sigil (+ added, ~ changed, - deleted) and dropping unchanged
// (' ') sections. Order follows tilth's output.
func changedSymbols(diff string) []string {
	var syms []string
	for _, line := range strings.Split(diff, "\n") {
		m := symbolHeader.FindStringSubmatch(line)
		if m == nil || m[1] == " " {
			continue
		}
		syms = append(syms, m[1]+m[2])
	}
	return syms
}
