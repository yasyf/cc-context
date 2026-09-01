// Package gtapi calls Graphite's CLI API over HTTP with the token gt already
// holds, so a submit costs one request instead of one Node process.
package gtapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/version"
)

const baseURLProd = "https://api.graphite.com/v1"

const requestTimeout = 30 * time.Second

// Client issues authenticated Graphite API requests against one API root.
type Client struct {
	base   string
	http   *http.Client
	tokens *tokenSource
}

var defaultClient = sync.OnceValue(func() *Client { return New(baseURLProd) })

// Default is the process-wide client against api.graphite.com. Its token
// resolves once, on the first request any caller makes.
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

// NewWithToken is New with a fixed token in place of the on-disk one, for
// tests outside this package that pair an httptest.Server with a token of
// their own.
func NewWithToken(baseURL, token string) *Client {
	c := New(baseURL)
	c.tokens.resolve = func() (string, error) { return token, nil }
	return c
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil)
}

func (c *Client) post(ctx context.Context, path string, params any) ([]byte, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("gtapi: encode %s: %w", path, err)
	}
	return c.do(ctx, http.MethodPost, path, body)
}

// unwrapResult decodes the result envelope the /graphite/cli routes answer
// with; check-auth, submit/pull-requests, and mergeability-status answer bare.
func unwrapResult[T any](payload []byte, path string) (T, error) {
	var resp struct {
		Result T `json:"result"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		var zero T
		return zero, fmt.Errorf("gtapi: decode %s: %w", path, err)
	}
	return resp.Result, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	token, err := c.tokens.get()
	if err != nil {
		return nil, err
	}
	target := c.base + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("gtapi: build request %s %s: %w", method, target, err)
	}
	// Graphite's Authorization scheme is the literal word token, not Bearer.
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("User-Agent", "ccx-gt/"+version.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gtapi: %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gtapi: read %s %s: %w", method, target, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(method, target, resp.StatusCode, payload)
	}
	return payload, nil
}
