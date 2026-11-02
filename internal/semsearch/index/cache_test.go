package index

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

func TestPersistedCacheRoundTrip(t *testing.T) {
	t.Parallel()
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
	if !reflect.DeepEqual(got.manifest, man) || !reflect.DeepEqual(got.chunks, chunks) || !reflect.DeepEqual(got.vectors, vectors) {
		t.Fatalf("loadPersisted() = %#v, %#v, %#v; want %#v, %#v, %#v", got.manifest, got.chunks, got.vectors, man, chunks, vectors)
	}
	wantLengths := make([]int, len(chunks))
	for i, c := range chunks {
		wantLengths[i] = len(rank.Tokenize(rank.EnrichForBM25(c)))
	}
	if got.terms == nil || !reflect.DeepEqual(got.terms.lengths, wantLengths) {
		t.Fatalf("loaded terms = %#v, want doc lengths %v", got.terms, wantLengths)
	}
}

func TestLoadPersistedRejectsMixedGenerations(t *testing.T) {
	t.Parallel()
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
	var rewritten bytes.Buffer
	if err := writeChunks(&rewritten, storedMan.Generation, chunks); err != nil {
		t.Fatalf("write chunks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, chunksFile), rewritten.Bytes(), 0o600); err != nil {
		t.Fatalf("write chunks.bin: %v", err)
	}
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

// TestCacheDirResolvesFromContext pins the seam: the cache root travels on the
// context a caller drives the index with, so the process environment never
// decides where one caller's index lands.
func TestCacheDirResolvesFromContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+root)

	dir, err := CacheDir(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Fatalf("CacheDir = %q, want it under the context's cache root %q", dir, root)
	}
}

func TestMappedTermsRankLikeTheBuiltBM25(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	chunks := []semsearch.Chunk{
		{Path: "auth/login.go", StartLine: 1, EndLine: 9, Content: "func Login(user, session string) error { return validateSession(session) }"},
		{Path: "auth/login.go", StartLine: 10, EndLine: 14, Content: "func validateSession(s string) error { return nil }"},
		{Path: "billing/invoice.go", StartLine: 1, EndLine: 5, Content: "type Invoice struct { Total int; userID string }"},
		{Path: "docs/README.md", StartLine: 1, EndLine: 3, Content: "Login sessions expire; invoices total per user."},
		{Path: "docs/empty.md", StartLine: 1, EndLine: 1, Content: ""},
	}
	vectors := make([][]float32, len(chunks))
	for i, c := range chunks {
		vectors[i] = detVec(c.Content, 4)
	}
	man := manifest{Schema: schemaVersion, Model: "m", Content: "code", Chunker: "c", Dims: 4, Files: []fileManifest{
		{Path: "auth/login.go", Start: 0, Count: 2},
		{Path: "billing/invoice.go", Start: 2, Count: 1},
		{Path: "docs/README.md", Start: 3, Count: 1},
		{Path: "docs/empty.md", Start: 4, Count: 1},
	}}
	if err := store(dir, man, chunks, vectors); err != nil {
		t.Fatalf("store: %v", err)
	}
	p := loadPersisted(dir, man.Model, man.Content, man.Chunker, man.Dims)
	if p == nil {
		t.Fatal("loadPersisted returned nil")
	}
	built := rank.BuildBM25(chunks)
	for _, query := range []string{"login session", "validateSession", "Invoice total user", "auth", "absent words", ""} {
		mapped, err := p.terms.bm25(query)
		if err != nil {
			t.Fatalf("bm25(%q): %v", query, err)
		}
		tokens := rank.Tokenize(query)
		if got, want := mapped.GetScores(tokens), built.GetScores(tokens); !reflect.DeepEqual(got, want) {
			t.Errorf("query %q: mapped scores %v, built %v", query, got, want)
		}
		qv := detVec(query, 4)
		got := rank.Rank(query, qv, p.chunks, p.vectors, rank.Options{TopK: 3, Rerank: true, BM25: mapped})
		want := rank.Rank(query, qv, chunks, vectors, rank.Options{TopK: 3, Rerank: true, BM25: built})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("query %q: mapped rank %v, built %v", query, got, want)
		}
	}
}

func TestLoadConvertsALegacyCacheWithoutReembedding(t *testing.T) {
	t.Parallel()
	repo := writeIndexRepo(t)
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	emb := &countingEmbedder{}
	content := []ContentType{ContentCode, ContentDocs}
	cold, err := Load(ctx, emb, repo, content, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("cold Load: %v", err)
	}
	want := append([]semsearch.Chunk(nil), cold.Chunks...)

	resolved, err := ResolveRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	contentK, chunkerID := ContentKey(content), DefaultChunker().ID()
	dir, err := variantCacheDir(ctx, resolved, variantKey(schemaVersion, "model-x", contentK, chunkerID, emb.Dims()))
	if err != nil {
		t.Fatal(err)
	}
	stored := loadPersisted(dir, "model-x", contentK, chunkerID, emb.Dims())
	if stored == nil {
		t.Fatal("no stored index after the cold Load")
	}
	legacyDir := filepath.Join(filepath.Dir(dir), variantKey(legacySchemaVersion, "model-x", contentK, chunkerID, emb.Dims()))
	writeLegacyCache(t, legacyDir, stored.manifest, stored.chunks, stored.vectors)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	emb.encoded = 0
	converted, err := Load(ctx, emb, repo, content, DefaultChunker(), "model-x")
	if err != nil {
		t.Fatalf("Load over a legacy cache: %v", err)
	}
	if emb.encoded != 0 {
		t.Errorf("converting the legacy cache re-embedded %d chunks, want 0", emb.encoded)
	}
	if !reflect.DeepEqual(converted.Chunks, want) {
		t.Errorf("converted chunks = %#v, want %#v", converted.Chunks, want)
	}
	if p := loadPersisted(dir, "model-x", contentK, chunkerID, emb.Dims()); p == nil || p.manifest.Schema != schemaVersion {
		t.Error("Load did not store the converted index in the current schema")
	}
}

func writeLegacyCache(t *testing.T, dir string, man manifest, chunks []semsearch.Chunk, vectors [][]float32) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	man.Schema = legacySchemaVersion
	writeCacheJSON(t, filepath.Join(dir, legacyChunksFile), chunkEnvelope{Generation: man.Generation, Chunks: chunks})
	var vecs bytes.Buffer
	if err := writeVectors(&vecs, man.Generation, vectors); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vectorsFile), vecs.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCacheJSON(t, filepath.Join(dir, manifestFile), man)
}
