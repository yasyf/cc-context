package web

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// bm25 is an Okapi BM25 index over a fixed corpus, built once per page from the
// chunk texts. Term statistics are derived at construction and never persisted.
type bm25 struct {
	postings map[string][]posting // term -> per-doc (index, term frequency)
	docLen   []int                // token count per document
	avgdl    float64              // mean document length
	n        int                  // document count
}

type posting struct {
	doc int
	tf  int
}

// newBM25 tokenizes each document and builds the term->postings index, per-doc
// lengths, and average document length.
func newBM25(docs []string) *bm25 {
	b := &bm25{
		postings: make(map[string][]posting),
		docLen:   make([]int, len(docs)),
		n:        len(docs),
	}
	var total int
	for i, doc := range docs {
		tokens := tokenize(doc)
		b.docLen[i] = len(tokens)
		total += len(tokens)
		tf := make(map[string]int)
		for _, t := range tokens {
			tf[t]++
		}
		for term, freq := range tf {
			b.postings[term] = append(b.postings[term], posting{doc: i, tf: freq})
		}
	}
	if b.n > 0 {
		b.avgdl = float64(total) / float64(b.n)
	}
	return b
}

// rank scores every document against query and returns all document indices
// best-first, ties broken by document order. The result is a permutation of
// [0, n): documents with no query-term match sort last in document order.
func (b *bm25) rank(query string) []int {
	scores := b.scores(query)
	order := make([]int, b.n)
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

// scores returns the BM25 score of each document for query. Query terms are
// deduplicated, so a term repeated in the query contributes once.
func (b *bm25) scores(query string) []float64 {
	scores := make([]float64, b.n)
	seen := make(map[string]bool)
	for _, term := range tokenize(query) {
		if seen[term] {
			continue
		}
		seen[term] = true
		plist, ok := b.postings[term]
		if !ok {
			continue
		}
		df := len(plist)
		// BM25+ non-negative IDF: ln((N-df+0.5)/(df+0.5) + 1).
		idf := math.Log((float64(b.n-df)+0.5)/(float64(df)+0.5) + 1)
		for _, p := range plist {
			tf := float64(p.tf)
			dl := float64(b.docLen[p.doc])
			denom := tf + bm25K1*(1-bm25B+bm25B*dl/b.avgdl)
			scores[p.doc] += idf * (tf * (bm25K1 + 1)) / denom
		}
	}
	return scores
}

// tokenize lowercases s and splits it into Unicode letter/digit runs; no
// stemming, no stopwords. Documents and queries pass through it identically.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}
