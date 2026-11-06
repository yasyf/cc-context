package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func ghRead(ctx context.Context, dir render.Dir, argv []string) (string, error) {
	env, _, err := ghReadEnv(ctx, dir, 0)
	if err != nil {
		return "", err
	}
	return render.RunCLIEnv(ctx, dir, "gh", argv, env)
}

func ghReadEnv(ctx context.Context, dir render.Dir, margin time.Duration) ([]string, time.Time, error) {
	return ghRepoReadEnv(ctx, dir, "", margin)
}

// ghRepoReadEnv is ghReadEnv for the repository named nameWithOwner, or for
// dir's checkout when it is "".
func ghRepoReadEnv(ctx context.Context, dir render.Dir, nameWithOwner string, margin time.Duration) ([]string, time.Time, error) {
	api := reviewsAPI()
	if ok, err := api.AppConfigured(ctx); err != nil || !ok {
		return nil, time.Time{}, err
	}
	if nameWithOwner == "" {
		repo, err := vcs.LookupRepo(ctx, dir, false)
		if err != nil {
			return nil, time.Time{}, err
		}
		nameWithOwner = repo.NameWithOwner
	}
	token, expires, err := api.ForRepo(nameWithOwner).AppToken(ctx, max(margin, ghapi.AppRefreshMargin))
	if err != nil || token == "" {
		return nil, time.Time{}, err
	}
	return []string{"GH_TOKEN=" + token}, expires, nil
}

func ghRateLimited(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "rate limit")
}

func rateLimitUntil(now time.Time, reset string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64)
	if err != nil || !time.Unix(seconds, 0).After(now) {
		return now.Add(ghRateLimitWait)
	}
	return time.Unix(seconds, 0).Add(2 * time.Second)
}

// ghReadWaiting is ghRead for a read the CI watch cannot do without: GitHub's
// rate limit does not end it, the read waits for the core quota's reset, says so
// once, and goes on.
func ghReadWaiting(ctx context.Context, dir render.Dir, argv []string) (string, error) {
	for {
		out, err := ghRead(ctx, dir, argv)
		if !ghRateLimited(err) {
			return out, err
		}
		reset, _ := ghRead(ctx, dir, []string{"api", "rate_limit", "--jq", ".resources.core.reset"})
		until := rateLimitUntil(time.Now(), reset)
		pacific, _ := time.LoadLocation("America/Los_Angeles")
		fmt.Fprintf(os.Stderr, "ship: GitHub rate limit on gh %s; waiting until %s to go on\n", argv[0], until.In(pacific).Format("3:04 PM"))
		if serr := sleepCtx(ctx, time.Until(until)); serr != nil {
			return "", errors.Join(err, serr)
		}
	}
}
