package index

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestLoadPrunesAncestorIgnoredPersistedFiles(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	root := filepath.Join(repo, "scope")
	generated := filepath.Join(root, ".build")
	for _, dir := range []string{filepath.Join(repo, ".git"), generated} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		filepath.Join(root, "keep.go"):       "package keep\n\nfunc Keep() int { return 1 }\n",
		filepath.Join(generated, "stale.go"): "package stale\n\nfunc Stale() int { return 2 }\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	emb := &countingEmbedder{}
	content := []ContentType{ContentCode}
	chunker := DefaultChunker()
	old, err := Load(ctx, emb, root, content, chunker, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if old.TotalFiles != 2 {
		t.Fatalf("primed files = %d, want 2", old.TotalFiles)
	}
	resolved, err := ResolveRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := variantCacheDir(ctx, resolved, variantKey("model-x", ContentKey(content), chunker.ID(), emb.Dims()))
	if err != nil {
		t.Fatal(err)
	}
	before, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Schema != 3 || len(before.Files) != 2 {
		t.Fatalf("primed schema/files = %d/%d, want 3/2", before.Schema, len(before.Files))
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/scope/.build/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emb.encoded = 0
	got, err := Load(ctx, emb, root, content, chunker, "model-x")
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalFiles != 1 || got.Reindexed != 0 || emb.encoded != 0 {
		t.Fatalf("SEMANTIC_ANCESTOR_STALE_CACHE files=%d reindexed=%d encoded=%d", got.TotalFiles, got.Reindexed, emb.encoded)
	}
	after, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Schema != before.Schema || after.Generation == before.Generation || len(after.Files) != 1 || after.Files[0].Path != "keep.go" {
		t.Fatalf("reconciled manifest = %+v", after)
	}
	keep := before.Files[1]
	if before.Files[0].Path == "keep.go" {
		keep = before.Files[0]
	}
	if keep.Count <= 0 || len(old.Chunks) == 0 || len(old.Vectors) == 0 {
		t.Fatal("primed survivor has no chunks or vectors")
	}
	if keep.Path != "keep.go" || !reflect.DeepEqual(got.Chunks, old.Chunks[keep.Start:keep.Start+keep.Count]) || !reflect.DeepEqual(got.Vectors, old.Vectors[keep.Start:keep.Start+keep.Count]) {
		t.Fatal("retained file chunks or vectors were not reused exactly")
	}
	if _, err := Load(ctx, emb, root, content, chunker, "model-x"); err != nil {
		t.Fatal(err)
	}
	warm, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Generation != after.Generation || emb.encoded != 0 {
		t.Fatal("reconciled warm load rewrote payload generation or re-embedded unchanged data")
	}
}
