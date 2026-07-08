package web

import (
	"fmt"
	nurl "net/url"
	"strings"

	readability "codeberg.org/readeck/go-readability"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"
)

// Extract reduces an HTML page to Markdown for the plain-HTTP and exa tiers,
// whose fetchers return HTML rather than ready markdown. It runs readability to
// isolate the main article, then converts that fragment with the commonmark,
// GFM table, and strikethrough plugins. When readability rejects the page — its
// isReadable heuristic legitimately declines non-article pages like docs indexes,
// surfacing as an error or empty content — the full body is converted instead so
// no page yields empty markdown. The title comes from readability when present,
// otherwise from the <title> element. pageURL resolves relative links; it may be
// empty.
func Extract(html string, pageURL string) (markdown string, title string, err error) {
	u, err := nurl.Parse(pageURL)
	if err != nil {
		return "", "", fmt.Errorf("parse page url %q: %w", pageURL, err)
	}

	article, rerr := readability.FromReader(strings.NewReader(html), u)

	source := article.Content
	if rerr != nil || strings.TrimSpace(source) == "" {
		source = html
	}

	markdown, err = htmlToMarkdown(source)
	if err != nil {
		return "", "", fmt.Errorf("convert html to markdown: %w", err)
	}

	title = article.Title
	if title == "" {
		title = titleTag(html)
	}

	return strings.TrimSpace(markdown) + "\n", title, nil
}

// htmlToMarkdown converts an HTML fragment with the commonmark, GFM table, and
// strikethrough plugins registered alongside the required base plugin.
func htmlToMarkdown(html string) (string, error) {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			table.NewTablePlugin(),
			strikethrough.NewStrikethroughPlugin(),
		),
	)

	md, err := conv.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("html-to-markdown: %w", err)
	}
	return md, nil
}

// titleTag returns the trimmed text of the first <title> element, or "" if the
// document has none.
func titleTag(html string) string {
	doc, err := xhtml.Parse(strings.NewReader(html))
	if err != nil {
		return ""
	}

	var find func(*xhtml.Node) string
	find = func(n *xhtml.Node) string {
		if n.Type == xhtml.ElementNode && n.Data == "title" && n.FirstChild != nil {
			return strings.TrimSpace(n.FirstChild.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if t := find(c); t != "" {
				return t
			}
		}
		return ""
	}
	return find(doc)
}
