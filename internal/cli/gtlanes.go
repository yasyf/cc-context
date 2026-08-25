package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// gtLaneRestack restacks the branches of a stack that live in other working
// copies, running gt once from each one, bottom-up. git will not move a branch a
// sibling checkout has checked out, so a single gt restack declines those
// branches and still exits 0 — running gt from that working copy's own dir is
// what reaches them.
//
// chain is ordered bottom-up — trunk-adjacent first — and is driven in that
// order, because restacking a branch leaves everything above it off its parent
// again; a top-down sweep converges only by accident. Callers holding a
// gtDownstack chain, which is branch-first, reverse it before calling.
//
// This working copy is not swept: gt restacks its branches from here, so the
// caller runs that itself and runs it last, and a stack no sibling holds costs
// no gt run at all.
//
// The declines gt printed come back mapped to the reason it gave. They are not a
// verdict on their own — the lane that declines a branch is usually a lane
// another run restacked it from — so a caller pairs them with a fresh gt state
// read and keeps only the reasons still standing.
func gtLaneRestack(ctx context.Context, errW io.Writer, prefix string, c vcs.Checkout, chain []string, classify gtLaneClassifier) ([]string, map[string]string, error) {
	holders, err := vcs.BranchHolders(ctx, c)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", prefix, err)
	}
	lanes := gtLanes(holders, c.Root, chain)
	declined := make(map[string]string)
	for _, dir := range lanes {
		output, err := gtRestackAt(ctx, render.Dir(dir), errW, classify)
		if err != nil {
			return nil, nil, err
		}
		for branch, reason := range gtSyncSkipped(output) {
			declined[branch] = reason
		}
	}
	return lanes, declined, nil
}

// gtLaneClassifier turns one lane's failed gt restack into the caller's own
// recovery step. The directory is passed because a conflict leaves that working
// copy mid-rebase, and a message naming the wrong one sends someone to a clean
// tree.
type gtLaneClassifier func(dir render.Dir, r gtResult, cause error) error

// gtLanes lists the sibling working copies to drive, keeping chain's bottom-up
// order and dropping duplicates. This working copy is never among them: gt
// already restacks its branches from here, so a stack no sibling holds yields
// nothing to drive.
func gtLanes(holders map[string]string, root string, chain []string) []string {
	var lanes []string
	seen := map[string]bool{root: true}
	for _, branch := range chain {
		holder := holders[branch]
		if holder == "" || seen[holder] {
			continue
		}
		seen[holder] = true
		lanes = append(lanes, holder)
	}
	return lanes
}

// gtLaneConflict names the working copy a conflicted restack left mid-rebase.
// A sweep stops in whichever lane conflicted, and gt continue only means
// anything from there — pointed at the caller's own tree it finds no rebase.
func gtLaneConflict(prefix string, here, dir render.Dir) string {
	if dir == here {
		return prefix + ": conflict — resolve the listed files, then gt continue (or gt abort); see the output above"
	}
	return fmt.Sprintf("%s: conflict in %s — resolve the listed files there, then gt continue (or gt abort) from that working copy", prefix, dir)
}

// gtRestackAt runs one gt restack in dir, returning everything gt printed.
func gtRestackAt(ctx context.Context, dir render.Dir, errW io.Writer, classify gtLaneClassifier) (string, error) {
	argv := []string{"restack", "--no-interactive"}
	r, runErr := gtRun(ctx, dir, argv, gtZeroSurfaces, errW)
	if err := gtReport(errW, r); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", classify(dir, r, runErr)
	}
	return r.Output, nil
}

// gtLaneResolved drops the declines the sweep itself answered. Every lane
// declines the branches the other lanes hold, so one sweep names most of the
// stack, and a decline over a branch whose working copy was driven describes a
// run that has since happened.
//
// The reason decides it, not the branch: only "checked out in <dir>" is a
// decline a sweep can answer, and only when that dir is one it drove. "frozen."
// and "merging." are reasons no lane resolves, and the caller's verdict is owed
// them however wide the sweep went.
func gtLaneResolved(declined map[string]string, driven []string) map[string]string {
	moved := make(map[string]bool, len(driven))
	for _, dir := range driven {
		moved[dir] = true
	}
	standing := make(map[string]string, len(declined))
	for branch, reason := range declined {
		if held, ok := strings.CutPrefix(reason, gtSkipHeld); ok && moved[held] {
			continue
		}
		standing[branch] = reason
	}
	return standing
}

// gtLaneSegment names how wide the sweep had to go, counting this working copy
// alongside the siblings, so a ship that reached three does not report the same
// word as one that restacked its own.
func gtLaneSegment(lanes []string) string {
	if len(lanes) == 0 {
		return "restacked"
	}
	return fmt.Sprintf("restacked across %d working copies", len(lanes)+1)
}

// gtBottomUp reverses a gtDownstack chain, which runs branch-first, into the
// trunk-adjacent-first order a lane sweep is driven in.
func gtBottomUp(chain []string) []string {
	bottomUp := slices.Clone(chain)
	slices.Reverse(bottomUp)
	return bottomUp
}
