// Package web fetches, chunks, indexes, and serves web pages as token-bounded
// outlines, sections, and search hits.
package web

import (
	"errors"
	"time"
)

// Tier names a fetch backend in the cascade. jina runs always-on; the rest are
// enabled only when their API key is set, with plain http as the keyless
// fallback and browserbase as the stealth backstop.
type Tier string

const (
	// TierJina is the always-on r.jina.ai reader (keyless, IP-limited).
	TierJina Tier = "jina"
	// TierExa is the Exa /contents endpoint (EXA_API_KEY).
	TierExa Tier = "exa"
	// TierFirecrawl is the Firecrawl /v2/scrape endpoint (FIRECRAWL_API_KEY).
	TierFirecrawl Tier = "firecrawl"
	// TierHTTP is the plain HTTP GET fallback.
	TierHTTP Tier = "http"
	// TierBrowserbase is the stealth Browserbase fetch (BROWSERBASE_API_KEY).
	TierBrowserbase Tier = "browserbase"
	// TierJinaRender is the jina reader with headless-browser rendering, the
	// first lane of the thin-content render escalation (JINA_API_KEY).
	TierJinaRender Tier = "jina-render"
	// TierFirecrawlRender is Firecrawl's scrape with a client-side render wait,
	// the escalation's second lane (FIRECRAWL_API_KEY).
	TierFirecrawlRender Tier = "firecrawl-render"
	// TierAgentBrowser is the local agent-browser CLI, the escalation's last lane
	// and the only one that runs for local targets.
	TierAgentBrowser Tier = "agent-browser"
)

// Sentinel fetch failures. A cascade wraps these with %w so callers branch with
// errors.Is; internal/cli maps them onto exit codes (ErrGone -> not-found).
var (
	// ErrGone reports that the target no longer exists (HTTP 404/410, or a jina
	// 200 body carrying data.warning). It aborts the cascade from any tier.
	ErrGone = errors.New("target is gone")
	// ErrAuthRequired reports that the target demands authentication (HTTP 401).
	ErrAuthRequired = errors.New("authentication required")
	// ErrBlocked reports that the target requires a stealth fetch (a challenge
	// signature or 403/429/503) and no browserbase tier was available. The
	// wrapping error names the env var that would enable it.
	ErrBlocked = errors.New("blocked: page requires a stealth fetch")
)

// FetchResult is the raw output of the fetch cascade for one URL, before
// extraction and chunking. Exactly one of Markdown or HTML is populated:
// markdown-returning tiers (jina, firecrawl, browserbase) set Markdown, while
// HTML-returning tiers (http, exa) set HTML for the local extractor. ETag and
// LastMod carry the response headers used for conditional revalidation.
type FetchResult struct {
	Tier     Tier
	FinalURL string
	Title    string
	Markdown string
	HTML     string
	ETag     string
	LastMod  string
	// Links is the page's link summary as [text, url] pairs in page (nav) order,
	// populated only by the jina render pass. renderFetch appends it as a ## Links
	// section to the winning result's Markdown after thinness is classified, so it
	// never pads a thin body over the floor. See renderFetch.
	Links [][]string
}

// Page is a fetched, extracted, and chunked web page — the unit persisted to
// the cache as one gzipped JSON file per normalized URL.
type Page struct {
	Version    int
	URL        string // normalized, the cache key source
	FinalURL   string // after redirects
	Title      string
	Tier       Tier
	Thin       bool // the served body is a thin client-side shell no render lane could improve
	FetchedAt  time.Time
	ETag       string
	LastMod    string
	ContentSHA string // hex sha256 of Markdown, compared on refetch
	Markdown   string // every byte belongs to exactly one Chunk
	RawHTML    string // source HTML when a tier returned HTML, else empty
	Sections   []Section
	Chunks     []Chunk
	Vectors    [][]float32 // per-chunk embeddings, lazily filled on first search
	EmbedModel string      // model that produced Vectors; a mismatch discards the cache entry on load
}

// Section is one node in a page's heading tree. Start and End are byte offsets
// into Page.Markdown; Start is the heading's line start (not the text after the
// "#"), and End is the next heading of any level (or the document end), so
// sections partition the document — a subtree span is computed by walking
// descendants, not stored.
type Section struct {
	ID     string // dotted path like "2.3.1"; the preamble before the first heading is "0"
	Level  int    // heading depth 1-6; the preamble is 0
	Title  string
	Parent string // ID of the enclosing section, empty for a top-level section
	Start  int
	End    int
}

// Chunk is a token-bounded slice of Page.Markdown; its text is Markdown[Start:End),
// never stored twice.
type Chunk struct {
	Index      int
	Section    string // ID of the deepest Section containing this chunk
	Breadcrumb string // heading path like "Getting Started > Install"
	Start      int
	End        int
	Hash       string // 4-char anchor.Of hash (Crockford base32) of the chunk text, for cites
}
