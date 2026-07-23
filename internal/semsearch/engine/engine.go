// Package engine is the native search/related orchestration: it loads (or
// builds) a repo's index, embeds the query on the resident model2vec engine,
// runs the rank stage (search) or semble's find_related selector path
// (related), and returns ranked []semsearch.Result. It is the in-process
// replacement for the semble MCP/CLI lane.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
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

// Search runs native semantic search over the repo (a.Path, else cwd) and
// returns the ranked results. Content types come from a.Kind (default code+docs);
// snippets are truncated to a.MaxSnippetLines (≤0 = full chunk).
func Search(ctx context.Context, emb index.Embedder, a backend.Args) ([]semsearch.Result, error) {
	if strings.TrimSpace(a.Query) == "" {
		return nil, nil
	}
	idx, content, err := loadCached(ctx, emb, a)
	if err != nil {
		return nil, err
	}
	return SearchLoaded(ctx, emb, idx, content, a)
}

// SearchLoaded ranks a.Query against an already-loaded index: it embeds the
// query, runs the rank stage over idx's resident chunks/vectors reusing the
// prebuilt idx.BM25 (nil rebuilds per call), and truncates snippets. Search
// loads idx via the resident cache and calls this; the bench time loop calls it
// directly to time warm serving without a per-query reload.
func SearchLoaded(ctx context.Context, emb index.Embedder, idx *index.Index, content []index.ContentType, a backend.Args) ([]semsearch.Result, error) {
	if len(idx.Chunks) == 0 {
		return nil, nil
	}
	vecs, err := emb.Encode(ctx, []string{a.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	opts := rank.Options{TopK: topK(a.K), Rerank: hasCode(content), BM25: idx.BM25}
	results := rank.Rank(a.Query, vecs[0], idx.Chunks, idx.Vectors, opts)
	truncate(results, a.MaxSnippetLines)
	return results, nil
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
	idx, _, err := loadCached(ctx, emb, a)
	if err != nil {
		return nil, err
	}

	repoRel := repoRelative(idx.Root, file)
	srcIdx := resolveChunk(idx.Chunks, repoRel, line)
	if srcIdx < 0 {
		return nil, fmt.Errorf("semsearch: no indexed chunk contains %s:%d", file, line)
	}

	targetLang := index.DetectLanguage(idx.Chunks[srcIdx].Path)
	queryVec := idx.Vectors[srcIdx]

	type scored struct {
		idx   int
		score float64
	}
	var hits []scored
	for i, c := range idx.Chunks {
		if targetLang != "" && index.DetectLanguage(c.Path) != targetLang {
			continue // find_related's same-language selector
		}
		hits = append(hits, scored{idx: i, score: rank.Cosine(queryVec, idx.Vectors[i])})
	}
	// Score desc, then (start_line, path, corpus index) ascending — rank's
	// canonical tie-break, total even when two chunks share path+start_line.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		ci, cj := idx.Chunks[hits[i].idx], idx.Chunks[hits[j].idx]
		if ci.StartLine != cj.StartLine {
			return ci.StartLine < cj.StartLine
		}
		if ci.Path != cj.Path {
			return ci.Path < cj.Path
		}
		return hits[i].idx < hits[j].idx
	})

	k := topK(a.K)
	// Over-fetch by one so removing the source itself still leaves k results.
	if len(hits) > k+1 {
		hits = hits[:k+1]
	}
	out := make([]semsearch.Result, 0, k)
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
			Content:       snippet(c.Content, a.MaxSnippetLines),
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
	repo, err := repoOrCwd(a.Path)
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
	residentIndex = map[indexKey]*index.Index{}
)

// indexKey identifies a resident index by its resolved repo path and the
// parameters that make two loads incompatible.
type indexKey struct {
	repo    string
	content string
	model   string
	chunker string
}

// loadCached returns the repo's loaded index from the resident cache, loading it
// once on a miss and retaining it for the process's lifetime. It resolves the
// repo exactly as load/index.Load does so a hit and its populating miss share a
// key.
func loadCached(ctx context.Context, emb index.Embedder, a backend.Args) (*index.Index, []index.ContentType, error) {
	content, err := index.ParseContent(a.Kind)
	if err != nil {
		return nil, nil, err
	}
	repo, err := repoOrCwd(a.Path)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := index.ResolveRoot(repo)
	if err != nil {
		return nil, nil, err
	}
	chunker := index.DefaultChunker()
	key := indexKey{repo: resolved, content: index.ContentKey(content), model: ModelID, chunker: chunker.ID()}

	residentMu.Lock()
	defer residentMu.Unlock()
	if idx := residentIndex[key]; idx != nil {
		return idx, content, nil
	}
	idx, err := index.Load(ctx, emb, repo, content, chunker, ModelID)
	if err != nil {
		return nil, nil, err
	}
	residentIndex[key] = idx
	return idx, content, nil
}

// CloseIndexCache drops every retained index so the process frees the memory.
// The MCP server calls it on shutdown alongside the embedder; tests call it to
// force a fresh load.
func CloseIndexCache() {
	residentMu.Lock()
	defer residentMu.Unlock()
	residentIndex = map[indexKey]*index.Index{}
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
// chunk paths use: it resolves the path against cwd, then relativizes to the
// repo root, falling back to the cleaned input when it lies outside the repo.
func repoRelative(root, file string) string {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(mustGetwd(), file)
	}
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(file))
}

// truncate caps each result's content to maxLines in place.
func truncate(results []semsearch.Result, maxLines int) {
	for i := range results {
		results[i].Content = snippet(results[i].Content, maxLines)
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

// repoOrCwd returns path when set, else the current working directory.
func repoOrCwd(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return os.Getwd()
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("engine: getwd: %v", err))
	}
	return wd
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
