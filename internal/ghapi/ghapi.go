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
	"strings"
	"sync"
	"time"

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
	base   string
	http   *http.Client
	tokens *tokenSource
}

var defaultClient = sync.OnceValue(func() *Client { return New(baseURLProd) })

// Default is the process-wide client against api.github.com. Its token resolves
// once, on the first request any caller makes.
func Default() *Client { return defaultClient() }

// New builds a client rooted at baseURL with a token source of its own. Tests
// point baseURL at an httptest.Server; production callers want Default.
func New(baseURL string) *Client {
	return &Client{
		base:   strings.TrimSuffix(baseURL, "/"),
		http:   &http.Client{},
		tokens: &tokenSource{resolve: resolveToken},
	}
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
		payload, header, err := c.do(ctx, http.MethodGet, ref, nil)
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
	payload, _, err := c.do(ctx, http.MethodPost, "/graphql", body)
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
	if len(resp.Errors) > 0 {
		return out, &GraphQLError{Messages: resp.Errors}
	}
	return resp.Data, nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// do re-resolves the token once on a 401, since a watch can outlive the token
// it started with.
func (c *Client) do(ctx context.Context, method, ref string, body []byte) ([]byte, http.Header, error) {
	target := c.resolveRef(ref)
	reresolved := false
	waits := 0
	for {
		token, err := c.tokens.get(ctx)
		if err != nil {
			return nil, nil, err
		}
		status, header, payload, err := c.send(ctx, method, target, body, token)
		if err != nil {
			return nil, nil, err
		}
		if status == http.StatusUnauthorized && !reresolved {
			reresolved = true
			if err := c.tokens.refresh(ctx, token); err != nil {
				return nil, nil, err
			}
			continue
		}
		if wait, ok := retryDelay(status, header, time.Now()); ok && waits < maxRateLimitRetries {
			waits++
			if err := sleep(ctx, wait); err != nil {
				return nil, nil, err
			}
			continue
		}
		if status < 200 || status > 299 {
			return nil, nil, statusError(method, target, status, payload)
		}
		return payload, header, nil
	}
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
