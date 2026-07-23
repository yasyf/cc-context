package engine_test

import (
	"context"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

// TestModelIDIncludesRevision covers I3(a): the cache identity must fold in the
// pinned weights revision, not just the repo name, so a weights bump (Repo holds,
// Revision moves) invalidates the on-disk cache instead of serving stale vectors.
func TestModelIDIncludesRevision(t *testing.T) {
	if engine.ModelID == embed.CodePin.Repo {
		t.Fatalf("engine.ModelID = %q equals the bare repo; a revision bump would not invalidate the cache", engine.ModelID)
	}
	if !strings.Contains(engine.ModelID, embed.CodePin.Revision) {
		t.Errorf("engine.ModelID = %q, want it to include the pinned revision %q", engine.ModelID, embed.CodePin.Revision)
	}
	if want := embed.CodePin.Repo + "@" + embed.CodePin.Revision; engine.ModelID != want {
		t.Errorf("engine.ModelID = %q, want %q", engine.ModelID, want)
	}
}

// fakeEmbedder maps each text to a deterministic unit vector, so the engine runs
// without the resident WASM weights.
type fakeEmbedder struct{}

func (fakeEmbedder) Dims() int { return 16 }

func (fakeEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = unitVec(text, 16)
	}
	return out, nil
}

func unitVec(s string, dims int) []float32 {
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

func repoPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestSearch(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	results, err := engine.Search(context.Background(), fakeEmbedder{}, backend.Args{
		Query:           "authenticated user session login flow",
		Path:            repoPath(t),
		K:               5,
		MaxSnippetLines: 2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}
	for _, r := range results {
		if strings.HasSuffix(r.FilePath, ".json") {
			t.Errorf("data language leaked into results: %s", r.FilePath)
		}
		if lines := strings.Count(r.Content, "\n") + 1; lines > 2 {
			t.Errorf("snippet %s has %d lines, want ≤2", r.FilePath, lines)
		}
	}
}

func TestSearchContentNarrowing(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	results, err := engine.Search(context.Background(), fakeEmbedder{}, backend.Args{
		Query: "documentation login flow",
		Path:  repoPath(t),
		Kind:  "code",
		K:     10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if strings.HasSuffix(r.FilePath, ".md") {
			t.Errorf("--content code should exclude docs, got %s", r.FilePath)
		}
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	results, err := engine.Search(context.Background(), fakeEmbedder{}, backend.Args{Query: "   ", Path: repoPath(t)})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if results != nil {
		t.Errorf("empty query = %v, want nil", results)
	}
}

func TestRelated(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	results, err := engine.Related(context.Background(), fakeEmbedder{}, backend.Args{
		Query: "auth/login.go:4",
		Path:  repoPath(t),
		K:     5,
	})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Related returned no results")
	}
	for _, r := range results {
		// The seed chunk (login.go:1-…) is dropped; every result carries a cosine.
		if r.FilePath == "auth/login.go" {
			t.Errorf("related returned the source file's chunk: %+v", r)
		}
		if r.SemanticScore == nil || *r.SemanticScore != r.Score {
			t.Errorf("related result %s: SemanticScore=%v Score=%v, want equal and set", r.FilePath, r.SemanticScore, r.Score)
		}
		// find_related restricts to the source's language (Go); no markdown.
		if index.DetectLanguage(r.FilePath) != "go" {
			t.Errorf("related crossed languages: %s", r.FilePath)
		}
	}
}

func TestRelatedUnknownLocation(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	_, err := engine.Related(context.Background(), fakeEmbedder{}, backend.Args{
		Query: "auth/login.go:99999",
		Path:  repoPath(t),
	})
	if err == nil {
		t.Fatal("Related on an out-of-range line should error")
	}
}
