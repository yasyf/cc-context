package index

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
)

const pruneLockWait = time.Second

// Prune deletes the index files of caches whose worktree is gone or that went
// unused for maxAge, skipping any another process holds locked.
func Prune(ctx context.Context, maxAge time.Duration) error {
	base, err := cache.Dir(ctx, "semsearch")
	if err != nil {
		return err
	}
	repos, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !repo.IsDir() || !isKey(repo.Name()) {
			continue
		}
		pruneRepo(ctx, filepath.Join(base, repo.Name()), cutoff)
	}
	return nil
}

func pruneRepo(ctx context.Context, repoDir string, cutoff time.Time) {
	variants, err := os.ReadDir(repoDir)
	if err != nil {
		return
	}
	for _, v := range variants {
		if !v.IsDir() {
			continue
		}
		dir := filepath.Join(repoDir, v.Name())
		stale := func() bool { return hasManifest(dir) && (worktreeGone(repoDir) || lastUsedBefore(dir, cutoff)) }
		if stale() {
			removeLocked(ctx, dir, stale)
		}
	}
}

func worktreeGone(repoDir string) bool {
	root, err := os.ReadFile(filepath.Join(repoDir, rootFile)) //nolint:gosec // path is under the trusted cache dir
	if err != nil {
		return true
	}
	_, err = os.Stat(string(root))
	return errors.Is(err, os.ErrNotExist)
}

func lastUsedBefore(dir string, cutoff time.Time) bool {
	fi, err := os.Stat(filepath.Join(dir, lastUsedFile))
	return err != nil || fi.ModTime().Before(cutoff)
}

// removeLocked deletes the index files but keeps dir and its lockfile, so a Load
// already queued on the lock still excludes every later one.
func removeLocked(ctx context.Context, dir string, stale func() bool) {
	lockCtx, cancel := context.WithTimeout(ctx, pruneLockWait)
	defer cancel()
	_ = cache.WithLock(lockCtx, dir, "index", func() error {
		if !stale() {
			return nil
		}
		for _, name := range []string{manifestFile, chunksFile, vectorsFile, lastUsedFile} {
			_ = os.Remove(filepath.Join(dir, name))
		}
		return nil
	})
}

func isKey(name string) bool {
	_, err := hex.DecodeString(name)
	return len(name) == 64 && err == nil
}
