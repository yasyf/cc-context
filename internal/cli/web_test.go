package cli_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cli"
	"github.com/yasyf/cc-context/internal/lookpath"
)

// webFixtureHTML is a small article with a preamble paragraph, an H1, and two
// H2s — enough to exercise the heading tree, a section read, and search ranking.
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
// and forces the uv-gated embedder off so ccx web runs hermetically: the
// loopback httptest target takes the plain-HTTP tier and search ranks BM25-only,
// touching no network beyond the fixture server and spawning no subprocess.
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
}

// startWebFixture serves webFixtureHTML on a loopback httptest server.
func startWebFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webFixtureHTML))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runWeb executes one ccx web invocation through the real root command and
// returns its combined stdout/stderr.
func runWeb(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v\n%s", args, err, out.String())
	}
	return out.String()
}

// sectionRef extracts the "§<id>" reference from the outline line naming heading.
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

func TestWebOutlineReadSearchE2E(t *testing.T) {
	isolateWeb(t)
	srv := startWebFixture(t)

	// outline lists the heading tree with stable section refs.
	outline := runWeb(t, "web", "outline", srv.URL)
	if !strings.Contains(outline, "§") {
		t.Errorf("outline missing section refs:\n%s", outline)
	}
	for _, heading := range []string{"Handling Errors", "Installing"} {
		if !strings.Contains(outline, heading) {
			t.Errorf("outline missing heading %q:\n%s", heading, outline)
		}
	}

	// A section ref echoed from the outline reads back exactly that section.
	ref := sectionRef(t, outline, "Handling Errors")
	read := runWeb(t, "web", "read", srv.URL, "--section", ref)
	if !strings.Contains(read, "errors.Is") {
		t.Errorf("read of §%s missing its body text:\n%s", ref, read)
	}
	if strings.Contains(read, "package manager") {
		t.Errorf("read of §%s leaked the sibling Installing section:\n%s", ref, read)
	}

	// search returns cited chunks; the errors question surfaces the errors chunk.
	search := runWeb(t, "web", "search", srv.URL, "how do I handle errors")
	if !strings.Contains(search, "§") || !strings.Contains(search, "#") {
		t.Errorf("search output missing a cite line:\n%s", search)
	}
	if !strings.Contains(search, "errors.Is") {
		t.Errorf("search did not surface the errors chunk:\n%s", search)
	}
}
