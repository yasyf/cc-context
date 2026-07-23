package rank

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch"
)

const (
	goldenTestdataDir            = "../testdata"
	goldenDocumentCount          = 45
	goldenQueryCount             = 7
	goldenScoreRelativeTolerance = 1e-5
)

type goldenChunk struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type goldenChunks struct {
	Chunks []goldenChunk `json:"chunks"`
}

type goldenTokenDocument struct {
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Tokens    []string `json:"tokens"`
}

type goldenTokens struct {
	Documents []goldenTokenDocument `json:"documents"`
}

type goldenScoreDocument struct {
	FilePath  string  `json:"file_path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
}

type goldenScoreQuery struct {
	ID        string                `json:"id"`
	Kind      string                `json:"kind"`
	Query     string                `json:"query"`
	Tokens    []string              `json:"tokens"`
	Documents []goldenScoreDocument `json:"documents"`
}

type goldenScores struct {
	Queries []goldenScoreQuery `json:"queries"`
}

type goldenQuery struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Query string `json:"query"`
}

type goldenQueries struct {
	Queries []goldenQuery `json:"queries"`
}

func TestGoldenBM25Tokens(t *testing.T) {
	chunks := loadGoldenChunks(t)
	want := mustReadGolden[goldenTokens](t, "goldens/bm25_tokens.json")
	if len(want.Documents) != goldenDocumentCount {
		t.Fatalf("bm25 token document count = %d, want %d", len(want.Documents), goldenDocumentCount)
	}

	for i, document := range want.Documents {
		chunk := chunks[i]
		t.Run(goldenChunkID(chunk), func(t *testing.T) {
			assertGoldenLocation(t, document.FilePath, document.StartLine, document.EndLine, chunk)
			got := Tokenize(EnrichForBM25(chunk))
			if !slices.Equal(got, document.Tokens) {
				t.Errorf("Tokenize(EnrichForBM25(chunk)) = %v, want %v", got, document.Tokens)
			}
		})
	}
}

func TestGoldenBM25Scores(t *testing.T) {
	chunks := loadGoldenChunks(t)
	queries := mustReadGolden[goldenQueries](t, "queries.json")
	want := mustReadGolden[goldenScores](t, "goldens/bm25_scores.json")
	if len(queries.Queries) != goldenQueryCount {
		t.Fatalf("query count = %d, want %d", len(queries.Queries), goldenQueryCount)
	}
	if len(want.Queries) != goldenQueryCount {
		t.Fatalf("bm25 score query count = %d, want %d", len(want.Queries), goldenQueryCount)
	}

	bm := NewBM25()
	docOrder := make([]string, len(chunks))
	for i, chunk := range chunks {
		docOrder[i] = goldenChunkID(chunk)
		bm.AddDocument(docOrder[i], Tokenize(EnrichForBM25(chunk)))
	}
	bm.SetDocOrder(docOrder)

	for i, query := range queries.Queries {
		expected := want.Queries[i]
		t.Run(query.ID, func(t *testing.T) {
			if query.ID != expected.ID || query.Kind != expected.Kind || query.Query != expected.Query {
				t.Fatalf("query = %+v, score golden query = {%s %s %q}", query, expected.ID, expected.Kind, expected.Query)
			}
			queryTokens := Tokenize(query.Query)
			if !slices.Equal(queryTokens, expected.Tokens) {
				t.Fatalf("Tokenize(%q) = %v, want %v", query.Query, queryTokens, expected.Tokens)
			}
			if len(expected.Documents) != goldenDocumentCount {
				t.Fatalf("score document count = %d, want %d", len(expected.Documents), goldenDocumentCount)
			}

			got := bm.GetScores(queryTokens)
			if len(got) != len(expected.Documents) {
				t.Fatalf("GetScores(%q) len = %d, want %d", query.Query, len(got), len(expected.Documents))
			}
			for j, document := range expected.Documents {
				assertGoldenLocation(t, document.FilePath, document.StartLine, document.EndLine, chunks[j])
				if !withinRelativeTolerance(got[j], document.Score) {
					t.Errorf("GetScores(%q)[%d] = %.12g, want %.12g within relative tolerance %g", query.Query, j, got[j], document.Score, goldenScoreRelativeTolerance)
				}
			}
			assertDistinctScoreOrder(t, got, expected.Documents)
		})
	}
}

func TestGoldenQueryClassification(t *testing.T) {
	queries := mustReadGolden[goldenQueries](t, "queries.json")
	if len(queries.Queries) != goldenQueryCount {
		t.Fatalf("query count = %d, want %d", len(queries.Queries), goldenQueryCount)
	}

	for _, query := range queries.Queries {
		t.Run(query.ID, func(t *testing.T) {
			var want bool
			switch query.Kind {
			case "symbol":
				want = true
			case "nl":
				want = false
			default:
				t.Fatalf("unknown query kind %q", query.Kind)
			}
			if got := IsSymbolQuery(query.Query); got != want {
				t.Errorf("IsSymbolQuery(%q) = %v, want %v", query.Query, got, want)
			}
		})
	}
}

func loadGoldenChunks(t *testing.T) []semsearch.Chunk {
	t.Helper()
	fixture := mustReadGolden[goldenChunks](t, "goldens/chunks.json")
	if len(fixture.Chunks) != goldenDocumentCount {
		t.Fatalf("golden chunk count = %d, want %d", len(fixture.Chunks), goldenDocumentCount)
	}

	chunks := make([]semsearch.Chunk, len(fixture.Chunks))
	for i, location := range fixture.Chunks {
		startLine := location.StartLine
		// Semble can split adjacent AST chunks within one source line. The
		// line-only golden assigns that shared line's tokens to the first chunk.
		if i > 0 && fixture.Chunks[i-1].Path == location.Path && fixture.Chunks[i-1].EndLine == startLine {
			startLine++
		}
		content, err := readGoldenChunk(location, startLine)
		if err != nil {
			t.Fatal(err)
		}
		chunks[i] = semsearch.Chunk{
			Path:      location.Path,
			StartLine: location.StartLine,
			EndLine:   location.EndLine,
			Content:   content,
		}
	}
	return chunks
}

func readGoldenChunk(location goldenChunk, startLine int) (string, error) {
	path := filepath.Join(goldenTestdataDir, "corpus", filepath.FromSlash(location.Path))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read corpus file %q: %w", location.Path, err)
	}
	lines := strings.Split(string(data), "\n")
	if startLine < 1 || location.EndLine < startLine || location.EndLine > len(lines) {
		return "", fmt.Errorf("invalid line span %s:%d-%d for %d lines", location.Path, startLine, location.EndLine, len(lines))
	}
	return strings.Join(lines[startLine-1:location.EndLine], "\n"), nil
}

func mustReadGolden[T any](t *testing.T, name string) T {
	t.Helper()
	value, err := readGolden[T](filepath.Join(goldenTestdataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func readGolden[T any](path string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, fmt.Errorf("read golden %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("decode golden %q: %w", path, err)
	}
	return value, nil
}

func goldenChunkID(chunk semsearch.Chunk) string {
	return fmt.Sprintf("%s:%d-%d", chunk.Path, chunk.StartLine, chunk.EndLine)
}

func assertGoldenLocation(t *testing.T, path string, startLine, endLine int, chunk semsearch.Chunk) {
	t.Helper()
	if path != chunk.Path || startLine != chunk.StartLine || endLine != chunk.EndLine {
		t.Fatalf("golden location = %s:%d-%d, chunk = %s", path, startLine, endLine, goldenChunkID(chunk))
	}
}

func withinRelativeTolerance(got, want float64) bool {
	if want == 0 {
		return got == 0
	}
	return math.Abs(got-want)/math.Abs(want) <= goldenScoreRelativeTolerance
}

func assertDistinctScoreOrder(t *testing.T, got []float64, want []goldenScoreDocument) {
	t.Helper()
	for i := range want {
		for j := i + 1; j < len(want); j++ {
			if want[i].Score == want[j].Score {
				continue
			}
			if want[i].Score > want[j].Score && got[i] <= got[j] {
				t.Errorf("score order for documents %d and %d = (%g, %g), want order of (%g, %g)", i, j, got[i], got[j], want[i].Score, want[j].Score)
			}
			if want[i].Score < want[j].Score && got[i] >= got[j] {
				t.Errorf("score order for documents %d and %d = (%g, %g), want order of (%g, %g)", i, j, got[i], got[j], want[i].Score, want[j].Score)
			}
		}
	}
}

// Full chunk+embed+rank parity is covered by semsearch_test.TestFusedSearchGoldenParity.
