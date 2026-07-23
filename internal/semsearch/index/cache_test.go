package index

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

func TestPersistedCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	man, chunks, vectors := cacheFixture()

	if err := store(dir, man, chunks, vectors); err != nil {
		t.Fatalf("store: %v", err)
	}
	got := loadPersisted(dir, man.Model, man.Content, man.Chunker, man.Dims)
	if got == nil {
		t.Fatal("loadPersisted returned nil")
	}
	if len(got.manifest.Generation) != 32 {
		t.Fatalf("generation length = %d, want 32", len(got.manifest.Generation))
	}
	if _, err := hex.DecodeString(got.manifest.Generation); err != nil {
		t.Fatalf("generation is not hex: %v", err)
	}

	man.Generation = got.manifest.Generation
	want := &persisted{manifest: man, chunks: chunks, vectors: vectors}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadPersisted() = %#v, want %#v", got, want)
	}
}

func TestLoadPersistedRejectsMixedGenerations(t *testing.T) {
	dir := t.TempDir()
	man, chunks, vectors := cacheFixture()
	if err := store(dir, man, chunks, vectors); err != nil {
		t.Fatalf("store: %v", err)
	}

	storedMan, err := readManifest(dir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	generation := []byte(storedMan.Generation)
	if generation[0] == '0' {
		generation[0] = '1'
	} else {
		generation[0] = '0'
	}
	storedMan.Generation = string(generation)
	chunks[0].Content = "new content with the same chunk count"
	writeCacheJSON(t, filepath.Join(dir, chunksFile), chunkEnvelope{
		Generation: storedMan.Generation,
		Chunks:     chunks,
	})
	writeCacheJSON(t, filepath.Join(dir, manifestFile), storedMan)

	if got := loadPersisted(dir, man.Model, man.Content, man.Chunker, man.Dims); got != nil {
		t.Fatalf("loadPersisted() = %#v, want nil", got)
	}
}

func cacheFixture() (manifest, []semsearch.Chunk, [][]float32) {
	chunks := []semsearch.Chunk{
		{Path: "a.go", StartLine: 1, EndLine: 2, Content: "package a"},
		{Path: "a.go", StartLine: 3, EndLine: 4, Content: "func A() {}"},
	}
	return manifest{
		Schema:  schemaVersion,
		Model:   "model-x",
		Content: "code",
		Chunker: "chunker-x",
		Dims:    2,
		Files: []fileManifest{
			{Path: "a.go", MtimeNs: 1, Start: 0, Count: len(chunks)},
		},
	}, chunks, [][]float32{{1, 2}, {3, 4}}
}

func writeCacheJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}
