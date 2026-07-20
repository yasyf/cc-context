package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// newHunksCmd builds the "ccx vcs hunks" listing command: it prints each changed
// file's hunks as file:A-B#digest refs that feed ship's --skip-hunk / --only-hunk.
func newHunksCmd() *cobra.Command {
	var budget int
	cmd := &cobra.Command{
		Use:   "hunks [paths...]",
		Short: "List each changed file's hunks as skip-hunk/only-hunk refs",
		Long: `List each changed file's hunks as skip-hunk/only-hunk refs.

Each line is "<file>:<A>-<B>#<digest>	-<dels>+<adds>	<first changed line>". The
ref feeds "ccx vcs ship --skip-hunk <ref>" / "--only-hunk <ref>" to commit a
subset of a file's edits. With no paths, every changed file is listed; the base
is the committed content (git HEAD, jj @-), the current is the working copy.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHunks(cmd, args, budget)
		},
	}
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the output (0 = uncapped)")
	return cmd
}

// runHunks lists the hunks of each requested (or, with no paths, each changed)
// file, computed from the same committed base and working content ship commits
// from, so a listed ref always resolves at ship time.
func runHunks(cmd *cobra.Command, paths []string, budget int) error {
	ctx := cmd.Context()
	kind := vcs.Detect(workingDir())
	if kind == vcs.None {
		return errors.New("hunks: no git or jj repository in the working directory")
	}
	root, err := repoRoot(ctx, kind)
	if err != nil {
		return fmt.Errorf("hunks: %w", err)
	}
	if len(paths) == 0 {
		paths, err = changedFiles(ctx, kind, root)
		if err != nil {
			return err
		}
	} else {
		for i, p := range paths {
			rel, err := rootRel(root, p)
			if err != nil {
				return fmt.Errorf("hunks: %w", err)
			}
			paths[i] = rel
		}
	}
	var b strings.Builder
	for _, path := range paths {
		base, err := showFileBase(ctx, kind, path)
		if err != nil {
			return fmt.Errorf("hunks: %w", err)
		}
		current, err := hunksReadCurrent(filepath.Join(root, path))
		if err != nil {
			return err
		}
		for _, h := range hunk.Compute(base, current) {
			b.WriteString(formatHunkLine(path, h))
			b.WriteByte('\n')
		}
	}
	cmd.Print(render.Cap(b.String(), budget))
	return nil
}

// changedFiles enumerates the files that differ from the committed base — jj's
// working copy against @-, git's worktree against HEAD — as root-relative paths.
// git already emits root-relative names; jj emits them relative to the working
// directory, so they normalize to root.
func changedFiles(ctx context.Context, kind vcs.Kind, root string) ([]string, error) {
	var bin string
	var argv []string
	switch kind {
	case vcs.JJ:
		bin, argv = "jj", []string{"diff", "--name-only"}
	case vcs.Git:
		bin, argv = "git", []string{"diff", "--name-only", "HEAD"}
	default:
		return nil, errors.New("hunks: unsupported vcs")
	}
	out, err := render.RunCLI(ctx, bin, argv)
	if err != nil {
		return nil, fmt.Errorf("hunks: list changed files: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if kind == vcs.JJ {
			line, err = rootRel(root, line)
			if err != nil {
				return nil, fmt.Errorf("hunks: %w", err)
			}
		}
		files = append(files, line)
	}
	return files, nil
}

// hunksReadCurrent reads path's working content, treating a missing file as an
// empty current (a deletion) so a removed file still lists as one deletion hunk.
func hunksReadCurrent(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the caller's own listing target, not untrusted input
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hunks: read %s: %w", path, err)
	}
	return data, nil
}

// formatHunkLine renders one hunk as "<ref>\t-<dels>+<adds>\t<first changed line>".
func formatHunkLine(path string, h hunk.Hunk) string {
	return fmt.Sprintf("%s\t-%d+%d\t%s", hunkListRef(path, h), len(h.Old), len(h.New), hunkFirstLine(h))
}

// hunkListRef renders a hunk's post-image ref (path:A-B#digest).
func hunkListRef(path string, h hunk.Hunk) string {
	return formatHunkRef(path, hunkRef(h))
}

// hunkRef is the anchor ccx vcs hunks emits and matchHunkRef resolves against. A
// change or insertion uses its new-line span; a pure deletion anchors at its
// post-image start, so a fresh ref matches at distance 0 and identical deletions
// get distinct lines.
func hunkRef(h hunk.Hunk) anchor.Ref {
	ref := anchor.Ref{Line: h.NewStart, Hash: h.Digest}
	if h.NewEnd > h.NewStart {
		ref.End = h.NewEnd
	}
	return ref
}

// hunkFirstLine returns the hunk's first changed line trimmed of surrounding
// whitespace: its first new line, or the first deleted line for a pure deletion.
func hunkFirstLine(h hunk.Hunk) string {
	if len(h.New) > 0 {
		return strings.TrimSpace(h.New[0])
	}
	if len(h.Old) > 0 {
		return strings.TrimSpace(h.Old[0])
	}
	return ""
}
