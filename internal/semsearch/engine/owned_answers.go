package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/semsearch/index"
	"github.com/yasyf/cc-context/internal/semsearch/rank"
)

// MatchAnswers preserves chunk top-k and file deduplication before answer exclusions.
func MatchAnswers(ctx context.Context, emb index.Embedder, corpus index.OwnedAnswerCorpus, root, query string, excluded []string, k int) (ids []string, err error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	idx, err := index.LoadOwnedAnswers(ctx, emb, corpus, root, index.DefaultChunker(), ModelID)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, idx.Close()) }()
	vecs, err := emb.Encode(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	hits := rank.RankAnswers(query, vecs[0], idx, topK(k))
	seen := map[string]bool{}
	for _, id := range excluded {
		seen[id] = true
	}
	ids = make([]string, 0, len(hits))
	for _, hit := range hits {
		id := strings.Clone(strings.TrimSuffix(hit.FilePath, ".md"))
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}
