package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
)

const vcsRebasePrefix = "vcs rebase"

var (
	rebaseValueFlags     = map[string]bool{"--onto": true, "--trailer": true, "--whitespace": true, "--empty": true, "--exec": true, "--strategy": true, "--strategy-option": true}
	rebaseValueShorts    = "CxsX"
	rebaseOptionalShorts = "Sr"
	rebaseIdleFlags      = map[string]bool{"--abort": true, "--quit": true, "--edit-todo": true, "--show-current-patch": true}
	rebaseTodoPick       = map[string]bool{"p": true, "pick": true, "r": true, "reword": true, "e": true, "edit": true, "s": true, "squash": true, "f": true, "fixup": true}
)

type rebaseRange struct {
	onto, upstream, branch       string
	keepBase, root, resume, idle bool
}

func newVcsRebaseCmd() *cobra.Command {
	var chdir string
	var configs []string
	cmd := &cobra.Command{
		Use:   "rebase [-C <dir>] [-c <name>=<value>]... -- [<git rebase args>...]",
		Short: "Run git rebase, fetching the blobs it merges in one batch first in a partial clone",
		Long: `Run git rebase with the arguments after --, unchanged, and exit with its code.

In a partial clone whose promisor remote filters blobs, git fetches the blobs
a rebase merges one commit at a time, writing a promisor pack for each commit.
Before git rebase starts, ccx fetches them in one batch: the blobs at every
path the replayed commits change, at each of those commits, at their base, and
at the onto commit. --continue and --skip read the remaining picks from the
rebase's todo list and fetch against the current HEAD. --abort, --quit,
--edit-todo, --show-current-patch, and --root fetch nothing, and neither does a
clone without a blob-filtered promisor remote. A batch that fails is logged as
a warning, and git rebase runs anyway.

-C and -c reach git ahead of rebase, as git -C and git -c take them.`,
		Example: "  ccx vcs rebase -- --onto origin/main main~3\n  ccx vcs rebase -c rerere.enabled=false -- --continue\n  ccx vcs rebase -C ../lane -- -q origin/main",
		Args: func(cmd *cobra.Command, _ []string) error {
			if cmd.ArgsLenAtDash() != 0 {
				return errors.New("name the git rebase arguments after --: ccx vcs rebase -- --onto origin/main main~3")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return runVcsRebase(cmd, chdir, configs, args) },
	}
	cmd.Flags().StringVarP(&chdir, "chdir", "C", "", "run as if git rebase was started in this directory")
	cmd.Flags().StringArrayVarP(&configs, "config", "c", nil, "pass a configuration parameter to git, as git -c does (repeatable)")
	return cmd
}

func runVcsRebase(cmd *cobra.Command, chdir string, configs, args []string) error {
	ctx := cmd.Context()
	dir := workingDir(ctx)
	switch {
	case filepath.IsAbs(chdir):
		dir = chdir
	case chdir != "":
		dir = filepath.Join(dir, chdir)
	}
	if err := rebasePrefetch(ctx, render.Dir(dir), args); err != nil {
		slog.Warn(vcsRebasePrefix+": batch-fetch the rebase's blobs from the promisor remote; git rebase fetches them commit by commit instead", "args", strings.Join(args, " "), "err", err)
	}
	argv := make([]string, 0, 2*len(configs)+1+len(args))
	for _, kv := range configs {
		argv = append(argv, "-c", kv)
	}
	git := exec.Command("git", append(append(argv, "rebase"), args...)...) //nolint:gosec // args is the caller's own git rebase command
	git.Dir = dir
	git.Env = slices.Concat(os.Environ(), render.EnvFrom(ctx))
	git.Stdin, git.Stdout, git.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	return ghExit(git.Run())
}

func rebasePrefetch(ctx context.Context, dir render.Dir, args []string) error {
	remote, err := replayPromisor(ctx, dir)
	if err != nil || remote == "" {
		return err
	}
	r := rebaseParse(args)
	switch {
	case r.resume:
		return rebaseFetchTodo(ctx, dir, remote)
	case r.idle, r.root:
		return nil
	}
	upstream, branch := cmp.Or(r.upstream, "@{upstream}"), cmp.Or(r.branch, "HEAD")
	onto, err := rebaseOnto(ctx, dir, r, upstream, branch)
	if err != nil {
		return err
	}
	return replayFetchRange(ctx, dir, remote, onto, upstream, branch)
}

func rebaseParse(args []string) rebaseRange {
	var r rebaseRange
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case arg == "-":
			positional = append(positional, "@{-1}")
		case arg == "--continue" || arg == "--skip":
			r.resume = true
		case rebaseIdleFlags[arg]:
			r.idle = true
		case arg == "--root":
			r.root = true
		case arg == "--keep-base":
			r.keepBase = true
		case strings.HasPrefix(arg, "--"):
			name, value, inline := strings.Cut(arg, "=")
			if !rebaseValueFlags[name] {
				continue
			}
			if !inline && i+1 < len(args) {
				i++
				value = args[i]
			}
			if name == "--onto" {
				r.onto = value
			}
		case strings.HasPrefix(arg, "-"):
			i += rebaseShortValue(arg)
		default:
			positional = append(positional, arg)
		}
	}
	switch {
	case r.root && len(positional) > 0:
		r.branch = positional[0]
	case len(positional) > 1:
		r.upstream, r.branch = positional[0], positional[1]
	case len(positional) == 1:
		r.upstream = positional[0]
	}
	return r
}

func rebaseShortValue(arg string) int {
	for j := 1; j < len(arg); j++ {
		switch {
		case strings.IndexByte(rebaseOptionalShorts, arg[j]) >= 0:
			return 0
		case strings.IndexByte(rebaseValueShorts, arg[j]) >= 0:
			if j == len(arg)-1 {
				return 1
			}
			return 0
		}
	}
	return 0
}

func rebaseOnto(ctx context.Context, dir render.Dir, r rebaseRange, upstream, branch string) (string, error) {
	left, right, symmetric := strings.Cut(r.onto, "...")
	switch {
	case r.keepBase:
		left, right = upstream, branch
	case symmetric:
		left, right = cmp.Or(left, "HEAD"), cmp.Or(right, "HEAD")
	default:
		return cmp.Or(r.onto, upstream), nil
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", left, right})
	if err != nil {
		return "", fmt.Errorf("resolve the onto commit: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func rebaseFetchTodo(ctx context.Context, dir render.Dir, remote string) error {
	state, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "rebase-merge"})
	if err != nil {
		return err
	}
	todo, err := os.ReadFile(filepath.Join(strings.TrimSpace(state), "git-rebase-todo")) //nolint:gosec // the repository's own rebase todo
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	picks := rebaseTodoPicks(string(todo))
	if len(picks) == 0 {
		return nil
	}
	commits, err := render.RunCLI(ctx, dir, "git", append([]string{"rev-parse"}, picks...))
	if err != nil {
		return err
	}
	return replayFetchCommits(ctx, dir, remote, "HEAD", picks[0]+"^", commits)
}

func rebaseTodoPicks(todo string) []string {
	var picks []string
	for line := range strings.Lines(todo) {
		fields := strings.Fields(line)
		if len(fields) < 2 || !rebaseTodoPick[fields[0]] {
			continue
		}
		commit := fields[1]
		if (fields[0] == "f" || fields[0] == "fixup") && (commit == "-C" || commit == "-c") && len(fields) > 2 {
			commit = fields[2]
		}
		picks = append(picks, commit)
	}
	return picks
}
