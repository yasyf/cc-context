package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/yasyf/cc-context/internal/render"
)

const envCodexThreadKey = "CODEX_THREAD_ID"

func cciClaimPRs(ctx context.Context, errW io.Writer, dir render.Dir, prefix string, opened, updated []int) {
	if render.Getenv(ctx, envClaudeSessionKey) == "" && render.Getenv(ctx, envCodexThreadKey) == "" {
		return
	}
	if render.LookPath(ctx, "cci") == "" {
		return
	}
	for _, c := range []struct {
		rel string
		prs []int
	}{{"opened", opened}, {"updated", updated}} {
		if len(c.prs) == 0 {
			continue
		}
		argv := []string{"claim", "--rel", c.rel}
		for _, pr := range c.prs {
			argv = append(argv, "--pr", strconv.Itoa(pr))
		}
		if _, err := render.RunCLI(ctx, dir, "cci", argv); err != nil {
			_, _ = fmt.Fprintf(errW, "%s: cci claim --rel %s: %v\n", prefix, c.rel, err)
		}
	}
}

func gtClaimPosted(ctx context.Context, errW io.Writer, dir render.Dir, prefix string, posted []gtSubmitBranch, entries map[string]stackEntry) {
	var opened, updated []int
	for _, b := range posted {
		switch {
		case b.pr != 0:
			updated = append(updated, b.pr)
		case entries[b.name].PR != 0:
			opened = append(opened, entries[b.name].PR)
		}
	}
	cciClaimPRs(ctx, errW, dir, prefix, opened, updated)
}
