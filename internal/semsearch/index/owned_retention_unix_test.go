//go:build !windows

package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func ownedRetentionDatabase(t *testing.T, root string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "ccx", "*", "index.sqlite"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("owned database paths=%v err=%v", paths, err)
	}
	return paths[0]
}

func TestOwnedAnswersRetainsTwoGenerations(t *testing.T) {
	for _, n := range []int{32, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			corpus, source := ownedAnswerFixture(n)
			root := t.TempDir()
			emb := &countingEmbedder{}
			chunker := &ownedAnswerChunker{}
			for step := 0; step < 12; step++ {
				corpus.Generation = fmt.Sprintf("retention-%08d", step)
				expected := 0
				if step == 0 {
					expected = n
				} else if step%3 == 0 {
					document := &corpus.Documents[0]
					source.texts[document.ID] = fmt.Sprintf("# Changed\n\nbody version %d", step)
					document.TextDigest = contentHash(source.texts[document.ID])
					expected = 1
				}
				source.reads.Store(0)
				chunker.calls.Store(0)
				emb.encoded = 0
				idx, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "retention")
				if err != nil {
					t.Fatal(err)
				}
				if source.reads.Load() != int64(expected) || chunker.calls.Load() != int64(expected) || emb.encoded != expected {
					t.Fatalf("step=%d reads/chunks/embed=%d/%d/%d want=%d", step, source.reads.Load(), chunker.calls.Load(), emb.encoded, expected)
				}
				if err := idx.Close(); err != nil {
					t.Fatal(err)
				}
				path := ownedRetentionDatabase(t, root)
				db, err := ownedAnswerDB(path, "ro")
				if err != nil {
					t.Fatal(err)
				}
				var cuts, members, documents int
				if err := db.QueryRowContext(t.Context(), "SELECT (SELECT COUNT(*) FROM cut),(SELECT COUNT(*) FROM cut_member),(SELECT COUNT(*) FROM doc_content)").Scan(&cuts, &members, &documents); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				wanted := min(step+1, 2)
				if cuts != wanted || members != wanted*n {
					t.Fatalf("retained %d generations and %d members, want %d and %d at history=%d step=%d", cuts, members, wanted, wanted*n, n, step)
				}
				if documents != n+step/3 {
					t.Fatalf("D2 content rows=%d want=%d", documents, n+step/3)
				}
				matrices, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.matrix"))
				if err != nil || len(matrices) != wanted {
					t.Fatalf("matrix count=%d want=%d err=%v", len(matrices), wanted, err)
				}
				var matrixBytes int64
				for _, matrix := range matrices {
					info, err := os.Stat(matrix)
					if err != nil {
						t.Fatal(err)
					}
					matrixBytes += info.Size()
				}
				database, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				var walBytes int64
				if wal, err := os.Stat(path + "-wal"); err == nil {
					walBytes = wal.Size()
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				t.Logf("OWNED_GENERATION_HISTORY n=%d step=%d cuts=%d members=%d content_versions=%d matrices=%d matrix_bytes=%d database_bytes=%d wal_bytes=%d text_reads=%d chunker_inputs=%d embedded_texts=%d", n, step, cuts, members, documents, len(matrices), matrixBytes, database.Size(), walBytes, source.reads.Load(), chunker.calls.Load(), emb.encoded)
			}
		})
	}
}

func TestOwnedAnswersEvictionPreservesOpenMappingAndD2(t *testing.T) {
	corpus, source := ownedAnswerFixture(32)
	original := corpus
	original.Documents = slices.Clone(corpus.Documents)
	root := t.TempDir()
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	old, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "eviction")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	}()
	vector := slices.Clone(old.Vector(0))
	path := strings.Clone(old.Chunk(0).Path)
	for _, generation := range []string{"next", "newest"} {
		corpus.Generation = generation
		next, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "eviction")
		if err != nil {
			t.Fatal(err)
		}
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
	}
	database := ownedRetentionDatabase(t, root)
	oldMatrix := filepath.Join(filepath.Dir(database), contentHash(original.Generation)+".matrix")
	if _, err := os.Stat(oldMatrix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evicted matrix remains named: %v", err)
	}
	if !slices.Equal(old.Vector(0), vector) || old.Chunk(0).Path != path {
		t.Fatal("retirement changed an open immutable mapping")
	}
	source.err = errors.New("evicted cut reread canonical text")
	source.reads.Store(0)
	chunker.calls.Store(0)
	emb.encoded = 0
	restored, err := LoadOwnedAnswers(t.Context(), emb, original, root, chunker, "eviction")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	}()
	if source.reads.Load() != 0 || chunker.calls.Load() != 0 || emb.encoded != 0 || restored.Reindexed != 0 {
		t.Fatalf("evicted cut lost D2 reuse: reads/chunks/embed/reindexed=%d/%d/%d/%d", source.reads.Load(), chunker.calls.Load(), emb.encoded, restored.Reindexed)
	}
	if restored.Work.MaterializationSQLRows == 0 || !slices.Equal(restored.Vector(0), vector) || restored.Chunk(0).Path != path {
		t.Fatalf("evicted metadata rebuild changed results or was hidden: %+v", restored.Work)
	}
}

type ownedRetentionBlockedEmbedder struct {
	started chan struct{}
	resume  <-chan struct{}
}

func (e *ownedRetentionBlockedEmbedder) Dims() int { return 8 }

func (e *ownedRetentionBlockedEmbedder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	close(e.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.resume:
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = detVec(text, e.Dims())
	}
	return vectors, nil
}

func TestOwnedAnswersWarmMappingDoesNotWaitForEmbedding(t *testing.T) {
	corpus, source := ownedAnswerFixture(32)
	original := corpus
	original.Documents = slices.Clone(corpus.Documents)
	root := t.TempDir()
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	initial, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, chunker, "publication-lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	corpus.Documents = slices.Clone(corpus.Documents)
	corpus.Generation = "blocked-new-generation"
	source.texts[corpus.Documents[0].ID] += " changed"
	corpus.Documents[0].TextDigest = contentHash(source.texts[corpus.Documents[0].ID])
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	resume := make(chan struct{})
	built := make(chan struct{})
	var buildErr error
	go func() {
		defer close(built)
		idx, err := LoadOwnedAnswers(ctx, &ownedRetentionBlockedEmbedder{started: started, resume: resume}, corpus, root, chunker, "publication-lock")
		buildErr = err
		if err == nil {
			buildErr = idx.Close()
		}
	}()
	t.Cleanup(func() { cancel(); <-built })
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("new generation did not reach embedding")
	}
	warmed := make(chan struct{})
	var warmErr error
	var work OwnedAnswerWork
	go func() {
		defer close(warmed)
		idx, err := LoadOwnedAnswers(ctx, emb, original, root, chunker, "publication-lock")
		warmErr = err
		if err == nil {
			work = idx.Work
			warmErr = idx.Close()
		}
	}()
	t.Cleanup(func() { cancel(); <-warmed })
	select {
	case <-warmed:
	case <-time.After(2 * time.Second):
		t.Fatal("warm mapping waited behind the new generation's embedding")
	}
	if warmErr != nil {
		t.Fatal(warmErr)
	}
	if work.MaterializationSQLRows != 0 || work.MaterializedChunks != 0 || work.DecodedValues != 0 || work.PublishedBytes != 0 {
		t.Fatalf("warm mapping rebuilt data: %+v", work)
	}
	close(resume)
	select {
	case <-built:
	case <-time.After(2 * time.Second):
		t.Fatal("new generation did not finish publication")
	}
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}
