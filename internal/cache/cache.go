// Package cache manages cc-context's on-disk cache: directory resolution,
// atomic file installs, and cross-process advisory locking.
package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Dir resolves (and creates) the cache directory joined from sub, rooted at
// $CLAUDE_PLUGIN_DATA when set, else the user cache dir under "cc-context".
func Dir(sub ...string) (string, error) {
	root := os.Getenv("CLAUDE_PLUGIN_DATA")
	if root == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache dir: %w", err)
		}
		root = filepath.Join(base, "cc-context")
	}
	dir := filepath.Join(append([]string{root}, sub...)...)
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // rooted at CLAUDE_PLUGIN_DATA or the OS user cache dir, not user free-text
		return "", fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	return dir, nil
}

// Store atomically installs data at path with perm: it writes a sibling temp
// file, chmods it, and renames it over path.
func Store(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", path, err)
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return fmt.Errorf("chmod %q: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install %q: %w", path, err)
	}
	return nil
}

// WithLock runs fn while holding an exclusive OS advisory lock on
// dir/.<name>.lock, retrying acquisition until it succeeds or ctx is done. The
// kernel releases the lock on process death, so a killed holder leaves no
// stale lock; the lockfile stays in place (removing a locked file is racy).
func WithLock(ctx context.Context, dir, name string, fn func() error) error {
	path := filepath.Join(dir, "."+name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lockfile path is under the trusted cache dir
	if err != nil {
		return fmt.Errorf("open lock %q: %w", path, err)
	}
	if err := flockExclusive(ctx, f); err != nil {
		_ = f.Close()
		return fmt.Errorf("acquire lock %q: %w", path, err)
	}
	defer func() {
		_ = flockUnlock(f)
		_ = f.Close()
	}()
	return fn()
}
