package web

import (
	"regexp"
	"strings"
)

// Thin-content thresholds. A markdown-tier body is thin on its face when it
// falls under the markdown floor; an HTML-tier body must fall under the higher
// extract floor AND show structural app-shell evidence, since extraction can
// legitimately yield a short article that must never escalate.
const (
	thinMarkdownFloorTokens = 30
	thinExtractFloorTokens  = 150
	thinShellMinHTMLBytes   = 2048
	thinShellMinScripts     = 4
	// thinPhraseCeiling bounds the noscript-phrase scan to the head of the HTML,
	// mirroring challengeBodyCeiling: a JS-fallback phrase a real article merely
	// quotes deep in its body must not read as a shell marker.
	thinPhraseCeiling = 2048
)

// thinInput is the post-extraction view thinSignature classifies. Markdown is
// the extracted body for every tier; HTML is the source markup, populated only
// for the HTML-returning tiers (http, exa) and empty for the markdown tiers,
// which carry no shell to detect.
type thinInput struct {
	Markdown string
	HTML     string
}

// emptyDivRe matches an empty <div …></div> (only whitespace between the tags)
// and captures its attribute string. rootIDRe then tests that attribute string
// for an id whose value is a framework mount name. The two-step match avoids the
// false matches a single regex fell into — data-id="root", id="app-root",
// id="approot" — while still accepting spacing (id = "app") and unquoted values.
var (
	emptyDivRe = regexp.MustCompile(`(?is)<div\b([^>]*)>\s*</div>`)
	rootIDRe   = regexp.MustCompile(`(?i)(?:^|[\s"'])id\s*=\s*["']?(?:root|app|__next|___gatsby)["']?(?:[\s"']|$)`)
)

// hasEmptyRootDiv reports whether html carries an empty framework mount node —
// the primary app-shell signal: the framework injects the real DOM after load,
// so the served HTML holds an empty <div id="root"> (or app/__next/___gatsby).
func hasEmptyRootDiv(html string) bool {
	for _, m := range emptyDivRe.FindAllStringSubmatch(html, -1) {
		if rootIDRe.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// thinNoscriptPhrases are the visible JS-required fallbacks a client-rendered
// page shows in <noscript>; the create-react-app default leads. data-reactroot
// and __NEXT_DATA__ are deliberately absent — both ride on fully server-rendered
// pages, so they are not shell markers.
var thinNoscriptPhrases = []string{
	"you need to enable javascript to run this app",
	"please enable javascript to continue",
	"this application requires javascript",
}

// thinSignature reports whether in is a thin client-side app shell the render
// chain should try to escalate past. A markdown-tier body (HTML empty) is thin
// purely on falling under the markdown floor. An HTML-tier body must fall under
// the higher extract floor AND carry app-shell evidence, so a short but genuine
// article never escalates.
func thinSignature(in thinInput) bool {
	mdTokens := estimateTokens(in.Markdown)
	if in.HTML == "" {
		return mdTokens < thinMarkdownFloorTokens
	}
	if mdTokens >= thinExtractFloorTokens {
		return false
	}
	// Near-empty extraction is itself shell evidence: an HTML page that runs
	// scripts yet extracts under the markdown floor is a client-rendered shell
	// with a custom mount node no id allowlist would enumerate (e.g. redoc's
	// <div id="container">). The <script> requirement keeps script-less stubs —
	// meta-refresh redirects, tiny plain pages — out.
	if mdTokens < thinMarkdownFloorTokens && strings.Contains(strings.ToLower(in.HTML), "<script") {
		return true
	}
	return appShellEvidence(in.HTML)
}

// appShellEvidence reports whether html structurally resembles a client-rendered
// shell: an empty framework mount node (primary), a large script-heavy body
// (secondary), or a noscript JS-required fallback near the head.
func appShellEvidence(html string) bool {
	if hasEmptyRootDiv(html) {
		return true
	}
	lower := strings.ToLower(html)
	if len(html) >= thinShellMinHTMLBytes && strings.Count(lower, "<script") >= thinShellMinScripts {
		return true
	}
	head := lower
	if len(head) > thinPhraseCeiling {
		head = head[:thinPhraseCeiling]
	}
	for _, p := range thinNoscriptPhrases {
		if strings.Contains(head, p) {
			return true
		}
	}
	return false
}
