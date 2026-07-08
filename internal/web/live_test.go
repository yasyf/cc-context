package web

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The live tests in this file hit the real hosted tiers to verify each service's
// wire shape against production — the one thing the httptest suite cannot do,
// since it mocks the envelopes it is asserting. They are gated off by default:
// nothing runs unless CCX_WEB_LIVE is set, and the credit-spending browserbase
// tests additionally require CCX_WEB_LIVE_BROWSERBASE. CI, which sets neither,
// skips them all.

// requireLiveOptIn skips unless the caller opted into live network tests. Used by
// the always-on jina path, which needs no key.
func requireLiveOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv("CCX_WEB_LIVE") == "" {
		t.Skip("set CCX_WEB_LIVE=1 to run live hosted-tier tests")
	}
}

// requireLive skips unless the caller opted in and set the tier's API key, which
// it returns.
func requireLive(t *testing.T, keyEnv string) string {
	t.Helper()
	requireLiveOptIn(t)
	key := os.Getenv(keyEnv)
	if key == "" {
		t.Skipf("%s not set", keyEnv)
	}
	return key
}

// requirePaidLive additionally gates a browserbase test behind
// CCX_WEB_LIVE_BROWSERBASE, since every browserbase fetch spends credits.
func requirePaidLive(t *testing.T, keyEnv string) string {
	t.Helper()
	key := requireLive(t, keyEnv)
	if os.Getenv("CCX_WEB_LIVE_BROWSERBASE") == "" {
		t.Skip("set CCX_WEB_LIVE_BROWSERBASE=1 to run credit-spending browserbase tests")
	}
	return key
}

// TestLiveJinaKeyed confirms the keyed jina path returns markdown from a real
// 200 — and, by not erroring, that a set JINA_API_KEY does not 401 the way the
// keyless path does in production.
func TestLiveJinaKeyed(t *testing.T) {
	requireLive(t, envJinaKey)
	res, err := newTiers().jina(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("jina live fetch: %v", err)
	}
	if res.Tier != TierJina {
		t.Errorf("Tier = %q, want %q", res.Tier, TierJina)
	}
	if strings.TrimSpace(res.Markdown) == "" {
		t.Error("jina returned empty markdown")
	}
	t.Logf("jina keyed OK: title=%q markdown=%d bytes", res.Title, len(res.Markdown))
}

// TestLiveGoneClassification confirms a real target 404 maps to ErrGone. Live
// evidence showed jina signals target failures via its own HTTP status (a 422),
// not the theorized 200-trap warning, so the deterministic Gone path is the
// plain-HTTP tier reading the origin status directly.
func TestLiveGoneClassification(t *testing.T) {
	requireLiveOptIn(t)
	const missing = "https://raw.githubusercontent.com/yasyf/cc-context/main/does-not-exist-ccx-web-probe"
	_, err := newTiers().plainHTTP(context.Background(), missing, nil)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("plainHTTP on a 404 target: want ErrGone, got %v", err)
	}
	t.Logf("gone classification OK: %v", err)
}

// TestLiveExaTagged verifies exa's includeHtmlTags actually yields tagged HTML —
// the digest's open question. Plain text would collapse every exa page to a
// single root section, so the test extracts and chunks the response and asserts
// the heading structure survives.
func TestLiveExaTagged(t *testing.T) {
	key := requireLive(t, envExaKey)
	res, err := newTiers().exa(context.Background(), "https://go.dev/doc/effective_go", key)
	if err != nil {
		t.Fatalf("exa live fetch: %v", err)
	}
	if strings.TrimSpace(res.HTML) == "" {
		t.Fatal("exa returned empty HTML")
	}
	if !strings.Contains(res.HTML, "<") {
		t.Errorf("exa text carries no '<' — includeHtmlTags may not be honored; head=%.200q", res.HTML)
	}
	md, _, err := Extract(res.HTML, res.FinalURL)
	if err != nil {
		t.Fatalf("extract exa html: %v", err)
	}
	sections, _ := ChunkPage(md)
	if len(sections) <= 1 {
		t.Errorf("exa page chunked to %d sections, want >1 (headings survived); has <h tag=%v",
			len(sections), strings.Contains(res.HTML, "<h"))
	}
	t.Logf("exa OK: html=%d bytes, %d sections after extract", len(res.HTML), len(sections))
}

// TestLiveFirecrawl confirms the firecrawl v2 envelope: a real scrape returns
// data.markdown and, usually, data.metadata.title on the success path.
func TestLiveFirecrawl(t *testing.T) {
	key := requireLive(t, envFirecrawlKey)
	res, err := newTiers().firecrawl(context.Background(), "https://go.dev/doc/effective_go", key)
	if err != nil {
		t.Fatalf("firecrawl live fetch: %v", err)
	}
	if strings.TrimSpace(res.Markdown) == "" {
		t.Error("firecrawl returned empty markdown")
	}
	if res.Title == "" {
		t.Log("note: firecrawl returned empty title (metadata.title absent or renamed)")
	}
	t.Logf("firecrawl OK: title=%q markdown=%d bytes", res.Title, len(res.Markdown))
}

// TestLiveBrowserbaseShape pins the /v1/fetch {markdown,title} envelope and
// resolves the open BROWSERBASE_PROJECT_ID question: the tier sends only
// X-BB-API-Key today, so a project-required error surfaces here as the fix
// signal.
func TestLiveBrowserbaseShape(t *testing.T) {
	key := requirePaidLive(t, envBrowserbaseKey)
	res, err := newTiers().browserbase(context.Background(), "https://example.com", key)
	if err != nil {
		t.Fatalf("browserbase live fetch: %v (BROWSERBASE_PROJECT_ID set=%v)",
			err, os.Getenv("BROWSERBASE_PROJECT_ID") != "")
	}
	if strings.TrimSpace(res.Markdown) == "" {
		t.Error("browserbase returned empty markdown")
	}
	t.Logf("browserbase OK: title=%q markdown=%d bytes", res.Title, len(res.Markdown))
}

// TestLiveStealthEndToEnd drives the whole cascade against notoriously
// bot-protected sites — the one path no unit test can exercise for real. It is a
// probe: it logs which tier served each target and whether the stealth backstop
// engaged, and asserts only the invariant that a successful result never carries
// empty content (a challenge page served as success would).
func TestLiveStealthEndToEnd(t *testing.T) {
	requirePaidLive(t, envBrowserbaseKey)
	targets := []string{
		"https://www.ticketmaster.com",
		"https://www.zillow.com",
		"https://www.g2.com",
	}
	stealthEngaged := false
	for _, u := range targets {
		res, err := Fetch(context.Background(), u, nil)
		switch {
		case err == nil && res.Tier == TierBrowserbase:
			stealthEngaged = true
			t.Logf("%s: served via browserbase (%d bytes markdown)", u, len(res.Markdown))
		case err == nil:
			t.Logf("%s: served via %s without stealth (%d bytes)", u, res.Tier, len(res.Markdown)+len(res.HTML))
		case errors.Is(err, ErrBlocked):
			t.Logf("%s: ErrBlocked (stealth attempted, page unreachable): %v", u, err)
		case errors.Is(err, ErrGone), errors.Is(err, ErrAuthRequired):
			t.Logf("%s: clean typed error: %v", u, err)
		default:
			t.Logf("%s: cascade failure (likely transient): %v", u, err)
		}
		if err == nil && strings.TrimSpace(res.Markdown) == "" && strings.TrimSpace(res.HTML) == "" {
			t.Errorf("%s: success with empty content — a challenge page may have been served as success", u)
		}
	}
	if !stealthEngaged {
		t.Log("note: no target routed through browserbase this run — the sites served without challenging, or all cascaded to a clean error; stealth path not exercised end to end")
	}
}
