package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/vcs"
)

// shipDetachedFlags are the flags a detached ship cannot carry: it commits at
// HEAD and moves one remote branch, so every flag that places, restacks or
// describes a pull request belongs to a ship from the branch itself.
var shipDetachedFlags = []string{
	"dry-run", "no-push", "no-commit", "expect-remote", "append", "parent", "allow-trunk",
	"draft", "publish", "reviews", "pr-title", "pr-title-file", "pr-body-file", "replace-pr-text",
	"landed", "new-pr", "tip-only", "all-lanes", "include", "restack", "drop-commits", "evict-queued",
}

// shipDetached reports whether this ship commits at a detached HEAD and pushes
// it as --branch, the state a working copy is in when another working copy
// holds the branch. It refuses, before anything changes, a flag that ship
// cannot carry there and a --branch naming no local branch or trunk.
func shipDetached(cmd *cobra.Command, l lane, o shipOpts) (bool, error) {
	if !gitBacked(l) || o.branch == "" || o.newBranch != "" || o.noCommit {
		return false, nil
	}
	ctx, dir := cmd.Context(), l.dir()
	current, err := gitCurrentBranch(ctx, dir, "ship")
	if err != nil || current != "" {
		return false, err
	}
	for _, flag := range shipDetachedFlags {
		if cmd.Flags().Changed(flag) {
			return false, fmt.Errorf("ship: --%s does not apply to a ship from a detached HEAD, which commits at HEAD and pushes it as %s — drop it, or check out %s", flag, o.branch, o.branch)
		}
	}
	exists, err := gitRefExists(ctx, dir, "ship", "refs/heads/"+o.branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("ship: --branch %s names no local branch, and a detached HEAD pushes only onto one — git fetch origin %s:%s first, or cut one with --new-branch=%s", o.branch, o.branch, o.branch, o.branch)
	}
	trunk, err := gitTrunkBranch(ctx, dir)
	if err != nil {
		return false, err
	}
	if o.branch == trunk {
		return false, fmt.Errorf("ship: --branch %s names trunk, and a detached HEAD never pushes onto trunk — check out %s to advance it", trunk, trunk)
	}
	return true, nil
}

// runShipDetached commits at the detached HEAD and pushes HEAD as o.branch
// under ccx vcs push's lease, leaving refs/heads/<branch> and the working copy
// that holds it as they were, then warns that working copy is now stale.
func runShipDetached(cmd *cobra.Command, l lane, o shipOpts, sel *shipSelection) error {
	ctx, dir, errW := cmd.Context(), l.dir(), cmd.ErrOrStderr()
	o.noVerify = !shipVerify(cmd, o)
	hookSeg, err := shipCommit(ctx, errW, dir, vcs.Git, o, sel, branchPlan{action: branchAppend})
	if err != nil {
		return err
	}
	short, subject, err := shipDescribe(ctx, dir, vcs.Git)
	if err != nil {
		return err
	}
	pushed, err := vcsPushHead(ctx, l.checkout, o.branch, true, o.noVerify)
	if err != nil {
		return fmt.Errorf("%w — %s %q stays committed at the detached HEAD; ccx vcs push --branch %s retries the push", err, short, subject, o.branch)
	}
	head, err := gitRevParse(ctx, dir, "ship", "HEAD")
	if err != nil {
		return err
	}
	if err := shipWarnStaleHolder(ctx, errW, l, o.branch, head); err != nil {
		return err
	}
	segments := make([]string, 0, 4)
	if hookSeg != "" {
		segments = append(segments, hookSeg)
	}
	segments = append(segments, shipCommitSegment(false, short, subject), pushed)
	if !o.noPR {
		cciClaimPushed(ctx, errW, l, o.branch, "")
	}
	if o.noWatch {
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}
	ci := ciPR{branch: o.branch}
	if l.repo != nil {
		ci.nwo = l.repo.NameWithOwner
	}
	ciSeg, report, ciErr := shipWatchCIHead(ctx, errW, dir, ci, head, o.budget)
	if ciSeg != "" {
		segments = append(segments, ciSeg)
	}
	cmd.Println(strings.Join(segments, shipSep))
	for _, line := range report {
		cmd.Println(line)
	}
	return ciErr
}

// shipWarnStaleHolder names the working copy holding branch when its checkout
// sits anywhere but head, the commit this ship just pushed as branch.
func shipWarnStaleHolder(ctx context.Context, errW io.Writer, l lane, branch, head string) error {
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	holder := holders[branch]
	if holder == "" {
		return nil
	}
	local, err := gitRevParse(ctx, l.dir(), "ship", "refs/heads/"+branch)
	if err != nil || local == head {
		return err
	}
	_, _ = fmt.Fprintf(errW, "ship: %s holds %s at %s, now stale against the pushed %s — that working copy was left untouched; refresh it there once its own work is saved\n", holder, branch, shortOID(local), shortOID(head))
	return nil
}
