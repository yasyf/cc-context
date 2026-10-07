package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func addLinkedWorktree(t *testing.T, main, name string) string {
	t.Helper()
	linked := filepath.Join(filepath.Dir(main), name)
	admin := filepath.Join(main, ".git", "worktrees", name)
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
	writeIndexFiles(t, linked)
	return linked
}

func TestLoadSeedsFromClosestSiblingWorktree(t *testing.T) {
	t.Parallel()
	main := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeIndexFiles(t, main)
	stale := addLinkedWorktree(t, main, "stale")
	linked := addLinkedWorktree(t, main, "linked")
	for name, body := range map[string]string{"a.go": "package a\n\nfunc Alpha() int { return 1 }\n", "b.go": "package b\n\nfunc Beta() int { return 2 }\n"} {
		if err := os.WriteFile(filepath.Join(stale, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newBody := "package a\n\nfunc Alpha() string { return \"linked alpha\" }\n"
	if err := os.WriteFile(filepath.Join(linked, "a.go"), []byte(newBody), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())

	emb := &countingEmbedder{}
	for _, repo := range []string{main, stale} {
		if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
			t.Fatalf("Load %s: %v", repo, err)
		}
	}
	emb.encoded = 0
	idx, err := Load(ctx, emb, linked, []ContentType{ContentCode}, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("linked Load: %v", err)
	}
	if idx.Reindexed != 1 {
		t.Errorf("linked Reindexed = %d, want 1 (only the edited a.go, seeded from main rather than the newer stale)", idx.Reindexed)
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
