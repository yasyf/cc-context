package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vendor"
)

// webFixtureHTML is a small article with a preamble, an H1, and two H2s — enough
// to exercise the heading tree, a section read, and search ranking end to end.
const webFixtureHTML = `<!doctype html>
<html>
<head><title>Effective Widgets</title></head>
<body>
<article>
<h1>Effective Widgets</h1>
<p>Widgets are the core building block of the toolkit and this guide walks through building, running, and shipping them in production.</p>
<h2>Handling Errors</h2>
<p>Wrap every error with the %w verb so callers can inspect the cause with errors.Is instead of matching on error strings.</p>
<h2>Installing</h2>
<p>Install the widget toolkit with your package manager, then confirm the version before you begin building anything.</p>
</article>
</body>
</html>`

// isolateWeb points the web cache at a temp dir, unsets the fetch-tier API keys,
// and forces the uv-gated embedder off so ccx_web_* runs hermetically: the
// loopback httptest target takes the plain-HTTP tier and search ranks BM25-only.
func isolateWeb(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("JINA_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	t.Setenv("BROWSERBASE_API_KEY", "")
	prev := vendor.LookPath
	vendor.LookPath = func(string) string { return "" }
	t.Cleanup(func() { vendor.LookPath = prev })
}

func startWebFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webFixtureHTML))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sectionRef(t *testing.T, outline, heading string) string {
	t.Helper()
	for _, line := range strings.Split(outline, "\n") {
		if !strings.Contains(line, heading) {
			continue
		}
		i := strings.Index(line, "§")
		if i < 0 {
			continue
		}
		id, _, _ := strings.Cut(line[i+len("§"):], " ")
		return id
	}
	t.Fatalf("no section ref for %q in outline:\n%s", heading, outline)
	return ""
}

// TestWebToolsRoundTrip drives ccx_web_outline/read/search through the MCP proxy
// seam against a loopback fixture, guarding the proxy.call web special-case: a
// missing case there routes web ops to tilth while the CLI still works.
func TestWebToolsRoundTrip(t *testing.T) {
	isolateWeb(t)
	srv := startWebFixture(t)
	cs := connectTestServer(t)

	outline, isErr := callText(t, cs, "ccx_web_outline", map[string]any{"url": srv.URL})
	if isErr {
		t.Fatalf("ccx_web_outline is error: %s", outline)
	}
	if !strings.Contains(outline, "§") || !strings.Contains(outline, "Handling Errors") {
		t.Errorf("outline missing refs or heading:\n%s", outline)
	}

	ref := sectionRef(t, outline, "Handling Errors")
	read, isErr := callText(t, cs, "ccx_web_read", map[string]any{"url": srv.URL, "section": ref})
	if isErr {
		t.Fatalf("ccx_web_read is error: %s", read)
	}
	if !strings.Contains(read, "errors.Is") {
		t.Errorf("read of §%s missing its body text:\n%s", ref, read)
	}

	search, isErr := callText(t, cs, "ccx_web_search", map[string]any{"url": srv.URL, "query": "how do I handle errors"})
	if isErr {
		t.Fatalf("ccx_web_search is error: %s", search)
	}
	if !strings.Contains(search, "§") || !strings.Contains(search, "errors.Is") {
		t.Errorf("search missing a cite or the errors chunk:\n%s", search)
	}
}
