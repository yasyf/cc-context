package web

import (
	"strings"
	"testing"
)

// TestThinSignature is the pure predicate contract. The true rows are genuine
// client-side app shells — framework empty-mount nodes, a script-heavy shell, a
// noscript JS-required fallback, and a sub-floor markdown-tier snapshot. The false
// rows are the false positives the floors and FP discipline must reject: real
// short content over the floor, a redirect stub with no shell evidence, a long
// article that merely names a mount id or quotes the JS phrase, and the
// deliberate non-markers (data-reactroot, __NEXT_DATA__).
func TestThinSignature(t *testing.T) {
	// A large HTML body with four <script> tags and a non-root mount id: only the
	// secondary script-density signal can catch it (no empty root, no phrase).
	scriptHeavy := "<html><head>" + strings.Repeat("<meta>", 400) +
		`<script src="/a.js"></script><script src="/b.js"></script>` +
		`<script src="/c.js"></script><script src="/d.js"></script></head>` +
		`<body><div id="main"></div></body></html>`
	// A genuinely long article (>150 tokens extracted) short-circuits every HTML
	// signal, so quoting the JS phrase or naming id=root never escalates it.
	longArticle := strings.Repeat("real sentence of article prose here. ", 30)
	// The real redocly.github.io/redoc shell (trimmed, <2048 bytes): a custom
	// mount id ("container", not in the allowlist), two <script> tags, and no
	// article — so it evades the empty-root, script-density, and phrase signals
	// and is caught only by the near-empty-extract signal.
	redocShell := `<!doctype html><html><head><meta charset="UTF-8"/><title>Redoc Interactive Demo</title>` +
		`<meta name="description" content="Redoc Interactive Demo. OpenAPI-generated API Reference Documentation"/>` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"/>` +
		`<script defer="defer" src="redoc-demo.bundle.js"></script></head>` +
		`<body><div id="container"></div>` +
		`<script>(function(i,s,o,g,r,a,m){})(window,document,'script','https://www.google-analytics.com/analytics.js','ga');</script></body></html>`

	tests := []struct {
		name string
		in   thinInput
		want bool
	}{
		// Genuine app shells.
		{"react empty root", thinInput{Markdown: "Loading…\n", HTML: `<html><body><div id="root"></div><script src="/main.js"></script></body></html>`}, true},
		{"next empty root", thinInput{Markdown: "\n", HTML: `<div id="__next"></div>`}, true},
		{"vue empty root", thinInput{Markdown: "", HTML: `<div id="app"></div>`}, true},
		{"gatsby empty root", thinInput{Markdown: "", HTML: `<div id="___gatsby"></div>`}, true},
		{"unquoted empty root", thinInput{Markdown: "", HTML: `<div id=root></div>`}, true},
		{"single-quoted app root", thinInput{Markdown: "", HTML: `<div id='app'></div>`}, true},
		{"spaced id equals", thinInput{Markdown: "", HTML: `<div id = "app"></div>`}, true},
		{"uppercase id and value", thinInput{Markdown: "", HTML: `<div ID="ROOT"></div>`}, true},
		{"root id after other attrs", thinInput{Markdown: "", HTML: `<div class="wrap" id="root" data-x="y"></div>`}, true},
		{"script-heavy shell", thinInput{Markdown: "\n", HTML: scriptHeavy}, true},
		{"noscript boilerplate", thinInput{Markdown: "\n", HTML: `<html><body><noscript>You need to enable JavaScript to run this app.</noscript><div id="x"></div></body></html>`}, true},
		{"sub-floor markdown snapshot", thinInput{Markdown: "# App\n\nLoading…\n", HTML: ""}, true},

		// False positives the floors and FP discipline must reject.
		{"tiny but real README", thinInput{Markdown: "# my-tool\n\nA small command-line tool for converting between formats. Install with `brew install my-tool`, then run `my-tool --help` to see the options.\n", HTML: ""}, false},
		{"llms.txt", thinInput{Markdown: "# Project\n\n> A framework for building X.\n\n## Docs\n\n- [Quickstart](https://example.com/quickstart): get started fast\n- [API](https://example.com/api): the full reference\n", HTML: ""}, false},
		{"redirect stub", thinInput{Markdown: "\n", HTML: `<html><head><meta http-equiv="refresh" content="0;url=/new"></head><body></body></html>`}, false},
		{"long article naming id=root", thinInput{Markdown: longArticle, HTML: `<html><body><div id="root"></div>` + longArticle + `</body></html>`}, false},
		{"long article quoting the JS phrase", thinInput{Markdown: longArticle, HTML: `<html><body>` + longArticle + `<noscript>you need to enable javascript to run this app</noscript></body></html>`}, false},
		{"ssr page with data-reactroot", thinInput{Markdown: longArticle, HTML: `<div id="root" data-reactroot>` + longArticle + `</div>`}, false},
		{"empty data-reactroot div is not a marker", thinInput{Markdown: "\n", HTML: `<html><body><div data-reactroot></div><p>a little content</p></body></html>`}, false},
		{"data-id root is not a mount", thinInput{Markdown: "\n", HTML: `<html><body><div data-id="root"></div><p>a little content</p></body></html>`}, false},
		{"hyphenated app-root id is not a mount", thinInput{Markdown: "\n", HTML: `<html><body><div id="app-root"></div><p>a little content</p></body></html>`}, false},
		{"approot id is not a mount", thinInput{Markdown: "\n", HTML: `<html><body><div id="approot"></div><p>a little content</p></body></html>`}, false},
		{"__NEXT_DATA__ script with real content is not a shell", thinInput{
			Markdown: "This is a server-rendered article body with several genuine sentences of real prose that a reader would actually want to read here, comfortably past the extract floor and plainly not a shell.",
			HTML:     `<html><body><p>This is a server-rendered article body with several genuine sentences of real prose that a reader would actually want to read here, comfortably past the extract floor and plainly not a shell.</p><script id="__NEXT_DATA__" type="application/json">{}</script></body></html>`,
		}, false},
		{"non-empty root is not a shell", thinInput{Markdown: "\n", HTML: `<html><body><div id="root"><p>server rendered</p></div></body></html>`}, false},
		{"custom-mount shell with scripts and near-empty extract", thinInput{Markdown: "\n", HTML: redocShell}, true},
		{"script-less container shell stays false", thinInput{Markdown: "\n", HTML: `<!doctype html><html><head><title>Redirecting</title><meta http-equiv="refresh" content="0;url=/new"/>` + strings.Repeat(`<meta name="x" content="y"/>`, 40) + `</head><body><div id="container"></div></body></html>`}, false},
		{"tiny page with a script but real text over the floor", thinInput{
			Markdown: "A short but genuine article with clearly more than thirty tokens of actual readable prose content here now, for real, comfortably past the markdown floor.",
			HTML:     `<html><body><p>A short but genuine article with clearly more than thirty tokens of actual readable prose content here now, for real, comfortably past the markdown floor.</p><script src="/analytics.js"></script></body></html>`,
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := thinSignature(tt.in); got != tt.want {
				t.Errorf("thinSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}
