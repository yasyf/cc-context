package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newStackMoveCmd() *cobra.Command {
	var o shipOpts
	var onto string
	cmd := &cobra.Command{
		Use:   "move --onto <parent>",
		Short: "Move the checked-out branch and the branches above it onto another parent, keeping their pull requests",
		Long: `Move the branch checked out here, and every branch stacked above it, onto <parent>.

The branches above are replayed onto the moved branch's new head and pushed.
Each pull request keeps its number, reviews and comments. A pull request a
native GitHub stack holds on another base is moved out of that stack, retargeted
onto <parent>, and linked into <parent>'s stack the way ccx vcs stack submit links
one; the branches left below it are linked again as one stack. <parent> is trunk
or a published branch with an open pull request and no other open pull request
stacked on it.

This is how a branch goes below an open pull request: cut it with ccx vcs stack
new <name> --parent <trunk>, ship it, then run ccx vcs stack move --onto <name>
from the pull request's lane. It is also how a published branch changes parent.

--dry-run prints the plan, naming each pull request the run retargets, and moves
nothing. A conflict stops the run in a conflict workspace; ccx vcs stack continue
resumes it and ccx vcs stack abort drops it. A branch another working copy holds
stays where it is: the run refuses before anything moves, naming the working copy.

--branch moves <branch>'s stack from a detached HEAD, as ccx vcs stack submit
--branch does. Only a native GitHub stack lane moves a pull request in place; on
a Graphite lane, ccx vcs stack submit --parent <branch>=<parent> does the move.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := stackStandOn(cmd.Context(), "stack move", o.branch)
			if err != nil {
				return err
			}
			cmd.SetContext(ctx)
			l, err := resolveWriteLane(ctx, "stack move", workingDir(ctx), false)
			if err != nil {
				return err
			}
			if !l.native {
				return refuse("stack move: this lane does not submit native GitHub stacks — on a Graphite lane, ccx vcs stack submit --parent <branch>=<parent> moves a branch")
			}
			current, err := gitCurrentBranch(ctx, l.dir(), "stack move")
			if err != nil {
				return err
			}
			if current == "" {
				return errors.New("stack move: detached HEAD — check out the branch to move or name it with --branch")
			}
			o.moves = []string{current}
			o.moveInPlace = true
			return runStackSubmit(cmd, o, nil, []string{fmt.Sprintf("%s=%s", current, onto)}, "")
		},
	}
	cmd.Flags().StringVar(&onto, "onto", "", "the new parent: trunk, or a published branch with an open pull request")
	cmd.Flags().StringVar(&o.branch, "branch", "", stackBranchUsage)
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan and move nothing")
	cmd.Flags().BoolVar(&o.evictQueued, "evict-queued", false, stackEvictQueuedUsage)
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
	_ = cmd.MarkFlagRequired("onto")
	return cmd
}
