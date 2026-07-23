package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch/embed"
)

var _ Embedder = engineEmbedder{}

// cosineParityEpsilon gates agreement between the resident WASM engine and Python
// model2vec for potion-base-8M, measured as cosine distance (1 − cosine
// similarity). Observed distance across the sample is ≤ 3e-8, so this 1e-5 bound
// clears by three orders of magnitude.
const cosineParityEpsilon = 1e-5

// componentParityEpsilon is a secondary per-component sanity bound. Float16
// weights and different mean-pool summation orders drift each component by up to
// ~7e-5, so cosine is the real gate and this only catches gross divergence.
const componentParityEpsilon = 2e-4

// goldenBase8MPath is the oracle-generated parity fixture, owned by the embed
// package (potion-base-8M is its model too).
const goldenBase8MPath = "../semsearch/embed/testdata/golden_base8m.json"

type goldenVectors struct {
	Dims    int         `json:"dims"`
	Texts   []string    `json:"texts"`
	Vectors [][]float32 `json:"vectors"`
}

func loadGoldenBase8M(t *testing.T) goldenVectors {
	t.Helper()
	data, err := os.ReadFile(goldenBase8MPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var g goldenVectors
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return g
}

// TestWebEmbedParity proves the resident engine behind web search (WebPin,
// potion-base-8M) reproduces Python model2vec's vectors within epsilon, so cached
// page vectors stay comparable across the native/Python engine swap. It skips
// when the weights are neither cached nor downloadable (offline CI).
func TestWebEmbedParity(t *testing.T) {
	g := loadGoldenBase8M(t)

	eng, err := embed.New(context.Background(), WebPin)
	if errors.Is(err, embed.ErrWeightsUnavailable) {
		t.Skip("model weights unavailable (offline, empty cache) — skipping")
	}
	if err != nil {
		t.Fatalf("embed.New: %v", err)
	}
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
		// A text with no in-vocabulary tokens embeds to the zero vector on both
		// sides, where cosine is undefined; component agreement is the signal there.
		if l2norm(g.Vectors[i]) < 1e-6 {
			if maxDiff > componentParityEpsilon {
				t.Errorf("text %d: zero-vector mismatch, max component diff %g", i, maxDiff)
			}
			continue
		}
		if cosDist := 1 - embed.Cosine(got[i], g.Vectors[i]); cosDist > cosineParityEpsilon {
			t.Errorf("text %d: cosine distance %g exceeds epsilon %g (max component diff %g)",
				i, cosDist, cosineParityEpsilon, maxDiff)
		}
		if maxDiff > componentParityEpsilon {
			t.Errorf("text %d: max component diff %g exceeds sanity bound %g", i, maxDiff, componentParityEpsilon)
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

// TestEmbedIntegration exercises the resident web embedder end to end via the
// package accessor: 256 dims, L2-normalized vectors, and determinism within and
// across calls. It skips when the model weights are neither cached nor
// downloadable (offline CI).
func TestEmbedIntegration(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Cleanup(func() { _ = CloseEmbedder(context.Background()) })

	texts := []string{
		"how do I handle errors in Go",
		"install the package with homebrew",
		"how do I handle errors in Go",
	}
	e, err := sharedEmbedder(context.Background())
	if errors.Is(err, embed.ErrWeightsUnavailable) {
		t.Skip("model weights unavailable (offline, empty cache) — skipping")
	}
	if err != nil {
		t.Fatalf("sharedEmbedder: %v", err)
	}

	first, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed error: %v", err)
	}
	if len(first) != len(texts) {
		t.Fatalf("Embed returned %d vectors, want %d", len(first), len(texts))
	}
	for i, v := range first {
		if len(v) != 256 {
			t.Fatalf("vector %d has %d dims, want 256", i, len(v))
		}
		if norm := l2norm(v); math.Abs(norm-1) > 1e-3 {
			t.Errorf("vector %d L2 norm = %v, want 1±1e-3", i, norm)
		}
	}
	if !slices.Equal(first[0], first[2]) {
		t.Error("identical texts embedded to different vectors within one call")
	}

	second, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("second Embed error: %v", err)
	}
	for i := range first {
		if !slices.Equal(first[i], second[i]) {
			t.Errorf("vector %d differs across calls; embedding is not deterministic", i)
		}
	}
}
