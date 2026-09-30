package cleanupwatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type identity struct {
	Path string
	Dev  uint64
	Ino  uint64
}

func resolveTarget(path string) (identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return identity{}, fmt.Errorf("absolute %s: %w", path, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return identity{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	fi, err := os.Lstat(canonical)
	if err != nil {
		return identity{}, fmt.Errorf("stat %s: %w", canonical, err)
	}
	if !fi.IsDir() {
		return identity{}, fmt.Errorf("%s is not a directory", canonical)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, fmt.Errorf("stat %s: no device and inode", canonical)
	}
	return identity{Path: canonical, Dev: uint64(st.Dev), Ino: st.Ino}, nil //nolint:gosec // Stat_t.Dev is int32 on darwin; the same conversion on both sides keeps identities comparable
}

func canonicalOrSelf(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func canonicalSocket(socket string) string {
	return filepath.Join(canonicalOrSelf(filepath.Dir(socket)), filepath.Base(socket))
}

func within(path, dir string) bool {
	if path == dir {
		return true
	}
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return strings.HasPrefix(path, dir+"/")
}

func under(path, dir string) bool {
	if within(path, dir) {
		return true
	}
	di, err := os.Lstat(dir)
	if err != nil {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		if fi, err := os.Lstat(p); err == nil && os.SameFile(fi, di) {
			return true
		}
		if p == filepath.Dir(p) {
			return false
		}
	}
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Lstat(a)
	if err != nil {
		return false
	}
	bi, err := os.Lstat(b)
	return err == nil && os.SameFile(ai, bi)
}

func ignoredWithin(path, root string, ignore []string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	for _, dir := range ignore {
		if within(rel, filepath.Clean(dir)) {
			return true
		}
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
