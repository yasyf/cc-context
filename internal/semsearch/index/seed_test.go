package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func writeLinkedWorktrees(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	main := filepath.Join(base, "main")
	linked := filepath.Join(base, "linked")
	admin := filepath.Join(main, ".git", "worktrees", "linked")
	for _, dir := range []string{admin, linked} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeIndexFiles(t, main)
	writeIndexFiles(t, linked)
	return main, linked
}

func TestLoadSeedsFromSiblingWorktree(t *testing.T) {
	t.Parallel()
	main, linked := writeLinkedWorktrees(t)
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	newBody := "package a\n\nfunc Alpha() string { return \"linked alpha\" }\n"
	if err := os.WriteFile(filepath.Join(linked, "a.go"), []byte(newBody), 0o600); err != nil {
		t.Fatal(err)
	}

	emb := &countingEmbedder{}
	if _, err := Load(ctx, emb, main, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
		t.Fatalf("main Load: %v", err)
	}
	emb.encoded = 0
	idx, err := Load(ctx, emb, linked, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("linked Load: %v", err)
	}
	if idx.Reindexed != 1 {
		t.Errorf("linked Reindexed = %d, want 1 (only the edited a.go)", idx.Reindexed)
	}
	if want := len(DefaultChunker().ChunkFile(ctx, "a.go", "go", newBody)); emb.encoded != want {
		t.Errorf("linked Load embedded %d texts, want %d (a.go's chunks only)", emb.encoded, want)
	}
}

func TestLoadReusesTouchedUnchangedFile(t *testing.T) {
	t.Parallel()
	repo := writeIndexRepo(t)
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	emb := &countingEmbedder{}
	if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
		t.Fatalf("cold Load: %v", err)
	}
	resolved, err := ResolveRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := variantCacheDir(ctx, resolved, variantKey("model-x", ContentKey([]ContentType{ContentCode}), DefaultChunker().ID(), emb.Dims()))
	if err != nil {
		t.Fatal(err)
	}
	cold, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(repo, "a.go"), future, future); err != nil {
		t.Fatal(err)
	}
	emb.encoded = 0
	idx, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("touched Load: %v", err)
	}
	if idx.Reindexed != 0 || emb.encoded != 0 {
		t.Errorf("touching a.go reindexed %d files and embedded %d texts, want 0 and 0", idx.Reindexed, emb.encoded)
	}
	retimed, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if retimed.Generation == cold.Generation {
		t.Errorf("the touched Load kept generation %q, so the new mtime was never recorded", cold.Generation)
	}
}
