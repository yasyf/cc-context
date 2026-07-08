package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/version"
)

// Per-tier request timeouts, applied by each tier via context.WithTimeout under
// the whole-cascade deadline (see fetch.go).
const (
	jinaTimeout        = 35 * time.Second
	exaTimeout         = 30 * time.Second
	firecrawlTimeout   = 35 * time.Second
	httpTimeout        = 20 * time.Second
	browserbaseTimeout = 65 * time.Second
)

// Env vars gating the keyed tiers. jina's key is optional (it raises rate
// limits); the rest enable their tier only when set.
const (
	envJinaKey        = "JINA_API_KEY"
	envExaKey         = "EXA_API_KEY"
	envFirecrawlKey   = "FIRECRAWL_API_KEY"
	envBrowserbaseKey = "BROWSERBASE_API_KEY"
)

// Production service base URLs; the tiers struct overrides them in tests.
const (
	jinaBaseProd        = "https://r.jina.ai"
	exaBaseProd         = "https://api.exa.ai"
	firecrawlBaseProd   = "https://api.firecrawl.dev"
	browserbaseBaseProd = "https://api.browserbase.com"
)

// maxBodyBytes caps the plain-HTTP tier's read (and, defensively, every hosted
// tier's JSON response) at 10 MiB via an io.LimitReader.
const maxBodyBytes = 10 << 20

// errStealthRequired is the internal cascade signal that a tier's output is a
// bot/DDoS challenge (or a 403/429/503): the page needs the stealth backstop.
// It never escapes fetch — the orchestrator translates it to a browserbase
// attempt or ErrBlocked.
var errStealthRequired = errors.New("target requires a stealth fetch")

// challengeMarkersTight are the challenge-page-specific substrings scanned
// against raw, un-extracted HTML (the plain-HTTP and exa tiers). A genuine page
// may embed a bot-sensor script — e.g. walmart.com carries window._pxAppId on a
// normal 200 — so the raw path matches only strings unique to an interstitial,
// never the bare "_px"/"datadome" sensor tokens. All markers are lowercase;
// challengeSignature lowercases the body once so matching is case-insensitive.
var challengeMarkersTight = []string{
	"just a moment",
	"challenges.cloudflare.com",
	"cf-chl",
	"px-captcha",
	"geo.captcha-delivery.com",
	"attention required! | cloudflare",
}

// challengeMarkersLoose adds the bare sensor tokens to the tight set for the
// cleaned-markdown tiers (jina, firecrawl, browserbase), where extraction has
// already stripped page scripts so a lone "_px"/"datadome" betrays a challenge.
var challengeMarkersLoose = append([]string{"_px", "datadome"}, challengeMarkersTight...)

// statusInText matches an embedded HTTP error status (4xx/5xx) inside a hosted
// tier's free-text warning, e.g. jina's "Target URL returned error 404".
var statusInText = regexp.MustCompile(`\b([45]\d\d)\b`)

// tiers holds the shared HTTP client, the per-service base URLs, and the DNS
// resolver the split-DNS gate consults. The base URLs default to production and
// are swapped for httptest servers under test; lookupIP defaults to the system
// resolver and is faked in tests.
type tiers struct {
	client          *http.Client
	jinaBase        string
	exaBase         string
	firecrawlBase   string
	browserbaseBase string
	lookupIP        func(ctx context.Context, network, host string) ([]net.IP, error)
}

// newTiers builds the production cascade backends. The shared client refuses a
// redirect onto a local target so a public URL can never be steered onto a
// loopback/private address (see refuseLocalRedirect).
func newTiers() *tiers {
	return &tiers{
		client:          &http.Client{CheckRedirect: refuseLocalRedirect},
		jinaBase:        jinaBaseProd,
		exaBase:         exaBaseProd,
		firecrawlBase:   firecrawlBaseProd,
		browserbaseBase: browserbaseBaseProd,
		lookupIP:        net.DefaultResolver.LookupIP,
	}
}

// refuseLocalRedirect is the shared client's CheckRedirect policy: it follows
// ordinary redirects but refuses a hop whose host is a local target, so a public
// URL cannot be redirected onto a loopback/private address and cached under the
// public key. It preserves the net/http default cap of 10 redirects.
func refuseLocalRedirect(req *http.Request, via []*http.Request) error {
	if localTarget(req.URL.Hostname()) {
		return fmt.Errorf("refusing redirect to local target %q", req.URL.Host)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// jina fetches targetURL through the always-on r.jina.ai reader, returning
// markdown. It requests the JSON envelope so a target failure carried as an
// HTTP-200 body warning (the "200-trap") is caught instead of cached.
func (t *tiers) jina(ctx context.Context, targetURL string) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, jinaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.jinaBase+"/"+targetURL, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("jina: build request: %w", err)
	}
	req.Header.Set("X-Respond-With", "markdown")
	req.Header.Set("Accept", "application/json")
	if key := os.Getenv(envJinaKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("jina: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimited(resp.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("jina: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, serviceFailure(TierJina, resp.StatusCode)
	}

	var env struct {
		Data struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Warning string `json:"warning"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return FetchResult{}, fmt.Errorf("jina: decode envelope: %w", err)
	}

	// data.warning is jina's informational channel — cache-snapshot notices, soft
	// "maybe CAPTCHA" hints — not its error channel; a real target/service failure
	// rides in the HTTP status (handled above). So a warning is only decisive when
	// there is no content to serve: then a status embedded in it (the "200-trap",
	// a target 404 relayed at HTTP 200) is classified, else it explains the miss.
	content := strings.TrimSpace(env.Data.Content)
	if content == "" {
		if w := env.Data.Warning; w != "" {
			if status := statusFromText(w); status != 0 {
				if err := classifyTargetStatus(TierJina, status); err != nil {
					return FetchResult{}, err
				}
			}
			return FetchResult{}, fmt.Errorf("jina: no content: %s", w)
		}
		return FetchResult{}, errors.New("jina: empty content")
	}
	// Content present: a warning is a note, not a failure. An actual challenge
	// page is caught by its content signature below, and a warning that still
	// carries an explicit HTTP error status is honored (gone/auth abort, a
	// 403/429/503 escalates to the stealth backstop).
	if w := env.Data.Warning; w != "" {
		if status := statusFromText(w); status != 0 {
			if err := classifyTargetStatus(TierJina, status); err != nil {
				return FetchResult{}, err
			}
		}
	}
	if challengeSignature(resp.Header, env.Data.Content, challengeMarkersLoose) {
		return FetchResult{}, fmt.Errorf("jina: %w", errStealthRequired)
	}

	final := env.Data.URL
	if final == "" {
		final = targetURL
	}
	return FetchResult{Tier: TierJina, FinalURL: final, Title: env.Data.Title, Markdown: env.Data.Content}, nil
}

// exa fetches targetURL through Exa's /contents endpoint with HTML tags
// preserved, returning HTML for the local extractor so headings survive.
func (t *tiers) exa(ctx context.Context, targetURL, key string) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, exaTimeout)
	defer cancel()

	reqBody := struct {
		URLs []string `json:"urls"`
		Text struct {
			IncludeHTMLTags bool `json:"includeHtmlTags"`
		} `json:"text"`
	}{URLs: []string{targetURL}}
	reqBody.Text.IncludeHTMLTags = true

	resp, err := t.postJSON(ctx, t.exaBase+"/contents", reqBody, map[string]string{"x-api-key": key})
	if err != nil {
		return FetchResult{}, fmt.Errorf("exa: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimited(resp.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("exa: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, serviceFailure(TierExa, resp.StatusCode)
	}

	var out struct {
		Results []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return FetchResult{}, fmt.Errorf("exa: decode response: %w", err)
	}
	if len(out.Results) == 0 || strings.TrimSpace(out.Results[0].Text) == "" {
		return FetchResult{}, errors.New("exa: no content")
	}

	r := out.Results[0]
	if challengeSignature(resp.Header, r.Text, challengeMarkersTight) {
		return FetchResult{}, fmt.Errorf("exa: %w", errStealthRequired)
	}
	final := r.URL
	if final == "" {
		final = targetURL
	}
	return FetchResult{Tier: TierExa, FinalURL: final, Title: r.Title, HTML: r.Text}, nil
}

// firecrawl fetches targetURL through Firecrawl's /v2/scrape endpoint, returning
// markdown. The target's own status rides in data.metadata.statusCode, so a
// service-level 200 can still report a gone target.
func (t *tiers) firecrawl(ctx context.Context, targetURL, key string) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, firecrawlTimeout)
	defer cancel()

	reqBody := struct {
		URL             string   `json:"url"`
		Formats         []string `json:"formats"`
		OnlyMainContent bool     `json:"onlyMainContent"`
	}{URL: targetURL, Formats: []string{"markdown"}, OnlyMainContent: true}

	resp, err := t.postJSON(ctx, t.firecrawlBase+"/v2/scrape", reqBody, map[string]string{"Authorization": "Bearer " + key})
	if err != nil {
		return FetchResult{}, fmt.Errorf("firecrawl: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimited(resp.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("firecrawl: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, serviceFailure(TierFirecrawl, resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Markdown string `json:"markdown"`
			Metadata struct {
				StatusCode int    `json:"statusCode"`
				Title      string `json:"title"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return FetchResult{}, fmt.Errorf("firecrawl: decode response: %w", err)
	}
	if !out.Success {
		return FetchResult{}, errors.New("firecrawl: service reported success=false")
	}
	if sc := out.Data.Metadata.StatusCode; sc != 0 {
		if err := classifyTargetStatus(TierFirecrawl, sc); err != nil {
			return FetchResult{}, err
		}
	}
	if challengeSignature(resp.Header, out.Data.Markdown, challengeMarkersLoose) {
		return FetchResult{}, fmt.Errorf("firecrawl: %w", errStealthRequired)
	}
	if strings.TrimSpace(out.Data.Markdown) == "" {
		return FetchResult{}, errors.New("firecrawl: empty markdown")
	}
	return FetchResult{Tier: TierFirecrawl, FinalURL: targetURL, Title: out.Data.Metadata.Title, Markdown: out.Data.Markdown}, nil
}

// plainHTTP fetches targetURL directly, returning HTML capped at 10 MiB. When
// prior is non-nil it revalidates with conditional headers; a 304 short-circuits
// to ErrNotModified so the caller keeps the prior chunks and vectors.
func (t *tiers) plainHTTP(ctx context.Context, targetURL string, prior *Page) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("http: build request: %w", err)
	}
	req.Header.Set("User-Agent", "ccx-web/"+version.String())
	if prior != nil {
		if prior.ETag != "" {
			req.Header.Set("If-None-Match", prior.ETag)
		}
		if prior.LastMod != "" {
			req.Header.Set("If-Modified-Since", prior.LastMod)
		}
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("http: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if prior != nil && resp.StatusCode == http.StatusNotModified {
		return FetchResult{}, fmt.Errorf("http: %w", ErrNotModified)
	}
	if err := classifyTargetStatus(TierHTTP, resp.StatusCode); err != nil {
		return FetchResult{}, err
	}

	body, err := readLimited(resp.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("http: read body: %w", err)
	}
	if challengeSignature(resp.Header, body, challengeMarkersTight) {
		return FetchResult{}, fmt.Errorf("http: %w", errStealthRequired)
	}

	return FetchResult{
		Tier:     TierHTTP,
		FinalURL: resp.Request.URL.String(),
		HTML:     body,
		ETag:     resp.Header.Get("ETag"),
		LastMod:  resp.Header.Get("Last-Modified"),
	}, nil
}

// browserbase fetches targetURL through Browserbase's stealth proxy, returning
// markdown. A 403 (or a challenge that survives even the proxy) means the page
// cannot be reached: ErrBlocked.
func (t *tiers) browserbase(ctx context.Context, targetURL, key string) (FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, browserbaseTimeout)
	defer cancel()

	reqBody := struct {
		URL            string `json:"url"`
		Format         string `json:"format"`
		Proxies        bool   `json:"proxies"`
		AllowRedirects bool   `json:"allowRedirects"`
	}{URL: targetURL, Format: "markdown", Proxies: true, AllowRedirects: true}

	resp, err := t.postJSON(ctx, t.browserbaseBase+"/v1/fetch", reqBody, map[string]string{"X-BB-API-Key": key})
	if err != nil {
		return FetchResult{}, fmt.Errorf("browserbase: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readLimited(resp.Body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("browserbase: read body: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return FetchResult{}, fmt.Errorf("browserbase: forbidden: %w", ErrBlocked)
	}
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, serviceFailure(TierBrowserbase, resp.StatusCode)
	}

	// The /v1/fetch envelope carries the markdown in "content" and the target's
	// own status in "statusCode" (there is no title field).
	var out struct {
		Content    string `json:"content"`
		StatusCode int    `json:"statusCode"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return FetchResult{}, fmt.Errorf("browserbase: decode response: %w", err)
	}
	// browserbase is the terminal tier, so a target failure it relays is final:
	// gone/auth abort as themselves, anything else ≥ 400 is unreachable-even-with-
	// stealth (ErrBlocked), never errStealthRequired (there is no further tier).
	switch sc := out.StatusCode; {
	case sc == http.StatusNotFound, sc == http.StatusGone:
		return FetchResult{}, fmt.Errorf("browserbase: target returned %d: %w", sc, ErrGone)
	case sc == http.StatusUnauthorized:
		return FetchResult{}, fmt.Errorf("browserbase: target returned 401: %w", ErrAuthRequired)
	case sc >= 400:
		return FetchResult{}, fmt.Errorf("browserbase: target returned %d: %w", sc, ErrBlocked)
	}
	if challengeSignature(resp.Header, out.Content, challengeMarkersLoose) {
		return FetchResult{}, fmt.Errorf("browserbase: challenge persisted: %w", ErrBlocked)
	}
	if strings.TrimSpace(out.Content) == "" {
		return FetchResult{}, errors.New("browserbase: empty content")
	}
	return FetchResult{Tier: TierBrowserbase, FinalURL: targetURL, Markdown: out.Content}, nil
}

// postJSON marshals body and POSTs it with the given headers, returning the live
// response for the caller to read and close.
func (t *tiers) postJSON(ctx context.Context, url string, body any, headers map[string]string) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return t.client.Do(req)
}

// classifyTargetStatus maps an origin status code that a tier observed for the
// target — directly (plain HTTP) or reported in a hosted tier's envelope — onto
// a cascade signal, or nil for a normal (< 400) status. 404/410 abort with
// ErrGone, 401 with ErrAuthRequired, 403/429/503 flag stealth, and any other
// 4xx/5xx is a plain cascade-able failure.
func classifyTargetStatus(tier Tier, status int) error {
	switch {
	case status == http.StatusNotFound, status == http.StatusGone:
		return fmt.Errorf("%s: target returned %d: %w", tier, status, ErrGone)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s: target returned 401: %w", tier, ErrAuthRequired)
	case status == http.StatusForbidden, status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable:
		return fmt.Errorf("%s: target returned %d: %w", tier, status, errStealthRequired)
	case status >= 400:
		return fmt.Errorf("%s: target returned %d", tier, status)
	default:
		return nil
	}
}

// serviceFailure reports a hosted tier's own non-200 status: a service-side
// failure the cascade logs and steps past, distinct from a target failure.
func serviceFailure(tier Tier, status int) error {
	return fmt.Errorf("%s: service returned status %d", tier, status)
}

// challengeSignature reports whether h or body carry a bot/DDoS challenge
// marker. markers is the set for the caller's content class — challengeMarkersTight
// for raw un-extracted HTML, challengeMarkersLoose for cleaned markdown. body is
// matched case-insensitively (the markers are lowercase). h may be nil (hosted
// tiers pass their service headers, which is harmless); the cf-mitigated header
// is authoritative on every path.
func challengeSignature(h http.Header, body string, markers []string) bool {
	if h != nil && strings.EqualFold(h.Get("cf-mitigated"), "challenge") {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// statusFromText extracts the first 4xx/5xx status embedded in free text, or 0.
func statusFromText(s string) int {
	m := statusInText.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// readLimited reads at most maxBodyBytes from r into a string.
func readLimited(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	return string(b), nil
}
