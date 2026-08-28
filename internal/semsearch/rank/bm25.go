package rank

import (
	"fmt"
	"math"
	"strings"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// Constants ported verbatim from semble/index/bm25.py (semble 0.5.2).
const (
	bm25K1 = 1.5  // term-frequency saturation
	bm25B  = 0.75 // document-length normalization
)

type posting struct {
	doc int32
	tf  int32
}

// BM25 is a hand-rolled BM25 inverted index over a fixed set of documents,
// mirroring semble/index/bm25.py. It is a distinct implementation from
// internal/web/bm25.go, whose k1 and tokenizer differ. Scores accumulate in
// float32 in query-term first-occurrence order — matching semble's numpy float32
// array and Counter iteration — so two documents with identical length and term
// frequencies reach bit-identical scores and the canonical tie-break fires
// deterministically (a float64 accumulation over map-order terms does not).
//
// Documents are stored once, as per-term posting lists in insertion order. Each
// document contributes to a term's accumulator at most once, so iterating a
// posting list in insertion order rather than map order leaves every score
// bit-identical while holding one int32 pair per pair instead of a map entry.
type BM25 struct {
	ids         map[string]int32 // chunk id → insertion index
	docLengths  []int32          // insertion index → token count
	totalDocLen int
	postings    map[string][]posting // term → postings, in insertion order
	positions   []int32              // insertion index → doc-order position, -1 when absent
	nOrder      int
}

// NewBM25 creates an empty index.
func NewBM25() *BM25 {
	return &BM25{
		ids:      map[string]int32{},
		postings: map[string][]posting{},
	}
}

// AddDocument indexes one document, panicking on a duplicate id.
func (b *BM25) AddDocument(chunkID string, tokens []string) {
	if _, ok := b.ids[chunkID]; ok {
		panic(fmt.Sprintf("rank: chunk_id already indexed: %s", chunkID))
	}
	doc := int32(len(b.docLengths))
	b.ids[chunkID] = doc
	b.docLengths = append(b.docLengths, int32(len(tokens)))
	b.totalDocLen += len(tokens)
	counts := map[string]int32{}
	for _, t := range tokens {
		counts[t]++
	}
	for term, count := range counts {
		b.postings[term] = append(b.postings[term], posting{doc: doc, tf: count})
	}
}

// SetDocOrder fixes the chunk order that GetScores' output is aligned to.
func (b *BM25) SetDocOrder(chunkIDs []string) {
	b.nOrder = len(chunkIDs)
	b.positions = make([]int32, len(b.docLengths))
	for i := range b.positions {
		b.positions[i] = -1
	}
	for i, id := range chunkIDs {
		if doc, ok := b.ids[id]; ok {
			b.positions[doc] = int32(i)
		}
	}
}

// GetScores returns BM25 scores for a tokenized query, aligned with the doc
// order. Mirrors semble/index/bm25.py BM25.get_scores (without the weight mask,
// which serves selector filtering outside this stage's scope). Terms iterate in
// first-occurrence order (Python Counter) and each document's score accumulates
// in float32 (semble's numpy float32 array), so the result is bit-stable across
// calls and tied documents compare exactly equal — a float64 sum over map-order
// terms is not associative and would vary run to run.
func (b *BM25) GetScores(tokens []string) []float64 {
	scores := make([]float64, b.nOrder)
	corpusSize := len(b.docLengths)
	if len(tokens) == 0 || corpusSize == 0 || b.nOrder == 0 {
		return scores
	}
	avgdl := float64(b.totalDocLen) / float64(corpusSize)
	terms := make([]string, 0, len(tokens))
	queryTF := map[string]int{}
	for _, t := range tokens {
		if _, ok := queryTF[t]; !ok {
			terms = append(terms, t)
		}
		queryTF[t]++
	}
	acc := make([]float32, b.nOrder)
	for _, term := range terms {
		docs := b.postings[term]
		if len(docs) == 0 {
			continue
		}
		qtf := queryTF[term]
		df := len(docs)
		idf := math.Log(1 + (float64(corpusSize)-float64(df)+0.5)/(float64(df)+0.5))
		for _, p := range docs {
			idx := b.positions[p.doc]
			if idx < 0 {
				continue
			}
			dl := b.docLengths[p.doc]
			tfc := float64(p.tf) / (bm25K1*(1-bm25B+bm25B*float64(dl)/avgdl) + float64(p.tf))
			acc[idx] += float32(float64(qtf) * idf * tfc)
		}
	}
	for i, v := range acc {
		scores[i] = float64(v)
	}
	return scores
}

// EnrichForBM25 appends file-path components to a chunk's content so path-based
// queries score, repeating the filename stem twice and adding the last three
// directory components. Mirrors semble/index/sparse.py enrich_for_bm25; assumes
// chunk.Path is repo-relative.
func EnrichForBM25(chunk semsearch.Chunk) string {
	stem := pathStem(chunk.Path)
	dirParts := pathParentDirs(chunk.Path)
	if len(dirParts) > 3 {
		dirParts = dirParts[len(dirParts)-3:]
	}
	dirText := strings.Join(dirParts, " ")
	return fmt.Sprintf("%s %s %s %s", chunk.Content, stem, stem, dirText)
}
