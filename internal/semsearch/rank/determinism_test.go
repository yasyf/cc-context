package rank

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// TestRankZeroQueryVectorScoresZero covers R1: an out-of-vocabulary query embeds
// to an all-zero vector. rank.Cosine must return 0 (not NaN) for every chunk, so
// the SemanticScore is a real *float64 pointing at 0, the canonical tie-break
// still orders deterministically, and the result set json-marshals (a NaN would
// make json.Marshal fail with "unsupported value: NaN").
func TestRankZeroQueryVectorScoresZero(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("auth/session.py", 1, "def login(session):\n    return session.user"),
		mkChunk("db/query.py", 3, "def run_query(sql):\n    return execute(sql)"),
		mkChunk("util/misc.py", 7, "def helper():\n    return 42"),
	}
	vectors := [][]float32{{1, 0}, {0.4, 0.9}, {0, 1}}
	zeroQuery := []float32{0, 0} // out-of-vocabulary → zero embedding

	var first []semsearch.Result
	for run := 0; run < 100; run++ {
		got := Rank("session login query", zeroQuery, chunks, vectors, Options{TopK: 3, Rerank: true})
		if len(got) == 0 {
			t.Fatal("zero-vector query returned no results")
		}
		for _, r := range got {
			if r.SemanticScore == nil {
				t.Fatalf("result %s: SemanticScore is nil, want a pointer to 0", r.FilePath)
			}
			if *r.SemanticScore != 0 {
				t.Errorf("result %s: SemanticScore = %v, want 0", r.FilePath, *r.SemanticScore)
			}
			if math.IsNaN(*r.SemanticScore) || math.IsNaN(r.Score) {
				t.Fatalf("result %s: NaN leaked (score=%v semantic=%v)", r.FilePath, r.Score, *r.SemanticScore)
			}
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("json.Marshal(results): %v", err)
		}
		if first == nil {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("zero-vector query order not deterministic:\n run  = %+v\nfirst = %+v", got, first)
		}
	}
}

// TestBM25GetScoresDeterministic covers R2: the query-term loop must iterate in
// first-occurrence order and accumulate in float32, so a document matching
// several terms (whose per-term contributions sum non-associatively) scores
// bit-identically on every call. The pre-fix map-order float64 accumulation
// varied run to run.
func TestBM25GetScoresDeterministic(t *testing.T) {
	bm := buildDeterminismBM25()
	// A query whose terms span a wide range of contribution magnitudes: "index"
	// is rare (high idf), "the" is common (low idf), and the target doc repeats
	// several of them at differing frequencies.
	query := Tokenize("the index engine query the token store the")

	want := bm.GetScores(query)
	for run := 0; run < 500; run++ {
		got := bm.GetScores(query)
		if !slices.Equal(got, want) {
			t.Fatalf("GetScores not bit-stable across calls on run %d:\n got = %v\nwant = %v", run, got, want)
		}
	}
}

// TestBM25TrueTiesExactlyEqual covers R2: two documents with identical length and
// identical term frequencies for the query terms accumulate exactly-equal scores,
// so the canonical tie-break — not float noise — decides their order.
func TestBM25TrueTiesExactlyEqual(t *testing.T) {
	bm := NewBM25()
	// Docs 0 and 1 are true ties: same tokens (hence same length and same tf for
	// every query term), distinct ids. Doc 2 is filler for a realistic corpus.
	bm.AddDocument("0", Tokenize("parse token stream reader"))
	bm.AddDocument("1", Tokenize("parse token stream reader"))
	bm.AddDocument("2", Tokenize("unrelated helper body"))
	bm.SetDocOrder([]string{"0", "1", "2"})

	query := Tokenize("parse token stream")
	got := bm.GetScores(query)
	if got[0] != got[1] {
		t.Errorf("true-tie docs scored %v and %v, want exactly equal", got[0], got[1])
	}
	if got[0] == 0 {
		t.Fatal("true-tie docs scored 0; fixture does not exercise the tie")
	}
}

// TestRankBM25PrebuiltEqualsRebuiltDeterministic covers R2's prebuilt-vs-rebuilt
// equivalence over a tie-bearing fixture across many runs: the prebuilt index and
// a per-call rebuild yield byte-identical result sets, and each is stable.
func TestRankBM25PrebuiltEqualsRebuiltDeterministic(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("pkg/parse.go", 1, "func Parse(token, stream, reader) {}"),
		mkChunk("pkg/parse2.go", 1, "func Parse(token, stream, reader) {}"),
		mkChunk("pkg/other.go", 1, "func Unrelated(helper, body) {}"),
	}
	vectors := [][]float32{{1, 0}, {1, 0}, {0, 1}}
	queryVec := []float32{0.9, 0.1}
	prebuilt := BuildBM25(chunks)

	want := Rank("parse token stream", queryVec, chunks, vectors, Options{TopK: 3, Rerank: false, BM25: prebuilt})
	for run := 0; run < 100; run++ {
		rebuilt := Rank("parse token stream", queryVec, chunks, vectors, Options{TopK: 3, Rerank: false})
		pre := Rank("parse token stream", queryVec, chunks, vectors, Options{TopK: 3, Rerank: false, BM25: prebuilt})
		if !reflect.DeepEqual(rebuilt, want) {
			t.Fatalf("rebuilt result set drifted on run %d:\n got = %+v\nwant = %+v", run, rebuilt, want)
		}
		if !reflect.DeepEqual(pre, want) {
			t.Fatalf("prebuilt result set drifted on run %d:\n got = %+v\nwant = %+v", run, pre, want)
		}
	}
}

// TestRankTotalOrderSharedPathStartLine covers R3: two distinct chunks sharing
// both path and start_line, engineered to an exact fused-score tie, must order
// deterministically. The semantic leg ranks chunk 1 over chunk 0 by a single
// position and the BM25 leg ranks chunk 0 over chunk 1 by a single position, so
// at alpha 0.5 their fused scores are exactly equal; only the corpus-index final
// tie-break makes TopK reproducible (the union set is built from a Go map).
func TestRankTotalOrderSharedPathStartLine(t *testing.T) {
	chunks := []semsearch.Chunk{
		{Path: "min.js", StartLine: 1, EndLine: 2, Content: "widget widget"}, // 0: higher BM25 (tf=2), lower cosine
		{Path: "min.js", StartLine: 1, EndLine: 3, Content: "widget"},        // 1: lower BM25 (tf=1), higher cosine
	}
	vectors := [][]float32{{0.6, 0.8}, {1, 0}}
	queryVec := []float32{1, 0}

	var first []int
	for run := 0; run < 100; run++ {
		got := Rank("widget", queryVec, chunks, vectors, Options{TopK: 2, Rerank: false})
		if len(got) != 2 {
			t.Fatalf("got %d results, want 2", len(got))
		}
		// The two share path+start_line; EndLine distinguishes them (0→2, 1→3).
		order := []int{got[0].EndLine, got[1].EndLine}
		if first == nil {
			first = order
			continue
		}
		if !slices.Equal(order, first) {
			t.Fatalf("TopK order not deterministic on run %d: got EndLine order %v, want %v", run, order, first)
		}
	}
	// Corpus index breaks the tie: chunk 0 (EndLine 2) precedes chunk 1 (EndLine 3).
	if !slices.Equal(first, []int{2, 3}) {
		t.Errorf("tie order = %v, want [2 3] (corpus-index ascending)", first)
	}
}

// buildDeterminismBM25 builds a corpus whose target document matches many query
// terms at differing frequencies, so its accumulated score is sensitive to term
// ordering — the substrate the R2 determinism test relies on.
func buildDeterminismBM25() *BM25 {
	bm := NewBM25()
	docs := []string{
		"the index engine query the token store the index engine query token",
		"the store token cache",
		"engine query planner",
		"index token index",
		"reader writer buffer the",
		"query the query the query",
		"token store token store",
		"unrelated content body here",
	}
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = fmt.Sprintf("%d", i)
		bm.AddDocument(ids[i], Tokenize(d))
	}
	bm.SetDocOrder(ids)
	return bm
}
