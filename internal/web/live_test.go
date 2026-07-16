package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/lookpath"
)

// TestLiveJinaRenderPass confirms the jina render pass returns markdown from a
// real JS-heavy page and pins the data.links shape: an array of [text, url]
// pairs. jina carries the summary on FetchResult.Links (renderFetch appends it),
// so a passing decode plus well-formed pairs validate that shape against
// production — a different shape would fail the envelope decode inside jina.
func TestLiveJinaRenderPass(t *testing.T) {
	requireLive(t, envJinaKey)
	ctx, cancel := context.WithTimeout(context.Background(), jinaTimeout+5*time.Second)
	defer cancel()

	res, err := newTiers().jina(ctx, "https://excalidraw.com", true)
	if err != nil {
		t.Fatalf("jina render fetch: %v", err)
	}
	if res.Tier != TierJinaRender {
		t.Errorf("Tier = %q, want %q", res.Tier, TierJinaRender)
	}
	if strings.TrimSpace(res.Markdown) == "" {
		t.Error("jina render returned empty markdown")
	}
	for i, pair := range res.Links {
		if len(pair) < 2 {
			t.Errorf("links[%d] = %v, want a [text, url] pair", i, pair)
		}
	}
	if len(res.Links) == 0 {
		t.Log("jina render OK but returned no link summary")
	} else {
		t.Logf("jina render OK: %d links, first=%v", len(res.Links), res.Links[0])
	}
}

// TestLiveAgentBrowserRenderedRead drives the real agent-browser lane against a
// live page, skipping when the binary is absent.
func TestLiveAgentBrowserRenderedRead(t *testing.T) {
	requireLiveOptIn(t)
	if lookpath.Find(agentBrowserBin) == "" {
		t.Skipf("%s not on PATH", agentBrowserBin)
	}
	res, err := newTiers().agentBrowser(context.Background(), "https://example.com", false)
	if err != nil {
		t.Fatalf("agent-browser rendered read: %v", err)
	}
	if res.Tier != TierAgentBrowser {
		t.Errorf("Tier = %q, want %q", res.Tier, TierAgentBrowser)
	}
	if strings.TrimSpace(res.Markdown) == "" {
		t.Error("agent-browser returned empty markdown")
	}
	t.Logf("agent-browser OK: title=%q markdown=%d bytes", res.Title, len(res.Markdown))
}

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
	res, err := newTiers().jina(context.Background(), "https://example.com", false)
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
	res, err := newTiers().firecrawl(context.Background(), "https://go.dev/doc/effective_go", key, false)
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
// bot-protected sites. It is a probe, not the escalation proof: these targets
// usually serve through jina without ever challenging, so the stealth backstop
// rarely engages here. TestLiveCascadeEscalatesToBrowserbase is the deterministic
// proof. This one logs which tier served each target and asserts the invariant
// that a successful result never carries empty content (a challenge page served
// as success would).
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
		t.Log("note: no target routed through browserbase this run — the sites served without challenging, or all cascaded to a clean error; see TestLiveCascadeEscalatesToBrowserbase for the deterministic escalation proof")
	}
}

// liveBrowserUA is sent on the raw probes so a benign target answers 200 rather
// than 403-ing a bare Go client, reproducing the collision-prone bytes Bug 1 hit.
const liveBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// liveGet performs a plain GET that bypasses the tier cascade's status
// classification, exposing the real response bytes and headers regardless of
// status — the input challengeSignature is asserted over directly. A transport
// error skips the row, since network flakiness must never redden the suite. It
// uses net/http.NewRequestWithContext (not http.Get) to stay off gosec G107.
func liveGet(ctx context.Context, t *testing.T, u string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("build request %q: %v", u, err)
	}
	req.Header.Set("User-Agent", liveBrowserUA)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Skipf("%s: transport error (transient): %v", u, err)
	}
	body, err := readLimited(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Skipf("%s: read body: %v", u, err)
	}
	return resp, body
}

// hostLabel is a subtest name for a target URL — its host, scheme and path
// stripped.
func hostLabel(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// firedSignal names the first challenge signal in would trip, mirroring
// challengeSignature's own order — used only to explain a live verdict in a log.
func firedSignal(in challengeInput) string {
	if in.Header != nil {
		if strings.EqualFold(in.Header.Get("cf-mitigated"), "challenge") {
			return "header cf-mitigated: challenge"
		}
		if strings.EqualFold(in.Header.Get("x-datadome"), "protected") {
			return "header x-datadome: protected"
		}
	}
	title := strings.ToLower(in.Title)
	for _, m := range challengeTitleMarkers {
		if strings.Contains(title, m) {
			return "title marker " + m
		}
	}
	body := strings.ToLower(in.Body)
	markers := bodyMarkersTight
	if in.Kind == cleanMarkdown {
		markers = bodyMarkersLoose
	}
	for _, m := range markers {
		if markerHit(body, m) {
			return "body marker " + m
		}
	}
	if in.Kind == cleanMarkdown && len(in.Body) <= challengeBodyCeiling &&
		strings.Contains(body, "performing security verification") &&
		strings.Contains(body, "security service") &&
		strings.Contains(body, "not a bot") {
		return "short-body conjunctive fallback"
	}
	return "none"
}

// firstNonPreambleSection returns the ID of the first non-preamble §-section in
// an outline (skipping §0), for a section read whose span is a proper subset of
// the whole page.
func firstNonPreambleSection(t *testing.T, outline string) string {
	t.Helper()
	for _, line := range strings.Split(outline, "\n") {
		i := strings.Index(line, "§")
		if i < 0 {
			continue
		}
		id := line[i+len("§"):]
		if sp := strings.IndexAny(id, " \t"); sp >= 0 {
			id = id[:sp]
		}
		if id == "" || id == "0" {
			continue
		}
		return id
	}
	t.Fatalf("no non-preamble §-section in outline:\n%s", outline)
	return ""
}

// TestLiveJinaChallengeAt200 is Bug 2's live regression: keyed jina fetching a
// real Cloudflare interstitial returns HTTP 200 whose challenge lives only in
// data.title and data.warning. The jina tier must classify it as errStealthRequired,
// never serve it as an article.
func TestLiveJinaChallengeAt200(t *testing.T) {
	requireLive(t, envJinaKey)
	ctx, cancel := context.WithTimeout(context.Background(), jinaTimeout+5*time.Second)
	defer cancel()

	res, err := newTiers().jina(ctx, "https://nopecha.com/demo/cloudflare", false)
	switch {
	case errors.Is(err, errStealthRequired):
		t.Logf("jina classified the interstitial as a challenge: %v", err)
	case err == nil:
		t.Fatalf("jina served the Cloudflare interstitial as content (Bug 2 regressed): title=%q, %d bytes markdown",
			res.Title, len(res.Markdown))
	default:
		t.Skipf("jina transient error (not a challenge classification): %v", err)
	}
}

// TestLiveCascadeEscalatesToBrowserbase proves the whole point of this work: a
// challenge on nopecha does not short-circuit the cascade — every tier is tried,
// then browserbase runs as the stealth backstop. The key order matters: read the
// real browserbase key first, zero every key, restore only browserbase, so keyless
// jina 401s and plain HTTP hits the 403 that flips the stealth flag. With EXA and
// FIRECRAWL zeroed too, the attempt order is exactly jina → http → browserbase.
func TestLiveCascadeEscalatesToBrowserbase(t *testing.T) {
	key := requirePaidLive(t, envBrowserbaseKey)
	isolateKeys(t)
	t.Setenv(envBrowserbaseKey, key)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var mu sync.Mutex
	var order []Tier
	ts := newTiers()
	ts.onAttempt = func(tier Tier, _ error) {
		mu.Lock()
		order = append(order, tier)
		mu.Unlock()
	}

	res, err := ts.fetch(ctx, "https://nopecha.com/demo/cloudflare", nil)

	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()

	if !slices.Contains(got, TierBrowserbase) {
		t.Skipf("browserbase never attempted (target stopped challenging): attempts=%v, tier=%q, err=%v", got, res.Tier, err)
	}

	switch {
	case err == nil && res.Tier == TierBrowserbase:
		t.Logf("escalation reached browserbase and it served the page (%d bytes markdown)", len(res.Markdown))
	case errors.Is(err, ErrBlocked):
		t.Logf("escalation reached browserbase and the challenge persisted (ErrBlocked, browserbase-exclusive): %v", err)
	default:
		t.Skipf("browserbase attempted but errored transiently (neither success nor ErrBlocked): tier=%q, err=%v", res.Tier, err)
	}

	if want := []Tier{TierJina, TierHTTP, TierBrowserbase}; !slices.Equal(got, want) {
		t.Errorf("cascade attempt order = %v, want %v", got, want)
	}
}

// TestLivePlainHTTPDetectsChallenge splits the plain-HTTP challenge check honestly
// in two: a 403 is classified by status before the body is ever scanned, so the
// status subtest proves nothing about marker detection, and a separate subtest
// runs challengeSignature over the real bytes to prove body/header detection.
func TestLivePlainHTTPDetectsChallenge(t *testing.T) {
	requireLiveOptIn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// C1 — status classification: plainHTTP returns errStealthRequired straight
	// from a 403/429/503, before the body is scanned. Proves STATUS handling only.
	t.Run("status", func(t *testing.T) {
		statusTargets := []string{
			"https://nopecha.com/demo/cloudflare",
			"https://www.seloger.com",
			"https://www.leboncoin.fr",
			"https://www.zillow.com",
			"https://www.indeed.com",
			"https://www.glassdoor.com",
		}
		for _, u := range statusTargets {
			t.Run(hostLabel(u), func(t *testing.T) {
				_, err := newTiers().plainHTTP(ctx, u, nil)
				if !errors.Is(err, errStealthRequired) {
					t.Skipf("plainHTTP(%s): want errStealthRequired via status, got %v (site returned 200 or drifted)", u, err)
				}
				t.Logf("%s: status classified to errStealthRequired — proves STATUS handling only, says nothing about marker detection", u)
			})
		}
	})

	// C2 — body signature over real bytes: DataDome sites carry
	// geo.captcha-delivery.com (or the x-datadome header), zillow carries
	// px-captcha, and nopecha fires via the cf-mitigated HEADER, not a body marker.
	t.Run("body signature over real bytes", func(t *testing.T) {
		bodyTargets := []struct {
			name, url, expect string
		}{
			{"seloger-datadome", "https://www.seloger.com", "geo.captcha-delivery.com body marker or x-datadome header"},
			{"leboncoin-datadome", "https://www.leboncoin.fr", "geo.captcha-delivery.com body marker or x-datadome header"},
			{"zillow-px", "https://www.zillow.com", "px-captcha body marker"},
			{"nopecha-header", "https://nopecha.com/demo/cloudflare", "cf-mitigated challenge header (not a body marker)"},
		}
		for _, tt := range bodyTargets {
			t.Run(tt.name, func(t *testing.T) {
				resp, body := liveGet(ctx, t, tt.url)
				if resp.StatusCode == http.StatusOK {
					t.Skipf("%s: returned 200 (stopped challenging) — no challenge bytes to classify", tt.url)
				}
				in := challengeInput{Header: resp.Header, Title: titleTag(body), Body: body, Kind: rawHTML}
				if !challengeSignature(in) {
					t.Errorf("%s: challengeSignature=false over a status-%d, %d-byte body; expected %s",
						tt.url, resp.StatusCode, len(body), tt.expect)
					return
				}
				t.Logf("%s: status=%d, %d bytes, fired via %q (expected %s)",
					tt.url, resp.StatusCode, len(body), firedSignal(in), tt.expect)
			})
		}
	})
}

// TestLiveBenignPagesNotChallenged is the test that would have caught Bug 1: three
// benign 200 pages whose bytes embed a marker substring (ticketmaster's i18n JSON,
// nowsecure's Turnstile widget, wikipedia's px-captcha thumbnail) plus two clean
// controls must all classify as non-challenges over their real bytes.
func TestLiveBenignPagesNotChallenged(t *testing.T) {
	requireLiveOptIn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	targets := []string{
		"https://www.ticketmaster.com",
		"https://nowsecure.nl",
		"https://en.wikipedia.org/wiki/CAPTCHA",
		"https://example.com",
		"https://go.dev",
	}
	for _, u := range targets {
		t.Run(hostLabel(u), func(t *testing.T) {
			resp, body := liveGet(ctx, t, u)
			if resp.StatusCode != http.StatusOK {
				t.Skipf("%s: status %d (a genuine block is not a false positive)", u, resp.StatusCode)
			}
			in := challengeInput{Header: resp.Header, Title: titleTag(body), Body: body, Kind: rawHTML}
			if challengeSignature(in) {
				t.Errorf("%s: FALSE POSITIVE — challengeSignature=true on a benign 200 (%d bytes); fired via %q",
					u, len(body), firedSignal(in))
				return
			}
			t.Logf("%s: benign 200 correctly not challenged (%d bytes)", u, len(body))
		})
	}
}

// TestLiveWebRunOutlineReadSearch is the first live exercise of web.Run itself —
// every other live test drives a tier method, and every unit test stubs fetchPage.
// It fetches go.dev's effective_go once, then outlines, reads, and searches it,
// asserting the 24h page cache short-circuits every refetch: fetchPage runs exactly
// once across all three ops.
func TestLiveWebRunOutlineReadSearch(t *testing.T) {
	requireLiveOptIn(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var fetches atomic.Int32
	prev := fetchPage
	fetchPage = func(ctx context.Context, u string, p *Page) (FetchResult, error) {
		fetches.Add(1)
		return prev(ctx, u, p)
	}
	t.Cleanup(func() { fetchPage = prev })

	ctx, cancel := context.WithTimeout(context.Background(), cascadeDeadline+30*time.Second)
	defer cancel()
	const target = "https://go.dev/doc/effective_go"

	outline, err := Run(ctx, backend.OpWebOutline, backend.Args{URL: target})
	if err != nil {
		t.Skipf("outline fetch failed (likely transient network/upstream): %v", err)
	}
	if n := strings.Count(outline, "§"); n <= 1 {
		t.Fatalf("outline has %d §-refs, want > 1:\n%s", n, outline)
	}

	sec := firstNonPreambleSection(t, outline)
	readSec, err := Run(ctx, backend.OpWebRead, backend.Args{URL: target, Section: sec})
	if err != nil {
		t.Fatalf("read §%s: %v", sec, err)
	}
	if strings.TrimSpace(readSec) == "" {
		t.Fatalf("read of §%s is empty", sec)
	}
	readFull, err := Run(ctx, backend.OpWebRead, backend.Args{URL: target, Full: true})
	if err != nil {
		t.Fatalf("read --full: %v", err)
	}
	if len(readSec) >= len(readFull) {
		t.Errorf("read of §%s is %d bytes, not strictly shorter than --full's %d", sec, len(readSec), len(readFull))
	}

	search, err := Run(ctx, backend.OpWebSearch, backend.Args{URL: target, Query: "how do I handle errors in Go", K: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if Supported() {
		if strings.HasPrefix(search, "# 0 results") {
			t.Errorf("search returned 0 hits: %s", firstLine(search))
		}
		if !strings.Contains(search, "§") || !strings.Contains(search, "#") {
			t.Errorf("search hits carry no §…# cite:\n%s", search)
		}
	} else {
		t.Logf("uv absent: search ran BM25-only, skipping cite-quality assertion (%s)", firstLine(search))
	}

	if got := fetches.Load(); got != 1 {
		t.Errorf("fetchPage called %d times across outline+read+search, want 1 (the 24h page cache must short-circuit refetch)", got)
	}
}

// firstLine returns s up to its first newline, for a compact one-line log.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
