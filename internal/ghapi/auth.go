package ghapi

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Quota is one identity's GraphQL rate limit as GitHub reports it.
type Quota struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// Identity is who a request authenticates as: the gh user, or a GitHub App
// installation along with when its token expires.
type Identity struct {
	Kind         string     `json:"kind"`
	Name         string     `json:"name"`
	Installation int64      `json:"installation,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	Quota        *Quota     `json:"quota,omitempty"`
}

// Auth is the identity a repository-bound client's reads and writes resolve
// to. Reason says why reads stay on the user's token when they do.
type Auth struct {
	Repo   string   `json:"repo"`
	Reads  Identity `json:"reads"`
	Writes Identity `json:"writes"`
	Reason string   `json:"reason,omitempty"`
}

const (
	identityApp  = "app"
	identityUser = "user"
)

const userAuthQuery = `query { viewer { login } rateLimit { limit remaining resetAt } }`

const appAuthQuery = `query { rateLimit { limit remaining resetAt } }`

// Auth reports which identity c's reads and writes on its repository resolve
// to, with each identity's remaining GraphQL quota. It mints the app token a
// read would, so it doubles as the check that minting works.
func (c *Client) Auth(ctx context.Context) (Auth, error) {
	user, err := GraphQL[struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		RateLimit Quota `json:"rateLimit"`
	}](ctx, c.asUser(), userAuthQuery, nil)
	if err != nil {
		return Auth{}, err
	}
	writes := Identity{Kind: identityUser, Name: user.Viewer.Login, Quota: &user.RateLimit}
	auth := Auth{Repo: c.repo, Reads: writes, Writes: writes}
	if userTokenPinned(ctx) {
		auth.Reason = "GH_TOKEN or GITHUB_TOKEN pins the user's token"
		return auth, nil
	}
	src, err := c.apps.get(ctx)
	if err != nil {
		return Auth{}, err
	}
	if src == nil {
		path, err := AppConfigPath(ctx)
		if err != nil {
			return Auth{}, err
		}
		auth.Reason = "no GitHub App configured in " + path
		return auth, nil
	}
	tok, id, err := src.token(ctx, c.repo, AppRefreshMargin, "")
	switch {
	case errors.Is(err, errNotInstalled):
		auth.Reason = fmt.Sprintf("%s is not installed on %s", src.cfg.Name, c.repo)
		return auth, nil
	case err != nil:
		auth.Reason = fmt.Sprintf("%s is unavailable: %v", src.cfg.Name, err)
		return auth, nil
	}
	app, err := GraphQL[struct {
		RateLimit Quota `json:"rateLimit"`
	}](ctx, c, appAuthQuery, nil)
	if err != nil {
		return Auth{}, err
	}
	auth.Reads = Identity{Kind: identityApp, Name: src.cfg.Name, Installation: id, ExpiresAt: &tok.ExpiresAt, Quota: &app.RateLimit}
	return auth, nil
}
