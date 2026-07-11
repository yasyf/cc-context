package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yasyf/cc-context/internal/vendor"
)

// disableAgentBrowser stubs vendor.LookPath to report every binary absent, so a
// render-chain test exercising only the hosted lanes never spawns the real
// agent-browser that may be installed on the dev host.
func disableAgentBrowser(t *testing.T) {
	t.Helper()
	prev := vendor.LookPath
	vendor.LookPath = func(string) string { return "" }
	t.Cleanup(func() { vendor.LookPath = prev })
}

func TestRenderFetchJinaRenderHeaders(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envJinaKey, "jina-key")
	disableAgentBrowser(t)

	var gotHeaders http.Header
	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"content": "# Rendered\n\n" + strings.Repeat("real rendered prose. ", 20),
					"title":   "Doc",
					"url":     "https://example.com/final",
					"links":   [][]string{{"Quickstart", "https://example.com/quickstart"}, {"API", "https://example.com/api"}},
				},
			})
		},
	})

	res, stillThin, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err != nil {
		t.Fatalf("renderFetch: %v", err)
	}
	if stillThin {
		t.Error("stillThin = true, want false for a real rendered result")
	}
	if res.Tier != TierJinaRender {
		t.Errorf("Tier = %q, want %q", res.Tier, TierJinaRender)
	}
	for k, want := range map[string]string{
		"X-Engine":             "browser",
		"X-Respond-Timing":     "mutation-idle",
		"X-No-Cache":           "true",
		"X-With-Links-Summary": "all",
	} {
		if got := gotHeaders.Get(k); got != want {
			t.Errorf("render header %s = %q, want %q", k, got, want)
		}
	}
	if n := strings.Count(res.Markdown, "## Links"); n != 1 {
		t.Errorf("## Links section appears %d times, want exactly once:\n%s", n, res.Markdown)
	}
	for _, link := range []string{"- [API](https://example.com/api)", "- [Quickstart](https://example.com/quickstart)"} {
		if !strings.Contains(res.Markdown, link) {
			t.Errorf("markdown missing link line %q", link)
		}
	}
}

func TestRenderFetchOrderJinaThenFirecrawl(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	disableAgentBrowser(t)

	var attempts []Tier
	var waitFor int
	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, _ *http.Request) {
			// A thin render result forces escalation to firecrawl. The fat links
			// map is the F4 regression guard: appended to "loading" it would cross
			// the 30-token floor, so classifying on links-padded markdown would
			// wrongly accept jina here. Classification must see the content alone.
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": map[string]any{
					"content": "loading",
					"title":   "X",
					"url":     "https://example.com/final",
					"links": [][]string{
						{"Introduction", "https://example.com/intro"},
						{"Quickstart", "https://example.com/quickstart"},
						{"Guides", "https://example.com/guides"},
						{"Reference", "https://example.com/reference"},
						{"API", "https://example.com/api"},
						{"Changelog", "https://example.com/changelog"},
					},
				},
			})
		},
		firecrawl: func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				WaitFor int `json:"waitFor"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			waitFor = body.WaitFor
			writeJSON(t, w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"markdown": "# Real\n\n" + strings.Repeat("firecrawl rendered prose. ", 20), "metadata": map[string]any{"statusCode": 200, "title": "Real"}},
			})
		},
	})
	ts.onAttempt = func(tier Tier, _ error) { attempts = append(attempts, tier) }

	res, stillThin, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err != nil {
		t.Fatalf("renderFetch: %v", err)
	}
	if stillThin || res.Tier != TierFirecrawlRender {
		t.Errorf("got tier=%q stillThin=%v, want firecrawl-render not thin", res.Tier, stillThin)
	}
	if waitFor != firecrawlRenderWaitMS {
		t.Errorf("firecrawl waitFor = %d, want %d", waitFor, firecrawlRenderWaitMS)
	}
	if want := []Tier{TierJinaRender, TierFirecrawlRender}; !slices.Equal(attempts, want) {
		t.Errorf("attempts = %v, want %v", attempts, want)
	}
}

func TestRenderFetchTerminalAcceptsLargestThin(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	disableAgentBrowser(t)

	var jinaHits, fcHits atomic.Int32
	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, _ *http.Request) {
			jinaHits.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"data": map[string]any{"content": "tiny", "title": "X", "url": "https://example.com/final"}})
		},
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			fcHits.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"markdown": "a bit more but still thin", "metadata": map[string]any{"statusCode": 200}}})
		},
	})

	res, stillThin, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err != nil {
		t.Fatalf("renderFetch: %v", err)
	}
	if !stillThin {
		t.Error("stillThin = false, want true (every lane came back thin)")
	}
	if res.Markdown != "a bit more but still thin" {
		t.Errorf("Markdown = %q, want the larger firecrawl body", res.Markdown)
	}
	if jinaHits.Load() != 1 || fcHits.Load() != 1 {
		t.Errorf("hits jina=%d fc=%d, want each lane run exactly once (no loop)", jinaHits.Load(), fcHits.Load())
	}
}

func TestRenderFetchNoLaneAvailableErrors(t *testing.T) {
	isolateKeys(t)
	disableAgentBrowser(t)
	ts := testTiers(t, services{}) // any hosted-tier hit fails the test

	_, _, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err == nil || !strings.Contains(err.Error(), "no render lane") {
		t.Fatalf("err = %v, want a no-render-lane error", err)
	}
}

func TestRenderFetchChallengeSkipsLane(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	disableAgentBrowser(t)

	ts := testTiers(t, services{
		jina: jinaClean(t, challengeBody, "Just a moment..."),
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"markdown": "# Real\n\n" + strings.Repeat("unblocked rendered prose. ", 20), "metadata": map[string]any{"statusCode": 200, "title": "Real"}}})
		},
	})

	res, _, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err != nil {
		t.Fatalf("renderFetch: %v", err)
	}
	if res.Tier != TierFirecrawlRender {
		t.Errorf("Tier = %q, want firecrawl-render (the challenged jina lane is skipped)", res.Tier)
	}
	if strings.Contains(res.Markdown, challengeBody) {
		t.Error("served markdown carries the challenge body")
	}
}

func TestRenderFetchGoneLaneSkipsNotAborts(t *testing.T) {
	isolateKeys(t)
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	disableAgentBrowser(t)

	var fcHits atomic.Int32
	ts := testTiers(t, services{
		jina: func(w http.ResponseWriter, _ *http.Request) {
			// The 200-trap: a target 404 in data.warning makes jina return ErrGone.
			writeJSON(t, w, http.StatusOK, map[string]any{"data": map[string]any{"warning": "Target URL returned error 404: Not Found"}})
		},
		firecrawl: func(w http.ResponseWriter, _ *http.Request) {
			fcHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		},
	})

	_, _, err := ts.renderFetch(context.Background(), remoteTargetURL)
	if err == nil {
		t.Fatal("renderFetch: want a joined failure, got nil")
	}
	// A lane ErrGone must not abort the chain: firecrawl still ran.
	if fcHits.Load() != 1 {
		t.Errorf("firecrawl hits = %d, want 1 (a Gone lane must not abort the chain)", fcHits.Load())
	}
	// No sentinel escapes renderFetch — the joined failures render as text.
	if errors.Is(err, ErrGone) {
		t.Error("ErrGone leaked out of renderFetch's joined error")
	}
	if errors.Is(err, errStealthRequired) {
		t.Error("errStealthRequired leaked out of renderFetch's joined error")
	}
}

func TestRenderFetchLocalTargetAgentBrowserOnly(t *testing.T) {
	isolateKeys(t)
	// Keys are set but must be ignored: the hosted lanes can't reach a local
	// target, so any jina/firecrawl hit trips the guard handlers below.
	t.Setenv(envJinaKey, "jina-key")
	t.Setenv(envFirecrawlKey, "fc-key")
	stubAgentBrowser(t, mustJSON(t, okBatch("# Local\n\n"+strings.Repeat("rendered local content. ", 10), "Local")), 0)

	ts := testTiers(t, services{})
	var attempts []Tier
	ts.onAttempt = func(tier Tier, _ error) { attempts = append(attempts, tier) }

	res, _, err := ts.renderFetch(context.Background(), "http://localhost:1234/app")
	if err != nil {
		t.Fatalf("renderFetch: %v", err)
	}
	if res.Tier != TierAgentBrowser {
		t.Errorf("Tier = %q, want %q", res.Tier, TierAgentBrowser)
	}
	if !slices.Equal(attempts, []Tier{TierAgentBrowser}) {
		t.Errorf("attempts = %v, want only agent-browser (hosted lanes skipped for a local target)", attempts)
	}
}
