package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/vendor"
)

// fixtureMarkdown is a small heading tree with a preamble, an H1, two H2s, and
// an H3 under the first H2 — enough to exercise sections, subtree reads, sibling
// navigation, and ranking.
const fixtureMarkdown = "intro paragraph before any heading.\n\n" +
	"# Guide\n\noverview text for the guide.\n\n" +
	"## Errors\n\nHandle errors by wrapping them with %w in Go.\n\n" +
	"### Wrapping\n\nUse fmt.Errorf with the %w verb to wrap.\n\n" +
	"## Install\n\nInstall with homebrew: brew install ccx.\n"

const fixtureURL = "https://example.com/guide"

// fetchFunc mirrors the signature of Fetch (and the fetchPage seam).
type fetchFunc = func(context.Context, string, *Page) (FetchResult, error)

func withFetch(t *testing.T, fn fetchFunc) {
	t.Helper()
	prev := fetchPage
	fetchPage = fn
	t.Cleanup(func() { fetchPage = prev })
}

func withEmbedder(t *testing.T, e Embedder) {
	t.Helper()
	prev := embedder
	embedder = e
	t.Cleanup(func() { embedder = prev })
}

// setUV forces Supported() on or off by stubbing the uv lookup, independent of
// whether uv is installed on the test host.
func setUV(t *testing.T, present bool) {
	t.Helper()
	prev := vendor.LookPath
	vendor.LookPath = func(string) string {
		if present {
			return "/usr/bin/uv"
		}
		return ""
	}
	t.Cleanup(func() { vendor.LookPath = prev })
}

// markdownFetch stubs the cascade to return fixed markdown as a jina result and
// counts how many times it is called.
func markdownFetch(md, title string, calls *atomic.Int32) fetchFunc {
	return func(_ context.Context, url string, _ *Page) (FetchResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		return FetchResult{Tier: TierJina, FinalURL: url, Title: title, Markdown: md}, nil
	}
}

// fakeEmbedder returns deterministic 3-dim unit vectors keyed on coarse topic
// so a query aligns densely with the matching chunk, and records call count.
type fakeEmbedder struct {
	calls atomic.Int32
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		lt := strings.ToLower(t)
		switch {
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

// indexedPage builds a chunked page for md, stamped at the current test clock.
func indexedPage(url, md string) *Page {
	sections, chunks := ChunkPage(md)
	return &Page{
		URL:        url,
		FinalURL:   url,
		Title:      "Guide",
		Tier:       TierJina,
		FetchedAt:  timeNow(),
		ContentSHA: contentSHA(md),
		Markdown:   md,
		Sections:   sections,
		Chunks:     chunks,
	}
}

func attachVectors(p *Page) *Page {
	p.Vectors = make([][]float32, len(p.Chunks))
	for i := range p.Vectors {
		p.Vectors[i] = []float32{1, 0, 0}
	}
	p.EmbedModel = EmbedModelID
	return p
}

// firstCiteSection extracts the section ID of the first cite line in search
// output, e.g. "https://… §1.1#k7fq  (0.031)  Guide > Errors" -> "1.1".
func firstCiteSection(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, citeSep)
		if i < 0 {
			continue
		}
		rest := line[i+len(citeSep):]
		hashIdx := strings.IndexByte(rest, '#')
		if hashIdx < 0 {
			continue
		}
		return rest[:hashIdx]
	}
	t.Fatalf("no cite line in output:\n%s", out)
	return ""
}

func TestRunOutlineThenReadRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	var calls atomic.Int32
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", &calls))

	outline, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err != nil {
		t.Fatalf("outline Run: %v", err)
	}
	for _, want := range []string{"# Guide", fixtureURL, "jina", "§0  (preamble)", "§1  Guide", "§1.1  Errors", "§1.1.1  Wrapping", "§1.2  Install", "chunks)"} {
		if !strings.Contains(outline, want) {
			t.Errorf("outline missing %q:\n%s", want, outline)
		}
	}

	// The §ID echoed from the outline reads back that section's subtree.
	read, err := Run(context.Background(), backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1.1"})
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	for _, want := range []string{"## Errors", "### Wrapping", "— §next 1.2"} {
		if !strings.Contains(read, want) {
			t.Errorf("read of §1.1 missing %q:\n%s", want, read)
		}
	}
	if strings.Contains(read, "## Install") {
		t.Errorf("read of §1.1 leaked the sibling §1.2 Install section:\n%s", read)
	}

	// The second Run served from cache: no second fetch.
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1 (read must hit the fresh cache)", calls.Load())
	}
}

func TestRunReadFullAndBare(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	for _, tt := range []struct {
		name string
		args backend.Args
	}{
		{"full", backend.Args{URL: fixtureURL, Full: true}},
		{"bare", backend.Args{URL: fixtureURL}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Run(context.Background(), backend.OpWebRead, tt.args)
			if err != nil {
				t.Fatalf("read Run: %v", err)
			}
			if out != fixtureMarkdown {
				t.Errorf("read = %q, want the whole page", out)
			}
		})
	}
}

func TestRunReadSiblingFooter(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	read, err := Run(context.Background(), backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "1.2"})
	if err != nil {
		t.Fatalf("read Run: %v", err)
	}
	if !strings.Contains(read, "— §prev 1.1") {
		t.Errorf("read of §1.2 missing prev-sibling footer:\n%s", read)
	}
	if strings.Contains(read, "§next") {
		t.Errorf("read of last sibling §1.2 should have no next footer:\n%s", read)
	}
}

func TestRunReadUnknownSection(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	_, err := Run(context.Background(), backend.OpWebRead, backend.Args{URL: fixtureURL, Section: "9.9"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("read of unknown section: err = %v, want a not-found error", err)
	}
}

func TestRunSearchHybrid(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	setUV(t, true)
	fake := &fakeEmbedder{}
	withEmbedder(t, fake)
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	out, err := Run(context.Background(), backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "how do I handle errors", K: 3})
	if err != nil {
		t.Fatalf("search Run: %v", err)
	}
	if !strings.HasPrefix(out, "# 3 results for") {
		t.Errorf("search header wrong:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("top hit section = %q, want %q (Errors):\n%s", got, "1.1", out)
	}
	if !strings.Contains(out, fixtureURL+citeSep) {
		t.Errorf("search hits missing cite lines:\n%s", out)
	}
	if fake.calls.Load() == 0 {
		t.Error("hybrid search never called the embedder")
	}
	if strings.Contains(out, UnsupportedReason) {
		t.Errorf("hybrid search wrongly printed the BM25-only note:\n%s", out)
	}

	// A first search persists the chunk vectors for reuse.
	page, err := Load(fixtureURL, EmbedModelID)
	if err != nil || page == nil {
		t.Fatalf("Load after search: page=%v err=%v", page, err)
	}
	if len(page.Vectors) != len(page.Chunks) || page.EmbedModel != EmbedModelID {
		t.Errorf("vectors not persisted: %d vectors for %d chunks, model %q", len(page.Vectors), len(page.Chunks), page.EmbedModel)
	}
}

func TestRunSearchBM25OnlyWhenUnsupported(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	setUV(t, false)
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	out, err := Run(context.Background(), backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "handle errors"})
	if err != nil {
		t.Fatalf("search Run: %v", err)
	}
	if !strings.Contains(out, UnsupportedReason) {
		t.Errorf("BM25-only search missing the degradation note:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("BM25-only top hit = %q, want %q:\n%s", got, "1.1", out)
	}
}

func TestRunSearchDegradesOnEmbedError(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	setUV(t, true)
	withEmbedder(t, &fakeEmbedder{err: fmt.Errorf("driver blew up")})
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	out, err := Run(context.Background(), backend.OpWebSearch, backend.Args{URL: fixtureURL, Query: "handle errors"})
	if err != nil {
		t.Fatalf("search must not fail on embed error: %v", err)
	}
	if !strings.Contains(out, "BM25") {
		t.Errorf("embed-failure search missing the BM25 fallback note:\n%s", out)
	}
	if got := firstCiteSection(t, out); got != "1.1" {
		t.Errorf("degraded top hit = %q, want %q:\n%s", got, "1.1", out)
	}
}

func TestRunForceRefetch(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	var calls atomic.Int32
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", &calls))

	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Without Force the fresh cache serves; with Force the cascade runs again.
	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("cached Run: %v", err)
	}
	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2 (initial + forced; the middle Run is cached)", calls.Load())
	}
}

func TestRunNotModifiedPreservesVectors(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	defer setClock(base)()

	// A stale cached page carrying vectors; the refetch will 304.
	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = base.Add(-48 * time.Hour)
	if err := Save(prior); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	wantVecs := len(prior.Vectors)

	withFetch(t, func(_ context.Context, _ string, p *Page) (FetchResult, error) {
		if p == nil {
			t.Error("revalidation fetch got a nil prior")
		}
		return FetchResult{}, fmt.Errorf("http: %w", ErrNotModified)
	})

	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := Load(fixtureURL, EmbedModelID)
	if err != nil || got == nil {
		t.Fatalf("Load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != wantVecs {
		t.Errorf("Vectors = %d, want %d preserved across 304", len(got.Vectors), wantVecs)
	}
	if !got.FetchedAt.Equal(base) {
		t.Errorf("FetchedAt = %v, want bumped to %v", got.FetchedAt, base)
	}
}

func TestRunContentUnchangedPreservesVectors(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	defer setClock(base)()

	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = base.Add(-48 * time.Hour)
	if err := Save(prior); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	wantVecs := len(prior.Vectors)

	// A hosted-tier refetch returning byte-identical markdown: same ContentSHA.
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))

	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := Load(fixtureURL, EmbedModelID)
	if err != nil || got == nil {
		t.Fatalf("Load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != wantVecs {
		t.Errorf("Vectors = %d, want %d preserved when ContentSHA is unchanged", len(got.Vectors), wantVecs)
	}
}

func TestRunContentChangedDropsVectors(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	defer setClock(base)()

	prior := attachVectors(indexedPage(fixtureURL, fixtureMarkdown))
	prior.FetchedAt = base.Add(-48 * time.Hour)
	if err := Save(prior); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	changed := fixtureMarkdown + "\n## New\n\nfresh content.\n"
	withFetch(t, markdownFetch(changed, "Guide", nil))

	if _, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL, Force: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := Load(fixtureURL, "")
	if err != nil || got == nil {
		t.Fatalf("Load: page=%v err=%v", got, err)
	}
	if len(got.Vectors) != 0 {
		t.Errorf("Vectors = %d, want 0 dropped on content change", len(got.Vectors))
	}
	if got.ContentSHA != contentSHA(changed) {
		t.Errorf("ContentSHA not updated to the changed body")
	}
}

func TestRunGonePropagates(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	withFetch(t, func(_ context.Context, _ string, _ *Page) (FetchResult, error) {
		return FetchResult{}, fmt.Errorf("jina: %w", ErrGone)
	})

	_, err := Run(context.Background(), backend.OpWebOutline, backend.Args{URL: fixtureURL})
	if err == nil {
		t.Fatal("Run: want ErrGone to propagate")
	}
	// The sentinel must survive the wrap so the CLI can map it onto an exit code.
	if !errors.Is(err, ErrGone) {
		t.Errorf("err = %v, want it to wrap ErrGone", err)
	}
}

func TestRunPanicsOnNonWebOp(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	defer setClock(time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC))()
	withFetch(t, markdownFetch(fixtureMarkdown, "Guide", nil))
	defer func() {
		if recover() == nil {
			t.Error("Run did not panic on a non-web op")
		}
	}()
	_, _ = Run(context.Background(), backend.OpSearch, backend.Args{URL: fixtureURL})
}
