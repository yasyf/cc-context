package semsearch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
	"github.com/yasyf/cc-context/internal/semsearch/chunk"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

const (
	fusedTestdataDir      = "testdata"
	cosineParityTolerance = 1e-5
	fusedScoreTolerance   = 1e-4
	// Golden semantic_score values are semble's live cosines, computed against
	// the query vector while still float16 (Vicinity normalizes the f16 query);
	// embeddings.json serializes f32-converted vectors, which reproduce those
	// values only to ~3e-4 (golden-vs-golden). Ranking is unaffected — RRF is
	// rank-based and fused scores hold 1e-4 — so this display-only field gets a
	// tolerance covering the serialization artifact, not the engine.
	semanticScoreTolerance = 5e-4
)

type fusedGoldenChunk struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type fusedGoldenFile struct {
	FilePath       string `json:"file_path"`
	Classification string `json:"classification"`
}

type fusedGoldenChunks struct {
	Chunks []fusedGoldenChunk `json:"chunks"`
	Files  []fusedGoldenFile  `json:"files"`
}

type fusedGoldenEmbedding struct {
	FilePath  string    `json:"file_path"`
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Vector    []float32 `json:"vector"`
}

type fusedGoldenQueryEmbedding struct {
	ID     string    `json:"id"`
	Query  string    `json:"query"`
	Vector []float32 `json:"vector"`
}

type fusedGoldenEmbeddings struct {
	Dims    int                         `json:"dims"`
	Chunks  []fusedGoldenEmbedding      `json:"chunks"`
	Queries []fusedGoldenQueryEmbedding `json:"queries"`
}

type fusedGoldenQuery struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

type fusedGoldenQueries struct {
	Queries []fusedGoldenQuery `json:"queries"`
}

type fusedGoldenResult struct {
	FilePath      string   `json:"file_path"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	Score         float64  `json:"score"`
	SemanticScore *float64 `json:"semantic_score"`
}

type fusedGoldenSearchQuery struct {
	ID      string              `json:"id"`
	Kind    string              `json:"kind"`
	Query   string              `json:"query"`
	TopK    int                 `json:"top_k"`
	Alpha   float64             `json:"alpha"`
	Results []fusedGoldenResult `json:"results"`
}

type fusedGoldenSearches struct {
	Queries []fusedGoldenSearchQuery `json:"queries"`
}

func TestFusedSearchGoldenParity(t *testing.T) {
	chunkGolden := readFusedGolden[fusedGoldenChunks](t, "goldens/chunks.json")
	chunks := chunkFusedCorpus(t, chunkGolden.Files)
	assertFusedChunkParity(t, chunks, chunkGolden.Chunks)
	t.Logf("chunk parity: %d chunks match in order", len(chunks))

	ctx := context.Background()
	engine, err := embed.New(ctx)
	if errors.Is(err, embed.ErrWeightsUnavailable) {
		t.Skip("model weights unavailable (offline, empty cache) — skipping")
	}
	if err != nil {
		t.Fatalf("embed.New: %v", err)
	}
	defer func() { _ = engine.Close(ctx) }()

	embeddingGolden := readFusedGolden[fusedGoldenEmbeddings](t, "goldens/embeddings.json")
	if engine.Dims() != embeddingGolden.Dims {
		t.Fatalf("embedding dimensions = %d, want %d", engine.Dims(), embeddingGolden.Dims)
	}
	if len(embeddingGolden.Chunks) != len(chunks) {
		t.Fatalf("golden chunk embedding count = %d, want %d", len(embeddingGolden.Chunks), len(chunks))
	}

	chunkTexts := make([]string, len(chunks))
	for i, corpusChunk := range chunks {
		chunkTexts[i] = corpusChunk.Content
		assertFusedEmbeddingLocation(t, embeddingGolden.Chunks[i], corpusChunk)
	}
	chunkVectors, err := engine.Encode(ctx, chunkTexts)
	if err != nil {
		t.Fatalf("encode chunks: %v", err)
	}
	chunkCosineFloor := assertFusedVectorParity(t, "chunk", chunkVectors, fusedChunkGoldenVectors(embeddingGolden.Chunks), embeddingGolden.Dims)

	queries := readFusedGolden[fusedGoldenQueries](t, "queries.json")
	if len(embeddingGolden.Queries) != len(queries.Queries) {
		t.Fatalf("golden query embedding count = %d, want %d", len(embeddingGolden.Queries), len(queries.Queries))
	}
	queryTexts := make([]string, len(queries.Queries))
	queryGoldenVectors := make([][]float32, len(queries.Queries))
	for i, query := range queries.Queries {
		embedding := embeddingGolden.Queries[i]
		if embedding.ID != query.ID || embedding.Query != query.Query {
			t.Fatalf("query embedding[%d] = {%s %q}, want {%s %q}", i, embedding.ID, embedding.Query, query.ID, query.Query)
		}
		queryTexts[i] = query.Query
		queryGoldenVectors[i] = embedding.Vector
	}
	queryVectors, err := engine.Encode(ctx, queryTexts)
	if err != nil {
		t.Fatalf("encode queries: %v", err)
	}
	queryCosineFloor := assertFusedVectorParity(t, "query", queryVectors, queryGoldenVectors, embeddingGolden.Dims)
	t.Logf("embedding parity: minimum cosine similarity %.12g", min(chunkCosineFloor, queryCosineFloor))

	searchGolden := readFusedGolden[fusedGoldenSearches](t, "goldens/search_results.json")
	if len(searchGolden.Queries) != len(queries.Queries) {
		t.Fatalf("golden search query count = %d, want %d", len(searchGolden.Queries), len(queries.Queries))
	}

	var maxScoreDelta, maxSemanticDelta float64
	exactOrderCount := 0
	for i, query := range queries.Queries {
		expected := searchGolden.Queries[i]
		assertFusedQueryMetadata(t, query, expected)
		got := rank.Rank(query.Query, queryVectors[i], chunks, chunkVectors, rank.Options{
			TopK:   query.TopK,
			Rerank: true,
		})
		scoreDelta, semanticDelta, exactOrder := assertFusedRankParity(t, query.ID, got, expected.Results)
		maxScoreDelta = max(maxScoreDelta, scoreDelta)
		maxSemanticDelta = max(maxSemanticDelta, semanticDelta)
		if exactOrder {
			exactOrderCount++
		}
	}
	t.Logf("fused parity: maximum score delta %.12g, maximum semantic score delta %.12g", maxScoreDelta, maxSemanticDelta)
	t.Logf("rank order: %d/%d queries exact; exact-score ties may permute", exactOrderCount, len(queries.Queries))
}

func chunkFusedCorpus(t *testing.T, files []fusedGoldenFile) []semsearch.Chunk {
	t.Helper()
	var chunks []semsearch.Chunk
	for _, file := range files {
		if file.Classification != "indexed" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(fusedTestdataDir, "corpus", filepath.FromSlash(file.FilePath)))
		if err != nil {
			t.Fatalf("read corpus file %q: %v", file.FilePath, err)
		}
		chunks = append(chunks, chunk.Chunk(file.FilePath, content)...)
	}
	return chunks
}

func assertFusedChunkParity(t *testing.T, got []semsearch.Chunk, want []fusedGoldenChunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chunk count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Path != want[i].Path || got[i].StartLine != want[i].StartLine || got[i].EndLine != want[i].EndLine {
			t.Fatalf("chunk[%d] = %s, want %s:%d-%d", i, fusedChunkID(got[i]), want[i].Path, want[i].StartLine, want[i].EndLine)
		}
	}
}

func assertFusedEmbeddingLocation(t *testing.T, embedding fusedGoldenEmbedding, corpusChunk semsearch.Chunk) {
	t.Helper()
	if embedding.FilePath != corpusChunk.Path || embedding.StartLine != corpusChunk.StartLine || embedding.EndLine != corpusChunk.EndLine {
		t.Fatalf("golden embedding location = %s:%d-%d, chunk = %s", embedding.FilePath, embedding.StartLine, embedding.EndLine, fusedChunkID(corpusChunk))
	}
}

func fusedChunkGoldenVectors(embeddings []fusedGoldenEmbedding) [][]float32 {
	vectors := make([][]float32, len(embeddings))
	for i := range embeddings {
		vectors[i] = embeddings[i].Vector
	}
	return vectors
}

func assertFusedVectorParity(t *testing.T, kind string, got, want [][]float32, dims int) float64 {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s vector count = %d, want %d", kind, len(got), len(want))
	}
	cosineFloor := math.Inf(1)
	for i := range want {
		if len(got[i]) != dims || len(want[i]) != dims {
			t.Fatalf("%s vector[%d] dimensions = %d/%d, want %d", kind, i, len(got[i]), len(want[i]), dims)
		}
		similarity := embed.Cosine(got[i], want[i])
		cosineFloor = min(cosineFloor, similarity)
		if math.IsNaN(similarity) || similarity < 1-cosineParityTolerance {
			t.Errorf("%s vector[%d] cosine similarity = %.12g, want at least %.12g", kind, i, similarity, 1-cosineParityTolerance)
		}
	}
	return cosineFloor
}

func assertFusedQueryMetadata(t *testing.T, query fusedGoldenQuery, expected fusedGoldenSearchQuery) {
	t.Helper()
	if query.ID != expected.ID || query.Kind != expected.Kind || query.Query != expected.Query || query.TopK != expected.TopK {
		t.Fatalf("query = %+v, search golden = {%s %s %q top_k=%d}", query, expected.ID, expected.Kind, expected.Query, expected.TopK)
	}
	if alpha := rank.ResolveAlpha(query.Query, nil); alpha != expected.Alpha {
		t.Fatalf("ResolveAlpha(%q, nil) = %g, want %g", query.Query, alpha, expected.Alpha)
	}
}

func assertFusedRankParity(t *testing.T, queryID string, got []semsearch.Result, want []fusedGoldenResult) (float64, float64, bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s result count = %d, want %d", queryID, len(got), len(want))
	}

	positions := make(map[string]int, len(got))
	for i, result := range got {
		id := fusedResultID(result.FilePath, result.StartLine, result.EndLine)
		if _, exists := positions[id]; exists {
			t.Fatalf("%s returned duplicate result %s", queryID, id)
		}
		positions[id] = i
	}

	var maxScoreDelta, maxSemanticDelta float64
	exactOrder := true
	for i, expected := range want {
		id := fusedResultID(expected.FilePath, expected.StartLine, expected.EndLine)
		position, ok := positions[id]
		if !ok {
			t.Errorf("%s missing result %s", queryID, id)
			exactOrder = false
			continue
		}
		if position != i {
			exactOrder = false
		}

		actual := got[position]
		scoreDelta := math.Abs(actual.Score - expected.Score)
		maxScoreDelta = max(maxScoreDelta, scoreDelta)
		if scoreDelta > fusedScoreTolerance {
			t.Errorf("%s result %s score = %.12g, want %.12g (delta %.12g)", queryID, id, actual.Score, expected.Score, scoreDelta)
		}
		if expected.SemanticScore == nil {
			if actual.SemanticScore != nil {
				t.Errorf("%s result %s semantic score = %.12g, want nil", queryID, id, *actual.SemanticScore)
			}
		} else if actual.SemanticScore == nil {
			t.Errorf("%s result %s semantic score = nil, want %.12g", queryID, id, *expected.SemanticScore)
		} else {
			semanticDelta := math.Abs(*actual.SemanticScore - *expected.SemanticScore)
			maxSemanticDelta = max(maxSemanticDelta, semanticDelta)
			if semanticDelta > semanticScoreTolerance {
				t.Errorf("%s result %s semantic score = %.12g, want %.12g (delta %.12g)", queryID, id, *actual.SemanticScore, *expected.SemanticScore, semanticDelta)
			}
		}
	}

	for i := range want {
		left := fusedResultID(want[i].FilePath, want[i].StartLine, want[i].EndLine)
		leftPosition, leftOK := positions[left]
		for j := i + 1; j < len(want); j++ {
			if want[i].Score == want[j].Score {
				continue
			}
			right := fusedResultID(want[j].FilePath, want[j].StartLine, want[j].EndLine)
			rightPosition, rightOK := positions[right]
			if leftOK && rightOK && leftPosition >= rightPosition {
				t.Errorf("%s distinct-score order reversed: %s before %s", queryID, left, right)
			}
		}
	}

	return maxScoreDelta, maxSemanticDelta, exactOrder
}

func readFusedGolden[T any](t *testing.T, name string) T {
	t.Helper()
	var value T
	path := filepath.Join(fusedTestdataDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %q: %v", path, err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode golden %q: %v", path, err)
	}
	return value
}

func fusedChunkID(chunk semsearch.Chunk) string {
	return fusedResultID(chunk.Path, chunk.StartLine, chunk.EndLine)
}

func fusedResultID(path string, startLine, endLine int) string {
	return fmt.Sprintf("%s:%d-%d", path, startLine, endLine)
}
