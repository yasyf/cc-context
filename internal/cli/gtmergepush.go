package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func gtRefuseMergingPush(ctx context.Context, dir render.Dir, s gtSubmit, tr vcs.Trunk, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) error {
	pushed := make(map[string]string, len(plan))
	for _, b := range plan {
		pushed[b.name] = b.head
	}
	var merged, numbers []string
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
			return err
		}
		if held {
			merged = append(merged, fmt.Sprintf("#%d (%s) into %s", b.pr, b.name, pr.BaseRefName))
			numbers = append(numbers, fmt.Sprintf("#%d", b.pr))
		}
	}
	if len(merged) == 0 {
		return nil
	}
	problem := fmt.Sprintf("pushing would make GitHub merge %s — each pull request still targets a branch this push moves onto a head that holds its own, which GitHub reads as merged, closing it and deleting its branch; retarget %s onto %s first, with gh pr edit <number> --base %s",
		strings.Join(merged, ", "), strings.Join(numbers, ", "), tr.Name(), tr.Name())
	return errors.New(gtStuck(s.prefix, problem, s.suffix))
}
