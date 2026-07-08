package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	return &tiers{
		client:          &http.Client{},
		jinaBase:        startServer(t, "jina", svc.jina),
		exaBase:         startServer(t, "exa", svc.exa),
		firecrawlBase:   startServer(t, "firecrawl", svc.firecrawl),
		browserbaseBase: startServer(t, "browserbase", svc.browserbase),
	}
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

	got, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
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

	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>real</body></html>")
	})
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

	got, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
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

	// The target guard fails the test if plain HTTP is reached: ErrGone must abort.
	_, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
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

	_, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestFetchAuthRequiredFromPlainHTTP(t *testing.T) {
	isolateKeys(t)
	target := startTarget(t, status(http.StatusUnauthorized))
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})

	_, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}

func TestFetchService429Cascades(t *testing.T) {
	isolateKeys(t)
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<html><body>ok</body></html>")
	})
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})

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
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})
	ts := testTiers(t, services{
		jina: jinaClean(t, "Just a moment...", ""),
		browserbase: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{"markdown": "# Real\n\nunblocked content", "title": "Real"})
		},
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
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})
	// browserbase guard: it must not be called without a key.
	ts := testTiers(t, services{jina: jinaClean(t, "Just a moment...", "")})

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
	target := startTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Just a moment...")
	})
	ts := testTiers(t, services{
		jina:        jinaClean(t, "Just a moment...", ""),
		browserbase: status(http.StatusForbidden),
	})

	_, err := ts.fetch(context.Background(), target, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked (browserbase 403)", err)
	}
}

func TestFetchErrorsJoinOnTotalFailure(t *testing.T) {
	isolateKeys(t)
	target := startTarget(t, status(http.StatusInternalServerError))
	ts := testTiers(t, services{jina: status(http.StatusInternalServerError)})

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

	got, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
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

	got, err := ts.fetch(context.Background(), startTarget(t, nil), nil)
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
	target := startTarget(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("If-None-Match = %q, want %q", r.Header.Get("If-None-Match"), `"v1"`)
		}
		w.WriteHeader(http.StatusNotModified)
	})
	ts := testTiers(t, services{jina: status(http.StatusTooManyRequests)})

	_, err := ts.fetch(context.Background(), target, prior)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
}

func startTarget(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	return startServer(t, "http-target", h)
}
