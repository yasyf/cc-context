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

// BM25 is a hand-rolled BM25 inverted index over a fixed set of documents,
// mirroring semble/index/bm25.py. It is a distinct implementation from
// internal/web/bm25.go, whose k1 and tokenizer differ. Scores accumulate in
// float32 in query-term first-occurrence order — matching semble's numpy float32
// array and Counter iteration — so two documents with identical length and term
// frequencies reach bit-identical scores and the canonical tie-break fires
// deterministically (a float64 accumulation over map-order terms does not).
type BM25 struct {
	documents   map[string]map[string]int // chunk id → term counts
	docLengths  map[string]int
	totalDocLen int
	postings    map[string]map[string]int // term → {chunk id: count}
	docOrder    []string
	positions   map[string]int
}

// NewBM25 creates an empty index.
func NewBM25() *BM25 {
	return &BM25{
		documents:  map[string]map[string]int{},
		docLengths: map[string]int{},
		postings:   map[string]map[string]int{},
		positions:  map[string]int{},
	}
}

// AddDocument indexes one document, panicking on a duplicate id.
func (b *BM25) AddDocument(chunkID string, tokens []string) {
	if _, ok := b.documents[chunkID]; ok {
		panic(fmt.Sprintf("rank: chunk_id already indexed: %s", chunkID))
	}
	counts := map[string]int{}
	for _, t := range tokens {
		counts[t]++
	}
	b.documents[chunkID] = counts
	b.docLengths[chunkID] = len(tokens)
	b.totalDocLen += len(tokens)
	for term, count := range counts {
		posting := b.postings[term]
		if posting == nil {
			posting = map[string]int{}
			b.postings[term] = posting
		}
		posting[chunkID] = count
	}
}

// SetDocOrder fixes the chunk order that GetScores' output is aligned to.
func (b *BM25) SetDocOrder(chunkIDs []string) {
	b.docOrder = chunkIDs
	b.positions = make(map[string]int, len(chunkIDs))
	for i, id := range chunkIDs {
		b.positions[id] = i
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
	scores := make([]float64, len(b.docOrder))
	corpusSize := len(b.documents)
	if len(tokens) == 0 || corpusSize == 0 {
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
	acc := make([]float32, len(b.docOrder))
	for _, term := range terms {
		docs := b.postings[term]
		if len(docs) == 0 {
			continue
		}
		qtf := queryTF[term]
		df := len(docs)
		idf := math.Log(1 + (float64(corpusSize)-float64(df)+0.5)/(float64(df)+0.5))
		for chunkID, tf := range docs {
			idx, ok := b.positions[chunkID]
			if !ok {
				continue
			}
			dl := b.docLengths[chunkID]
			tfc := float64(tf) / (bm25K1*(1-bm25B+bm25B*float64(dl)/avgdl) + float64(tf))
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
