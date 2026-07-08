package web

import (
	"math"
	"reflect"
	"testing"
)

// floatsClose reports whether every element of got is within eps of want.
func floatsClose(got, want []float64, eps float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > eps {
			return false
		}
	}
	return true
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace", "   \t\n", nil},
		{"lower and punct", "Hello, World!", []string{"hello", "world"}},
		{"digits form runs", "GET /v2/scrape 200x", []string{"get", "v2", "scrape", "200x"}},
		{"underscore splits, unicode letters kept", "café_MENU", []string{"café", "menu"}},
		{"apostrophe splits, no contraction handling", "don't", []string{"don", "t"}},
		{"symmetric doc/query shape", "The  cat.", []string{"the", "cat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tokenize(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// bm25Corpus is the fixture for the scoring/ranking tables. Token stats:
//
//	doc0 "apple apple apple banana"  -> apple×3, banana×1, len 4
//	doc1 "apple banana banana"       -> apple×1, banana×2, len 3
//	doc2 "banana cherry"             -> banana×1, cherry×1, len 2
//
// N=3, total tokens=9, avgdl=3.0. Document frequencies: apple df=2, banana df=3,
// cherry df=1. IDF = ln((N-df+0.5)/(df+0.5)+1):
//
//	idf(apple)  = ln(1.5/2.5 + 1) = ln(1.6)   = 0.4700036292457356
//	idf(banana) = ln(0.5/3.5 + 1) = ln(8/7)   = 0.13353139262452257
//	idf(cherry) = ln(2.5/1.5 + 1) = ln(8/3)   = 0.9808292530117262
//
// Per-term score = idf · tf·(k1+1) / (tf + k1·(1-b + b·dl/avgdl)), k1=1.2, b=0.75.
var bm25Corpus = []string{
	"apple apple apple banana",
	"apple banana banana",
	"banana cherry",
}

func TestBM25Scores(t *testing.T) {
	b := newBM25(bm25Corpus)
	if b.avgdl != 3.0 {
		t.Fatalf("avgdl = %v, want 3.0", b.avgdl)
	}
	if got := b.docLen; !reflect.DeepEqual(got, []int{4, 3, 2}) {
		t.Fatalf("docLen = %v, want [4 3 2]", got)
	}

	tests := []struct {
		name  string
		query string
		want  []float64
	}{
		{
			// apple only. doc0: denom=3+1.2(0.25+0.75·4/3)=4.5, num=3·2.2=6.6 ->
			//   idf·6.6/4.5. doc1: denom=2.2, num=2.2 -> idf·1. doc2: no match.
			"single term",
			"apple",
			[]float64{0.6893386562270789, 0.4700036292457355, 0},
		},
		{
			// banana in all three. doc0(tf1,dl4): denom=2.5 -> idf·2.2/2.5.
			// doc1(tf2,dl3): denom=3.2 -> idf·4.4/3.2. doc2(tf1,dl2): denom=1.9.
			"present in every doc",
			"banana",
			[]float64{0.11750762550957987, 0.18360566485871854, 0.15461529672313143},
		},
		{
			// per-doc sum of the apple and banana columns above.
			"multi term",
			"apple banana",
			[]float64{0.8068462817366587, 0.653609294104454, 0.15461529672313143},
		},
		{
			// cherry only in doc2 (tf1,dl2): denom=1.9 -> idf·2.2/1.9.
			"rare term",
			"cherry",
			[]float64{0, 0, 1.1356970298030518},
		},
		{"no match", "grape", []float64{0, 0, 0}},
		{"query term deduped, scored once", "apple apple apple", []float64{0.6893386562270789, 0.4700036292457355, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.scores(tt.query)
			if !floatsClose(got, tt.want, 1e-9) {
				t.Errorf("scores(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestBM25Rank(t *testing.T) {
	b := newBM25(bm25Corpus)
	tests := []struct {
		name  string
		query string
		want  []int
	}{
		{"descending by score", "apple", []int{0, 1, 2}},        // 0.689 > 0.470 > 0
		{"reorders across docs", "banana", []int{1, 2, 0}},      // 0.184 > 0.155 > 0.118
		{"multi term", "apple banana", []int{0, 1, 2}},          // 0.807 > 0.654 > 0.155
		{"top then zero-score tie", "cherry", []int{2, 0, 1}},   // doc2, then 0,1 tie -> doc order
		{"all zero -> document order", "grape", []int{0, 1, 2}}, // full tie -> doc order
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := b.rank(tt.query); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("rank(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestBM25RankTieByDocumentOrder(t *testing.T) {
	// doc0 and doc1 are identical, so "hello" scores them exactly equal
	// (0.4344571362775707 each); the tie must resolve to document order.
	b := newBM25([]string{"hello world", "hello world", "goodbye"})
	sc := b.scores("hello")
	if sc[0] != sc[1] {
		t.Fatalf("expected exact score tie, got %v", sc)
	}
	if got := b.rank("hello"); !reflect.DeepEqual(got, []int{0, 1, 2}) {
		t.Errorf("rank(hello) = %v, want [0 1 2] (tie broken by doc order)", got)
	}
}

func TestBM25Degenerate(t *testing.T) {
	if got := newBM25(nil).rank("anything"); len(got) != 0 {
		t.Errorf("rank over empty corpus = %v, want empty", got)
	}
	b := newBM25([]string{"solo document"})
	if got := b.rank("solo"); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("rank over single doc = %v, want [0]", got)
	}
	if got := b.rank("absent"); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("rank of unmatched query = %v, want [0]", got)
	}
}
