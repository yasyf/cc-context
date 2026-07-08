package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// services bundles the hosted-tier handlers for one cascade test; a nil handler
// installs a guard that fails the test if that tier is ever hit.
type services struct {
	jina        http.HandlerFunc
	exa         http.HandlerFunc
	firecrawl   http.HandlerFunc
	browserbase http.HandlerFunc
}

// remoteTargetHost is a non-local stand-in for a real target: the localTarget
// gate treats it as fetchable, so a cascade test exercises the hosted tiers. Its
// dial is rerouted onto an httptest listener by the hostMapper.
const (
	remoteTargetHost = "target.example"
	remoteTargetURL  = "http://" + remoteTargetHost + "/page"
)

// hostMapper is a hermetic test RoundTripper: it rewrites each request onto the
// loopback authority registered for its host and refuses any unmapped host, so a
// cascade test drives a non-local target URL while never touching the real
// network. Only plainHTTP dials the target; the hosted tiers keep their own
// httptest base URLs, which testTiers registers here.
type hostMapper struct {
	base  http.RoundTripper
	hosts map[string]string
}

func (m *hostMapper) RoundTrip(req *http.Request) (*http.Response, error) {
	authority, ok := m.hosts[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("hostMapper: refusing request to unmapped host %q", req.URL.Host)
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = authority
	return m.base.RoundTrip(req)
}

func isolateKeys(t *testing.T) {
	t.Helper()
	t.Setenv(envJinaKey, "")
	t.Setenv(envExaKey, "")
	t.Setenv(envFirecrawlKey, "")
	t.Setenv(envBrowserbaseKey, "")
}

// startServer runs an httptest server for h, or a "must not be reached" guard
// when h is nil, and returns its base URL.
func startServer(t *testing.T, name string, h http.HandlerFunc) string {
	t.Helper()
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("%s tier hit unexpectedly: %s %s", name, r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func testTiers(t *testing.T, svc services) *tiers {
	t.Helper()
	jina := startServer(t, "jina", svc.jina)
	exa := startServer(t, "exa", svc.exa)
	firecrawl := startServer(t, "firecrawl", svc.firecrawl)
	browserbase := startServer(t, "browserbase", svc.browserbase)

	m := &hostMapper{base: &http.Transport{}, hosts: map[string]string{}}
	for _, base := range []string{jina, exa, firecrawl, browserbase} {
		host := mustHost(t, base)
		m.hosts[host] = host
	}
	return &tiers{
		client:          &http.Client{Transport: m},
		jinaBase:        jina,
		exaBase:         exa,
		firecrawlBase:   firecrawl,
		browserbaseBase: browserbase,
	}
}

// serveRemoteTarget starts a target server for h, maps the non-local
// remoteTargetHost onto it in the shared client, and returns remoteTargetURL to
// hand fetch: the gate treats the target as remote while plainHTTP still reaches h.
func serveRemoteTarget(t *testing.T, ts *tiers, h http.HandlerFunc) string {
	t.Helper()
	mapperOf(ts).hosts[remoteTargetHost] = startListener(t, h)
	return remoteTargetURL
}

// serveLocalTarget starts a loopback target server for h, registers its own
// authority in the shared client, and returns its local URL — so the localTarget
// gate fires while plainHTTP can still reach h.
func serveLocalTarget(t *testing.T, ts *tiers, h http.HandlerFunc) string {
	t.Helper()
	addr := startListener(t, h)
	mapperOf(ts).hosts[addr] = addr
	return "http://" + addr + "/page"
}

// startListener starts an httptest server for h and returns its host:port.
func startListener(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return mustHost(t, srv.URL)
}

func mapperOf(ts *tiers) *hostMapper { return ts.client.Transport.(*hostMapper) }

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u.Host
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// jinaClean returns a jina handler serving markdown content in the JSON envelope.
func jinaClean(t *testing.T, content, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": map[string]any{"content": content, "title": title, "url": "https://example.com/final"},
		})
	}
}

// status returns a handler that writes only status, standing in for a service
// that rate-limits or errors.
func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestFetchJinaSuccess(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: jinaClean(t, "# Doc\n\nhello", "Doc")})

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierJina {
		t.Errorf("Tier = %q, want %q", got.Tier, TierJina)
	}
	if got.Markdown != "# Doc\n\nhello" {
		t.Errorf("Markdown = %q", got.Markdown)
	}
	if got.HTML != "" {
		t.Errorf("HTML = %q, want empty (jina returns markdown)", got.HTML)
	}
	if got.Title != "Doc" {
		t.Errorf("Title = %q, want %q", got.Title, "Doc")
	}
}

func TestFetchKeyUnsetTiersSkipped(t *testing.T) {
	isolateKeys(t) // exa and firecrawl keys stay empty
	var exaHits, fcHits atomic.Int32

	ts := testTiers(t, services{
		jina: status(http.StatusTooManyRequests),
		// Both are wired to succeed; the key gate must keep them from running.
		exa: func(w http.ResponseWriter, _ *http.Request) {
			exaHits.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"results": []any{map[string]any{"text": "<p>x</p>"}}})
		},
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			fcHits.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"markdown": "x", "metadata": map[string]any{"statusCode": 200}}})
		},
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>real</body></html>")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierHTTP {
		t.Errorf("Tier = %q, want %q", got.Tier, TierHTTP)
	}
	if exaHits.Load() != 0 {
		t.Errorf("exa hit %d times with EXA_API_KEY unset, want 0", exaHits.Load())
	}
	if fcHits.Load() != 0 {
		t.Errorf("firecrawl hit %d times with FIRECRAWL_API_KEY unset, want 0", fcHits.Load())
	}
}

func TestFetchCascadeOrderJinaThenExa(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envExaKey, "exa-key")
	var jinaHits, exaHits atomic.Int32

	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, _ *http.Request) {
			jinaHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		},
		exa: func(w http.ResponseWriter, _ *http.Request) {
			exaHits.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"results": []any{map[string]any{"title": "Exa", "text": "<h1>Exa</h1><p>body</p>", "url": "https://example.com/x"}},
			})
		},
	})

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierExa {
		t.Errorf("Tier = %q, want %q", got.Tier, TierExa)
	}
	if got.HTML != "<h1>Exa</h1><p>body</p>" {
		t.Errorf("HTML = %q, want the exa text (routed to the local extractor)", got.HTML)
	}
	if got.Markdown != "" {
		t.Errorf("Markdown = %q, want empty (exa returns HTML)", got.Markdown)
	}
	if jinaHits.Load() != 1 || exaHits.Load() != 1 {
		t.Errorf("hits jina=%d exa=%d, want jina tried before exa (1,1)", jinaHits.Load(), exaHits.Load())
	}
}

func TestFetchGoneFromJina200Trap(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, _ *http.Request) {
			// The 200-trap: HTTP 200, target failure in data.warning.
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": map[string]any{"warning": "Target URL returned error 404: Not Found"},
			})
		},
	})

	// The target is unmapped: ErrGone from jina must abort before plain HTTP dials.
	_, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestFetchGoneFromFirecrawlStatusCode(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envFirecrawlKey, "fc-key")
	ts := testTiers(t, services{
		jina: status(http.StatusTooManyRequests),
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			// Service-level 200, but the target's own status is 404.
			writeJSON(t, w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"markdown": "", "metadata": map[string]any{"statusCode": 404}},
			})
		},
	})

	_, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestFetchAuthRequiredFromPlainHTTP(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})
	target := serveRemoteTarget(t, ts, status(http.StatusUnauthorized))

	_, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

func TestFetchService429Cascades(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>ok</body></html>")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierHTTP {
		t.Errorf("Tier = %q, want %q (jina 429 must cascade)", got.Tier, TierHTTP)
	}
}

func TestFetchChallengeIn200RoutesToBrowserbase(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envBrowserbaseKey, "bb-key")

	// jina and the plain-HTTP target both return a Cloudflare challenge in a 200.
	ts := testTiers(t, services{
		jina: jinaClean(t, "Just a moment...", ""),
		browserbase: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{"markdown": "# Real\n\nunblocked content", "title": "Real"})
		},
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierBrowserbase {
		t.Errorf("Tier = %q, want %q", got.Tier, TierBrowserbase)
	}
	if got.Markdown != "# Real\n\nunblocked content" {
		t.Errorf("Markdown = %q, want the browserbase content", got.Markdown)
	}
	// The challenge body must never surface as a successful result.
	if strings.Contains(got.Markdown, "Just a moment") || strings.Contains(got.HTML, "Just a moment") {
		t.Errorf("challenge body leaked into a successful FetchResult: %+v", got)
	}
}

func TestFetchBlockedWithoutBrowserbaseKey(t *testing.T) {
	isolateKeys(t) // BROWSERBASE_API_KEY stays empty
	// browserbase guard: it must not be called without a key.
	ts := testTiers(t, services{jina: jinaClean(t, "Just a moment...", "")})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if !strings.Contains(err.Error(), envBrowserbaseKey) {
		t.Errorf("err = %q, want it to name %s", err, envBrowserbaseKey)
	}
	if got.Markdown != "" || got.HTML != "" {
		t.Errorf("blocked fetch returned content: %+v", got)
	}
}

func TestFetchBrowserbase403Blocked(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envBrowserbaseKey, "bb-key")
	ts := testTiers(t, services{
		jina:        jinaClean(t, "Just a moment...", ""),
		browserbase: status(http.StatusForbidden),
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})

	_, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked (browserbase 403)", err)
	}
}

func TestFetchErrorsJoinOnTotalFailure(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: status(http.StatusInternalServerError)})
	target := serveRemoteTarget(t, ts, status(http.StatusInternalServerError))

	_, err := ts.fetch(context.Background(), target, nil)
	if err == nil {
		t.Fatal("fetch: want an error when every tier fails")
	}
	// Not a target-level sentinel: an ordinary total failure.
	if errors.Is(err, ErrGone) || errors.Is(err, ErrBlocked) || errors.Is(err, ErrAuthRequired) {
		t.Errorf("err = %v, want a plain joined failure", err)
	}
	for _, want := range []string{string(TierJina), string(TierHTTP), "500"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestFetchExaSuccessRoutesHTML(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envExaKey, "exa-key")
	ts := testTiers(t, services{
		jina: status(http.StatusTooManyRequests),
		exa: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"results": []any{map[string]any{"title": "Exa", "text": "<h1>Exa</h1>", "url": "https://example.com/x"}},
			})
		},
	})

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierExa || got.HTML != "<h1>Exa</h1>" || got.Markdown != "" {
		t.Errorf("got = %+v, want exa HTML result", got)
	}
}

func TestFetchFirecrawlSuccess(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envFirecrawlKey, "fc-key")
	ts := testTiers(t, services{
		jina: status(http.StatusTooManyRequests),
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"markdown": "# FC\n\nbody", "metadata": map[string]any{"statusCode": 200, "title": "FC"}},
			})
		},
	})

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierFirecrawl || got.Markdown != "# FC\n\nbody" || got.Title != "FC" {
		t.Errorf("got = %+v, want firecrawl markdown result", got)
	}
}

func TestFetchNotModifiedRevalidation(t *testing.T) {
	isolateKeys(t)
	prior := &Page{ETag: `"v1"`, LastMod: "Mon, 07 Jul 2026 12:00:00 GMT"}
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("If-None-Match = %q, want %q", r.Header.Get("If-None-Match"), `"v1"`)
		}
		w.WriteHeader(http.StatusNotModified)
	})

	_, err := ts.fetch(context.Background(), target, prior)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
}

func TestFetchLocalTargetSkipsHostedTiers(t *testing.T) {
	isolateKeys(t)
	// Every key set: only the local gate can keep the hosted tiers from running.
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envExaKey, "exa-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	t.Setenv(envBrowserbaseKey, "bb-key")

	ts := testTiers(t, services{}) // every hosted tier is a "must not be reached" guard
	target := serveLocalTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>local only</body></html>")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierHTTP {
		t.Errorf("Tier = %q, want %q (local target must use plain HTTP only)", got.Tier, TierHTTP)
	}
	if got.HTML != "<html><body>local only</body></html>" {
		t.Errorf("HTML = %q", got.HTML)
	}
}

func TestFetchLocalTargetBlockedNoBrowserbase(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envBrowserbaseKey, "bb-key") // set, yet browserbase must never be reached

	ts := testTiers(t, services{}) // browserbase is a guard
	target := serveLocalTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...") // a challenge on a local page
	})

	_, err := ts.fetch(context.Background(), target, nil)
	if err == nil {
		t.Fatal("fetch: want a failure for a blocked local target")
	}
	if !errors.Is(err, errStealthRequired) {
		t.Errorf("err = %v, want it to wrap errStealthRequired (local challenge, no fallthrough)", err)
	}
	if errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want the plain stealth failure, not a browserbase ErrBlocked verdict", err)
	}
}

func TestLocalTarget(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{"localhost", "localhost", true},
		{"localhost mixed case", "LocalHost", true},
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"private 10", "10.1.2.3", true},
		{"private 172", "172.16.5.4", true},
		{"private 192", "192.168.1.5", true},
		{"link-local", "169.254.10.20", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"mdns .local", "printer.local", true},
		{"corp .internal", "wiki.internal", true},
		{"public host", "example.com", false},
		{"public dns ip", "8.8.8.8", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localTarget(tt.host); got != tt.want {
				t.Errorf("localTarget(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func startTarget(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	return startServer(t, "http-target", h)
}
