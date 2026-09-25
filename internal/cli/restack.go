package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	jjRestackAncestorRevset = "trunk() & ::@"
	jjRestackStackRevset    = "trunk()..@"
	jjRestackConflictRevset = "conflicts() & @::"
)

var errRestackDetached = errors.New("restack: detached HEAD — check out a branch before restacking")

// errRestackBehind is a pass that ended with a branch still off trunk, which
// exit 0 would let a caller take for a current base.
type errRestackBehind struct {
	Trunk    string
	Branches []string
	Summary  string
}

func (e *errRestackBehind) Error() string {
	return fmt.Sprintf("restack: %s still behind %s: %s — %s; re-run once the cause above is cleared, or run ccx vcs stack rebase to resolve them in an isolated workspace",
		gtBranchCount(len(e.Branches)), e.Trunk, strings.Join(e.Branches, ", "), e.Summary)
}

func gtBranchCount(n int) string {
	if n == 1 {
		return "1 branch"
	}
	return fmt.Sprintf("%d branches", n)
}

type restackOpts struct {
	noGT bool
}

func newRestackCmd() *cobra.Command {
	var o restackOpts
	cmd := &cobra.Command{
		Use:   "restack",
		Short: "Fetch and restack the working-copy stack onto trunk",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestack(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	return cmd
}

func runRestack(cmd *cobra.Command, o restackOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "restack", workingDir(ctx), o.noGT)
	if err != nil {
		return err
	}
	if l.gt {
		return runStackRebase(cmd, stackRebaseOpts{noPush: true})
	}

	var summary string
	switch l.kind {
	case vcs.JJ:
		summary, err = restackJJ(ctx, l.dir())
	case vcs.Git:
		summary, err = restackGit(ctx, l.dir())
	default:
		panic(fmt.Sprintf("restack: unsupported vcs kind %d", l.kind))
	}
	if err != nil {
		return err
	}
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
}

func restackJJ(ctx context.Context, dir render.Dir) (string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if len(trunkNames) != 1 {
		return "", fmt.Errorf("restack: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", trunkNames)
	}
	trunk := trunkNames[0]

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"git", "fetch"}); err != nil {
		return "", fmt.Errorf("restack: jj git fetch: %w", err)
	}
	ancestors, err := jjLogLines(ctx, dir, "restack", jjRestackAncestorRevset)
	if err != nil {
		return "", err
	}
	if len(ancestors) > 0 {
		return "fetched · already up to date", nil
	}

	rebased, err := jjRestackOntoTrunk(ctx, dir, trunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched · rebased %d commit(s) onto %s", rebased, trunk), nil
}

func jjRestackOntoTrunk(ctx context.Context, dir render.Dir, trunk string) (int, error) {
	stack, err := jjLogLines(ctx, dir, "restack", jjRestackStackRevset)
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("restack: trunk %q is not an ancestor of @ but trunk()..@ is empty", trunk)
	}

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"rebase", "-b", "@", "--destination", "trunk()"}); err != nil {
		return 0, fmt.Errorf("restack: jj rebase onto trunk %q: %w — retry manually: jj rebase -b @ --destination 'trunk()'", trunk, err)
	}
	rebaseOp, err := jjOpID(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("restack: read jj rebase operation: %w", err)
	}

	conflicts, err := jjLogLines(ctx, dir, "restack", jjRestackConflictRevset)
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp})
		if revertErr == nil {
			return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed (rebase rolled back): %w", trunk, err)
		}
		return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", trunk, err, revertErr, rebaseOp)
	}
	if len(conflicts) > 0 {
		if _, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp}); revertErr != nil {
			return 0, fmt.Errorf("restack: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", trunk, revertErr, rebaseOp)
		}
		return 0, fmt.Errorf("restack: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @ --destination 'trunk()', then fix the conflicts (jj status)", trunk, len(conflicts), strings.Join(conflicts, "\n  "))
	}
	return len(stack), nil
}

func restackGit(ctx context.Context, dir render.Dir) (string, error) {
	branch, err := gitCurrentBranch(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errRestackDetached
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, branch)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"fetch", remote}); err != nil {
		return "", fmt.Errorf("restack: git fetch %s: %w", remote, err)
	}

	trunk, err := vcs.ResolveTrunk(ctx, dir, remote)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	upToDate, err := gitIsAncestor(ctx, dir, "restack", string(trunk.Ref()), "HEAD")
	if err != nil {
		return "", fmt.Errorf("restack: compare HEAD with %s: %w", trunk.Ref(), err)
	}
	if upToDate {
		return "fetched · already up to date", nil
	}

	if branch == trunk.Name() {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"merge", "--ff-only", string(trunk.Ref())}); err != nil {
			return "", fmt.Errorf("restack: fast-forward %s to %s: %w — resolve manually: git fetch %s && git merge --ff-only %s", branch, trunk.Ref(), err, remote, trunk.Ref())
		}
		return "fetched · fast-forwarded " + trunk.Name(), nil
	}

	if _, err := gitRebaseOnto(ctx, dir, "restack", trunk.Remote(), trunk.Name()); err != nil {
		return "", err
	}
	return "fetched · rebased onto " + trunk.Name(), nil
}
