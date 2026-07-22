package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

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
	cmd.AddCommand(newBenchSemsearchIndexCmd(), newBenchSemsearchQueryCmd())
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
			if err := engine.Warm(cmd.Context(), emb, backend.Args{Path: args[0], Kind: "code"}); err != nil {
				return fmt.Errorf("warm semantic-search index: %w", err)
			}
			return nil
		},
	}
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
