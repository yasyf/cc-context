package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

const layeredFixtureFiles = 40

func writeLayeredFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"a.go", "b.go", "c.md"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range layeredFixtureFiles {
		body := fmt.Sprintf("package pkg\n\n// Handler%02d serves shared requests for tenant %02d.\nfunc Handler%02d() string { return \"tenant%02d\" }\n", i, i, i, i)
		writeLayeredFile(t, dir, fmt.Sprintf("pkg/f%02d.go", i), body)
	}
}

func writeLayeredFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func layeredFamily(t *testing.T) (main, linked string) {
	t.Helper()
	main = filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeLayeredFiles(t, main)
	linked = addLinkedWorktree(t, main, "linked")
	writeLayeredFiles(t, linked)
	return main, linked
}

func indexManifest(ctx context.Context, t *testing.T, repo string) manifest {
	t.Helper()
	dir, err := CacheDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	variants, err := filepath.Glob(filepath.Join(dir, "*", manifestFile))
	if err != nil || len(variants) != 1 {
		t.Fatalf("manifests under %s = %v, %v", dir, variants, err)
	}
	man, err := readManifest(filepath.Dir(variants[0]))
	if err != nil {
		t.Fatal(err)
	}
	return man
}

func TestLayeredIndexSearchesLikeAFlatBuild(t *testing.T) {
	t.Parallel()
	main, linked := layeredFamily(t)
	writeLayeredFile(t, linked, "pkg/f07.go", "package pkg\n\n// Handler07 now rotates overlay credentials.\nfunc Handler07() string { return \"rotated\" }\n")
	writeLayeredFile(t, linked, "pkg/zz_added.go", "package pkg\n\n// Added wires the overlay-only quokka path.\nfunc Added() string { return \"quokka\" }\n")
	if err := os.Remove(filepath.Join(linked, "pkg", "f21.go")); err != nil {
		t.Fatal(err)
	}
	content := []ContentType{ContentCode}
	emb := &countingEmbedder{}

	shared := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir(), cacheEnv+"=")
	if _, err := Load(shared, emb, main, content, DefaultChunker(), "model-x"); err != nil {
		t.Fatalf("main Load: %v", err)
	}
	layered, err := Load(shared, emb, linked, content, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("linked Load: %v", err)
	}
	mainMan, linkedMan := indexManifest(shared, t, main), indexManifest(shared, t, linked)
	if len(mainMan.Segments) != 1 || len(linkedMan.Segments) != 2 || linkedMan.Segments[0] != mainMan.Segments[0] {
		t.Fatalf("segments: main %v, linked %v; want linked on main's segment plus one overlay", mainMan.Segments, linkedMan.Segments)
	}

	isolated := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir(), cacheEnv+"=")
	flat, err := Load(isolated, emb, linked, content, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("flat Load: %v", err)
	}
	if got := indexManifest(isolated, t, linked).Segments; len(got) != 1 {
		t.Fatalf("isolated segments = %v, want one whole segment", got)
	}

	if !reflect.DeepEqual(layered.Chunks, flat.Chunks) || !reflect.DeepEqual(layered.Vectors, flat.Vectors) {
		t.Fatal("layered corpus differs from the flat build of the same tree")
	}
	for _, query := range []string{"tenant handler", "rotated credentials", "quokka overlay", "tenant21", "Handler21", "shared requests", "absent words", ""} {
		layeredBM, err := layered.BM25For(query)
		if err != nil {
			t.Fatalf("layered BM25For(%q): %v", query, err)
		}
		flatBM, err := flat.BM25For(query)
		if err != nil {
			t.Fatalf("flat BM25For(%q): %v", query, err)
		}
		tokens := rank.Tokenize(query)
		if got, want := layeredBM.GetScores(tokens), flatBM.GetScores(tokens); !reflect.DeepEqual(got, want) {
			t.Errorf("query %q: layered scores %v, flat %v", query, got, want)
		}
		qv := detVec(query, emb.Dims())
		got := rank.Rank(query, qv, layered.Chunks, layered.Vectors, rank.Options{TopK: 5, Rerank: true, BM25: layeredBM})
		want := rank.Rank(query, qv, flat.Chunks, flat.Vectors, rank.Options{TopK: 5, Rerank: true, BM25: flatBM})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("query %q: layered results %v, flat %v", query, got, want)
		}
	}
}

func TestLoadRebasesPastTheOverlayShare(t *testing.T) {
	t.Parallel()
	main, linked := layeredFamily(t)
	for i := range layeredFixtureFiles / 4 {
		writeLayeredFile(t, linked, fmt.Sprintf("pkg/f%02d.go", i), fmt.Sprintf("package pkg\n\nfunc Rewritten%02d() int { return %d }\n", i, i))
	}
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir(), cacheEnv+"=")
	emb := &countingEmbedder{}
	for _, repo := range []string{main, linked} {
		if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
			t.Fatalf("Load %s: %v", repo, err)
		}
	}
	mainMan, linkedMan := indexManifest(ctx, t, main), indexManifest(ctx, t, linked)
	if len(linkedMan.Segments) != 1 || linkedMan.Segments[0] == mainMan.Segments[0] {
		t.Fatalf("segments: main %v, linked %v; want linked rebased onto a whole segment of its own", mainMan.Segments, linkedMan.Segments)
	}
}

func TestPruneSweepsOnlyUnreferencedSegmentsPastTheirGrace(t *testing.T) {
	t.Parallel()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir(), cacheEnv+"=")
	emb := &countingEmbedder{}
	live, gone := writeIndexRepo(t), writeIndexRepo(t)
	writeLayeredFile(t, gone, "gone.go", "package gone\n\nfunc Gone() {}\n")
	for _, repo := range []string{live, gone} {
		if _, err := Load(ctx, emb, repo, []ContentType{ContentCode}, DefaultChunker(), "model-x"); err != nil {
			t.Fatalf("Load %s: %v", repo, err)
		}
	}
	liveSeg, goneSeg := indexManifest(ctx, t, live).Segments[0], indexManifest(ctx, t, gone).Segments[0]
	root, err := semsearchDir(ctx, segmentsDir)
	if err != nil {
		t.Fatal(err)
	}
	man, chunks, vectors := cacheFixture()
	fresh := segmentKey("fresh", []segmentEntry{{Path: "a.go", Count: len(chunks)}})
	if err := publishSegment(root, fresh, []segmentEntry{{Path: "a.go", Offset: man.Files[0].Start, Count: len(chunks)}}, chunks, vectors); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(root, segmentTempPrefix+"abandoned")
	if err := os.MkdirAll(abandoned, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * segmentGrace)
	for _, dir := range []string{filepath.Join(root, liveSeg), filepath.Join(root, goneSeg), abandoned} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	if err := Prune(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for dir, want := range map[string]bool{liveSeg: true, fresh: true, goneSeg: false, filepath.Base(abandoned): false} {
		_, err := os.Stat(filepath.Join(root, dir))
		if got := err == nil; got != want {
			t.Errorf("segment %s kept = %t, want %t", dir, got, want)
		}
	}
}
