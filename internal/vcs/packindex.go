package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// PackIndexSnapshot captures complete pack pairs before an owned pack producer runs.
type PackIndexSnapshot struct {
	objectDir string
	indexes   []string
}

// CapturePackIndexes resolves the producer's effective Git object directory and records its packs.
func CapturePackIndexes(ctx context.Context, dir render.Dir) (PackIndexSnapshot, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "objects"})
	if err != nil {
		return PackIndexSnapshot{}, err
	}
	objectDir := strings.TrimSuffix(out, "\n")
	indexes, err := packIndexNames(ctx, objectDir)
	if err != nil {
		return PackIndexSnapshot{}, err
	}
	return PackIndexSnapshot{objectDir: objectDir, indexes: indexes}, nil
}

// WithNewPacks serializes publication only when the producer added a complete pack pair.
func (s PackIndexSnapshot) WithNewPacks(ctx context.Context, publish func(objectDir string, indexes []string) error) error {
	return cache.WithLock(ctx, s.objectDir, "cc-pack-index", func() error {
		indexes, err := packIndexNames(ctx, s.objectDir)
		if err != nil {
			return err
		}
		for _, name := range indexes {
			if _, found := slices.BinarySearch(s.indexes, name); !found {
				return publish(s.objectDir, indexes)
			}
		}
		return nil
	})
}

// Publish refreshes lookup metadata when newly produced indexes justify an amortized rewrite.
func (s PackIndexSnapshot) Publish(ctx context.Context, dir render.Dir) error {
	return s.WithNewPacks(ctx, func(objectDir string, indexes []string) error {
		due, err := packIndexPublicationDue(ctx, objectDir, indexes)
		if err != nil || !due {
			return err
		}
		_, err = render.RunCLI(ctx, dir, "git", []string{"multi-pack-index", "--object-dir=" + objectDir, "write", "--no-bitmap"})
		if err != nil {
			return fmt.Errorf("publish multi-pack index: %w", err)
		}
		return nil
	})
}

func packIndexNames(ctx context.Context, objectDir string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(objectDir, "pack"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pack directory: %w", err)
	}
	packs := make(map[string]struct{}, len(entries)/2)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".pack") {
			packs[strings.TrimSuffix(entry.Name(), ".pack")] = struct{}{}
		}
	}
	indexes := make([]string, 0, len(packs))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".idx") {
			continue
		}
		if _, found := packs[strings.TrimSuffix(name, ".idx")]; found {
			indexes = append(indexes, name)
		}
	}
	return indexes, nil
}
