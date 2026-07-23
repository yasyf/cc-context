package rank

import (
	"math"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func TestCosine(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0}, []float32{1, 0}, 1.0},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0.0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1.0},
		{"45 degrees", []float32{1, 1}, []float32{1, 0}, 1.0 / math.Sqrt2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cosine(tt.a, tt.b); !almostEqual(got, tt.want) {
				t.Errorf("Cosine(%v,%v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestRRFScores(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 1, ""), // 0
		mkChunk("b.py", 2, ""), // 1
		mkChunk("c.py", 3, ""), // 2
	}
	// scores 0.9/0.5/0.7 → ranks c0=1, c2=2, c1=3.
	got := rrfScores([]scored{{0, 0.9}, {1, 0.5}, {2, 0.7}}, chunks)
	want := map[int]float64{0: 1.0 / 61, 2: 1.0 / 62, 1: 1.0 / 63}
	for idx, w := range want {
		if !almostEqual(got[idx], w) {
			t.Errorf("rrf[%d] = %v, want %v", idx, got[idx], w)
		}
	}
}

func TestRRFScoresTieBrokenByStartLine(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 2, ""), // 0
		mkChunk("b.py", 1, ""), // 1
	}
	// equal scores → tie broken by (start_line, path): c1 (line 1) ranks first.
	got := rrfScores([]scored{{0, 0.5}, {1, 0.5}}, chunks)
	want := map[int]float64{1: 1.0 / 61, 0: 1.0 / 62}
	for idx, w := range want {
		if !almostEqual(got[idx], w) {
			t.Errorf("rrf[%d] = %v, want %v", idx, got[idx], w)
		}
	}
}

func TestFusionAlphaBlend(t *testing.T) {
	chunks := []semsearch.Chunk{
		mkChunk("a.py", 1, ""), // 0 — semantic only
		mkChunk("b.py", 2, ""), // 1 — both legs
		mkChunk("c.py", 3, ""), // 2 — bm25 only
	}
	// semantic leg: c0 rank1, c1 rank2. bm25 leg: c1 rank1, c2 rank2.
	normSem := rrfScores([]scored{{0, 0.9}, {1, 0.4}}, chunks)
	normBM := rrfScores([]scored{{1, 0.9}, {2, 0.4}}, chunks)

	const alpha = 0.5
	want := map[int]float64{
		0: alpha * (1.0 / 61),                  // semantic only
		1: alpha*(1.0/62) + (1-alpha)*(1.0/61), // both
		2: (1 - alpha) * (1.0 / 62),            // bm25 only
	}
	for idx, w := range want {
		got := alpha*normSem[idx] + (1-alpha)*normBM[idx]
		if !almostEqual(got, w) {
			t.Errorf("combined[%d] = %v, want %v", idx, got, w)
		}
	}
}
