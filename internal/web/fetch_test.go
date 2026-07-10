package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

// challengeBody is a benign sentinel wrapped by the challenge fixtures; a served
// result must never contain it. challengeHTML is a raw-HTML interstitial whose
// <title> trips the title-scoped challenge signal (the raw path no longer flags a
// generic phrase in the body).
const (
	challengeBody = "blocked page body"
	challengeHTML = "<html><head><title>Just a moment...</title></head><body>" + challengeBody + "</body></html>"
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
		lookupIP:        publicLookupIP,
	}
}

// publicLookupIP is testTiers' default DNS gate resolver: it reports a public
// address for every host so a remote test target is never diverted to plain
// HTTP. A test exercising the split-DNS gate overrides ts.lookupIP.
func publicLookupIP(context.Context, string, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
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

// TestFetchJinaBenignWarningServed pins the live finding that data.warning is
// jina's informational channel, not its error channel: a 200 carrying content
// alongside a status-less notice (here a cache-snapshot warning) is served, not
// discarded. Real target failures arrive as an HTTP status, covered elsewhere.
func TestFetchJinaBenignWarningServed(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"content": "# Doc\n\nreal content",
				"title":   "Doc",
				"url":     "https://example.com/final",
				"warning": "This is a cached snapshot of the original page, consider retry with caching opt-out.",
			},
		})
	}})

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierJina || got.Markdown != "# Doc\n\nreal content" {
		t.Errorf("got tier=%q markdown=%q, want jina + the real content", got.Tier, got.Markdown)
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
		jina: jinaClean(t, challengeBody, "Just a moment..."),
		browserbase: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{"content": "# Real\n\nunblocked content", "statusCode": 200})
		},
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, challengeHTML)
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
	// Neither challenge source's content — the jina body sentinel nor the raw-HTML
	// interstitial title — may surface as a successful result.
	for _, sentinel := range []string{challengeBody, "Just a moment"} {
		if strings.Contains(got.Markdown, sentinel) || strings.Contains(got.HTML, sentinel) {
			t.Errorf("challenge content %q leaked into a successful FetchResult: %+v", sentinel, got)
		}
	}
}

// bbChallengeToStealth wires jina + a target that both return a Cloudflare
// challenge, so the cascade escalates to the browserbase handler bb.
func bbChallengeToStealth(t *testing.T, bb http.HandlerFunc) (*tiers, string) {
	t.Helper()
	t.Setenv(envBrowserbaseKey, "bb-key")
	ts := testTiers(t, services{jina: jinaClean(t, challengeBody, "Just a moment..."), browserbase: bb})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, challengeHTML)
	})
	return ts, target
}

// TestFetchBrowserbaseContentEnvelope pins the live /v1/fetch shape: the markdown
// rides in "content" (not "markdown") and there is no title field.
func TestFetchBrowserbaseContentEnvelope(t *testing.T) {
	isolateKeys(t)
	ts, target := bbChallengeToStealth(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "abc", "statusCode": 200, "contentType": "text/markdown",
			"content": "# Real\n\nunblocked content",
		})
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierBrowserbase || got.Markdown != "# Real\n\nunblocked content" {
		t.Errorf("got tier=%q markdown=%q, want browserbase + content-field markdown", got.Tier, got.Markdown)
	}
}

// TestFetchBrowserbaseTargetStatusGone pins that a target 404 relayed in the
// browserbase envelope's statusCode aborts the cascade with ErrGone, since
// browserbase is terminal.
func TestFetchBrowserbaseTargetStatusGone(t *testing.T) {
	isolateKeys(t)
	ts, target := bbChallengeToStealth(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"statusCode": 404, "content": "404: Not Found"})
	})

	if _, err := ts.fetch(context.Background(), target, nil); !errors.Is(err, ErrGone) {
		t.Fatalf("want ErrGone from browserbase statusCode 404, got %v", err)
	}
}

func TestFetchBlockedWithoutBrowserbaseKey(t *testing.T) {
	isolateKeys(t) // BROWSERBASE_API_KEY stays empty
	// browserbase guard: it must not be called without a key.
	ts := testTiers(t, services{jina: jinaClean(t, challengeBody, "Just a moment...")})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, challengeHTML)
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
		jina:        jinaClean(t, challengeBody, "Just a moment..."),
		browserbase: status(http.StatusForbidden),
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, challengeHTML)
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

// TestFetchExaTitlelessChallengeEscalates pins the exa raw-HTML lane to the same
// title derivation as plainHTTP: an envelope with an empty title whose HTML
// carries a challenge <title> must escalate, not serve the interstitial as clean.
func TestFetchExaTitlelessChallengeEscalates(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{exa: func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"results": []any{map[string]any{"title": "", "text": challengeHTML, "url": "https://example.com/x"}},
		})
	}})

	_, err := ts.exa(context.Background(), remoteTargetURL, "exa-key")
	if !errors.Is(err, errStealthRequired) {
		t.Fatalf("err = %v, want errStealthRequired (challenge title only in the raw HTML)", err)
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
		_, _ = io.WriteString(w, challengeHTML) // a challenge on a local page
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

func TestFetchFirecrawlSuccessFalseCascades(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envFirecrawlKey, "fc-key")
	ts := testTiers(t, services{
		jina: status(http.StatusTooManyRequests),
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			// A 200 envelope with success:false carrying a quota page as markdown.
			writeJSON(t, w, http.StatusOK, map[string]any{
				"success": false,
				"data":    map[string]any{"markdown": "# Quota exceeded\n\nupgrade your plan", "metadata": map[string]any{"statusCode": 200}},
			})
		},
	})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>the real page</body></html>")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierHTTP {
		t.Errorf("Tier = %q, want %q (firecrawl success:false must cascade)", got.Tier, TierHTTP)
	}
	if strings.Contains(got.Markdown, "Quota") || strings.Contains(got.HTML, "Quota") {
		t.Errorf("firecrawl quota page leaked into the result: %+v", got)
	}
}

func TestFetchSplitDNSPrivateSkipsHostedTiers(t *testing.T) {
	isolateKeys(t)
	// Every hosted key set: only the split-DNS gate can keep them from running.
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envExaKey, "exa-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	t.Setenv(envBrowserbaseKey, "bb-key")

	ts := testTiers(t, services{}) // every hosted tier is a "must not be reached" guard
	// The public-looking name resolves entirely to a private address.
	ts.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.5")}, nil
	}
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>internal wiki</body></html>")
	})

	got, err := ts.fetch(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierHTTP {
		t.Errorf("Tier = %q, want %q (a private-resolving host must use plain HTTP only)", got.Tier, TierHTTP)
	}
}

func TestFetchSplitDNSPublicUsesHostedTiers(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: jinaClean(t, "# Doc\n\nhi", "Doc")})
	ts.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}, nil // one public IP is enough
	}

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierJina {
		t.Errorf("Tier = %q, want %q (any public address keeps the hosted cascade)", got.Tier, TierJina)
	}
}

func TestFetchSplitDNSErrorFallsThroughToHostedTiers(t *testing.T) {
	isolateKeys(t)
	ts := testTiers(t, services{jina: jinaClean(t, "# Doc\n\nhi", "Doc")})
	ts.lookupIP = func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("dial udp: i/o timeout")
	}

	got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.Tier != TierJina {
		t.Errorf("Tier = %q, want %q (a DNS failure must never block the hosted cascade)", got.Tier, TierJina)
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

// nopechaContent is the verbatim live jina data.content for the real Cloudflare
// interstitial at nopecha.com/demo/cloudflare. It carries no body marker and no
// title in the envelope: its only signal is the short-body interstitial-phrase scan,
// which matches "Performing security verification" — the path that catches a
// titleless interstitial through browserbase.
const nopechaContent = "![Image 1: Icon for nopecha.com](https://nopecha.com/favicon.ico)\n\n## nopecha.com\n\n## Performing security verification\n\nThis website uses a security service to protect against malicious bots. This page is displayed while the website verifies you are not a bot."

// TestChallengeSignature is the pure predicate contract. The false rows are the
// live HTTP-200 false positives that bare substring matching flagged (Bug 1); the
// true rows are the genuine challenges — origin headers, title markers, boundary-
// anchored body markers, and the short-body interstitial-phrase scan.
func TestChallengeSignature(t *testing.T) {
	// A body over challengeBodyCeiling that quotes a rendered-interstitial phrase:
	// only the length ceiling — not phrase absence — keeps it from matching. This is
	// the anti-Bug-1 guard for a long article that merely discusses a challenge page.
	longArticleQuotingPhrase := strings.Repeat("lorem ipsum dolor sit amet. ", 200) +
		"the interstitial reads: verifying you are human."
	tests := []struct {
		name string
		in   challengeInput
		want bool
	}{
		// Bug 1 — live false positives.
		{"ticketmaster i18n json", challengeInput{Body: `{"considerAssignModal.title":"Just a Moment","x":1}`, Kind: rawHTML}, false},
		{"nowsecure turnstile widget", challengeInput{Body: `<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>`, Kind: rawHTML}, false},
		{"wikipedia captcha thumbnail", challengeInput{Body: `![cat](//upload.wikimedia.org/wikipedia/commons/thumb/a/250px-Captchacat.png)`, Kind: cleanMarkdown}, false},
		{"max_px near miss raw", challengeInput{Body: "const max_px = 12;", Kind: rawHTML}, false},
		{"max_px near miss clean", challengeInput{Body: "const max_px = 12;", Kind: cleanMarkdown}, false},
		{"challenge phrase over the ceiling stays false", challengeInput{Body: longArticleQuotingPhrase, Kind: cleanMarkdown}, false},
		// A bare sensor token in a long extracted article is a benign mention: the
		// loose-marker upgrade is short-body only, same rationale as the phrase cap.
		{"datadome mention in long article", challengeInput{
			Body: strings.Repeat("lorem ipsum dolor sit amet. ", 200) + "datadome announced a funding round.",
			Kind: cleanMarkdown,
		}, false},
		{"nil header ignores in-body cf-mitigated", challengeInput{Header: nil, Body: "cf-mitigated: challenge", Kind: cleanMarkdown}, false},
		// Benign short cleanMarkdown bodies that resemble but never contain a phrase.
		{"datadome near-miss benign", challengeInput{Title: "", Body: "Please enable JS to view this page.", Kind: cleanMarkdown}, false},
		{"nowsecure benign extraction", challengeInput{Title: "", Body: "## nowsecure.nl\n\nYou have reached the site ok.", Kind: cleanMarkdown}, false},

		// Genuine challenges.
		{"cf-mitigated origin header", challengeInput{Header: http.Header{"Cf-Mitigated": {"challenge"}}, Body: "a wholly benign article", Kind: rawHTML}, true},
		{"x-datadome origin header", challengeInput{Header: http.Header{"X-Datadome": {"protected"}}, Body: "a wholly benign article", Kind: rawHTML}, true},
		{"just a moment title", challengeInput{Title: "Just a moment...", Body: "benign body", Kind: cleanMarkdown}, true},
		{"attention required title", challengeInput{Title: "Attention Required! | Cloudflare", Body: "benign body", Kind: rawHTML}, true},
		{"px-captcha anchored", challengeInput{Body: `<div id="px-captcha">`, Kind: rawHTML}, true},
		{"datadome delivery host", challengeInput{Body: "see https://geo.captcha-delivery.com/captcha/", Kind: rawHTML}, true},
		{"underscore px anchored clean", challengeInput{Body: `window._pxAppId = "x"`, Kind: cleanMarkdown}, true},
		{"datadome bare token short body", challengeInput{Body: "you have been blocked by datadome", Kind: cleanMarkdown}, true},
		// A short cleanMarkdown body whose entire content is a challenge phrase IS an
		// interstitial: the browserbase lane sees only this rendered text, no title.
		{"single challenge phrase short body", challengeInput{Body: "performing security verification", Kind: cleanMarkdown}, true},
		{"datadome interstitial body titleless", challengeInput{Title: "", Body: "Please enable JS and disable any ad blocker", Kind: cleanMarkdown}, true},
		{"verifying you are human body titleless", challengeInput{Title: "", Body: "Verifying you are human. This may take a few seconds.", Kind: cleanMarkdown}, true},
		{"nopecha interstitial titleless", challengeInput{Title: "", Body: nopechaContent, Kind: cleanMarkdown}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := challengeSignature(tt.in); got != tt.want {
				t.Errorf("challengeSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestFetchJinaWarningClasses drives jina's multi-class data.warning through the
// cascade: a benign notice serves, a CAPTCHA notice escalates to browserbase, a
// status-in-warning aborts (the 200-trap), an unrecognized notice serves, and an
// empty-content notice fails naming the warning.
func TestFetchJinaWarningClasses(t *testing.T) {
	const realContent = "# Doc\n\nreal content"
	const cacheWarning = "This is a cached snapshot of the original page, consider retry with caching opt-out."
	tests := []struct {
		name    string
		content string
		title   string
		warning string
		want    string // served | browserbase | gone | fail
		wantMD  string // markdown a served or browserbase result must carry
	}{
		{"benign cache snapshot served", realContent, "Doc", cacheWarning, "served", realContent},
		{"nopecha captcha warning escalates", nopechaContent, "Just a moment...", "This page maybe requiring CAPTCHA, please make sure you are authorized to access", "browserbase", "# Real\n\nunblocked content"},
		{"status in warning is gone", realContent, "Doc", "Target URL returned error 404: Not Found", "gone", ""},
		{"unclassified warning served", realContent, "Doc", "Quantum flux detected", "served", realContent},
		{"empty content fails naming warning", "", "Doc", cacheWarning, "fail", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateKeys(t)
			svc := services{jina: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"data": map[string]any{"content": tt.content, "title": tt.title, "url": "https://example.com/final", "warning": tt.warning},
				})
			}}
			if tt.want == "browserbase" {
				t.Setenv(envBrowserbaseKey, "bb-key")
				svc.browserbase = func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(t, w, http.StatusOK, map[string]any{"content": tt.wantMD, "statusCode": 200})
				}
			}
			ts := testTiers(t, svc)
			// The target stays unmapped: served/gone resolve at jina; the escalation and
			// empty-content cases let plainHTTP fail plainly so only jina's own stealth
			// signal can reach browserbase.
			got, err := ts.fetch(context.Background(), remoteTargetURL, nil)
			switch tt.want {
			case "served":
				if err != nil {
					t.Fatalf("fetch: %v, want served", err)
				}
				if got.Tier != TierJina || got.Markdown != tt.wantMD {
					t.Errorf("got tier=%q markdown=%q, want jina + %q", got.Tier, got.Markdown, tt.wantMD)
				}
			case "browserbase":
				if err != nil {
					t.Fatalf("fetch: %v, want browserbase escalation", err)
				}
				if got.Tier != TierBrowserbase || got.Markdown != tt.wantMD {
					t.Errorf("got tier=%q markdown=%q, want browserbase + %q", got.Tier, got.Markdown, tt.wantMD)
				}
				if strings.Contains(got.Markdown, "not a bot") {
					t.Errorf("interstitial content leaked into the result: %q", got.Markdown)
				}
			case "gone":
				if !errors.Is(err, ErrGone) {
					t.Fatalf("err = %v, want ErrGone", err)
				}
			case "fail":
				if err == nil {
					t.Fatalf("fetch = %+v, want a plain cascade failure", got)
				}
				if errors.Is(err, ErrGone) || errors.Is(err, ErrBlocked) {
					t.Errorf("err = %v, want a plain failure, not a target sentinel", err)
				}
				if !strings.Contains(err.Error(), tt.warning) {
					t.Errorf("err = %q, want it to name the warning %q", err, tt.warning)
				}
			}
		})
	}
}

// TestFetchPlainHTTPRawHTML drives the raw-HTML lane through the cascade (jina
// forced to a service 429 so plainHTTP is reached): the Bug-1 benign pages serve
// on the http tier, while a real challenge in the headers, title, or an anchored
// body marker escalates to browserbase.
func TestFetchPlainHTTPRawHTML(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		body     string
		escalate bool
	}{
		{"ticketmaster i18n json served", nil, `{"considerAssignModal.title":"Just a Moment","x":1}`, false},
		{"nowsecure turnstile widget served", nil, `<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>`, false},
		{"cf-mitigated header escalates", map[string]string{"cf-mitigated": "challenge"}, "<html><body>ok</body></html>", true},
		{"just a moment title escalates", nil, "<html><head><title>Just a moment...</title></head><body>hi</body></html>", true},
		{"px-captcha anchored escalates", nil, `<html><body><div id="px-captcha"></div></body></html>`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateKeys(t)
			svc := services{jina: status(http.StatusTooManyRequests)}
			if tt.escalate {
				t.Setenv(envBrowserbaseKey, "bb-key")
				svc.browserbase = func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(t, w, http.StatusOK, map[string]any{"content": "# Real\n\nunblocked", "statusCode": 200})
				}
			}
			ts := testTiers(t, svc)
			target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.body)
			})

			got, err := ts.fetch(context.Background(), target, nil)
			if tt.escalate {
				if err != nil {
					t.Fatalf("fetch: %v, want browserbase escalation", err)
				}
				if got.Tier != TierBrowserbase {
					t.Errorf("Tier = %q, want %q (raw-HTML challenge must escalate)", got.Tier, TierBrowserbase)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetch: %v, want the benign page served on the http tier", err)
			}
			if got.Tier != TierHTTP {
				t.Errorf("Tier = %q, want %q (a benign page must not trip the challenge gate)", got.Tier, TierHTTP)
			}
			if got.HTML != tt.body {
				t.Errorf("HTML = %q, want the round-tripped body %q", got.HTML, tt.body)
			}
		})
	}
}

// TestFetchOnAttemptOrder pins the cascade seam: onAttempt observes every tier in
// escalation order, fires before the early typed-error return, and the localTarget
// shortcut records nothing.
func TestFetchOnAttemptOrder(t *testing.T) {
	type attempt struct {
		tier Tier
		err  error
	}
	record := func(ts *tiers) *[]attempt {
		var got []attempt
		ts.onAttempt = func(tier Tier, err error) { got = append(got, attempt{tier, err}) }
		return &got
	}
	order := func(as []attempt) []Tier {
		out := make([]Tier, len(as))
		for i, a := range as {
			out[i] = a.tier
		}
		return out
	}

	t.Run("jina fails then http succeeds", func(t *testing.T) {
		isolateKeys(t)
		ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})
		got := record(ts)
		target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html><body>ok</body></html>")
		})
		if _, err := ts.fetch(context.Background(), target, nil); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if want := []Tier{TierJina, TierHTTP}; !slices.Equal(order(*got), want) {
			t.Errorf("order = %v, want %v", order(*got), want)
		}
	})

	t.Run("escalates jina http browserbase", func(t *testing.T) {
		isolateKeys(t)
		t.Setenv(envBrowserbaseKey, "bb-key")
		ts := testTiers(t, services{
			jina:        jinaClean(t, challengeBody, "Just a moment..."),
			browserbase: func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, http.StatusOK, map[string]any{"content": "# Real\n\nok", "statusCode": 200}) },
		})
		got := record(ts)
		target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, challengeHTML)
		})
		res, err := ts.fetch(context.Background(), target, nil)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if res.Tier != TierBrowserbase {
			t.Errorf("Tier = %q, want browserbase", res.Tier)
		}
		if want := []Tier{TierJina, TierHTTP, TierBrowserbase}; !slices.Equal(order(*got), want) {
			t.Errorf("order = %v, want %v", order(*got), want)
		}
	})

	t.Run("early gone still records jina", func(t *testing.T) {
		isolateKeys(t)
		ts := testTiers(t, services{jina: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{"data": map[string]any{"warning": "Target URL returned error 404: Not Found"}})
		}})
		got := record(ts)
		if _, err := ts.fetch(context.Background(), remoteTargetURL, nil); !errors.Is(err, ErrGone) {
			t.Fatalf("err = %v, want ErrGone", err)
		}
		if want := []Tier{TierJina}; !slices.Equal(order(*got), want) {
			t.Fatalf("order = %v, want %v (onAttempt must fire before the early typed-error return)", order(*got), want)
		}
		if !errors.Is((*got)[0].err, ErrGone) {
			t.Errorf("recorded jina err = %v, want it to wrap ErrGone", (*got)[0].err)
		}
	})

	t.Run("local target records nothing", func(t *testing.T) {
		isolateKeys(t)
		ts := testTiers(t, services{})
		got := record(ts)
		target := serveLocalTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "<html><body>local</body></html>")
		})
		if _, err := ts.fetch(context.Background(), target, nil); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(*got) != 0 {
			t.Errorf("onAttempt fired %v for a local target, want the shortcut to record nothing", order(*got))
		}
	})
}

// TestFetchBlockedNamesEarlierFailures pins Bug 3: with no browserbase key a stealth
// cascade fails with ErrBlocked naming the env var and the earlier tier failures,
// and — because the failures are joined with %v, not %w — errStealthRequired never
// leaks into the errors.Is chain.
func TestFetchBlockedNamesEarlierFailures(t *testing.T) {
	isolateKeys(t) // BROWSERBASE_API_KEY stays unset
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})
	target := serveRemoteTarget(t, ts, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, challengeHTML)
	})

	_, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if errors.Is(err, errStealthRequired) {
		t.Errorf("err = %v, must not leak errStealthRequired (join is %%v, not %%w)", err)
	}
	if !strings.Contains(err.Error(), envBrowserbaseKey) {
		t.Errorf("err = %q, want it to name %s", err, envBrowserbaseKey)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %q, want it to carry the earlier jina failure (429)", err)
	}
}

// TestFetchBrowserbaseServiceFailureNoStealthLeak pins the other join site: when a
// stealth-flagged cascade reaches browserbase and browserbase itself fails for an
// ordinary reason (its own 502, not a target 403), the final joined error carries
// the failures as text — errStealthRequired never escapes fetch on any path.
func TestFetchBrowserbaseServiceFailureNoStealthLeak(t *testing.T) {
	isolateKeys(t)
	ts, target := bbChallengeToStealth(t, status(http.StatusBadGateway))

	_, err := ts.fetch(context.Background(), target, nil)
	if err == nil {
		t.Fatal("fetch: want an error when browserbase fails after a stealth escalation")
	}
	if errors.Is(err, errStealthRequired) {
		t.Errorf("err = %v, must not leak errStealthRequired (join is %%v, not %%w)", err)
	}
	if errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want a plain joined failure, not ErrBlocked (browserbase 502 is a service failure)", err)
	}
	for _, want := range []string{string(TierBrowserbase), "502"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}
