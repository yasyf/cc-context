package web

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update regenerates the golden .md files from the live pipeline. The goldens are
// frozen output: run `go test ./internal/web -run TestExtract -update` once after a
// deliberate pipeline change, eyeball the diff, and commit. Without -update the
// test asserts the current output byte-for-byte against the frozen golden.
var update = flag.Bool("update", false, "regenerate extract golden files")

func TestExtract(t *testing.T) {
	const pageURL = "https://example.com/page"

	tests := []struct {
		name      string
		fixture   string
		wantTitle string
		// contains/notContains encode the semantic expectation each fixture proves,
		// independent of the exact golden bytes.
		contains    []string
		notContains []string
	}{
		{
			// Article-shaped page: readability isolates the <article>, so the nav
			// links and footer never reach the markdown.
			name:      "article",
			fixture:   "article.html",
			wantTitle: "Understanding Widgets — Widget Corp Blog",
			contains: []string{
				"Widgets are the fundamental building blocks",
				"## How Widgets Compose",
			},
			notContains: []string{
				"Copyright 2026", // footer stripped
				"Privacy Policy", // footer stripped
				"](/about)",      // nav stripped
			},
		},
		{
			// Docs-index-shaped page readability rejects (no scoreable prose): the
			// full-body branch runs, keeping the title <h1> readability would drop
			// and leaving relative URLs unresolved (readability's fixRelativeURIs
			// never ran).
			name:      "docs-index",
			fixture:   "docs-index.html",
			wantTitle: "Documentation Index",
			contains: []string{
				"# Documentation",   // title h1 survives -> full-body branch
				"](/guide/install)", // relative URL unresolved -> full-body branch
				"![Installation]",   // image link preserved
			},
			notContains: []string{
				"https://example.com", // full body does not resolve relative URLs
			},
		},
		{
			// Tables + code: the GFM table and strikethrough plugins fire and the
			// fenced code block survives intact.
			name:      "tables-code",
			fixture:   "tables-code.html",
			wantTitle: "Config API Reference",
			contains: []string{
				"| Name   | Type   | Description", // GFM table plugin
				"~~legacy loader~~",               // strikethrough plugin
				"```\nfunc main() {",              // fenced code block
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html, err := os.ReadFile(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatal(err)
			}

			got, title, err := Extract(string(html), pageURL)
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}

			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}

			goldenPath := filepath.Join("testdata", strings.TrimSuffix(tt.fixture, ".html")+".golden.md")
			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			wantMD, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(wantMD) {
				t.Errorf("markdown mismatch with %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, wantMD)
			}

			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("markdown missing %q", want)
				}
			}
			for _, bad := range tt.notContains {
				if strings.Contains(got, bad) {
					t.Errorf("markdown unexpectedly contains %q", bad)
				}
			}
		})
	}
}

func TestTitleTag(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{"present", `<html><head><title> Hello World </title></head><body>hi</body></html>`, "Hello World"},
		{"absent", `<html><head></head><body>hi</body></html>`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := titleTag(tt.html); got != tt.want {
				t.Errorf("titleTag() = %q, want %q", got, tt.want)
			}
		})
	}
}
