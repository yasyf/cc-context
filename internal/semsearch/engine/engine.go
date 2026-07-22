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

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

// defaultTopK matches semble's search/find_related default (top_k=5).
const defaultTopK = 5

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
	idx, content, err := load(ctx, emb, a)
	if err != nil {
		return nil, err
	}
	if len(idx.Chunks) == 0 {
		return nil, nil
	}

	vecs, err := emb.Encode(ctx, []string{a.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	opts := rank.Options{TopK: topK(a.K), Rerank: hasCode(content)}
	results := rank.Rank(a.Query, vecs[0], idx.Chunks, idx.Vectors, opts)
	truncate(results, a.MaxSnippetLines)
	return results, nil
}

// Related runs semble's find_related selector path: it resolves the chunk at
// a.Query's file:line, restricts to the source's language, ranks the rest by
// cosine similarity to the source's embedding, and drops the source itself.
func Related(ctx context.Context, emb index.Embedder, a backend.Args) ([]semsearch.Result, error) {
	file, line, err := splitLoc(a.Query)
	if err != nil {
		return nil, err
	}
	idx, _, err := load(ctx, emb, a)
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
	// Score desc, then (start_line, path) ascending — rank's canonical tie-break.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		ci, cj := idx.Chunks[hits[i].idx], idx.Chunks[hits[j].idx]
		if ci.StartLine != cj.StartLine {
			return ci.StartLine < cj.StartLine
		}
		return ci.Path < cj.Path
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

// load resolves the repo and returns its loaded index plus the content types.
func load(ctx context.Context, emb index.Embedder, a backend.Args) (*index.Index, []index.ContentType, error) {
	content, err := index.ParseContent(a.Kind)
	if err != nil {
		return nil, nil, err
	}
	repo, err := repoOrCwd(a.Path)
	if err != nil {
		return nil, nil, err
	}
	idx, err := index.Load(ctx, emb, repo, content, index.DefaultChunker(), embed.Repo)
	if err != nil {
		return nil, nil, err
	}
	return idx, content, nil
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
