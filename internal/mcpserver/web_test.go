package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/lookpath"
	"github.com/yasyf/cc-context/internal/web"
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

// isolateWeb makes ccx_web_* hermetic: the page cache points at a temp dir, the
// fetch-tier API keys are unset so the loopback httptest target takes the plain
// HTTP tier, every binary reports absent so no agent-browser render escalates,
// and topicEmbedder replaces the resident WASM engine so hybrid ranking runs
// without downloading the pinned model weights. Nothing beyond the fixture
// server is reachable, and no subprocess spawns.
func isolateWeb(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("JINA_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	t.Setenv("BROWSERBASE_API_KEY", "")
	prev := lookpath.Find
	lookpath.Find = func(string) string { return "" }
	t.Cleanup(func() { lookpath.Find = prev })
	t.Cleanup(web.SetEmbedderProvider(func(context.Context) (web.Embedder, error) {
		return topicEmbedder{}, nil
	}))
}

// topicEmbedder maps each text to a deterministic 3-dim unit vector keyed on
// coarse topic, so the errors query aligns densely with the errors chunk.
type topicEmbedder struct{}

func (topicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		switch lt := strings.ToLower(text); {
		case strings.Contains(lt, "error"):
			out[i] = []float32{1, 0, 0}
		case strings.Contains(lt, "install"):
			out[i] = []float32{0, 1, 0}
		default:
			out[i] = []float32{0, 0, 1}
		}
	}
	return out, nil
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
// seam against a loopback fixture, guarding the web dispatch case: a missing case
// there would route web ops to the semble MCP session while the CLI still works.
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
	for _, marker := range []string{web.UnsupportedReason, web.DegradedPrefix} {
		if strings.Contains(search, marker) {
			t.Errorf("search degraded to BM25-only (%q) instead of ranking with the injected embedder:\n%s", marker, search)
		}
	}
}
