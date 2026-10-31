// Package nativeapi publishes native GitHub stacks and reads GitHub or Trunk queue state.
package nativeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/version"
)

const (
	baseURLProd    = "https://api.github.com"
	requestTimeout = 30 * time.Second
)

var secondaryLimitWaits = []time.Duration{time.Minute, 2 * time.Minute}

// Client publishes GitHub PRs and reads the explicitly selected merge queue.
type Client struct {
	base          string
	http          *http.Client
	tokens        *tokenSource
	queueBase     string
	trunkSSM      *trunkSSMSource
	queueProvider string
}

var defaultClient = sync.OnceValue(func() *Client { return New(baseURLProd) })

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
	return c.request(ctx, c.base, token, method, path, body)
}

func (c *Client) request(ctx context.Context, base, token, method, path string, body []byte) ([]byte, error) {
	for _, wait := range secondaryLimitWaits {
		payload, retryAfter, err := c.attempt(ctx, base, token, method, path, body)
		if retryAfter < 0 {
			return payload, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, ctx.Err())
		case <-time.After(max(wait, retryAfter)):
		}
	}
	payload, _, err := c.attempt(ctx, base, token, method, path, body)
	return payload, err
}

func (c *Client) attempt(ctx context.Context, base, token, method, path string, body []byte) ([]byte, time.Duration, error) {
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
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, -1, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, secondaryLimit(resp, payload), statusError(method, req.URL.String(), resp.StatusCode, payload)
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
	var response struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return err
	}
	if len(response.Errors) > 0 {
		return &ResultError{Route: "graphql", Message: response.Errors[0].Message}
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return &ResultError{Route: "graphql", Message: "response contains no data"}
	}
	return json.Unmarshal(response.Data, out)
}
