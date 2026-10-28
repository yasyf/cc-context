package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/yasyf/cc-context/internal/render"
)

type pushChild struct {
	branch string
	head   string
	parent string
}

// refuseOffParentPush refuses a push leaving a branch off the head the remote
// holds for its parent, while that parent's pull request is open. A parent the
// same push moves is the caller's to check against the head it pushes.
func refuseOffParentPush(ctx context.Context, dir render.Dir, prefix, remote, trunk string, children []pushChild) error {
	var parents []string
	for _, c := range children {
		if c.parent != trunk && !slices.Contains(parents, c.parent) {
			parents = append(parents, c.parent)
		}
	}
	if len(parents) == 0 {
		return nil
	}
	remoteHeads, err := stackListRemoteHeads(ctx, dir, prefix, remote, parents)
	if err != nil {
		return err
	}
	var off []pushChild
	for _, c := range children {
		head := remoteHeads[c.parent]
		if c.parent == trunk || head == "" {
			continue
		}
		held, err := thinReachable(ctx, dir, head, c.head)
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if !held {
			off = append(off, c)
		}
	}
	if len(off) == 0 {
		return nil
	}
	offParents := map[string]bool{}
	for _, c := range off {
		offParents[c.parent] = true
	}
	prs, err := stackPRs(ctx, dir, trunk, slices.Sorted(maps.Keys(offParents)))
	if err != nil {
		return fmt.Errorf("%s: read the parents' pull requests: %w", prefix, err)
	}
	for _, c := range off {
		pr := prs[c.parent]
		if pr == nil || pr.State != "OPEN" || pr.Landed {
			continue
		}
		return fmt.Errorf("%s: %s lacks %s's head %s, the one its pull request #%d carries, so %s's pull request would sit on #%d's old commits and wait for a restack — replay %s onto that head first: ccx vcs stack rebase, or ccx vcs ship --tip-only --restack for the tip alone",
			prefix, c.branch, c.parent, shortOID(remoteHeads[c.parent]), pr.Number, c.branch, pr.Number, c.branch)
	}
	return nil
}
