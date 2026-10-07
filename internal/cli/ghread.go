package cli

import (
	"context"
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
