package rank

import (
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// TestRankPrebuiltBM25MatchesRebuilt asserts a prebuilt Options.BM25 yields a
// byte-identical result set to the nil (per-call rebuild) path over a
// multi-chunk fixture where BM25 legs matter and multiple chunks match.
func TestRankPrebuiltBM25MatchesRebuilt(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("auth/session.py", 1, "def login(session):\n    return session.user"),
		mkChunk("auth/session.py", 10, "def logout(session):\n    session.clear()"),
		mkChunk("db/query.py", 1, "def run_query(sql):\n    return execute(sql)"),
		mkChunk("util/misc.py", 1, "def helper():\n    return 42"),
	}
	vectors := [][]float32{{1, 0}, {0.8, 0.2}, {0.1, 0.9}, {0, 1}}
	queryVec := []float32{1, 0}

	prebuilt := BuildBM25(chunks)
	for _, tt := range []struct {
		name   string
		rerank bool
	}{
		{"rerank", true},
		{"no-rerank", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := Rank("session login", queryVec, chunks, vectors, Options{TopK: 4, Rerank: tt.rerank})
			got := Rank("session login", queryVec, chunks, vectors, Options{TopK: 4, Rerank: tt.rerank, BM25: prebuilt})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("prebuilt BM25 result set differs:\n got = %+v\nwant = %+v", got, want)
			}
		})
	}
}
