package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func newStackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Work a Graphite stack that spans one working copy per branch",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newStackNewCmd(),
		newStackListCmd(),
		newRestackCmd(),
		newStackSubmitCmd(),
	)
	return cmd
}

func newStackNewCmd() *cobra.Command {
	var parent string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Cut a branch stacked on this one, in a working copy of its own",
		Long: `Cut a branch named <name> stacked on this one, in a working copy of its own.

The branch is created directly in the new working copy, so the one you run this
from never changes branch — which is what makes a stack workable by several
agents at once, one lane each. The path is minted under the repository's pool,
gt adopts the branch onto --parent (the branch checked out here by default), and
the new working copy's path is the last thing printed, ready to hand to whoever
works the lane.

In a jj repository the lane is a git worktree carrying its own colocated jj, cut
with "jj git init --git-repo .": every lane then answers to git, gt and jj alike.
A jj workspace would not — it has no .git for gt to read.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackNew(cmd, args[0], parent)
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "branch to stack the new one on (default: the branch checked out here)")
	return cmd
}

func newStackListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the stack, naming the working copy holding each branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackList(cmd)
		},
	}
	return cmd
}

func newStackSubmitCmd() *cobra.Command {
	var draft bool
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Restack every lane, then submit the whole stack",
		Long: `Restack every lane, then submit the whole stack.

A submit pushes each branch onto the parent gt records for it, so a stack spread
across working copies has to be restacked in each of them first — which is the
sweep ccx vcs stack restack runs. This does both, in that order, so the submit
meets a stack that is already in the shape Graphite expects. The submit itself is
ccx vcs ship's: one atomic push moving every branch, each under the lease of its
last submitted version, then one post to Graphite's API per branch, bottom-up.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackSubmit(cmd, draft)
		},
	}
	cmd.Flags().BoolVar(&draft, "draft", false, "open new PRs as drafts")
	return cmd
}

func runStackNew(cmd *cobra.Command, name, parent string) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "stack new", workingDir(), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack new: this repository is not on the graphite lane, and a stack is Graphite's — run gt init, or cut a plain working copy with ccx vcs worktree add")
	}
	if parent == "" {
		if parent, err = gitCurrentBranch(ctx, l.dir(), "stack new"); err != nil {
			return err
		}
	}
	if parent == "" {
		return errors.New("stack new: HEAD is detached here, so there is no branch to stack on — check one out, or name it with --parent")
	}
	path, err := mintWorktreePath("stack new", l.checkout, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("stack new: mint pool for %q: %w", name, err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "add", "-b", name, path, parent}); err != nil {
		return fmt.Errorf("stack new: git worktree add %s: %w", path, err)
	}
	if err := stackFormLane(ctx, cmd.ErrOrStderr(), l, render.Dir(path), name, parent); err != nil {
		return errors.Join(err, stackUnwindLane(ctx, l.dir(), path, name))
	}
	cmd.Println(strings.Join([]string{"cut " + name + " onto " + parent, path}, shipSep))
	return nil
}

// stackFormLane finishes a lane the worktree already exists for: its own
// colocated jj where the repository has one, then the Graphite adoption without
// which no restack or submit reaches the branch.
func stackFormLane(ctx context.Context, errW io.Writer, l lane, path render.Dir, name, parent string) error {
	if l.checkout.Kind == vcs.JJ {
		if err := stackColocateJJ(ctx, path, name); err != nil {
			return err
		}
	}
	return gtTrackAt(ctx, path, errW, parent)
}

// stackUnwindLane takes back a lane that only half formed. Left in place it
// holds both the name and the branch, so the same stack new refuses on a retry
// and the fix is two git commands nobody was told about.
func stackUnwindLane(ctx context.Context, root render.Dir, path, name string) error {
	if _, err := render.RunCLI(ctx, root, "git", []string{"worktree", "remove", "--force", path}); err != nil {
		return fmt.Errorf("stack new: remove the half-formed lane at %s: %w", path, err)
	}
	if _, err := render.RunCLI(ctx, root, "git", []string{"branch", "-D", name}); err != nil {
		return fmt.Errorf("stack new: delete the half-formed branch %s: %w", name, err)
	}
	return nil
}

// stackColocateJJ gives a lane its own colocated jj. --colocate is refused
// inside a git worktree and a bare jj git init takes the same path, so the
// repository is named instead: --git-repo . resolves to the worktree's own
// gitdir pointer and colocates against the repository behind it, which is the
// one spelling jj accepts here.
//
// jj then points git's HEAD at the working-copy commit's parent, as colocation
// does everywhere, and gt has no branch to read from a detached HEAD. The files
// on disk are already the branch's, so HEAD is re-attached by name rather than
// by checkout.
func stackColocateJJ(ctx context.Context, path render.Dir, name string) error {
	if _, err := render.RunCLI(ctx, path, "jj", []string{"git", "init", "--git-repo", "."}); err != nil {
		return fmt.Errorf("stack new: jj git init --git-repo . in %s: %w", path, err)
	}
	if _, err := render.RunCLI(ctx, path, "git", []string{"symbolic-ref", "HEAD", "refs/heads/" + name}); err != nil {
		return fmt.Errorf("stack new: re-attach HEAD to %s in %s: %w", name, path, err)
	}
	return nil
}

// gtTrackAt adopts the branch a lane holds onto its parent, from inside that
// lane: gt reads the branch to track from the working copy it runs in.
func gtTrackAt(ctx context.Context, dir render.Dir, errW io.Writer, parent string) error {
	r, runErr := gtRun(ctx, dir, []string{"track", "--parent", parent, "--no-interactive"}, gtZeroFatal, errW)
	if err := gtReport(ctx, errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("stack new: gt track --parent %s: %w", parent, runErr)
	}
	return nil
}

func runStackList(cmd *cobra.Command) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "stack list", workingDir(), true, false)
	if err != nil {
		return err
	}
	stack, state, err := gtStackAll(ctx, l.dir(), "stack list")
	if err != nil {
		return err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("stack list: %w", err)
	}
	for _, branch := range stack {
		cmd.Println(stackListLine(branch, holders[branch], l.checkout.Root, state[branch]))
	}
	return nil
}

// gtStackAll lists the whole stack the current branch belongs to, bottom-up:
// its downstack to trunk, then everything tracked above it. The upstack half is
// gt state's parent map read backwards, and it is the half that matters here —
// with a working copy per branch, the branches above this one are exactly the
// ones this working copy cannot check out to ask about.
func gtStackAll(ctx context.Context, dir render.Dir, prefix string) ([]string, gtState, error) {
	branch, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	if branch == "" {
		return nil, nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", prefix)
	}
	state, err := gtStateQuery(ctx, dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, nil, err
	}
	if branch == trunk {
		return nil, nil, fmt.Errorf("%s: %s is trunk, and every stack in the repository sits on it — check out a branch of the one you mean", prefix, trunk)
	}
	stack, err := gtDownstack(prefix, state, branch, trunk)
	if err != nil {
		return nil, nil, err
	}
	slices.Reverse(stack)
	children := make(map[string][]string, len(state))
	for name, s := range state {
		if len(s.Parents) > 0 {
			children[s.Parents[0].Ref] = append(children[s.Parents[0].Ref], name)
		}
	}
	for _, kids := range children {
		slices.Sort(kids)
	}
	seen := map[string]bool{branch: true}
	for queue := []string{branch}; len(queue) > 0; {
		cur := queue[0]
		queue = queue[1:]
		for _, kid := range children[cur] {
			if seen[kid] {
				return nil, nil, fmt.Errorf("%s: gt state parent chain cycles at %s", prefix, kid)
			}
			seen[kid] = true
			stack = append(stack, kid)
			queue = append(queue, kid)
		}
	}
	return stack, state, nil
}

// stackListLine reads bottom-up, one branch per line, naming the working copy
// holding it — the answer to which lane a branch has to be worked from. A branch
// no working copy holds is named as such rather than left blank, since "nowhere"
// is the fact that decides whether a lane has to be cut for it.
func stackListLine(branch, holder, root string, state gtBranchState) string {
	where := "no working copy"
	switch holder {
	case "":
	case root:
		where = "here"
	default:
		where = holder
	}
	fields := []string{branch, where}
	if state.NeedsRestack {
		fields = append(fields, "needs restack")
	}
	return strings.Join(fields, shipSep)
}

func runStackSubmit(cmd *cobra.Command, draft bool) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()
	l, err := resolveLane(ctx, "stack submit", workingDir(), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack submit: this repository is not on the graphite lane, and a stack is Graphite's — ship the branch with ccx vcs ship instead")
	}
	chain, _, err := gtStackAll(ctx, l.dir(), "stack submit")
	if err != nil {
		return err
	}
	commonDir, err := gtCommonDir(ctx, l.dir(), "stack submit")
	if err != nil {
		return err
	}
	state, err := gtStateAt(ctx, commonDir, "stack submit")
	if err != nil {
		return err
	}
	result, err := gtRestackChain(ctx, "stack submit", l.checkout, l.dir(), state, chain)
	if err != nil {
		return fmt.Errorf("stack submit: %w", err)
	}
	// The restack rewrote the heads the submit force-pushes, so the state it
	// reads must be the one it left behind, not the one it was planned from.
	state, err = gtStateAt(ctx, commonDir, "stack submit")
	if err != nil {
		return err
	}
	trunk, err := gtTrunkBranch("stack submit", state)
	if err != nil {
		return err
	}
	sub := gtSubmit{prefix: "stack submit", draft: draft}
	if err := gtAnnounceStack(errW, sub.prefix, chain); err != nil {
		return err
	}
	if err := gtSubmitStack(ctx, l, sub, commonDir, state, trunk, chain); err != nil {
		return err
	}
	cmd.Println(strings.Join([]string{gtRestackSegment(result), fmt.Sprintf("submitted %d branches", len(chain))}, shipSep))
	return nil
}
