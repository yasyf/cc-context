package web

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
)

// schemaVersion is the on-disk page-file format version. Save stamps it into
// Page.Version; Load discards any entry carrying a different version, since an
// older layout cannot be trusted to decode into the current Page.
const schemaVersion = 1

// ttl bounds how long a persisted page is served before a refetch; Fresh
// compares it against Page.FetchedAt.
const ttl = 24 * time.Hour

// pageExt is the suffix of a persisted page file: gzipped JSON.
const pageExt = ".json.gz"

// maxCacheBytes caps the total size of the web cache directory. After each save
// the oldest-mtime pages are evicted until the directory fits. It is a var so
// tests can shrink the cap.
var maxCacheBytes int64 = 1 << 30 // 1 GiB

// timeNow is the clock Fresh and the LRU touch read; tests replace it.
var timeNow = time.Now

// Save persists page to the web cache as one gzipped JSON file named
// <CacheKey(page.URL)>.json.gz under cache.Dir("web"). It stamps the current
// schema version, writes atomically (a sibling temp file renamed over the
// target), then evicts oldest-mtime pages until the directory is under
// maxCacheBytes.
//
// There is no cross-process lock: embedding is idempotent and every persisted
// state is self-consistent, so concurrent writers race to a last-writer-wins
// outcome between equally valid pages.
func Save(page *Page) error {
	page.Version = schemaVersion

	dir, err := cache.Dir("web")
	if err != nil {
		return fmt.Errorf("resolve web cache dir: %w", err)
	}

	data, err := json.Marshal(toWire(page))
	if err != nil {
		return fmt.Errorf("marshal page %q: %w", page.URL, err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return fmt.Errorf("gzip page %q: %w", page.URL, err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("flush gzip for page %q: %w", page.URL, err)
	}

	path := filepath.Join(dir, CacheKey(page.URL)+pageExt)
	if err := cache.Store(path, buf.Bytes(), 0o640); err != nil {
		return fmt.Errorf("store page %q: %w", page.URL, err)
	}

	if err := evict(dir, maxCacheBytes); err != nil {
		return fmt.Errorf("evict web cache: %w", err)
	}
	return nil
}

// Load reads the persisted page for normURL, or reports a miss. A miss is
// (nil, nil): either no file exists, or the stored entry was unusable and has
// been discarded. Load returns an error only for an unexpected I/O failure.
//
// An entry is discarded — deleted, logged at Warn, and reported as a miss —
// when it fails to decode, carries a different schema version, holds vectors
// inconsistent with its chunks, or (when embedModel is non-empty) was embedded
// by a different model. Discarding a corrupt entry is the fail-fast move: a bad
// cache line is never served. embedModel is the caller's current embedding
// model; passing "" disables the model check (embeddings unavailable this run).
func Load(normURL, embedModel string) (*Page, error) {
	dir, err := cache.Dir("web")
	if err != nil {
		return nil, fmt.Errorf("resolve web cache dir: %w", err)
	}
	path := filepath.Join(dir, CacheKey(normURL)+pageExt)

	data, err := os.ReadFile(path) //nolint:gosec // path is rooted at the cache dir and keyed by sha256 hex
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read page %q: %w", path, err)
	}

	page, err := decodePage(data)
	if err != nil {
		discardEntry(path, "decode failed", err)
		return nil, nil
	}
	if page.Version != schemaVersion {
		discardEntry(path, "schema version mismatch",
			fmt.Errorf("stored version %d != %d", page.Version, schemaVersion))
		return nil, nil
	}
	if len(page.Vectors) != 0 && len(page.Vectors) != len(page.Chunks) {
		discardEntry(path, "vectors/chunks length mismatch",
			fmt.Errorf("%d vectors for %d chunks", len(page.Vectors), len(page.Chunks)))
		return nil, nil
	}
	if err := validateVectorDims(page.Vectors); err != nil {
		discardEntry(path, "ragged or zero-width vectors", err)
		return nil, nil
	}
	if embedModel != "" && page.EmbedModel != "" && page.EmbedModel != embedModel {
		discardEntry(path, "embed model mismatch",
			fmt.Errorf("stored model %q != %q", page.EmbedModel, embedModel))
		return nil, nil
	}

	// LRU touch: a load counts as a use, so bump mtime to defer eviction. The
	// bump is a best-effort hint; a failure never fails the load.
	t := timeNow()
	_ = os.Chtimes(path, t, t)
	return page, nil
}

// Fresh reports whether page is within the cache TTL (24h) of its FetchedAt
// time, i.e. servable without a refetch. A page fetched exactly ttl ago or
// earlier is stale.
func Fresh(page *Page) bool {
	return timeNow().Sub(page.FetchedAt) < ttl
}

// decodePage gunzips and JSON-decodes a persisted page file into a Page.
func decodePage(data []byte) (*Page, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	var w pageWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("unmarshal page json: %w", err)
	}
	return fromWire(&w), nil
}

// validateVectorDims reports an error when vecs is non-empty and its entries are
// not all the same nonzero length. A ragged or zero-width embedding matrix would
// corrupt denseOrder/dot, so a violation sends the cache entry down the discard
// path. An empty matrix (a never-embedded page) is valid.
func validateVectorDims(vecs [][]float32) error {
	if len(vecs) == 0 {
		return nil
	}
	dim := len(vecs[0])
	if dim == 0 {
		return errors.New("vector 0 has zero width")
	}
	for i, v := range vecs {
		if len(v) != dim {
			return fmt.Errorf("vector %d has width %d, want %d", i, len(v), dim)
		}
	}
	return nil
}

// discardEntry deletes a poisoned cache file and logs why. A missing file (a
// concurrent evictor won the race) is not an error.
func discardEntry(path, reason string, cause error) {
	slog.Warn("discarding web cache entry", "path", path, "reason", reason, "err", cause)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove discarded web cache entry", "path", path, "err", err)
	}
}

// evict deletes oldest-mtime page files until the total size of the cache
// directory is at or under capBytes. Only <hash>.json.gz files count toward the
// budget; in-flight temp files are ignored.
func evict(dir string, capBytes int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read cache dir %q: %w", dir, err)
	}

	type pageFile struct {
		path string
		size int64
		mod  time.Time
	}
	var files []pageFile
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), pageExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // raced with another evictor
			}
			return fmt.Errorf("stat cache entry %q: %w", e.Name(), err)
		}
		files = append(files, pageFile{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= capBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= capBytes {
			break
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict %q: %w", f.path, err)
		}
		total -= f.size
	}
	return nil
}

// pageWire is the on-disk shape of a Page. It mirrors Page field-for-field
// except Vectors, whose entries each encode as a base64 little-endian float32
// string (see encodedVec) so the file stays inspectable with `gunzip | jq`
// while packing embeddings densely.
type pageWire struct {
	Version    int
	URL        string
	FinalURL   string
	Title      string
	Tier       Tier
	FetchedAt  time.Time
	ETag       string
	LastMod    string
	ContentSHA string
	Markdown   string
	RawHTML    string
	Sections   []Section
	Chunks     []Chunk
	Vectors    []encodedVec
	EmbedModel string
}

func toWire(p *Page) *pageWire {
	w := &pageWire{
		Version:    p.Version,
		URL:        p.URL,
		FinalURL:   p.FinalURL,
		Title:      p.Title,
		Tier:       p.Tier,
		FetchedAt:  p.FetchedAt,
		ETag:       p.ETag,
		LastMod:    p.LastMod,
		ContentSHA: p.ContentSHA,
		Markdown:   p.Markdown,
		RawHTML:    p.RawHTML,
		Sections:   p.Sections,
		Chunks:     p.Chunks,
		EmbedModel: p.EmbedModel,
	}
	if len(p.Vectors) > 0 {
		w.Vectors = make([]encodedVec, len(p.Vectors))
		for i, v := range p.Vectors {
			w.Vectors[i] = encodedVec(v)
		}
	}
	return w
}

func fromWire(w *pageWire) *Page {
	p := &Page{
		Version:    w.Version,
		URL:        w.URL,
		FinalURL:   w.FinalURL,
		Title:      w.Title,
		Tier:       w.Tier,
		FetchedAt:  w.FetchedAt,
		ETag:       w.ETag,
		LastMod:    w.LastMod,
		ContentSHA: w.ContentSHA,
		Markdown:   w.Markdown,
		RawHTML:    w.RawHTML,
		Sections:   w.Sections,
		Chunks:     w.Chunks,
		EmbedModel: w.EmbedModel,
	}
	if len(w.Vectors) > 0 {
		p.Vectors = make([][]float32, len(w.Vectors))
		for i, v := range w.Vectors {
			p.Vectors[i] = []float32(v)
		}
	}
	return p
}

// encodedVec is one chunk's embedding. It marshals to a base64 string of its
// float32 elements laid out little-endian, four bytes each, so the on-disk file
// carries dense vectors without bloating the inspectable JSON.
type encodedVec []float32

func (v encodedVec) MarshalJSON() ([]byte, error) {
	raw := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(f))
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(raw))
}

func (v *encodedVec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("decode vector string: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("decode vector base64: %w", err)
	}
	if len(raw)%4 != 0 {
		return fmt.Errorf("vector byte length %d not a multiple of 4", len(raw))
	}
	out := make(encodedVec, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	*v = out
	return nil
}
