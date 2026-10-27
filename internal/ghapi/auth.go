package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	Pools        []Pool     `json:"pools,omitempty"`
}

// Auth is the identity a repository-bound client's reads and writes resolve
// to. Reason says why reads stay on the user's token when they do.
type Auth struct {
	Repo   string   `json:"repo"`
	Reads  Identity `json:"reads"`
	Writes Identity `json:"writes"`
	Reason string   `json:"reason,omitempty"`
}

// Identities name who a request authenticates as.
const (
	IdentityApp  = "app"
	IdentityUser = "user"
)

const userAuthQuery = `query { viewer { login } rateLimit { limit remaining resetAt } }`

const appAuthQuery = `query { rateLimit { limit remaining resetAt } }`

// Auth reports which identity c's reads and writes on its repository resolve
// to, with each identity's remaining GraphQL quota and every pool the quota
// ledger tracks for it. It mints the app token a
// read would, so it doubles as the check that minting works.
func (c *Client) Auth(ctx context.Context) (Auth, error) {
	c = c.Unfloored()
	writes, err := c.AsUser().userIdentity(ctx)
	if err != nil {
		return Auth{}, err
	}
	if writes.Pools, err = c.AsUser().Pools(ctx, IdentityUser); err != nil {
		return Auth{}, err
	}
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
	quota, spent := spentQuota(err, time.Now())
	switch {
	case spent:
	case err != nil:
		return Auth{}, err
	default:
		quota = &app.RateLimit
	}
	pools, err := c.Pools(ctx, IdentityApp)
	if err != nil {
		return Auth{}, err
	}
	auth.Reads = Identity{Kind: IdentityApp, Name: src.cfg.Name, Installation: id, ExpiresAt: &tok.ExpiresAt, Quota: quota, Pools: pools}
	return auth, nil
}

// userIdentity reads the gh user's login and GraphQL quota. A spent quota
// refuses the GraphQL query that would report it, so the login then comes
// from REST and the quota from the refusal's headers.
func (c *Client) userIdentity(ctx context.Context) (Identity, error) {
	user, err := GraphQL[struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		RateLimit Quota `json:"rateLimit"`
	}](ctx, c, userAuthQuery, nil)
	quota, spent := spentQuota(err, time.Now())
	switch {
	case spent:
		payload, _, _, err := c.do(ctx, http.MethodGet, "/user", nil, false)
		if err != nil {
			return Identity{}, err
		}
		if err := json.Unmarshal(payload, &user.Viewer); err != nil {
			return Identity{}, fmt.Errorf("ghapi: decode /user: %w", err)
		}
	case err != nil:
		return Identity{}, err
	default:
		quota = &user.RateLimit
	}
	return Identity{Kind: IdentityUser, Name: user.Viewer.Login, Quota: quota}, nil
}

// UserRESTQuota reads the gh user's REST core quota. GitHub does not count the
// read against it.
func (c *Client) UserRESTQuota(ctx context.Context) (Quota, error) {
	limits, err := Get[struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}](ctx, c.AsUser(), "/rate_limit")
	if err != nil {
		return Quota{}, err
	}
	core := limits.Resources.Core
	return Quota{Limit: core.Limit, Remaining: core.Remaining, ResetAt: time.Unix(core.Reset, 0).UTC()}, nil
}

func spentQuota(err error, now time.Time) (*Quota, bool) {
	var gql *GraphQLError
	if !errors.As(err, &gql) || !gql.Exhausted {
		return nil, false
	}
	return &Quota{Limit: gql.Limit, ResetAt: now.Add(gql.RetryAfter)}, true
}
