package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/semsearch/index"
)

type fakeBenchEmbedder struct{}

func (fakeBenchEmbedder) Dims() int { return 16 }

func (fakeBenchEmbedder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		h := fnv.New64a()
		_, _ = io.WriteString(h, text)
		r := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec // deterministic test vectors
		var norm float64
		out[i] = make([]float32, 16)
		for j := range out[i] {
			out[i][j] = float32(r.NormFloat64())
			norm += float64(out[i][j]) * float64(out[i][j])
		}
		norm = math.Sqrt(norm)
		for j := range out[i] {
			out[i][j] = float32(float64(out[i][j]) / norm)
		}
	}
	return out, nil
}

func TestBenchSemsearchQueryJSON(t *testing.T) {
	previous := newBenchSemsearchEmbedder
	newBenchSemsearchEmbedder = func(context.Context) (index.Embedder, error) {
		return fakeBenchEmbedder{}, nil
	}
	t.Cleanup(func() { newBenchSemsearchEmbedder = previous })

	repo, err := filepath.Abs(filepath.Join("..", "semsearch", "engine", "testdata", "repo"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		topK int
	}{
		{name: "one result", topK: 1},
		{name: "three results", topK: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			var out bytes.Buffer
			cmd := newBenchSemsearchQueryCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{repo, "authenticated session login", "--top-k", strconv.Itoa(tt.topK)})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("query: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != tt.topK {
				t.Fatalf("emitted %d rows, want %d\n%s", len(lines), tt.topK, out.String())
			}
			for _, line := range lines {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatalf("decode %q: %v", line, err)
				}
				if len(row) != 4 {
					t.Errorf("row keys = %v, want exactly four benchmark fields", row)
				}
				for _, key := range []string{"file_path", "start_line", "end_line", "score"} {
					if _, ok := row[key]; !ok {
						t.Errorf("row missing %q: %v", key, row)
					}
				}
			}
		})
	}
}

func TestBenchSemsearchTimeJSON(t *testing.T) {
	previous := newBenchSemsearchEmbedder
	newBenchSemsearchEmbedder = func(context.Context) (index.Embedder, error) {
		return fakeBenchEmbedder{}, nil
	}
	t.Cleanup(func() { newBenchSemsearchEmbedder = previous })

	repo, err := filepath.Abs(filepath.Join("..", "semsearch", "engine", "testdata", "repo"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		queries []string
	}{
		{name: "one query", queries: []string{"authenticated session login"}},
		{name: "two queries", queries: []string{"authenticated session login", "database migration"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
			queriesPath := filepath.Join(t.TempDir(), "queries.json")
			data, err := json.Marshal(tt.queries)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(queriesPath, data, 0o600); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			cmd := newBenchSemsearchTimeCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{repo, "--queries", queriesPath})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("time: %v", err)
			}

			var row map[string]any
			if err := json.Unmarshal(out.Bytes(), &row); err != nil {
				t.Fatalf("decode %q: %v", out.String(), err)
			}
			if len(row) != 6 {
				t.Errorf("row keys = %v, want exactly six benchmark fields", row)
			}
			for _, key := range []string{"cold_index_ms", "query_p50_ms", "query_mean_ms"} {
				if value, ok := row[key].(float64); !ok || value <= 0 {
					t.Errorf("%s = %v, want a positive number", key, row[key])
				}
			}
			for _, key := range []string{"chunks", "files"} {
				if value, ok := row[key].(float64); !ok || value <= 0 {
					t.Errorf("%s = %v, want a positive integer", key, row[key])
				}
			}
			perQuery, ok := row["per_query"].([]any)
			if !ok || len(perQuery) != len(tt.queries) {
				t.Fatalf("per_query = %v, want %d rows", row["per_query"], len(tt.queries))
			}
			for i, value := range perQuery {
				query, ok := value.(map[string]any)
				if !ok {
					t.Fatalf("per_query[%d] = %v, want object", i, value)
				}
				if len(query) != 2 || query["query"] != tt.queries[i] {
					t.Errorf("per_query[%d] = %v, want query and median_ms", i, query)
				}
				if median, ok := query["median_ms"].(float64); !ok || median <= 0 {
					t.Errorf("per_query[%d].median_ms = %v, want a positive number", i, query["median_ms"])
				}
			}
		})
	}
}

func TestBenchCommandIsHidden(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"bench"})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Hidden {
		t.Error("bench command is visible")
	}
	query, _, err := NewRootCmd().Find([]string{"bench", "semsearch", "query"})
	if err != nil {
		t.Fatal(err)
	}
	if got := query.Flags().Lookup("top-k").DefValue; got != "10" {
		t.Errorf("--top-k default = %s, want 10", got)
	}
}
