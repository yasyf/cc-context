package dispatch

import (
	"context"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

// slowFakeEmbedder maps each text to a deterministic unit vector, standing in for
// the resident WASM engine so runSemantic runs without weights.
type slowFakeEmbedder struct{}

func (slowFakeEmbedder) Dims() int { return 16 }

func (slowFakeEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = dispatchUnitVec(text, 16)
	}
	return out, nil
}

func dispatchUnitVec(s string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, s)
	r := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // deterministic test vectors
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		norm += float64(v[i]) * float64(v[i])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func writeDispatchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "package auth\n\nfunc Login(user, session string) error {\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "auth.go"), []byte(body), 0o600); err != nil {
		t.Fatalf("write repo: %v", err)
	}
	return dir
}

// TestRunSemanticSlowNoteCountsColdInit covers the SEAM fix: the latency timer
// must start before the embedder is constructed, so a slow cold first request
// (weight download, WASM compile, model load — here a delayed provider) trips the
// slow-search note. Before the fix the timer started after sharedEmbedder, so the
// cold-init latency was excluded and the note was wrongly omitted.
func TestRunSemanticSlowNoteCountsColdInit(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	engine.CloseIndexCache()
	t.Cleanup(engine.CloseIndexCache)

	prevThreshold := render.SlowSearchThreshold
	render.SlowSearchThreshold = time.Second
	t.Cleanup(func() { render.SlowSearchThreshold = prevThreshold })

	const coldInit = 1500 * time.Millisecond
	restore := SetEmbedderProvider(func(_ context.Context) (index.Embedder, error) {
		time.Sleep(coldInit) // the cold-init cost the timer must include
		return slowFakeEmbedder{}, nil
	})
	t.Cleanup(restore)

	repo := writeDispatchRepo(t)
	out, err := Run(context.Background(), backend.OpSearch, backend.Args{Query: "login user session", Path: repo, K: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "slow search") {
		t.Errorf("slow-search note missing; cold-init latency must count toward the timer:\n%s", out)
	}
}
