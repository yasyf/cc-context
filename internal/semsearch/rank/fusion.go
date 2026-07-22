// Package rank is the retrieval-scoring stage of semsearch: a bit-exact port of
// semble 0.5.2's tokenizer (semble/tokens.py), hand-rolled BM25
// (semble/index/bm25.py), RRF fusion + alpha blend (semble/search.py), and the
// full ranking stack (semble/ranking/*.py). It scores a query against a fixed
// set of chunks plus their embedding vectors and returns ranked results.
//
// Tie-breaking substrate: semble sorts the fused candidate set by start_line to
// counteract hash-order randomness (search.py). This port makes (start_line,
// path) the canonical tie-break everywhere — including the per-leg RRF ranking,
// where semble instead inherits numpy argpartition/argsort order. The two agree
// whenever leg scores are distinct (the common case) and diverge only on exactly
// tied cosine or BM25 leg scores, which numpy orders non-reproducibly.
package rank

import (
	"math"
	"sort"
	"strconv"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// rrfK is semble/search.py _RRF_K.
const rrfK = 60

// scored pairs a chunk (by index into the corpus) with a score, standing in for
// semble's Chunk→float dict entries while preserving explicit ordering.
type scored struct {
	idx   int
	score float64
}

// Options configure a Rank call, mirroring semble.search's knobs.
type Options struct {
	// TopK is the number of results to return; the candidate pool per leg is
	// TopK×5.
	TopK int
	// Alpha weights the semantic leg (1−Alpha to BM25); nil auto-detects via
	// IsSymbolQuery (0.3 symbol / 0.5 NL).
	Alpha *float64
	// Rerank enables the code-tuned ranking stack (coherence, query boosts,
	// path penalties, saturation). When false, results are sorted by fused
	// score alone.
	Rerank bool
}

// Rank scores chunks against a query and returns the top results. queryVec is
// the query embedding; vectors[i] is the embedding of chunks[i]. Mirrors
// semble/search.py search.
func Rank(query string, queryVec []float32, chunks []semsearch.Chunk, vectors [][]float32, opts Options) []semsearch.Result {
	alpha := ResolveAlpha(query, opts.Alpha)
	candidateCount := opts.TopK * 5

	semantic := searchSemantic(queryVec, vectors, chunks, candidateCount)
	semanticScores := make(map[int]float64, len(semantic))
	for _, h := range semantic {
		semanticScores[h.idx] = h.score
	}

	bm25 := searchBM25(buildBM25(chunks), query, chunks, candidateCount)

	normSemantic := rrfScores(semantic, chunks)
	normBM25 := rrfScores(bm25, chunks)

	union := unionOrdered(normSemantic, normBM25, chunks)
	var combined []scored
	for _, idx := range union {
		score := alpha*normSemantic[idx] + (1.0-alpha)*normBM25[idx]
		if score != 0 {
			combined = append(combined, scored{idx: idx, score: score})
		}
	}

	var ranked []scored
	if opts.Rerank {
		boostMultiChunkFiles(combined, chunks)
		combined = applyQueryBoost(combined, query, chunks)
		ranked = rerankTopk(combined, chunks, opts.TopK, alpha < 1.0)
	} else {
		sort.SliceStable(combined, func(i, j int) bool { return combined[i].score > combined[j].score })
		if len(combined) > opts.TopK {
			combined = combined[:opts.TopK]
		}
		ranked = combined
	}

	results := make([]semsearch.Result, len(ranked))
	for i, r := range ranked {
		c := chunks[r.idx]
		results[i] = semsearch.Result{
			FilePath:  c.Path,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Score:     r.score,
			Content:   c.Content,
		}
		if s, ok := semanticScores[r.idx]; ok {
			results[i].SemanticScore = &s
		}
	}
	return results
}

// Cosine returns the cosine similarity of two float32 vectors — semble's
// 1 − cosine_distance (index/dense.py). Computed in float64 (semble uses numpy
// float32); the widened precision only affects the reported SemanticScore, since
// fusion is rank-based.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// searchSemantic returns the top candidateCount chunks by cosine similarity.
// Mirrors semble/search.py _search_semantic over the whole corpus.
func searchSemantic(queryVec []float32, vectors [][]float32, chunks []semsearch.Chunk, candidateCount int) []scored {
	if candidateCount < 1 || len(vectors) == 0 {
		return nil
	}
	hits := make([]scored, len(vectors))
	for i := range vectors {
		hits[i] = scored{idx: i, score: Cosine(queryVec, vectors[i])}
	}
	sortByScoreThenCanonical(hits, chunks)
	if candidateCount < len(hits) {
		return hits[:candidateCount]
	}
	return hits
}

// searchBM25 returns the top candidateCount chunks by BM25 score, dropping
// zero-score (unmatched) chunks. Mirrors semble/search.py _search_bm25.
func searchBM25(bm *BM25, query string, chunks []semsearch.Chunk, candidateCount int) []scored {
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	raw := bm.GetScores(tokens)
	hits := make([]scored, len(raw))
	for i, s := range raw {
		hits[i] = scored{idx: i, score: s}
	}
	sortByScoreThenCanonical(hits, chunks)
	if candidateCount < len(hits) {
		hits = hits[:candidateCount]
	}
	var out []scored
	for _, h := range hits {
		if h.score > 0 {
			out = append(out, h)
		}
	}
	return out
}

// buildBM25 constructs the corpus BM25 index, tokenizing each chunk's
// path-enriched content. Mirrors semble/index/create.py indexing.
func buildBM25(chunks []semsearch.Chunk) *BM25 {
	bm := NewBM25()
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		id := strconv.Itoa(i)
		ids[i] = id
		bm.AddDocument(id, Tokenize(EnrichForBM25(c)))
	}
	bm.SetDocOrder(ids)
	return bm
}

// rrfScores maps each hit to its reciprocal-rank-fusion score 1/(k+rank).
// Mirrors semble/search.py _rrf_scores.
func rrfScores(hits []scored, chunks []semsearch.Chunk) map[int]float64 {
	out := make(map[int]float64, len(hits))
	if len(hits) == 0 {
		return out
	}
	ranked := make([]scored, len(hits))
	copy(ranked, hits)
	sortByScoreThenCanonical(ranked, chunks)
	for i, h := range ranked {
		out[h.idx] = 1.0 / (rrfK + float64(i+1))
	}
	return out
}

// unionOrdered returns the union of two score maps' chunk indices, sorted by
// (start_line, path) — semble/search.py's start-line pre-sort.
func unionOrdered(a, b map[int]float64, chunks []semsearch.Chunk) []int {
	set := make(map[int]bool, len(a)+len(b))
	for idx := range a {
		set[idx] = true
	}
	for idx := range b {
		set[idx] = true
	}
	union := make([]int, 0, len(set))
	for idx := range set {
		union = append(union, idx)
	}
	sort.Slice(union, func(i, j int) bool {
		ca, cb := chunks[union[i]], chunks[union[j]]
		if ca.StartLine != cb.StartLine {
			return ca.StartLine < cb.StartLine
		}
		return ca.Path < cb.Path
	})
	return union
}

// sortByScoreThenCanonical sorts hits by score descending, breaking ties by
// (start_line, path) ascending — a strict total order over distinct chunks.
func sortByScoreThenCanonical(hits []scored, chunks []semsearch.Chunk) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		ca, cb := chunks[hits[i].idx], chunks[hits[j].idx]
		if ca.StartLine != cb.StartLine {
			return ca.StartLine < cb.StartLine
		}
		return ca.Path < cb.Path
	})
}
