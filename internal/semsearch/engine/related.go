package engine

import (
	"cmp"
	"container/heap"
	"slices"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

type relatedScore struct {
	idx   int
	score float64
}

type relatedHeap struct {
	chunks []semsearch.Chunk
	hits   []relatedScore
}

func (h relatedHeap) Len() int { return len(h.hits) }
func (h relatedHeap) Less(i, j int) bool {
	return compareRelated(h.hits[i], h.hits[j], h.chunks) > 0
}
func (h relatedHeap) Swap(i, j int) { h.hits[i], h.hits[j] = h.hits[j], h.hits[i] }
func (h *relatedHeap) Push(v any)   { h.hits = append(h.hits, v.(relatedScore)) }
func (h *relatedHeap) Pop() any {
	v := h.hits[len(h.hits)-1]
	h.hits = h.hits[:len(h.hits)-1]
	return v
}

func compareRelated(a, b relatedScore, chunks []semsearch.Chunk) int {
	if a.score != b.score {
		if a.score > b.score {
			return -1
		}
		return 1
	}
	ca, cb := chunks[a.idx], chunks[b.idx]
	if ca.StartLine != cb.StartLine {
		return cmp.Compare(ca.StartLine, cb.StartLine)
	}
	if ca.Path != cb.Path {
		return cmp.Compare(ca.Path, cb.Path)
	}
	return cmp.Compare(a.idx, b.idx)
}

func relatedTopK(idx *index.Index, srcIdx, k int) []relatedScore {
	limit := min(k, len(idx.Chunks))
	if limit < len(idx.Chunks) {
		limit++
	}
	h := &relatedHeap{chunks: idx.Chunks, hits: make([]relatedScore, 0, limit)}
	targetLang := index.DetectLanguage(idx.Chunks[srcIdx].Path)
	queryVec := idx.Vectors[srcIdx]
	for i, c := range idx.Chunks {
		if targetLang != "" && index.DetectLanguage(c.Path) != targetLang {
			continue
		}
		candidate := relatedScore{idx: i, score: rank.Cosine(queryVec, idx.Vectors[i])}
		if len(h.hits) < limit {
			h.hits = append(h.hits, candidate)
			if len(h.hits) == limit {
				heap.Init(h)
			}
		} else if compareRelated(candidate, h.hits[0], idx.Chunks) < 0 {
			h.hits[0] = candidate
			heap.Fix(h, 0)
		}
	}
	slices.SortFunc(h.hits, func(a, b relatedScore) int { return compareRelated(a, b, idx.Chunks) })
	return h.hits
}
