package web

import "sort"

// Weighted reciprocal-rank fusion constants. score(c) = denseWeight/(rrfK +
// denseRank(c)) + lexWeight/(rrfK + lexRank(c)), ranks 1-based (best = rank 1);
// rrfK=60 is the canonical RRF constant.
const (
	rrfK        = 60.0
	denseWeight = 3.0
	lexWeight   = 1.0
)

// denseOrder scores every chunk vector by dot product against query (both
// L2-normalized upstream, so dot == cosine) and returns all chunk indices
// best-first, ties broken by chunk order — a permutation of [0, len(docVecs)).
func denseOrder(docVecs [][]float32, query []float32) []int {
	scores := make([]float64, len(docVecs))
	for i, v := range docVecs {
		scores[i] = dot(v, query)
	}
	order := make([]int, len(docVecs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, c := order[i], order[j]
		if scores[a] != scores[c] {
			return scores[a] > scores[c]
		}
		return a < c
	})
	return order
}

// fuse combines the dense and lexical best-first orderings into the top-k chunk
// indices by weighted reciprocal-rank fusion, best-first, ties broken by chunk
// order. dense and lex are permutations of [0, n); dense nil or empty degrades
// to lexical ranks alone at weight 1.0 (BM25-only mode). k is clamped to n.
func fuse(dense, lex []int, k int) []int {
	n := len(lex)
	fused := make([]float64, n)
	addRankScores(fused, lex, lexWeight)
	if len(dense) > 0 {
		addRankScores(fused, dense, denseWeight)
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, c := order[i], order[j]
		if fused[a] != fused[c] {
			return fused[a] > fused[c]
		}
		return a < c
	})
	if k < n {
		order = order[:k]
	}
	return order
}

// addRankScores adds weight/(rrfK + rank) to fused for each chunk, where rank is
// its 1-based position in the best-first order.
func addRankScores(fused []float64, order []int, weight float64) {
	for pos, doc := range order {
		fused[doc] += weight / (rrfK + float64(pos+1))
	}
}

// dot is the inner product of two equal-length float32 vectors in float64.
func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
