package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func gtMergingPushes(ctx context.Context, dir render.Dir, s gtSubmit, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) ([]gtSubmitBranch, error) {
	pushed := make(map[string]string, len(plan))
	for _, b := range plan {
		pushed[b.name] = b.head
	}
	var merging []gtSubmitBranch
	for _, b := range plan {
		pr, open := known[b.name]
		if !open || pr.IsBaseRefGraphiteBase || pr.BaseRefName == b.base {
			continue
		}
		base, moves := pushed[pr.BaseRefName]
		if !moves {
			continue
		}
		held, err := gitIsAncestor(ctx, dir, s.prefix, b.head, base)
		if err != nil {
			return nil, err
		}
		if held {
			merging = append(merging, b)
		}
	}
	return merging, nil
}

// Trunk never holds a branch's head, so the pull request survives the push. Trunk
// needs no Graphite record, so a GitHub base edit stands in when Graphite refuses.
func gtRetargetMergingPushes(ctx context.Context, errW io.Writer, dir render.Dir, client *gtapi.Client, owner, name string, s gtSubmit, tr vcs.Trunk, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) error {
	merging, err := gtMergingPushes(ctx, dir, s, plan, known)
	if err != nil {
		return err
	}
	for _, b := range merging {
		pr := known[b.name]
		_, err := client.SubmitPullRequests(ctx, gtapi.SubmitRequest{
			RepoOwner:       owner,
			RepoName:        name,
			TrunkBranchName: tr.Name(),
			PRs: []gtapi.SubmitPR{{
				Action:   gtapi.SubmitUpdate,
				Head:     b.name,
				HeadSha:  pr.Newest().HeadSha,
				Base:     tr.Name(),
				BaseSha:  s.trunkHead,
				PRNumber: b.pr,
			}},
		})
		if client.IsNative() && err != nil {
			return gtSubmitFailure(err, s)
		}
		var refused *gtapi.SubmitError
		switch {
		case errors.As(err, &refused):
			if err := dropRetarget(ctx, owner+"/"+name, b.pr, tr.Name()); err != nil {
				return err
			}
		case err != nil:
			return gtSubmitFailure(err, s)
		}
		if _, err := fmt.Fprintf(errW, "%s: retargeted #%d (%s) from %s onto %s — the push would make GitHub merge it into %s\n", s.prefix, b.pr, b.name, pr.BaseRefName, tr.Name(), pr.BaseRefName); err != nil {
			return fmt.Errorf("%s: name the pull request retargeted before the push: %w", s.prefix, err)
		}
		pr.BaseRefName = tr.Name()
		known[b.name] = pr
	}
	return nil
}
