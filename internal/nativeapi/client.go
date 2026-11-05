// Package nativeapi publishes native GitHub stacks and reads GitHub or Trunk queue state.
package nativeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/version"
)

const (
	baseURLProd    = "https://api.github.com"
	requestTimeout = 30 * time.Second
)

var (
	secondaryLimitWaits = []time.Duration{time.Minute, 2 * time.Minute}
	transientWaits      = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}
)

// Client publishes GitHub PRs and reads the explicitly selected merge queue.
type Client struct {
	base          string
	http          *http.Client
	tokens        *tokenSource
	queueBase     string
	trunkSSM      *trunkSSMSource
	queueProvider string
	charge        func(ctx context.Context, identity, target string, header http.Header) error
	appToken      func(ctx context.Context, nameWithOwner string) (string, error)
}

var defaultClient = sync.OnceValue(func() *Client {
	c := New(baseURLProd)
	c.charge = func(ctx context.Context, identity, target string, header http.Header) error {
		return ghapi.Default().Record(ctx, identity, target, header)
	}
	c.appToken = func(ctx context.Context, nameWithOwner string) (string, error) {
		token, _, err := ghapi.Default().ForRepo(nameWithOwner).AppToken(ctx, ghapi.AppRefreshMargin)
		return token, err
	}
	return c
})

// Default shares GitHub authentication and HTTP connections within this process.
func Default() *Client { return defaultClient() }

// New binds publication and metadata reads to a GitHub API root.
func New(baseURL string) *Client {
	return &Client{base: strings.TrimSuffix(baseURL, "/"), http: &http.Client{}, tokens: &tokenSource{resolve: resolveToken}, queueBase: "https://api.trunk.io"}
}

// WithQueueProvider selects queue reads without changing publication credentials.
func (c *Client) WithQueueProvider(provider string) *Client {
	selected := *c
	selected.queueProvider = provider
	return &selected
}

// NewWithToken uses the supplied GitHub credential for an isolated API endpoint.
func NewWithToken(baseURL, token string) *Client {
	c := New(baseURL)
	c.tokens.resolve = func() (string, error) { return token, nil }
	return c
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) post(ctx context.Context, path string, params any) ([]byte, error) {
	return c.send(ctx, http.MethodPost, path, params)
}

func (c *Client) send(ctx context.Context, method, path string, params any) ([]byte, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("github-stack: encode %s: %w", path, err)
	}
	return c.do(ctx, method, path, body)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	token, err := c.tokens.get()
	if err != nil {
		return nil, err
	}
	return c.request(ctx, c.base, token, ghapi.IdentityUser, method, path, body)
}

func (c *Client) request(ctx context.Context, base, token, identity, method, path string, body []byte) ([]byte, error) {
	limited, failed := 0, 0
	for {
		payload, retryAfter, err := c.attempt(ctx, base, token, identity, method, path, body)
		var wait time.Duration
		switch {
		case retryAfter >= 0 && limited < len(secondaryLimitWaits):
			wait = max(secondaryLimitWaits[limited], retryAfter)
			limited++
		case failed < len(transientWaits) && method != http.MethodPost && ctx.Err() == nil && transient(err):
			wait = transientWaits[failed]
			failed++
		case err != nil && failed > 0:
			return payload, fmt.Errorf("%w (after %d tries)", err, limited+failed+1)
		default:
			return payload, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
	}
}

func transient(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status >= http.StatusInternalServerError
	}
	return errors.As(err, new(net.Error)) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *Client) attempt(ctx context.Context, base, token, identity, method, path string, body []byte) ([]byte, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "ccx-github-stack/"+version.String())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, -1, fmt.Errorf("github-stack: %s %s: %w", method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if c.charge != nil && base == c.base {
		if err := c.charge(ctx, identity, req.URL.String(), resp.Header); err != nil {
			return nil, -1, err
		}
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, -1, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		refusal := statusError(method, req.URL.String(), resp.StatusCode, payload)
		refusal.Header = resp.Header
		if base != c.base {
			return nil, -1, refusal
		}
		return nil, secondaryLimit(resp, payload), refusal
	}
	return payload, -1, nil
}

func secondaryLimit(resp *http.Response, payload []byte) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return -1
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining != "0" && strings.Contains(strings.ToLower(statusMessage(payload)), "rate limit") {
		return 0
	}
	return -1
}

func (c *Client) graphql(ctx context.Context, query string, out any) error {
	payload, err := c.post(ctx, "/graphql", map[string]any{"query": query})
	if err != nil {
		return err
	}
	return decodeGraphQL(payload, out)
}

// readGraphQL runs a query against owner/repo on the GitHub App installation's
// token when the app is installed there, so it draws on the installation's
// quota instead of the user's, and on the user's token otherwise.
func (c *Client) readGraphQL(ctx context.Context, owner, repo, query string, out any) error {
	token := ""
	if c.appToken != nil {
		var err error
		if token, err = c.appToken(ctx, owner+"/"+repo); err != nil {
			return err
		}
	}
	if token == "" {
		return c.graphql(ctx, query, out)
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return fmt.Errorf("github-stack: encode /graphql: %w", err)
	}
	payload, err := c.request(ctx, c.base, token, ghapi.IdentityApp, http.MethodPost, "/graphql", body)
	if err != nil {
		return err
	}
	return decodeGraphQL(payload, out)
}

func decodeGraphQL(payload []byte, out any) error {
	var response struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Path    []any  `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return err
	}
	for _, failure := range response.Errors {
		if failure.Type != "NOT_FOUND" || len(failure.Path) != 2 || failure.Path[0] != "repository" {
			return &ResultError{Route: "graphql", Message: failure.Message}
		}
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return &ResultError{Route: "graphql", Message: "response contains no data"}
	}
	return json.Unmarshal(response.Data, out)
}
