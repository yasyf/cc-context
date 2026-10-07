package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func TestPruneDeletesGoneAndIdleCaches(t *testing.T) {
	t.Parallel()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	emb := &countingEmbedder{}
	repos := map[string]string{"gone": writeIndexRepo(t), "idle": writeIndexRepo(t), "live": writeIndexRepo(t)}
	cacheDirs := map[string]string{}
	for name, repo := range repos {
		if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
			t.Fatalf("Load %s: %v", name, err)
		}
		dir, err := CacheDir(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		cacheDirs[name] = dir
	}
	if err := os.RemoveAll(repos["gone"]); err != nil {
		t.Fatal(err)
	}
	idleVariants, err := filepath.Glob(filepath.Join(cacheDirs["idle"], "*", lastUsedFile))
	if err != nil || len(idleVariants) != 1 {
		t.Fatalf("idle last_used files = %v, %v", idleVariants, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(idleVariants[0], old, old); err != nil {
		t.Fatal(err)
	}

	if err := Prune(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for name, want := range map[string]bool{"gone": false, "idle": false, "live": true} {
		manifests, err := filepath.Glob(filepath.Join(cacheDirs[name], "*", manifestFile))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(manifests) > 0; got != want {
			t.Errorf("%s index kept = %t, want %t", name, got, want)
		}
	}
}
