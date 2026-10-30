// Package nativeapi publishes native GitHub stacks and reads GitHub or Trunk queue state.
package nativeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/version"
)

const (
	baseURLProd    = "https://api.github.com"
	requestTimeout = 30 * time.Second
)

// Client publishes GitHub PRs and reads the explicitly selected merge queue.
type Client struct {
	base          string
	http          *http.Client
	tokens        *tokenSource
	queueBase     string
	queueToken    func() (string, error)
	queueProvider string
}

var defaultClient = sync.OnceValue(func() *Client { return New(baseURLProd) })

// Default shares GitHub authentication and HTTP connections within this process.
func Default() *Client { return defaultClient() }

// New binds publication and metadata reads to a GitHub API root.
func New(baseURL string) *Client {
	return &Client{base: strings.TrimSuffix(baseURL, "/"), http: &http.Client{}, tokens: &tokenSource{resolve: resolveToken}, queueBase: "https://api.trunk.io", queueToken: func() (string, error) {
		token := os.Getenv("TRUNK_MERGE_API_KEY")
		if token == "" {
			return "", ErrNoQueueKey
		}
		return token, nil
	}}
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
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("github-stack: %s %s: %w", method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(method, req.URL.String(), resp.StatusCode, payload)
	}
	return payload, nil
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
