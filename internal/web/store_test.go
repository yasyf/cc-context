package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// samplePage builds a fixture page under url with nChunks chunks, nVectors
// per-chunk vectors, and the given embed model. Every page shares the same
// markdown body so their gzipped files compress to comparable sizes.
func samplePage(url string, nChunks, nVectors int, model string) *Page {
	md := "# Title\n\nbody text for the page under test.\n"
	p := &Page{
		URL:        url,
		FinalURL:   url + "?final",
		Title:      "Sample Page",
		Tier:       TierJina,
		FetchedAt:  time.Date(2026, 7, 7, 12, 0, 0, 123, time.UTC),
		ETag:       `"etag-abc"`,
		LastMod:    "Mon, 07 Jul 2026 12:00:00 GMT",
		ContentSHA: "deadbeefcafef00d",
		Markdown:   md,
		RawHTML:    "<html><body>source</body></html>",
		Sections: []Section{
			{ID: "0", Level: 0, Start: 0, End: len(md)},
			{ID: "1", Level: 1, Title: "Title", Start: 0, End: len(md)},
		},
		EmbedModel: model,
	}
	for i := range nChunks {
		p.Chunks = append(p.Chunks, Chunk{
			Index: i, Section: "0", Breadcrumb: "Title", Start: 0, End: len(md), Hash: "abcd",
		})
	}
	for i := range nVectors {
		p.Vectors = append(p.Vectors, []float32{float32(i), -float32(i), 0.5})
	}
	return p
}

// mustLoad loads url at model, failing the test on an error or a cache miss,
// and returns a non-nil page.
func mustLoad(ctx context.Context, t *testing.T, st *store, url, model string) *Page {
	t.Helper()
	got, err := st.load(ctx, url, model)
	if err != nil {
		t.Fatalf("load(%q, %q): %v", url, model, err)
	}
	if got == nil {
		t.Fatalf("load(%q, %q) returned a miss, want a hit", url, model)
	}
	return got
}

func webPagePath(ctx context.Context, t *testing.T, normURL string) string {
	t.Helper()
	dir, err := cache.Dir(ctx, "web")
	if err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	return filepath.Join(dir, CacheKey(normURL)+pageExt)
}

func webCacheBytes(ctx context.Context, t *testing.T) int64 {
	t.Helper()
	dir, err := cache.Dir(ctx, "web")
	if err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), pageExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("info %q: %v", e.Name(), err)
		}
		total += info.Size()
	}
	return total
}

func readGz(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip open %q: %v", path, err)
	}
	defer func() { _ = gz.Close() }()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read %q: %v", path, err)
	}
	return raw
}

// writeRaw drops arbitrary bytes at the cache path for url, standing in for a
// pre-existing (possibly poisoned) cache file.
func writeRaw(ctx context.Context, t *testing.T, url string, data []byte) string {
	t.Helper()
	path := webPagePath(ctx, t, url)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write raw page: %v", err)
	}
	return path
}

func gzJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/doc"
	const model = "potion-base-8M"

	// Vectors chosen to stress the base64 LE-float32 codec: signed values,
	// subnormals, extremes, and inf/nan bit patterns must survive bit-for-bit.
	want := samplePage(url, 3, 0, model)
	want.Vectors = [][]float32{
		{0, 1, -1, 0.5},
		{float32(math.Pi), math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32},
		{float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN()), 1.401298e-45},
	}

	if err := st.save(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if want.Version != schemaVersion {
		t.Errorf("save did not stamp Version: got %d, want %d", want.Version, schemaVersion)
	}

	got := mustLoad(ctx, t, st, url, model)

	if got.Version != schemaVersion {
		t.Errorf("Version = %d, want %d", got.Version, schemaVersion)
	}
	if got.URL != want.URL {
		t.Errorf("URL = %q, want %q", got.URL, want.URL)
	}
	if got.FinalURL != want.FinalURL {
		t.Errorf("FinalURL = %q, want %q", got.FinalURL, want.FinalURL)
	}
	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	if got.Tier != want.Tier {
		t.Errorf("Tier = %q, want %q", got.Tier, want.Tier)
	}
	if !got.FetchedAt.Equal(want.FetchedAt) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, want.FetchedAt)
	}
	if got.ETag != want.ETag {
		t.Errorf("ETag = %q, want %q", got.ETag, want.ETag)
	}
	if got.LastMod != want.LastMod {
		t.Errorf("LastMod = %q, want %q", got.LastMod, want.LastMod)
	}
	if got.ContentSHA != want.ContentSHA {
		t.Errorf("ContentSHA = %q, want %q", got.ContentSHA, want.ContentSHA)
	}
	if got.Markdown != want.Markdown {
		t.Errorf("Markdown = %q, want %q", got.Markdown, want.Markdown)
	}
	if got.RawHTML != want.RawHTML {
		t.Errorf("RawHTML = %q, want %q", got.RawHTML, want.RawHTML)
	}
	if got.EmbedModel != want.EmbedModel {
		t.Errorf("EmbedModel = %q, want %q", got.EmbedModel, want.EmbedModel)
	}
	if !reflect.DeepEqual(got.Sections, want.Sections) {
		t.Errorf("Sections = %+v, want %+v", got.Sections, want.Sections)
	}
	if !reflect.DeepEqual(got.Chunks, want.Chunks) {
		t.Errorf("Chunks = %+v, want %+v", got.Chunks, want.Chunks)
	}

	if len(got.Vectors) != len(want.Vectors) {
		t.Fatalf("Vectors len = %d, want %d", len(got.Vectors), len(want.Vectors))
	}
	for i := range want.Vectors {
		if len(got.Vectors[i]) != len(want.Vectors[i]) {
			t.Fatalf("Vectors[%d] len = %d, want %d", i, len(got.Vectors[i]), len(want.Vectors[i]))
		}
		for j := range want.Vectors[i] {
			gb, wb := math.Float32bits(got.Vectors[i][j]), math.Float32bits(want.Vectors[i][j])
			if gb != wb {
				t.Errorf("Vectors[%d][%d] bits = %#08x, want %#08x", i, j, gb, wb)
			}
		}
	}
}

func TestSaveLoadRoundTripsThin(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const model = "potion-base-8M"

	thin := samplePage("https://example.com/thin", 1, 0, model)
	thin.Thin = true
	if err := st.save(ctx, thin); err != nil {
		t.Fatalf("save thin: %v", err)
	}
	got, err := st.load(ctx, "https://example.com/thin", model)
	if err != nil || got == nil {
		t.Fatalf("load thin: page=%v err=%v", got, err)
	}
	if !got.Thin {
		t.Error("Thin = false after round-trip, want true")
	}

	// A non-thin page round-trips Thin=false, matching how an old cache entry
	// written before the field existed decodes.
	solid := samplePage("https://example.com/solid", 1, 0, model)
	if err := st.save(ctx, solid); err != nil {
		t.Fatalf("save solid: %v", err)
	}
	got2, err := st.load(ctx, "https://example.com/solid", model)
	if err != nil || got2 == nil {
		t.Fatalf("load solid: page=%v err=%v", got2, err)
	}
	if got2.Thin {
		t.Error("Thin = true for a non-thin page, want false")
	}
}

func TestLoadMissWhenAbsent(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	got, err := st.load(ctx, "https://example.com/never-fetched", "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("load = %+v, want a miss (nil, nil)", got)
	}
}

func TestLoadLazyPageWithoutVectors(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/lazy"

	// A freshly fetched page: chunks present, no vectors, no embed model. The
	// model check must be skipped and the page returned intact.
	if err := st.save(ctx, samplePage(url, 2, 0, "")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := mustLoad(ctx, t, st, url, "any-model")
	if len(got.Vectors) != 0 {
		t.Errorf("Vectors = %v, want empty", got.Vectors)
	}
}

func TestLoadDiscardsCorrupt(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/corrupt"
	path := writeRaw(ctx, t, url, []byte("this is not gzip"))

	got, err := st.load(ctx, url, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("load = %+v, want a miss", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupt entry not deleted: stat err = %v", err)
	}
}

func TestLoadDiscardsVersionMismatch(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/oldversion"
	old := pageWire{Version: schemaVersion + 1, URL: url, Markdown: "stale layout"}
	path := writeRaw(ctx, t, url, gzJSON(t, old))

	got, err := st.load(ctx, url, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("load = %+v, want a miss", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("version-mismatched entry not deleted: stat err = %v", err)
	}
}

func TestLoadDiscardsEmbedModelMismatch(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/model"
	if err := st.save(ctx, samplePage(url, 2, 2, "model-A")); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := webPagePath(ctx, t, url)

	got, err := st.load(ctx, url, "model-B")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("load = %+v, want a miss", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("model-mismatched entry not deleted: stat err = %v", err)
	}
}

func TestLoadDiscardsVectorLengthMismatch(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/lenmismatch"
	p := samplePage(url, 2, 0, "model-A")
	p.Vectors = [][]float32{{1, 2, 3}} // one vector, two chunks
	if err := st.save(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := webPagePath(ctx, t, url)

	got, err := st.load(ctx, url, "model-A")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("load = %+v, want a miss", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("length-mismatched entry not deleted: stat err = %v", err)
	}
}

func TestLoadDiscardsBadVectorDims(t *testing.T) {
	t.Parallel()
	st := newStore()
	tests := []struct {
		name    string
		vectors [][]float32
	}{
		{"ragged", [][]float32{{1, 2, 3}, {4, 5}}}, // two chunks, mismatched widths
		{"zerowidth", [][]float32{{}, {}}},         // two chunks, zero-width vectors
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := webCtx(t)
			url := "https://example.com/" + tt.name
			p := samplePage(url, len(tt.vectors), 0, "model-A")
			p.Vectors = tt.vectors
			if err := st.save(ctx, p); err != nil {
				t.Fatalf("save: %v", err)
			}
			path := webPagePath(ctx, t, url)

			got, err := st.load(ctx, url, "model-A")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got != nil {
				t.Errorf("load = %+v, want a miss", got)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("bad-dimension entry not deleted: stat err = %v", err)
			}
		})
	}
}

func TestFresh(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		fetchedAt time.Time
		want      bool
	}{
		{"just fetched", base, true},
		{"one minute short of ttl", base.Add(-(ttl - time.Minute)), true},
		{"exactly ttl old", base.Add(-ttl), false},
		{"well past ttl", base.Add(-25 * time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore()
			st.now = func() time.Time { return base }
			if got := st.fresh(&Page{FetchedAt: tt.fetchedAt}); got != tt.want {
				t.Errorf("fresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSaveIsInspectableJSON(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/inspect"
	p := samplePage(url, 2, 2, "model-A")
	if err := st.save(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw := readGz(t, webPagePath(ctx, t, url))
	// Decode the on-disk file as plain JSON — what `gunzip | jq` would see.
	var doc struct {
		Version  int
		URL      string
		Title    string
		Markdown string
		Vectors  []string
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("on-disk file is not plain JSON: %v", err)
	}
	if doc.Version != schemaVersion {
		t.Errorf("Version = %d, want %d", doc.Version, schemaVersion)
	}
	if doc.URL != url {
		t.Errorf("URL = %q, want %q", doc.URL, url)
	}
	if doc.Title != p.Title {
		t.Errorf("Title = %q, want %q", doc.Title, p.Title)
	}
	if doc.Markdown != p.Markdown {
		t.Errorf("Markdown = %q, want %q", doc.Markdown, p.Markdown)
	}
	if len(doc.Vectors) != 2 {
		t.Fatalf("Vectors len = %d, want 2", len(doc.Vectors))
	}
	// Each vector is a base64 string of 3 little-endian float32s = 12 bytes.
	decoded, err := base64.StdEncoding.DecodeString(doc.Vectors[0])
	if err != nil {
		t.Fatalf("vector is not base64: %v", err)
	}
	if len(decoded) != 12 {
		t.Errorf("decoded vector len = %d bytes, want 12", len(decoded))
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	ctx := webCtx(t)
	st := newStore()
	const url = "https://example.com/atomic"
	if err := st.save(ctx, samplePage(url, 1, 1, "model-A")); err != nil {
		t.Fatalf("save: %v", err)
	}

	dir, err := cache.Dir(ctx, "web")
	if err != nil {
		t.Fatalf("cache dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir entries = %v, want exactly the one page file (no partial/temp file)", names)
	}
	if !strings.HasSuffix(entries[0].Name(), pageExt) {
		t.Errorf("dir entry = %q, want a %s file", entries[0].Name(), pageExt)
	}
	// The single visible file must be a complete, loadable page.
	got, err := st.load(ctx, url, "model-A")
	if err != nil || got == nil {
		t.Errorf("saved file not loadable: got=%v err=%v", got, err)
	}
}

func TestEvictRemovesOldestUntilUnderCap(t *testing.T) {
	ctx := webCtx(t)
	st := newStore()
	st.capBytes = 1 << 40 // no eviction while seeding

	urls := []string{
		"https://example.com/p0",
		"https://example.com/p1",
		"https://example.com/p2",
		"https://example.com/p3",
	}
	for _, u := range urls {
		if err := st.save(ctx, samplePage(u, 1, 0, "")); err != nil {
			t.Fatalf("seed save %q: %v", u, err)
		}
	}

	dir, err := cache.Dir(ctx, "web")
	if err != nil {
		t.Fatalf("cache dir: %v", err)
	}

	// Assign strictly increasing mtimes: p0 oldest, p3 newest.
	base := time.Now().Add(-time.Hour)
	sizes := make([]int64, len(urls))
	for i, u := range urls {
		path := webPagePath(ctx, t, u)
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes %q: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		sizes[i] = info.Size()
	}

	// A cap that fits exactly the newest two pages forces the oldest two out.
	capBytes := sizes[2] + sizes[3]
	if err := evict(dir, capBytes); err != nil {
		t.Fatalf("evict: %v", err)
	}

	for i, u := range urls {
		_, err := os.Stat(webPagePath(ctx, t, u))
		gone := errors.Is(err, os.ErrNotExist)
		switch {
		case i < 2 && !gone:
			t.Errorf("p%d (oldest) should have been evicted", i)
		case i >= 2 && gone:
			t.Errorf("p%d (newest) should have survived: %v", i, err)
		}
	}
	if total := webCacheBytes(ctx, t); total > capBytes {
		t.Errorf("post-eviction total = %d bytes, want <= cap %d", total, capBytes)
	}
}

func TestSaveRespectsMaxCacheBytes(t *testing.T) {
	ctx := webCtx(t)
	st := newStore()

	// Measure one page's on-disk size, then cap the directory below two pages
	// so every subsequent Save must evict down to a single page.
	if err := st.save(ctx, samplePage("https://example.com/seed", 1, 0, "")); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	info, err := os.Stat(webPagePath(ctx, t, "https://example.com/seed"))
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	s := info.Size()
	st.capBytes = s + s/2

	for i := range 5 {
		u := fmt.Sprintf("https://example.com/x%d", i)
		if err := st.save(ctx, samplePage(u, 1, 0, "")); err != nil {
			t.Fatalf("save %q: %v", u, err)
		}
		if total := webCacheBytes(ctx, t); total > st.capBytes {
			t.Errorf("after save %d: total = %d bytes, want <= cap %d", i, total, st.capBytes)
		}
	}
}

// TestSaveLoadResolveCacheDirFromContext proves the page file lands under the
// $CLAUDE_PLUGIN_DATA the context carries rather than the process's: the process
// value is emptied, so a read off it would root the cache in the user cache dir.
func TestSaveLoadResolveCacheDirFromContext(t *testing.T) {
	st := newStore()
	t.Setenv("CLAUDE_PLUGIN_DATA", "")
	root := t.TempDir()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+root)

	const url = "https://example.com/ctx-rooted"
	if err := st.save(ctx, samplePage(url, 1, 0, "")); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "web", CacheKey(url)+pageExt)); err != nil {
		t.Fatalf("page not stored under the cache root the context carries: %v", err)
	}
	if got := mustLoad(ctx, t, st, url, ""); got.URL != url {
		t.Errorf("URL = %q, want %q", got.URL, url)
	}
}
