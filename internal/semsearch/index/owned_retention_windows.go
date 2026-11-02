//go:build windows

package index

import (
	"context"

	"github.com/yasyf/cc-context/internal/cache"
)

func mapRetainedOwnedAnswers(ctx context.Context, path, identity, generation string, documents, dims int) (*OwnedAnswerIndex, bool, error) {
	return mapOwnedAnswers(ctx, path, identity, generation, documents, dims)
}

func publishRetainedOwnedAnswers(ctx context.Context, path, identity, generation string, documents, dims int, data []byte) (*OwnedAnswerIndex, bool, error) {
	if err := cache.Store(path, data, 0o600); err != nil {
		return nil, false, err
	}
	return mapOwnedAnswers(ctx, path, identity, generation, documents, dims)
}
