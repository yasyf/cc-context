package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/render"
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
// renames), summarizes each commit's changed symbols via the native structural
// diff, and returns the budget-capped report.
func runHistory(ctx context.Context, path string, n, budget int) (string, error) {
	commits, err := logCommits(ctx, path, n)
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}

	var b strings.Builder
	for _, c := range commits {
		summary, err := commitSummary(ctx, cwd, c.short, c.path)
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
// symbols from the native structural diff of the first-parent..sha range scoped to
// path; the degraded (+added/-deleted) numstat when no symbols classify
// (non-structural files, comment-only edits); or "(added)" for a root commit with
// no parent to diff against. The range uses the resolved parent id rather than
// "sha^" so it resolves in a jj working copy too, where "^" is not a revset
// operator. dir is the repo the commitStat and diff commands run against.
func commitSummary(ctx context.Context, dir, sha, path string) (string, error) {
	parents, added, deleted, err := commitStat(ctx, sha, path)
	if err != nil {
		return "", err
	}
	if len(parents) == 0 {
		return "(added)", nil
	}
	syms, err := diff.ChangedSymbols(ctx, dir, parents[0]+".."+sha, path)
	if err != nil {
		return "", fmt.Errorf("changed symbols %s: %w", sha, err)
	}
	if len(syms) > 0 {
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
