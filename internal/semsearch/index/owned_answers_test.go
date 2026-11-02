package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/errgroup"
	"modernc.org/sqlite"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

type ownedAnswerTexts struct {
	texts map[string]string
	reads atomic.Int64
	err   error
}

func (s *ownedAnswerTexts) Text(ctx context.Context, id string) (string, error) {
	s.reads.Add(1)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.texts[id], s.err
}

type ownedAnswerChunker struct{ calls atomic.Int64 }

func (c *ownedAnswerChunker) ID() string { return "owned-test-v1" }

func (c *ownedAnswerChunker) ChunkFile(_ context.Context, path, _, text string) []semsearch.Chunk {
	c.calls.Add(1)
	return []semsearch.Chunk{{Path: path, StartLine: 1, EndLine: 3, Content: text}}
}

func ownedAnswerFixture(n int) (OwnedAnswerCorpus, *ownedAnswerTexts) {
	source := &ownedAnswerTexts{texts: map[string]string{}}
	corpus := OwnedAnswerCorpus{Binding: "records-epoch-A", Generation: "generation-A", Source: source}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%07x", i)
		text := fmt.Sprintf("# Answer %d\n\nalpha beta Unicode é κόσμος %d", i, i%3)
		source.texts[id] = text
		corpus.Documents = append(corpus.Documents, OwnedAnswerDocument{ID: id, Path: id + ".md", TextDigest: contentHash(text)})
	}
	slices.Reverse(corpus.Documents)
	return corpus, source
}

func TestOwnedAnswersWorkCounts(t *testing.T) {
	for _, n := range []int{32, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			corpus, source := ownedAnswerFixture(n)
			chunker := &ownedAnswerChunker{}
			emb := &countingEmbedder{}
			root := t.TempDir()
			load := func(reads, chunks, texts int) {
				t.Helper()
				source.reads.Store(0)
				chunker.calls.Store(0)
				emb.encoded = 0
				idx, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha beta", chunker, "test-model")
				if err != nil {
					t.Fatal(err)
				}
				if source.reads.Load() != int64(reads) || chunker.calls.Load() != int64(chunks) || emb.encoded != texts {
					t.Fatalf("reads/chunks/texts=%d/%d/%d, want %d/%d/%d", source.reads.Load(), chunker.calls.Load(), emb.encoded, reads, chunks, texts)
				}
				t.Logf("OWNED_ANSWER_WORK n=%d generation=%s canonical_text_reads=%d chunker_inputs=%d embedded_texts=%d numeric_chunks=%d vector_values=%d", n, corpus.Generation, source.reads.Load(), chunker.calls.Load(), emb.encoded, len(idx.Chunks), len(idx.Chunks)*emb.Dims())
				for _, c := range idx.Chunks {
					if c.Content != "" {
						t.Fatal("loaded index retained body content")
					}
				}
			}
			load(n, n, n)
			load(0, 0, 0)
			corpus.Generation = "body-edit"
			document := &corpus.Documents[0]
			source.texts[document.ID] += " changed"
			document.TextDigest = contentHash(source.texts[document.ID])
			load(1, 1, 1)
			corpus.Generation = "metadata-only-tip-edit"
			load(0, 0, 0)
			removed := corpus.Documents[0]
			corpus.Documents = corpus.Documents[1:]
			corpus.Generation = "delete"
			load(0, 0, 0)
			corpus.Documents = append(corpus.Documents, removed)
			corpus.Generation = "restore"
			load(0, 0, 0)
		})
	}
}

func TestOwnedAnswersExactRankParity(t *testing.T) {
	corpus, source := ownedAnswerFixture(32)
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	chunks := make([]semsearch.Chunk, 0, len(corpus.Documents))
	documents := slices.Clone(corpus.Documents)
	slices.SortFunc(documents, func(a, b OwnedAnswerDocument) int { return strings.Compare(a.Path, b.Path) })
	vectors := make([][]float32, 0, len(documents))
	for _, doc := range documents {
		chunks = append(chunks, chunker.ChunkFile(t.Context(), doc.Path, "markdown", source.texts[doc.ID])...)
		vectors = append(vectors, detVec(source.texts[doc.ID], emb.Dims()))
	}
	root := t.TempDir()
	for _, query := range []string{"alpha beta alpha", "0000001", "é κόσμος", "nomatch", ""} {
		for _, k := range []int{1, 5, 40} {
			idx, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, query, chunker, "test-model")
			if err != nil {
				t.Fatal(err)
			}
			for _, qvec := range [][]float32{detVec(query, emb.Dims()), make([]float32, emb.Dims())} {
				want := rank.Rank(query, qvec, chunks, vectors, rank.Options{TopK: k})
				got := rank.Rank(query, qvec, idx.Chunks, idx.Vectors, rank.Options{TopK: k, BM25: idx.BM25})
				for i := range want {
					want[i].Content = ""
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("rank differs query=%q k=%d\ngot=%v\nwant=%v", query, k, got, want)
				}
			}
		}
	}
}

func TestOwnedAnswersConcurrentCold(t *testing.T) {
	corpus, source := ownedAnswerFixture(32)
	chunker := &ownedAnswerChunker{}
	root := t.TempDir()
	var group errgroup.Group
	for range 2 {
		group.Go(func() error {
			_, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, "alpha", chunker, "test-model")
			return err
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	if source.reads.Load() != 32 || chunker.calls.Load() != 32 {
		t.Fatalf("duplicate cold work: reads=%d chunks=%d", source.reads.Load(), chunker.calls.Load())
	}
}

func TestOwnedAnswersHeldReaderAllowsGenerationAdvance(t *testing.T) {
	corpus, _ := ownedAnswerFixture(2)
	root := t.TempDir()
	chunker := &ownedAnswerChunker{}
	if _, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, "alpha", chunker, "model"); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "ccx", "*", "index.sqlite"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("cache paths=%v %v", paths, err)
	}
	db, err := ownedAnswerDB(paths[0], "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	reader, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	oldGeneration := corpus.Generation
	var documents int
	if err := reader.QueryRowContext(t.Context(), "SELECT documents FROM cut WHERE generation=?", oldGeneration).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 2 {
		t.Fatalf("old documents=%d", documents)
	}
	corpus.Generation = "metadata-advance-with-held-reader"
	if _, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, "alpha", chunker, "model"); err != nil {
		var sqliteError *sqlite.Error
		if errors.As(err, &sqliteError) && sqliteError.Code() == 5 {
			t.Fatalf("generation advance blocked by held reader: SQLITE_BUSY (5): %v", err)
		}
		t.Fatalf("generation advance returned unexpected error: %v", err)
	}
	var advanced int
	if err := reader.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM cut WHERE generation=?", corpus.Generation).Scan(&advanced); err != nil {
		t.Fatal(err)
	}
	if advanced != 0 {
		t.Fatal("held reader changed snapshots during publication")
	}
	if err := reader.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, "alpha", chunker, "model"); err != nil {
		t.Fatalf("next reader did not see advanced generation: %v", err)
	}
}

func TestOwnedAnswersPublishedMainFileNeedsNoSidecars(t *testing.T) {
	corpus, source := ownedAnswerFixture(2)
	root := t.TempDir()
	chunker := &ownedAnswerChunker{}
	if _, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, root, "alpha", chunker, "model"); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "ccx", "*", "index.sqlite"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("cache paths=%v %v", paths, err)
	}
	relative, err := filepath.Rel(root, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	copyRoot := t.TempDir()
	copyPath := filepath.Join(copyRoot, relative)
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	source.err = errors.New("copied cache attempted source fallback")
	source.reads.Store(0)
	if _, err := LoadOwnedAnswers(t.Context(), &countingEmbedder{}, corpus, copyRoot, "alpha", chunker, "model"); err != nil {
		t.Fatalf("published main file requires missing sidecar: %v", err)
	}
	if source.reads.Load() != 0 {
		t.Fatal("copied cache read canonical source")
	}
}

func TestOwnedAnswersFailureAtomicityAndCorruption(t *testing.T) {
	corpus, source := ownedAnswerFixture(2)
	root := t.TempDir()
	chunker := &ownedAnswerChunker{}
	emb := &countingEmbedder{}
	sentinel := errors.New("source failure")
	source.err = sentinel
	if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, "model"); !errors.Is(err, sentinel) {
		t.Fatalf("source failure=%v", err)
	}
	paths, err := filepath.Glob(filepath.Join(root, "ccx", "*", "index.sqlite"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("partial published cache: %v %v", paths, err)
	}
	source.err = nil
	if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, "model"); err != nil {
		t.Fatal(err)
	}
	paths, err = filepath.Glob(filepath.Join(root, "ccx", "*", "index.sqlite"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("cache paths=%v %v", paths, err)
	}
	db, err := ownedAnswerDB(paths[0], "rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE cut_member SET content_key='corrupt' WHERE ordinal=0")
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("corrupt cache=%v %v", err, closeErr)
	}
	source.reads.Store(0)
	if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, "model"); err == nil {
		t.Fatal("corrupt membership accepted")
	}
	if source.reads.Load() != 0 {
		t.Fatal("corruption triggered a source fallback")
	}
}

func TestOwnedAnswersCancellation(t *testing.T) {
	corpus, _ := ownedAnswerFixture(2)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LoadOwnedAnswers(ctx, &countingEmbedder{}, corpus, t.TempDir(), "alpha", &ownedAnswerChunker{}, "model"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestOwnedAnswersSourceAndModelIdentity(t *testing.T) {
	corpus, source := ownedAnswerFixture(2)
	root := t.TempDir()
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	for _, identity := range []struct{ binding, model string }{{"epoch-A", "model-A"}, {"epoch-B", "model-A"}, {"epoch-B", "model-B"}} {
		corpus.Binding = identity.binding
		source.reads.Store(0)
		emb.encoded = 0
		if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, identity.model); err != nil {
			t.Fatal(err)
		}
		if source.reads.Load() != 2 || emb.encoded != 2 {
			t.Fatalf("reused another identity: reads=%d texts=%d", source.reads.Load(), emb.encoded)
		}
	}
}

func TestOwnedAnswersFailedAdvanceRetainsPublishedGeneration(t *testing.T) {
	corpus, source := ownedAnswerFixture(2)
	root := t.TempDir()
	emb := &countingEmbedder{}
	chunker := &ownedAnswerChunker{}
	if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, "model"); err != nil {
		t.Fatal(err)
	}
	old := corpus
	corpus.Documents = slices.Clone(corpus.Documents)
	corpus.Generation = "failed-advance"
	corpus.Documents[0].TextDigest = contentHash("changed")
	sentinel := errors.New("changed source failure")
	source.err = sentinel
	if _, err := LoadOwnedAnswers(t.Context(), emb, corpus, root, "alpha", chunker, "model"); !errors.Is(err, sentinel) {
		t.Fatalf("advance error=%v", err)
	}
	source.reads.Store(0)
	emb.encoded = 0
	if _, err := LoadOwnedAnswers(t.Context(), emb, old, root, "alpha", chunker, "model"); err != nil {
		t.Fatal(err)
	}
	if source.reads.Load() != 0 || emb.encoded != 0 {
		t.Fatal("failed advance changed the published generation")
	}
}

func TestOwnedAnswersDefaultChunkerLegacyParity(t *testing.T) {
	corpus, source := ownedAnswerFixture(3)
	first := &corpus.Documents[0]
	source.texts[first.ID] += strings.Repeat("\n\n## Heading\n\nalpha é κόσμος text across chunk boundaries.\n", 500)
	first.TextDigest = contentHash(source.texts[first.ID])
	last := &corpus.Documents[2]
	source.texts[last.ID] = strings.Repeat("x", maxFileBytes+1)
	last.TextDigest = contentHash(source.texts[last.ID])
	repo := t.TempDir()
	for _, document := range corpus.Documents {
		if err := os.WriteFile(filepath.Join(repo, document.Path), []byte(source.texts[document.ID]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+t.TempDir())
	legacy, err := Load(ctx, &countingEmbedder{}, repo, []ContentType{ContentDocs}, DefaultChunker(), "model")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, query := range []string{"alpha alpha", "é κόσμος", "Heading", "absent"} {
		owned, err := LoadOwnedAnswers(ctx, &countingEmbedder{}, corpus, root, query, DefaultChunker(), "model")
		if err != nil {
			t.Fatal(err)
		}
		if len(owned.Chunks) != len(legacy.Chunks) || !reflect.DeepEqual(owned.Vectors, legacy.Vectors) {
			t.Fatal("default chunker/vector corpus differs from legacy")
		}
		for _, k := range []int{1, 5, 20} {
			vector := detVec(query, 8)
			want := rank.Rank(query, vector, legacy.Chunks, legacy.Vectors, rank.Options{TopK: k, BM25: legacy.BM25})
			got := rank.Rank(query, vector, owned.Chunks, owned.Vectors, rank.Options{TopK: k, BM25: owned.BM25})
			for i := range want {
				want[i].Content = ""
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy rank differs query=%q k=%d", query, k)
			}
		}
	}
}
