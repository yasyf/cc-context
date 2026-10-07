// Package ghapi calls GitHub's REST and GraphQL APIs over HTTP with the token
// gh already holds, so a poll costs one request instead of one process per call.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/version"
)

const baseURLProd = "https://api.github.com"

// requestTimeout bounds one request. The client carries no timeout of its own,
// so a paginated walk gets this budget per page rather than for the whole walk.
const requestTimeout = 30 * time.Second

// nextRel matches a Link header's rel="next" entry. Matching the bracketed URL
// and its rel together keeps a comma inside a URL from splitting the header.
var nextRel = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// Client issues authenticated GitHub API requests against one API root.
type Client struct {
	base    string
	http    *http.Client
	tokens  *tokenSource
	apps    *appLoader
	repo    string
	retries int
}

var defaultClient = sync.OnceValue(func() *Client {
	c := New(baseURLProd)
	c.apps = defaultAppLoader(baseURLProd)
	return c
})

// Default is the process-wide client against api.github.com. Its token resolves
// once, on the first request any caller makes.
func Default() *Client { return defaultClient() }

// New builds a client rooted at baseURL with a token source of its own. Tests
// point baseURL at an httptest.Server; production callers want Default.
func New(baseURL string) *Client {
	return &Client{
		base:    strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{},
		tokens:  &tokenSource{resolve: resolveToken},
		apps:    &appLoader{load: func(context.Context) (*appSource, error) { return nil, nil }},
		retries: maxRateLimitRetries,
	}
}

// ForRepo returns a copy of c whose reads authenticate as the GitHub App named
// in AppConfigPath when the app is installed on nameWithOwner, so they draw on
// the installation's quota instead of the user's. Writes keep the user's token.
func (c *Client) ForRepo(nameWithOwner string) *Client {
	bound := *c
	bound.repo = strings.ToLower(nameWithOwner)
	return &bound
}

func (c *Client) asUser() *Client {
	user := *c
	user.repo = ""
	return &user
}

// Unwaiting returns a copy of c that hands a rate-limited response straight
// back instead of sitting out a short Retry-After, for a caller that keeps its
// own backoff across processes.
func (c *Client) Unwaiting() *Client {
	unwaiting := *c
	unwaiting.retries = 0
	return &unwaiting
}

// Paginate walks ref's Link rel="next" chain and returns every page's elements
// flattened, the contract `gh api --paginate` gives its callers. A chain that
// names a page the walk already fetched fails with ErrPaginationCycle rather
// than accumulating pages until the caller's context or memory runs out.
func Paginate[T any](ctx context.Context, c *Client, ref string) ([]T, error) {
	var items []T
	seen := map[string]bool{}
	for ref != "" {
		target := c.resolveRef(ref)
		if seen[target] {
			return nil, fmt.Errorf("ghapi: paginate %s: %w", target, ErrPaginationCycle)
		}
		seen[target] = true
		payload, header, _, err := c.do(ctx, http.MethodGet, ref, nil, true)
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(payload, &page); err != nil {
			return nil, fmt.Errorf("ghapi: decode %s: %w", ref, err)
		}
		items = append(items, page...)
		ref = nextLink(header)
	}
	return items, nil
}

// GraphQL posts query with variables and decodes the response's data field into
// T. A body carrying GraphQL errors returns *GraphQLError even though GitHub
// answered 200.
func GraphQL[T any](ctx context.Context, c *Client, query string, variables map[string]any) (T, error) {
	var out T
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return out, fmt.Errorf("ghapi: encode graphql request: %w", err)
	}
	payload, header, asApp, err := c.do(ctx, http.MethodPost, "/graphql", body, !isMutation(query))
	if err != nil {
		return out, err
	}
	var resp struct {
		Data   T                `json:"data"`
		Errors []GraphQLMessage `json:"errors"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return out, fmt.Errorf("ghapi: decode graphql response: %w", err)
	}
	if asApp && slices.ContainsFunc(resp.Errors, func(m GraphQLMessage) bool { return m.Type == "FORBIDDEN" }) {
		return GraphQL[T](ctx, c.asUser(), query, variables)
	}
	if len(resp.Errors) > 0 {
		wait, exhausted := quotaReset(header, time.Now())
		limit, _ := strconv.Atoi(header.Get("X-RateLimit-Limit"))
		return out, &GraphQLError{Messages: resp.Errors, Exhausted: exhausted, RetryAfter: wait, Limit: limit}
	}
	return resp.Data, nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// do re-resolves the token once on a 401, since a watch can outlive the token
// it started with. It reports whether the response came back to the app's token.
func (c *Client) do(ctx context.Context, method, ref string, body []byte, read bool) ([]byte, http.Header, bool, error) {
	target := c.resolveRef(ref)
	var cred credential = c.tokens
	if read {
		var err error
		if cred, err = c.readCredential(ctx); err != nil {
			return nil, nil, false, err
		}
	}
	reresolved := false
	waits := 0
	for {
		asApp := cred != credential(c.tokens)
		token, err := cred.get(ctx)
		if err != nil {
			return nil, nil, false, err
		}
		status, header, payload, err := c.send(ctx, method, target, body, token)
		if err != nil {
			return nil, nil, false, err
		}
		if status == http.StatusUnauthorized && !reresolved {
			reresolved = true
			if err := cred.refresh(ctx, token); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if asApp && status == http.StatusForbidden && integrationDenied(payload) {
			cred = c.tokens
			continue
		}
		if wait, ok := retryDelay(status, header, time.Now()); ok && waits < c.retries {
			waits++
			if err := sleep(ctx, wait); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		if status < 200 || status > 299 {
			return nil, nil, false, statusError(method, target, status, header, payload, time.Now())
		}
		return payload, header, asApp, nil
	}
}

type credential interface {
	get(ctx context.Context) (string, error)
	refresh(ctx context.Context, stale string) error
}

func (c *Client) readCredential(ctx context.Context) (credential, error) {
	src, err := c.readApp(ctx)
	if err != nil || src == nil {
		return c.tokens, err
	}
	if _, _, err := src.token(ctx, c.repo, AppRefreshMargin, ""); err != nil {
		src.unavailable(err)
		return c.tokens, nil
	}
	return &appCredential{src: src, repo: c.repo}, nil
}

func (c *Client) readApp(ctx context.Context) (*appSource, error) {
	if c.repo == "" || userTokenPinned(ctx) {
		return nil, nil
	}
	return c.apps.get(ctx)
}

// AppConfigured reports whether reads may go through a GitHub App at all: one
// is named in AppConfigPath and no GH_TOKEN or GITHUB_TOKEN pins the user's.
func (c *Client) AppConfigured(ctx context.Context) (bool, error) {
	if userTokenPinned(ctx) {
		return false, nil
	}
	src, err := c.apps.get(ctx)
	return src != nil, err
}

// AppToken returns the installation token a gh subprocess reading c's
// repository should carry as GH_TOKEN, with at least margin left, and its
// expiry. It returns "" when reads stay on the user's token.
func (c *Client) AppToken(ctx context.Context, margin time.Duration) (string, time.Time, error) {
	src, err := c.readApp(ctx)
	if err != nil || src == nil {
		return "", time.Time{}, err
	}
	tok, _, err := src.token(ctx, c.repo, margin, "")
	if err != nil {
		src.unavailable(err)
		return "", time.Time{}, nil
	}
	return tok.Token, tok.ExpiresAt, nil
}

func userTokenPinned(ctx context.Context) bool {
	return slices.ContainsFunc(envTokens, func(name string) bool { return strings.TrimSpace(render.Getenv(ctx, name)) != "" })
}

func isMutation(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(query), "mutation")
}

func integrationDenied(payload []byte) bool {
	var body struct {
		Message string `json:"message"`
	}
	return json.Unmarshal(payload, &body) == nil && body.Message == "Resource not accessible by integration"
}

func (c *Client) send(ctx context.Context, method, target string, body []byte, token string) (int, http.Header, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("ghapi: build request %s %s: %w", method, target, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccx-gh/"+version.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("ghapi: %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("ghapi: read %s %s: %w", method, target, err)
	}
	return resp.StatusCode, resp.Header, payload, nil
}

func (c *Client) resolveRef(ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return c.base + "/" + strings.TrimPrefix(ref, "/")
}

func nextLink(header http.Header) string {
	match := nextRel.FindStringSubmatch(header.Get("Link"))
	if match == nil {
		return ""
	}
	return match[1]
}
