package rank

import (
	"reflect"
	"testing"
)

func TestBM25QueryPostingsExactScores(t *testing.T) {
	corpus := []string{"alpha beta beta", "beta alpha", "κόσμος é alpha", ""}
	legacy := NewBM25()
	ids := []string{"a", "b", "c", "d"}
	lengths := make([]int, len(corpus))
	terms := map[string][]BM25Posting{}
	for i, text := range corpus {
		tokens := Tokenize(text)
		lengths[i] = len(tokens)
		legacy.AddDocument(ids[i], tokens)
		counts := map[string]int{}
		for _, term := range tokens {
			counts[term]++
		}
		for term, count := range counts {
			terms[term] = append(terms[term], BM25Posting{Document: i, Frequency: count})
		}
	}
	legacy.SetDocOrder(ids)
	for _, query := range []string{"alpha beta alpha", "beta alpha beta", "κόσμος é", "absent", ""} {
		selected := map[string][]BM25Posting{}
		for _, term := range Tokenize(query) {
			selected[term] = terms[term]
		}
		bm, err := BM25FromPostings(lengths, selected)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := bm.GetScores(Tokenize(query)), legacy.GetScores(Tokenize(query)); !reflect.DeepEqual(got, want) {
			t.Fatalf("query=%q got=%v want=%v", query, got, want)
		}
	}
}

func TestBM25RejectsInvalidPostings(t *testing.T) {
	for _, entries := range [][]BM25Posting{{{Document: -1, Frequency: 1}}, {{Document: 1, Frequency: 1}}, {{Document: 0, Frequency: 3}}, {{Document: 0, Frequency: 0}}, {{Document: 0, Frequency: 1}, {Document: 0, Frequency: 1}}} {
		if _, err := BM25FromPostings([]int{2}, map[string][]BM25Posting{"alpha": entries}); err == nil {
			t.Fatalf("accepted %v", entries)
		}
	}
}
