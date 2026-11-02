// Package rank is the retrieval-scoring stage of semsearch: a bit-exact port of
// semble 0.5.2's tokenizer (semble/tokens.py), hand-rolled BM25
// (semble/index/bm25.py), RRF fusion + alpha blend (semble/search.py), and the
// full ranking stack (semble/ranking/*.py). It scores a query against a fixed
// set of chunks plus their embedding vectors and returns ranked results.
//
// Tie-breaking substrate: semble sorts the fused candidate set by start_line to
// counteract hash-order randomness (search.py). This port makes (start_line,
// path, corpus index) the canonical tie-break everywhere — including the per-leg
// RRF ranking, where semble instead inherits numpy argpartition/argsort order.
// The corpus-index final key makes the order a strict total order even when two
// distinct chunks share both path and start_line. The two agree whenever leg
// scores are distinct (the common case) and diverge only on exactly tied cosine
// or BM25 leg scores, which numpy orders non-reproducibly.
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
	// BM25 is an optional prebuilt BM25 over the same chunks in the same order;
	// nil rebuilds per call.
	BM25 *BM25
}

// Rank scores chunks against a query and returns the top results. queryVec is
// the query embedding; vectors[i] is the embedding of chunks[i]. Mirrors
// semble/search.py search.
func Rank(query string, queryVec []float32, chunks []semsearch.Chunk, vectors [][]float32, opts Options) []semsearch.Result {
	return rankCorpus(query, queryVec, denseCorpus{chunks: chunks, vectors: vectors}, chunks, opts, nil)
}

func rankCorpus(query string, queryVec []float32, source corpus, chunks []semsearch.Chunk, opts Options, bm BM25Scorer) []semsearch.Result {
	alpha := ResolveAlpha(query, opts.Alpha)
	candidateCount := opts.TopK * 5

	semantic := searchSemantic(queryVec, source, candidateCount)
	semanticScores := make(map[int]float64, len(semantic))
	for _, h := range semantic {
		semanticScores[h.idx] = h.score
	}

	if bm == nil {
		loaded := opts.BM25
		if loaded == nil {
			loaded = buildBM25(chunks)
		}
		bm = loaded
	}
	bm25 := searchBM25(bm, query, source.Chunk, candidateCount)

	normSemantic := rrfScores(semantic, source.Chunk)
	normBM25 := rrfScores(bm25, source.Chunk)

	union := unionOrdered(normSemantic, normBM25, source.Chunk)
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
		c := source.Chunk(r.idx)
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
// fusion is rank-based. A zero-norm vector (an out-of-vocabulary query embeds to
// all zeros) yields 0, mirroring embed.Cosine — dividing by a zero norm would
// produce NaN, which defeats the canonical tie-break and fails json.Marshal.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func searchSemantic(queryVec []float32, source corpus, candidateCount int) []scored {
	if candidateCount < 1 || source.Len() == 0 {
		return nil
	}
	hits := make([]scored, source.Len())
	for i := range hits {
		hits[i] = scored{idx: i, score: Cosine(queryVec, source.Vector(i))}
	}
	sortByScoreThenCanonical(hits, source.Chunk)
	if candidateCount < len(hits) {
		return hits[:candidateCount]
	}
	return hits
}

func searchBM25(bm BM25Scorer, query string, chunkAt func(int) semsearch.Chunk, candidateCount int) []scored {
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	raw := bm.GetScores(tokens)
	hits := make([]scored, len(raw))
	for i, s := range raw {
		hits[i] = scored{idx: i, score: s}
	}
	sortByScoreThenCanonical(hits, chunkAt)
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

// BuildBM25 constructs the corpus BM25 index over chunks in the given order,
// for callers that prebuild it once and reuse it across Rank calls via
// Options.BM25 instead of paying buildBM25 on every query.
func BuildBM25(chunks []semsearch.Chunk) *BM25 { return buildBM25(chunks) }

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

func rrfScores(hits []scored, chunkAt func(int) semsearch.Chunk) map[int]float64 {
	out := make(map[int]float64, len(hits))
	if len(hits) == 0 {
		return out
	}
	ranked := make([]scored, len(hits))
	copy(ranked, hits)
	sortByScoreThenCanonical(ranked, chunkAt)
	for i, h := range ranked {
		out[h.idx] = 1.0 / (rrfK + float64(i+1))
	}
	return out
}

func unionOrdered(a, b map[int]float64, chunkAt func(int) semsearch.Chunk) []int {
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
		ca, cb := chunkAt(union[i]), chunkAt(union[j])
		if ca.StartLine != cb.StartLine {
			return ca.StartLine < cb.StartLine
		}
		if ca.Path != cb.Path {
			return ca.Path < cb.Path
		}
		return union[i] < union[j] // corpus index — distinct chunks can share path+start_line
	})
	return union
}

func sortByScoreThenCanonical(hits []scored, chunkAt func(int) semsearch.Chunk) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		ca, cb := chunkAt(hits[i].idx), chunkAt(hits[j].idx)
		if ca.StartLine != cb.StartLine {
			return ca.StartLine < cb.StartLine
		}
		if ca.Path != cb.Path {
			return ca.Path < cb.Path
		}
		return hits[i].idx < hits[j].idx
	})
}
