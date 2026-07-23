package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch/embed"
	"github.com/yasyf/cc-context/internal/semsearch/engine"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

const benchSemsearchTopK = 10

var newBenchSemsearchEmbedder = func(ctx context.Context) (index.Embedder, error) {
	return embed.New(ctx)
}

type benchSemsearchResult struct {
	FilePath  string  `json:"file_path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Score     float64 `json:"score"`
}

type benchSemsearchTiming struct {
	ColdIndexMS float64                   `json:"cold_index_ms"`
	QueryP50MS  float64                   `json:"query_p50_ms"`
	QueryMeanMS float64                   `json:"query_mean_ms"`
	PerQuery    []benchSemsearchQueryTime `json:"per_query"`
	Chunks      int                       `json:"chunks"`
	Files       int                       `json:"files"`
}

type benchSemsearchQueryTime struct {
	Query    string  `json:"query"`
	MedianMS float64 `json:"median_ms"`
}

func newBenchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "bench",
		Short:  "Internal benchmark adapters",
		Args:   cobra.NoArgs,
		RunE:   groupHelp,
		Hidden: true,
	}
	cmd.AddCommand(newBenchSemsearchCmd())
	return cmd
}

func newBenchSemsearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "semsearch",
		Short: "Benchmark the native semantic-search engine",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(newBenchSemsearchIndexCmd(), newBenchSemsearchQueryCmd(), newBenchSemsearchTimeCmd())
	return cmd
}

func newBenchSemsearchIndexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "index <repo-root>",
		Short: "Build or warm the persistent semantic-search index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			emb, err := newBenchSemsearchEmbedder(cmd.Context())
			if err != nil {
				return fmt.Errorf("create benchmark embedder: %w", err)
			}
			if _, err := engine.Warm(cmd.Context(), emb, backend.Args{Path: args[0], Kind: "code"}); err != nil {
				return fmt.Errorf("warm semantic-search index: %w", err)
			}
			return nil
		},
	}
}

func newBenchSemsearchTimeCmd() *cobra.Command {
	var queriesPath string
	cmd := &cobra.Command{
		Use:   "time <repo-root>",
		Short: "Time cold indexing and warm in-process semantic search",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			queries, err := readBenchSemsearchQueries(queriesPath)
			if err != nil {
				return err
			}
			emb, err := newBenchSemsearchEmbedder(cmd.Context())
			if err != nil {
				return fmt.Errorf("create benchmark embedder: %w", err)
			}
			return writeBenchSemsearchTiming(cmd.Context(), cmd.OutOrStdout(), emb, args[0], queries)
		},
	}
	cmd.Flags().StringVar(&queriesPath, "queries", "", "JSON file containing an array of query strings")
	_ = cmd.MarkFlagRequired("queries")
	return cmd
}

func readBenchSemsearchQueries(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // --queries is explicitly supplied benchmark input
	if err != nil {
		return nil, fmt.Errorf("read benchmark queries: %w", err)
	}
	var queries []string
	if err := json.Unmarshal(data, &queries); err != nil {
		return nil, fmt.Errorf("decode benchmark queries: %w", err)
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("decode benchmark queries: query array is empty")
	}
	return queries, nil
}

func writeBenchSemsearchTiming(
	ctx context.Context, out io.Writer, emb index.Embedder, repo string, queries []string,
) error {
	dir, err := index.CacheDir(repo)
	if err != nil {
		return fmt.Errorf("resolve semantic-search cache: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear semantic-search cache: %w", err)
	}

	a := backend.Args{Path: repo, Kind: "code"}
	started := time.Now()
	if _, err := engine.Warm(ctx, emb, a); err != nil {
		return fmt.Errorf("build cold semantic-search index: %w", err)
	}
	coldIndexMS := elapsedMS(started)

	idx, err := engine.Warm(ctx, emb, a)
	if err != nil {
		return fmt.Errorf("reload warm semantic-search index: %w", err)
	}

	perQuery := make([]benchSemsearchQueryTime, 0, len(queries))
	medians := make([]float64, 0, len(queries))
	for _, query := range queries {
		queryArgs := a
		queryArgs.Query = query
		if _, err := engine.Search(ctx, emb, queryArgs); err != nil {
			return fmt.Errorf("warm up benchmark query %q: %w", query, err)
		}

		runs := make([]float64, 5)
		for i := range runs {
			started = time.Now()
			if _, err := engine.Search(ctx, emb, queryArgs); err != nil {
				return fmt.Errorf("benchmark query %q: %w", query, err)
			}
			runs[i] = elapsedMS(started)
		}
		median := medianFloat64(runs)
		medians = append(medians, median)
		perQuery = append(perQuery, benchSemsearchQueryTime{Query: query, MedianMS: median})
	}

	var sum float64
	for _, median := range medians {
		sum += median
	}
	return json.NewEncoder(out).Encode(benchSemsearchTiming{
		ColdIndexMS: coldIndexMS,
		QueryP50MS:  medianFloat64(medians),
		QueryMeanMS: sum / float64(len(medians)),
		PerQuery:    perQuery,
		Chunks:      len(idx.Chunks),
		Files:       idx.TotalFiles,
	})
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

func medianFloat64(values []float64) float64 {
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func newBenchSemsearchQueryCmd() *cobra.Command {
	var topK int
	cmd := &cobra.Command{
		Use:   "query <repo-root> <query>",
		Short: "Run semantic search and emit ranked JSON lines",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			emb, err := newBenchSemsearchEmbedder(cmd.Context())
			if err != nil {
				return fmt.Errorf("create benchmark embedder: %w", err)
			}
			return writeBenchSemsearchResults(cmd.Context(), cmd.OutOrStdout(), emb, backend.Args{
				Path:  args[0],
				Query: args[1],
				Kind:  "code",
				K:     topK,
			})
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", benchSemsearchTopK, "number of ranked chunks to emit")
	return cmd
}

func writeBenchSemsearchResults(
	ctx context.Context, out io.Writer, emb index.Embedder, a backend.Args,
) error {
	results, err := engine.Search(ctx, emb, a)
	if err != nil {
		return fmt.Errorf("search benchmark repo: %w", err)
	}
	enc := json.NewEncoder(out)
	for _, result := range results {
		if err := enc.Encode(benchSemsearchResult{
			FilePath:  result.FilePath,
			StartLine: result.StartLine,
			EndLine:   result.EndLine,
			Score:     result.Score,
		}); err != nil {
			return fmt.Errorf("encode benchmark result: %w", err)
		}
	}
	return nil
}
