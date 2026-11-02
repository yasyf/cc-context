package engine

import (
	"context"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/semsearch/index"
)

// MatchAnswers preserves chunk top-k and file deduplication before answer exclusions.
func MatchAnswers(ctx context.Context, emb index.Embedder, corpus index.OwnedAnswerCorpus, root, query string, excluded []string, k int) ([]string, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	idx, err := index.LoadOwnedAnswers(ctx, emb, corpus, root, query, index.DefaultChunker(), ModelID)
	if err != nil {
		return nil, err
	}
	hits, err := SearchLoaded(ctx, emb, idx, []index.ContentType{index.ContentDocs}, backend.Args{Query: query, K: k})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, id := range excluded {
		seen[id] = true
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		id := strings.TrimSuffix(hit.FilePath, ".md")
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}
