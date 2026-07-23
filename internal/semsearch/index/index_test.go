package index

import (
	"context"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/semsearch"
)

// countingEmbedder records how many texts it has embedded, so tests can assert
// that partial reindex re-embeds only changed files.
type countingEmbedder struct{ encoded int }

func (c *countingEmbedder) Dims() int { return 8 }

func (c *countingEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	c.encoded += len(texts)
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = detVec(text, 8)
	}
	return out, nil
}

func detVec(s string, dims int) []float32 {
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

// altChunker chunks like the default chunker but reports a distinct ID, to
// exercise cache invalidation on a chunker change.
type altChunker struct{}

func (altChunker) ID() string { return "alt-v1" }
func (altChunker) ChunkFile(p, l, c string) []semsearch.Chunk {
	return DefaultChunker().ChunkFile(p, l, c)
}

func writeIndexRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"a.go": "package a\n\nfunc Alpha() string { return \"alpha\" }\n",
		"b.go": "package b\n\nfunc Beta() string { return \"beta\" }\n",
		"c.md": "# Doc\n\nSome documentation text for the beta feature.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestLoadBuildAndWarmReload(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := writeIndexRepo(t)
	ctx := context.Background()

	emb := &countingEmbedder{}
	idx, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("cold Load: %v", err)
	}
	if idx.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2 (code only)", idx.TotalFiles)
	}
	if idx.Reindexed != 2 {
		t.Errorf("cold Reindexed = %d, want 2", idx.Reindexed)
	}
	if len(idx.Chunks) == 0 || len(idx.Chunks) != len(idx.Vectors) {
		t.Fatalf("chunks=%d vectors=%d", len(idx.Chunks), len(idx.Vectors))
	}
	if emb.encoded != len(idx.Chunks) {
		t.Errorf("cold embedded %d texts, want %d (one per chunk)", emb.encoded, len(idx.Chunks))
	}

	emb.encoded = 0
	warm, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("warm Load: %v", err)
	}
	if warm.Reindexed != 0 {
		t.Errorf("warm Reindexed = %d, want 0", warm.Reindexed)
	}
	if emb.encoded != 0 {
		t.Errorf("warm reload embedded %d texts, want 0", emb.encoded)
	}
	if len(warm.Chunks) != len(idx.Chunks) {
		t.Errorf("warm chunk count = %d, want %d", len(warm.Chunks), len(idx.Chunks))
	}
}

func TestLoadPartialReindex(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := writeIndexRepo(t)
	ctx := context.Background()

	emb := &countingEmbedder{}
	if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
		t.Fatalf("cold Load: %v", err)
	}

	// Rewrite a.go and bump its mtime so only it is re-embedded.
	newBody := "package a\n\nfunc Alpha() string { return \"changed alpha body\" }\n"
	aPath := filepath.Join(repo, "a.go")
	if err := os.WriteFile(aPath, []byte(newBody), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(aPath, future, future); err != nil {
		t.Fatal(err)
	}
	wantA := len(DefaultChunker().ChunkFile("a.go", "go", newBody))

	emb.encoded = 0
	idx, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("incremental Load: %v", err)
	}
	if idx.Reindexed != 1 {
		t.Errorf("Reindexed = %d, want 1 (only a.go)", idx.Reindexed)
	}
	if emb.encoded != wantA {
		t.Errorf("re-embedded %d texts, want %d (a.go's chunks only)", emb.encoded, wantA)
	}
}

func TestLoadInvalidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		model   string
		chunker Chunker
		content []ContentType
	}{
		{"model change", "model-y", DefaultChunker(), []ContentType{ContentCode}},
		{"content change", "model-x", DefaultChunker(), []ContentType{ContentCode, ContentDocs}},
		{"chunker change", "model-x", altChunker{}, []ContentType{ContentCode}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			repo := writeIndexRepo(t)
			emb := &countingEmbedder{}
			if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
				t.Fatalf("cold Load: %v", err)
			}
			emb.encoded = 0
			got, err := Load(ctx, emb, repo, tc.content, tc.chunker, tc.model)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.Reindexed != got.TotalFiles || got.TotalFiles == 0 {
				t.Errorf("expected full rebuild: Reindexed=%d TotalFiles=%d", got.Reindexed, got.TotalFiles)
			}
			if emb.encoded != len(got.Chunks) {
				t.Errorf("re-embedded %d texts, want all %d chunks", emb.encoded, len(got.Chunks))
			}
		})
	}
}

// dimsEmbedder is a counting embedder with a configurable output width, for the
// dims-mismatch cache-rejection test.
type dimsEmbedder struct {
	dims    int
	encoded int
}

func (d *dimsEmbedder) Dims() int { return d.dims }

func (d *dimsEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	d.encoded += len(texts)
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = detVec(text, d.dims)
	}
	return out, nil
}

// TestLoadRevisionBumpRebuilds covers I3(a): a weights bump changes the model
// identity (embed.Repo holds, embed.Revision moves — engine folds both into the
// modelID it passes here), so the cached vectors must be discarded and rebuilt
// rather than served stale against new-weights query embeddings.
func TestLoadRevisionBumpRebuilds(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := writeIndexRepo(t)
	ctx := context.Background()

	emb := &countingEmbedder{}
	if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "minishlab/potion-code-16M-v2@rev1"); err != nil {
		t.Fatalf("cold Load: %v", err)
	}

	emb.encoded = 0
	got, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "minishlab/potion-code-16M-v2@rev2")
	if err != nil {
		t.Fatalf("revision-bump Load: %v", err)
	}
	if got.TotalFiles == 0 || got.Reindexed != got.TotalFiles {
		t.Errorf("revision bump should force full rebuild: Reindexed=%d TotalFiles=%d", got.Reindexed, got.TotalFiles)
	}
	if emb.encoded != len(got.Chunks) {
		t.Errorf("re-embedded %d texts, want all %d chunks (no stale reuse)", emb.encoded, len(got.Chunks))
	}
}

// TestLoadDimsMismatchRebuilds covers I3(b): a persisted cache whose vector width
// no longer matches the live embedder (a dimension change under the same model
// string) must be rejected by loadPersisted and rebuilt, so mismatched vectors
// never reach rank.Cosine (which would panic).
func TestLoadDimsMismatchRebuilds(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := writeIndexRepo(t)
	ctx := context.Background()

	small := &dimsEmbedder{dims: 8}
	cold, err := Load(ctx, small, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("cold Load (dims 8): %v", err)
	}
	if len(cold.Vectors) == 0 || len(cold.Vectors[0]) != 8 {
		t.Fatalf("cold vectors dims = %d, want 8", vecDims(cold.Vectors))
	}

	// Same model string, wider embedder: the cached 8-dim vectors must not load.
	wide := &dimsEmbedder{dims: 16}
	got, err := Load(ctx, wide, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("reload (dims 16): %v", err)
	}
	if got.TotalFiles == 0 || got.Reindexed != got.TotalFiles {
		t.Errorf("dims mismatch should force full rebuild: Reindexed=%d TotalFiles=%d", got.Reindexed, got.TotalFiles)
	}
	if d := vecDims(got.Vectors); d != 16 {
		t.Errorf("rebuilt vectors dims = %d, want 16 (stale 8-dim cache rejected)", d)
	}
}

func vecDims(vectors [][]float32) int {
	if len(vectors) == 0 {
		return 0
	}
	return len(vectors[0])
}

func TestLoadNoIndexableFiles(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "data.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emb := &countingEmbedder{}
	if _, err := Load(context.Background(), emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err == nil {
		t.Fatal("Load over a repo with no indexable files should error")
	}
}
