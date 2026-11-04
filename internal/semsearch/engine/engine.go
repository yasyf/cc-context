// Package engine is the native search/related orchestration: it loads (or
// builds) a repo's index, embeds the query on the resident model2vec engine,
// runs the rank stage (search) or semble's find_related selector path
// (related), and returns ranked []semsearch.Result. It is the in-process
// replacement for the semble MCP/CLI lane.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
	"github.com/yasyf/cc-context/internal/workspace"
)

// defaultTopK matches semble's search/find_related default (top_k=5).
const defaultTopK = 5

// ModelID is the cache identity for the embedding model: the code pin's repo plus
// its pinned weights revision. A weights bump changes Revision while Repo holds, so
// folding the revision in invalidates the on-disk cache — otherwise stale
// old-weights vectors would be served against new-weights query embeddings,
// silently wrong.
var ModelID = embed.CodePin.Repo + "@" + embed.CodePin.Revision

// Fused-search golden parity (chunk→embed→rank against
// testdata/goldens/search_results.json) lives in semsearch's
// fused_parity_test.go; the engine layer adds the walker and cache on top and
// is exercised by the fake-embedder tests here.

// Search runs native semantic search over the repo (a.Path, else the project
// root) and returns the ranked results. Content types come from a.Kind (default
// code+docs); snippets are truncated to a.MaxSnippetLines (≤0 = full chunk).
func Search(ctx context.Context, emb index.Embedder, a backend.Args) ([]semsearch.Result, error) {
	if strings.TrimSpace(a.Query) == "" {
		return nil, nil
	}
	idx, content, release, err := loadCached(ctx, emb, a)
	if err != nil {
		return nil, err
	}
	defer release()
	return SearchLoaded(ctx, emb, idx, content, a)
}

// SearchLoaded ranks a.Query against an already-loaded index: it embeds the
// query, runs the rank stage over idx's chunks/vectors with the corpus BM25
// idx.BM25For supplies, and truncates snippets into strings of their own, so the
// results outlive idx. Search loads idx via the resident cache and calls this;
// the bench time loop calls it directly to time warm serving without a
// per-query reload.
func SearchLoaded(ctx context.Context, emb index.Embedder, idx *index.Index, content []index.ContentType, a backend.Args) ([]semsearch.Result, error) {
	if len(idx.Chunks) == 0 {
		return nil, nil
	}
	vecs, err := emb.Encode(ctx, []string{a.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	bm, err := idx.BM25For(a.Query)
	if err != nil {
		return nil, fmt.Errorf("load query postings: %w", err)
	}
	opts := rank.Options{TopK: topK(a.K), Rerank: hasCode(content), BM25: bm}
	results := rank.Rank(a.Query, vecs[0], idx.Chunks, idx.Vectors, opts)
	truncate(results, a.MaxSnippetLines)
	return results, nil
}

// HasIndex reports whether repo already has an index for a's content types,
// without loading the embedder or building anything.
func HasIndex(ctx context.Context, a backend.Args, repo string) (bool, error) {
	content, err := index.ParseContent(a.Kind)
	if err != nil {
		return false, err
	}
	return index.HasExisting(ctx, repo, content, index.DefaultChunker(), ModelID)
}

// Warm builds or refreshes the persistent index for the requested repo.
func Warm(ctx context.Context, emb index.Embedder, a backend.Args) (*index.Index, error) {
	idx, _, err := load(ctx, emb, a)
	return idx, err
}

// Related runs semble's find_related selector path: it resolves the chunk at
// a.Query's file:line, restricts to the source's language, ranks the rest by
// cosine similarity to the source's embedding, and drops the source itself.
func Related(ctx context.Context, emb index.Embedder, a backend.Args) ([]semsearch.Result, error) {
	file, line, err := splitLoc(a.Query)
	if err != nil {
		return nil, err
	}
	idx, _, release, err := loadCached(ctx, emb, a)
	if err != nil {
		return nil, err
	}
	defer release()

	repoRel := repoRelative(ctx, idx.Root, file)
	srcIdx := resolveChunk(idx.Chunks, repoRel, line)
	if srcIdx < 0 {
		return nil, fmt.Errorf("semsearch: no indexed chunk contains %s:%d", file, line)
	}

	k := topK(a.K)
	hits := relatedTopK(idx, srcIdx, k)
	out := make([]semsearch.Result, 0, min(k, len(hits)))
	for _, h := range hits {
		if h.idx == srcIdx {
			continue
		}
		c := idx.Chunks[h.idx]
		score := h.score
		out = append(out, semsearch.Result{
			FilePath:      c.Path,
			StartLine:     c.StartLine,
			EndLine:       c.EndLine,
			Score:         score,
			SemanticScore: &score,
			Content:       strings.Clone(snippet(c.Content, a.MaxSnippetLines)),
		})
		if len(out) == k {
			break
		}
	}
	return out, nil
}

// load resolves the repo and returns its freshly loaded index plus the content
// types. Warm calls it to (re)build the persistent index; the resident
// serving path uses loadCached.
func load(ctx context.Context, emb index.Embedder, a backend.Args) (*index.Index, []index.ContentType, error) {
	content, err := index.ParseContent(a.Kind)
	if err != nil {
		return nil, nil, err
	}
	repo, err := repoOrRoot(ctx, a.Path)
	if err != nil {
		return nil, nil, err
	}
	idx, err := index.Load(ctx, emb, repo, content, index.DefaultChunker(), ModelID)
	if err != nil {
		return nil, nil, err
	}
	return idx, content, nil
}

// residentIndex is the process-lifetime index cache: one loaded *index.Index per
// (resolved repo, content, model, chunker). A warm MCP server serves many
// search/related queries against the retained in-memory index instead of
// re-reading the disk cache and rebuilding BM25 on every call. It mirrors
// semble's serve-from-memory model — a repo edited mid-process is NOT
// re-indexed until the process restarts, the intended semble-equivalent
// behavior. The one-query-per-process CLI always misses; only the resident MCP
// server (and the bench time loop, which loads once) benefit. Mirrors the
// resident embedder singleton in internal/dispatch: package-level state, a
// mutex, and a Close that frees it.
var (
	residentMu    sync.Mutex
	residentIndex = map[indexKey]*residentEntry{}
)

const (
	// residentCap bounds the resident cache. A server almost always serves one
	// workspace; a second entry covers the default-content then all-content
	// pattern without letting one session retain an index per repo it names.
	residentCap = 2
	diskIdleTTL = 14 * 24 * time.Hour
)

type residentEntry struct {
	idx      *index.Index
	lastUsed time.Time
	holds    int
	evicted  bool
}

// release ends one loadCached hold, closing the index once it has been evicted
// and no query still reads it.
func (e *residentEntry) release() {
	residentMu.Lock()
	defer residentMu.Unlock()
	e.holds--
	if e.evicted && e.holds == 0 {
		e.idx.Close()
	}
}

// drop evicts key from resident, closing its index unless a query still holds
// it. The caller holds residentMu.
func drop(resident map[indexKey]*residentEntry, key indexKey) {
	e := resident[key]
	delete(resident, key)
	e.evicted = true
	if e.holds == 0 {
		e.idx.Close()
	}
}

// indexKey identifies a resident index by its resolved repo path and the
// parameters that make two loads incompatible.
type indexKey struct {
	repo    string
	content string
	model   string
	chunker string
}

// loadCached returns the repo's loaded index from the resident cache, loading it
// once on a miss, plus the release that ends this caller's hold on it: an
// evicted index stays open until its last holder releases it. It resolves the
// repo exactly as load/index.Load does so a hit and its populating miss share a
// key.
func loadCached(ctx context.Context, emb index.Embedder, a backend.Args) (*index.Index, []index.ContentType, func(), error) {
	content, err := index.ParseContent(a.Kind)
	if err != nil {
		return nil, nil, nil, err
	}
	repo, err := repoOrRoot(ctx, a.Path)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved, err := index.ResolveRoot(repo)
	if err != nil {
		return nil, nil, nil, err
	}
	chunker := index.DefaultChunker()
	key := indexKey{repo: resolved, content: index.ContentKey(content), model: ModelID, chunker: chunker.ID()}

	residentMu.Lock()
	defer residentMu.Unlock()
	if e := residentIndex[key]; e != nil {
		e.lastUsed = time.Now()
		e.holds++
		return e.idx, content, e.release, nil
	}
	idx, err := index.Load(ctx, emb, repo, content, chunker, ModelID)
	if err != nil {
		return nil, nil, nil, err
	}
	evictOldest(residentIndex, residentCap-1)
	e := &residentEntry{idx: idx, lastUsed: time.Now(), holds: 1}
	residentIndex[key] = e
	return idx, content, e.release, nil
}

// evictOldest drops least-recently-used entries from resident until at most n
// remain. The caller holds residentMu.
func evictOldest(resident map[indexKey]*residentEntry, n int) {
	for len(resident) > n {
		var oldest indexKey
		first := true
		for k, e := range resident {
			if first || e.lastUsed.Before(resident[oldest].lastUsed) {
				oldest, first = k, false
			}
		}
		drop(resident, oldest)
	}
}

// SweepIdle drops every resident index last queried before cutoff and returns
// how many it dropped. The caller returns the freed pages to the OS.
func SweepIdle(cutoff time.Time) int {
	residentMu.Lock()
	defer residentMu.Unlock()
	return sweepIdle(residentIndex, cutoff)
}

func sweepIdle(resident map[indexKey]*residentEntry, cutoff time.Time) int {
	dropped := 0
	for k, e := range resident {
		if e.lastUsed.Before(cutoff) {
			drop(resident, k)
			dropped++
		}
	}
	return dropped
}

// PruneDisk deletes every on-disk index no Load has touched for diskIdleTTL. The
// MCP server runs it once at startup; the one-shot CLI never calls it.
func PruneDisk(ctx context.Context) {
	if err := index.Prune(ctx, diskIdleTTL); err != nil && ctx.Err() == nil {
		slog.Warn("semsearch: prune index cache", "err", err)
	}
}

// CloseIndexCache drops every retained index so the process frees the memory.
// The MCP server calls it on shutdown alongside the embedder; tests call it to
// force a fresh load.
func CloseIndexCache() {
	residentMu.Lock()
	defer residentMu.Unlock()
	for k := range residentIndex {
		drop(residentIndex, k)
	}
}

// resolveChunk returns the index of the chunk containing line in file, or -1 —
// semble's resolve_chunk: a chunk whose range strictly contains the line wins
// over one that merely ends on it (character-granular boundaries can share a
// line).
func resolveChunk(chunks []semsearch.Chunk, file string, line int) int {
	fallback := -1
	for i, c := range chunks {
		if c.Path != file || line < c.StartLine || line > c.EndLine {
			continue
		}
		if line < c.EndLine {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

// repoRelative normalizes a user-given file path to the repo-relative slash form
// chunk paths use: it resolves the path against the project root, then
// relativizes to the repo root, falling back to the cleaned input when it lies
// outside the repo.
func repoRelative(ctx context.Context, root, file string) string {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(mustRoot(ctx), file)
	}
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(file))
}

// truncate caps each result's content to maxLines in place.
func truncate(results []semsearch.Result, maxLines int) {
	for i := range results {
		results[i].Content = strings.Clone(snippet(results[i].Content, maxLines))
	}
}

// snippet returns the first maxLines lines of content, or all of it when
// maxLines ≤ 0 — semble's max_snippet_lines=None semantics.
func snippet(content string, maxLines int) string {
	if maxLines <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= maxLines {
		return content
	}
	return strings.Join(lines[:maxLines], "\n")
}

func topK(k int) int {
	if k > 0 {
		return k
	}
	return defaultTopK
}

// hasCode reports whether code was indexed; semble reranks search results when
// ContentType.CODE is present.
func hasCode(content []index.ContentType) bool {
	for _, t := range content {
		if t == index.ContentCode {
			return true
		}
	}
	return false
}

// repoOrRoot returns path when set, else the project root.
func repoOrRoot(ctx context.Context, path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return workspace.RootFrom(ctx)
}

func mustRoot(ctx context.Context) string {
	root, err := workspace.RootFrom(ctx)
	if err != nil {
		panic(fmt.Sprintf("engine: resolve root: %v", err))
	}
	return root
}

// splitLoc parses a "file:line" location into its file and 1-indexed line.
func splitLoc(loc string) (file string, line int, err error) {
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return "", 0, fmt.Errorf("semsearch: location %q is not file:line", loc)
	}
	n := 0
	for _, r := range loc[i+1:] {
		if r < '0' || r > '9' {
			return "", 0, fmt.Errorf("semsearch: location %q has non-numeric line", loc)
		}
		n = n*10 + int(r-'0')
	}
	if loc[i+1:] == "" {
		return "", 0, fmt.Errorf("semsearch: location %q has no line", loc)
	}
	return loc[:i], n, nil
}
