package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/diff"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/secrets"
	"github.com/yasyf/cc-context/internal/vcs"
)

func newHistoryCmd() *cobra.Command {
	var (
		number int
		budget int
		reveal bool
	)
	cmd := &cobra.Command{
		Use:   "history <path>",
		Short: "Per-commit summary of a file's changed symbols (replaces git log -p)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, note := resolveHistoryPath(args[0])
			out, err := runHistory(cmd.Context(), path, number, budget, reveal)
			if err != nil {
				return err
			}
			cmd.Print(note + out)
			return nil
		},
	}
	cmd.Flags().IntVarP(&number, "number", "n", 10, "max commits to summarize")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the output")
	cmd.Flags().BoolVar(&reveal, "reveal-secrets", false, "print detected secrets raw instead of masked")
	return cmd
}

func resolveHistoryPath(path string) (string, string) {
	a, note, err := backend.ResolvePath(backend.OpStructural, backend.Args{Paths: []string{path}})
	if err != nil {
		return path, ""
	}
	return a.Paths[0], note
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
// diff, and returns the budget-capped report. Unless reveal, the report —
// commit subjects and changed-symbol names both — is masked in path's rule
// context, the shared footer appended after the cap.
func runHistory(ctx context.Context, path string, n, budget int, reveal bool) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	dir := render.Dir(cwd)
	commits, err := logCommits(ctx, dir, path, n)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, c := range commits {
		summary, err := commitSummary(ctx, dir, c.short, c.path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s %s %s\n    %s\n", c.short, c.date, c.subject, summary)
	}
	out := b.String()
	var ids []string
	if !reveal {
		out, ids = secrets.Mask(out, path)
	}
	return render.WithSecretsFooter(render.Cap(out, budget), ids), nil
}

// logCommits runs the pinned `git log --follow --name-status` enumeration over
// path and reads each record's hash, date, subject, and the file's then-current
// name (following renames across the file's history). The name comes from the
// commit's last name-status entry — the rename destination on a rename, the
// then-current name otherwise.
func logCommits(ctx context.Context, dir render.Dir, path string, n int) ([]historyCommit, error) {
	records, err := vcs.GitLogNameStatus(ctx, vcs.GitArgs{
		Dir:   dir,
		Sub:   []string{"log", "--follow", "--date=short", "-n", strconv.Itoa(n)},
		Paths: []string{path},
	}, "%h", "%ad", "%s")
	if err != nil {
		return nil, fmt.Errorf("history %q: %w", path, err)
	}
	commits := make([]historyCommit, 0, len(records))
	for _, r := range records {
		c := historyCommit{short: r.Fields[0], date: r.Fields[1], subject: r.Fields[2]}
		if len(r.Entries) > 0 {
			c.path = r.Entries[len(r.Entries)-1].New
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// commitSummary returns the indented symbol line for one commit: the changed
// symbols from the native structural diff of the first-parent..sha range scoped to
// path; the degraded (+added/-deleted) numstat when no symbols classify
// (non-structural files, comment-only edits); or "(added)" for a root commit with
// no parent to diff against. The range uses the resolved parent id rather than
// "sha^" so it resolves in a jj working copy too, where "^" is not a revset
// operator. dir is the repo the commitStat and diff commands run against.
func commitSummary(ctx context.Context, dir render.Dir, sha, path string) (string, error) {
	parents, added, deleted, err := commitStat(ctx, dir, sha, path)
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
// A binary file's "-" counts decode to zero.
func commitStat(ctx context.Context, dir render.Dir, sha, path string) (parents []string, added, deleted int, err error) {
	header, records, err := vcs.GitNumstat(ctx, vcs.GitArgs{
		Dir:   dir,
		Sub:   []string{"show"},
		Revs:  []vcs.GitRef{vcs.UnsafeRef(sha)},
		Paths: []string{path},
	}, "%P")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("commit stat %s: %w", sha, err)
	}
	for _, r := range records {
		a, _ := strconv.Atoi(r.Added)
		d, _ := strconv.Atoi(r.Deleted)
		added += a
		deleted += d
	}
	return strings.Fields(header[0]), added, deleted, nil
}
