package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// errSubmitInherited is a branch proposing commits the remote trunk already
// holds. They are patch-equivalent copies rather than its own work, so the pull
// request proposes files nobody on this branch changed.
type errSubmitInherited struct {
	Branch  string
	Trunk   string
	Commits []string
}

func (e *errSubmitInherited) Error() string {
	return fmt.Sprintf("%s carries %d commit(s) %s already holds, so its pull request proposes work the branch does not own: %s — rebase it onto %s with %s before submitting",
		e.Branch, len(e.Commits), e.Trunk, strings.Join(e.Commits, ", "), e.Trunk, gtRebaseStep)
}

// gtRefuseInherited refuses a submit whose branches carry commits the remote
// trunk already holds. The measure is patch identity, not ancestry: a copy made
// by a replay off a drifted base has a sha of its own and no ancestry says so.
// Each branch is limited to its own commits by its base, so a stacked branch
// does not re-report its downstack's.
func gtRefuseInherited(ctx context.Context, prefix string, dir render.Dir, tr vcs.Trunk, plan []gtSubmitBranch) error {
	for _, b := range plan {
		copies, err := gtCherryCopies(ctx, prefix, dir, string(tr.Ref()), b.head, b.baseSha)
		if err != nil {
			return err
		}
		if len(copies) > 0 {
			return fmt.Errorf("%s: %w", prefix, &errSubmitInherited{Branch: b.name, Trunk: string(tr.Ref()), Commits: copies})
		}
	}
	return nil
}

// gtCherryCopies names the commits of head, back to base, whose patch upstream
// already carries — git cherry's "-" mark.
func gtCherryCopies(ctx context.Context, prefix string, dir render.Dir, upstream, head, base string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"cherry", "--abbrev=12", upstream, head, base})
	if err != nil {
		return nil, fmt.Errorf("%s: git cherry %s %s %s: %w", prefix, upstream, head, base, err)
	}
	var copies []string
	for line := range strings.Lines(out) {
		if sha, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
			copies = append(copies, sha)
		}
	}
	return copies, nil
}
