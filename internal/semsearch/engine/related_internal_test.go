package engine

import (
	"math"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

func fullSortRelated(idx *index.Index, srcIdx, k int) []relatedScore {
	targetLang := index.DetectLanguage(idx.Chunks[srcIdx].Path)
	hits := make([]relatedScore, 0)
	for i, c := range idx.Chunks {
		if targetLang != "" && index.DetectLanguage(c.Path) != targetLang {
			continue
		}
		hits = append(hits, relatedScore{idx: i, score: rank.Cosine(idx.Vectors[srcIdx], idx.Vectors[i])})
	}
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
	if k < len(hits)-1 {
		hits = hits[:k+1]
	}
	return hits
}

func relatedFixture(n int, r *rand.Rand) *index.Index {
	idx := &index.Index{}
	paths := []string{"a.go", "b.go", "a.py", "a.md", "unknown.data", "a.go"}
	for range n {
		idx.Chunks = append(idx.Chunks, semsearch.Chunk{
			Path: paths[r.Intn(len(paths))], StartLine: r.Intn(8),
		})
		vec := make([]float32, 8)
		for j := range vec {
			vec[j] = float32(r.Intn(3) - 1)
		}
		idx.Vectors = append(idx.Vectors, vec)
	}
	return idx
}

func TestRelatedTopKMatchesFullSort(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(517)) //nolint:gosec // deterministic test vectors, not security
	for _, n := range []int{1, 2, 17, 128, 4096} {
		idx := relatedFixture(n, r)
		for range 12 {
			src := r.Intn(n)
			for _, requested := range []int{-9, 0, 1, 5, 16, n, n + 5, math.MaxInt} {
				k := topK(requested)
				got := relatedTopK(idx, src, k)
				want := fullSortRelated(idx, src, k)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("n=%d src=%d k=%d: got %v, want %v", n, src, requested, got, want)
				}
			}
		}
	}
}

func TestRelatedTopKTiesAndSelfExclusion(t *testing.T) {
	t.Parallel()
	idx := &index.Index{
		Chunks: []semsearch.Chunk{
			{Path: "b.go", StartLine: 1},
			{Path: "a.go", StartLine: 1},
			{Path: "a.go", StartLine: 1},
			{Path: "a.py", StartLine: 0},
			{Path: "z.go", StartLine: 0},
		},
		Vectors: [][]float32{{1}, {1}, {1}, {1}, {1}},
	}
	for src := range idx.Chunks {
		for _, k := range []int{1, 2, 5} {
			got := relatedTopK(idx, src, k)
			want := fullSortRelated(idx, src, k)
			got = slices.DeleteFunc(got, func(hit relatedScore) bool { return hit.idx == src })
			want = slices.DeleteFunc(want, func(hit relatedScore) bool { return hit.idx == src })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("src=%d k=%d: got %v, want %v", src, k, got, want)
			}
		}
	}
}

func TestRelatedTopKStorageDoesNotGrowWithCorpus(t *testing.T) {
	const k = 5
	for _, n := range []int{64, 4096} {
		idx := relatedFixture(n, rand.New(rand.NewSource(518))) //nolint:gosec // deterministic test vectors, not security
		allocations := testing.AllocsPerRun(3, func() {
			hits := relatedTopK(idx, 0, k)
			if cap(hits) > k+1 {
				t.Fatalf("n=%d: score capacity=%d, want <=%d", n, cap(hits), k+1)
			}
		})
		if allocations > 4 {
			t.Fatalf("n=%d: allocations=%g, want <=4", n, allocations)
		}
	}
}
