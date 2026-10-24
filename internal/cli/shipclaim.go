package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const envCodexThreadKey = "CODEX_THREAD_ID"

func cciClaimable(ctx context.Context) bool {
	if render.Getenv(ctx, envClaudeSessionKey) == "" && render.Getenv(ctx, envCodexThreadKey) == "" {
		return false
	}
	return render.LookPath(ctx, "cci") != ""
}

type cciOpened struct {
	pr     int
	branch string
}

func cciClaimPRs(ctx context.Context, errW io.Writer, dir render.Dir, prefix, repo string, opened []cciOpened, updated []int) {
	if !cciClaimable(ctx) {
		return
	}
	var openedPRs []int
	var branches []string
	for _, o := range opened {
		openedPRs, branches = append(openedPRs, o.pr), append(branches, o.branch)
	}
	for _, c := range []struct {
		rel      string
		prs      []int
		branches []string
	}{{"opened", openedPRs, branches}, {"updated", updated, nil}} {
		if len(c.prs) == 0 {
			continue
		}
		argv := []string{"claim", "--rel", c.rel}
		for _, pr := range c.prs {
			argv = append(argv, "--pr", repo+"#"+strconv.Itoa(pr))
		}
		for _, branch := range c.branches {
			argv = append(argv, "--branch", branch)
		}
		if _, err := render.RunCLI(ctx, dir, "cci", argv); err != nil {
			_, _ = fmt.Fprintf(errW, "%s: cci claim --rel %s: %v\n", prefix, c.rel, err)
		}
	}
}

func gtClaimPosted(ctx context.Context, errW io.Writer, dir render.Dir, prefix, repo string, posted []gtSubmitBranch, entries map[string]stackEntry) {
	var opened []cciOpened
	var updated []int
	for _, b := range posted {
		switch {
		case b.pr != 0:
			updated = append(updated, b.pr)
		case entries[b.name].PR != 0:
			opened = append(opened, cciOpened{pr: entries[b.name].PR, branch: b.name})
		}
	}
	cciClaimPRs(ctx, errW, dir, prefix, repo, opened, updated)
}

func cciClaimPushed(ctx context.Context, errW io.Writer, l lane, branch, trunk string) {
	if branch == "" || branch == trunk || !cciClaimable(ctx) {
		return
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "ship: cci claim: %v\n", err)
		return
	}
	pr, found, err := lookupPR(ctx, repo.NameWithOwner, branch)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "ship: cci claim: %v\n", err)
		return
	}
	if found {
		cciClaimPRs(ctx, errW, l.dir(), "ship", repo.NameWithOwner, nil, []int{pr.Number})
	}
}
