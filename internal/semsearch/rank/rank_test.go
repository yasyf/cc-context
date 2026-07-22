package rank

import (
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func TestRankEndToEnd(t *testing.T) {
	// End-to-end hybrid search. Expected fused scores are the semble 0.5.2 oracle
	// output for the same leg composition (search() with the two legs mocked to
	// this membership): the semantic leg ranks c0 over c1, the BM25 leg (query
	// "session") matches only c0.
	chunks := []semsearch.Chunk{
		mkChunk("session.py", 1, "def login(session): return session"),
		mkChunk("other.py", 1, "def unrelated(): pass"),
	}
	vectors := [][]float32{{1, 0}, {0, 1}}
	queryVec := []float32{1, 0}

	got := Rank("session", queryVec, chunks, vectors, Options{TopK: 2, Rerank: true})

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}

	semantic := func(v float64) *float64 { return &v }
	want := []semsearch.Result{
		{
			FilePath:      "session.py",
			StartLine:     1,
			EndLine:       2,
			Score:         0.03934426229508197,
			SemanticScore: semantic(1.0),
			Content:       "def login(session): return session",
		},
		{
			FilePath:      "other.py",
			StartLine:     1,
			EndLine:       2,
			Score:         0.00967741935483871,
			SemanticScore: semantic(0.0),
			Content:       "def unrelated(): pass",
		},
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.FilePath != w.FilePath || g.StartLine != w.StartLine || g.EndLine != w.EndLine ||
			g.Content != w.Content || !almostEqual(g.Score, w.Score) ||
			g.SemanticScore == nil || !almostEqual(*g.SemanticScore, *w.SemanticScore) {
			t.Errorf("result[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func TestRankSymbolQueryRoutesToBM25Alpha(t *testing.T) {
	// A symbol query resolves alpha=0.3 and applies the definition boost, floating
	// the defining chunk to the top even though the semantic leg favours the other.
	chunks := []semsearch.Chunk{
		mkChunk("widget.py", 1, "class Widget:\n    def render(self): pass"),
		mkChunk("misc.py", 1, "w = Widget()  # just a reference"),
	}
	vectors := [][]float32{{0, 1}, {1, 0}} // semantic favours misc.py
	queryVec := []float32{1, 0}

	got := Rank("Widget", queryVec, chunks, vectors, Options{TopK: 2, Rerank: true})
	if len(got) == 0 {
		t.Fatal("no results")
	}
	if got[0].FilePath != "widget.py" {
		t.Errorf("top result = %q, want widget.py (definition boost)", got[0].FilePath)
	}
	if got[0].Score <= got[1].Score {
		t.Errorf("results not sorted by score descending: %v", got)
	}
}
