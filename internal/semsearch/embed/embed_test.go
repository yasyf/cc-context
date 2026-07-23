package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch/embed"
)

// cosineParityEpsilon gates agreement between the WASM path and Python
// model2vec, measured as cosine distance (1 − cosine similarity) — the metric
// the downstream ranker actually uses. Observed distance across the sample is
// ≤ 3e-8, so this 1e-5 bound clears by three orders of magnitude.
const cosineParityEpsilon = 1e-5

// componentParityEpsilon is a secondary per-component sanity bound on the unit
// vectors. It cannot be 1e-5: the model's weights are float16 and the two
// implementations mean-pool in different summation orders (Rust sequential f32,
// numpy pairwise), which drifts each component by up to ~7e-5. Cosine (above)
// is the real gate; this just catches gross divergence.
const componentParityEpsilon = 2e-4

type golden struct {
	Dims    int         `json:"dims"`
	Texts   []string    `json:"texts"`
	Vectors [][]float32 `json:"vectors"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return g
}

// mustEngine builds an Engine or skips when the weights are neither cached nor
// downloadable (offline CI). Any other failure is fatal.
func mustEngine(tb testing.TB) *embed.Engine {
	tb.Helper()
	eng, err := embed.New(context.Background(), embed.CodePin)
	if errors.Is(err, embed.ErrWeightsUnavailable) {
		tb.Skip("model weights unavailable (offline, empty cache) — skipping")
	}
	if err != nil {
		tb.Fatalf("embed.New: %v", err)
	}
	return eng
}

func TestEncodeParity(t *testing.T) {
	g := loadGolden(t)
	eng := mustEngine(t)
	defer func() { _ = eng.Close(context.Background()) }()

	if eng.Dims() != g.Dims {
		t.Fatalf("Dims() = %d, want %d", eng.Dims(), g.Dims)
	}

	got, err := eng.Encode(context.Background(), g.Texts)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(got) != len(g.Vectors) {
		t.Fatalf("got %d vectors, want %d", len(got), len(g.Vectors))
	}

	for i := range got {
		if len(got[i]) != g.Dims {
			t.Errorf("vector %d has %d dims, want %d", i, len(got[i]), g.Dims)
			continue
		}
		var maxDiff float64
		for j := range got[i] {
			if d := math.Abs(float64(got[i][j]) - float64(g.Vectors[i][j])); d > maxDiff {
				maxDiff = d
			}
		}
		// A text with no in-vocabulary tokens (empty/whitespace) embeds to the
		// zero vector on both sides, where cosine is undefined. Component
		// agreement is the signal there.
		if l2norm(g.Vectors[i]) < 1e-6 {
			if maxDiff > componentParityEpsilon {
				t.Errorf("text %d (%q): zero-vector mismatch, max component diff %g", i, truncate(g.Texts[i]), maxDiff)
			}
			continue
		}
		cosDist := 1 - embed.Cosine(got[i], g.Vectors[i])
		if cosDist > cosineParityEpsilon {
			t.Errorf("text %d (%q): cosine distance %g exceeds epsilon %g (max component diff %g)",
				i, truncate(g.Texts[i]), cosDist, cosineParityEpsilon, maxDiff)
		}
		if maxDiff > componentParityEpsilon {
			t.Errorf("text %d (%q): max component diff %g exceeds sanity bound %g",
				i, truncate(g.Texts[i]), maxDiff, componentParityEpsilon)
		}
	}
}

func l2norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestEncodeDeterministic(t *testing.T) {
	g := loadGolden(t)
	eng := mustEngine(t)
	defer func() { _ = eng.Close(context.Background()) }()

	a, err := eng.Encode(context.Background(), g.Texts)
	if err != nil {
		t.Fatalf("Encode a: %v", err)
	}
	b, err := eng.Encode(context.Background(), g.Texts)
	if err != nil {
		t.Fatalf("Encode b: %v", err)
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("non-deterministic at [%d][%d]: %v != %v", i, j, a[i][j], b[i][j])
			}
		}
	}
}

func TestEncodeEmptyBatch(t *testing.T) {
	eng := mustEngine(t)
	defer func() { _ = eng.Close(context.Background()) }()

	got, err := eng.Encode(context.Background(), nil)
	if err != nil {
		t.Fatalf("Encode(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Encode(nil) = %d vectors, want 0", len(got))
	}
}

func TestEncodeBatchInvariant(t *testing.T) {
	g := loadGolden(t)
	eng := mustEngine(t)
	defer func() { _ = eng.Close(context.Background()) }()

	batched, err := eng.Encode(context.Background(), g.Texts)
	if err != nil {
		t.Fatalf("Encode batch: %v", err)
	}
	for i, text := range g.Texts {
		one, err := eng.Encode(context.Background(), []string{text})
		if err != nil {
			t.Fatalf("Encode single %d: %v", i, err)
		}
		for j := range one[0] {
			if one[0][j] != batched[i][j] {
				t.Fatalf("batch-variance at text %d comp %d: %v != %v", i, j, one[0][j], batched[i][j])
			}
		}
	}
}

func TestCosine(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"scaled", []float32{1, 1}, []float32{2, 2}, 1},
		{"zero-vector", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embed.Cosine(tt.a, tt.b); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Cosine(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCosinePanicsOnLengthMismatch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on length mismatch")
		}
	}()
	embed.Cosine([]float32{1, 2}, []float32{1})
}

// benchCorpus returns n realistic code chunks, cycling a small snippet set.
func benchCorpus(n int) []string {
	snippets := []string{
		"func (e *Engine) Encode(ctx context.Context, texts []string) ([][]float32, error) {\n\treturn e.encodeBatch(ctx, texts)\n}",
		"def tokenize(text: str) -> list[int]:\n    return [vocab[t] for t in text.split() if t in vocab]",
		"SELECT u.id, u.name, count(o.id) FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.id;",
		"class LRUCache:\n    def __init__(self, capacity):\n        self.capacity = capacity\n        self.store = OrderedDict()",
		"impl Iterator for Counter {\n    type Item = u32;\n    fn next(&mut self) -> Option<u32> { self.count += 1; Some(self.count) }\n}",
		"const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };",
	}
	out := make([]string, n)
	for i := range out {
		out[i] = snippets[i%len(snippets)]
	}
	return out
}

func BenchmarkEncode(b *testing.B) {
	eng := mustEngine(b)
	defer func() { _ = eng.Close(context.Background()) }()

	corpus := benchCorpus(512)
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		if _, err := eng.Encode(ctx, corpus); err != nil {
			b.Fatalf("Encode: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*len(corpus))/b.Elapsed().Seconds(), "texts/sec")
}

func truncate(s string) string {
	const limit = 40
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
