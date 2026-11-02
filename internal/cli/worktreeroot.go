package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const worktreeRootKey = "ccx.worktreeRoot"

func mintGitWorktreePath(ctx context.Context, prefix string, c vcs.Checkout, name string) (string, error) {
	paths, err := gitWorktreePaths(ctx, prefix, c, name)
	if err != nil {
		return "", err
	}
	if len(paths) == 1 {
		return paths[0], nil
	}
	for _, root := range []string{c.Root, c.MainRoot} {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, paths[0])
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s: destination %s is inside checkout %s", prefix, paths[0], root)
		}
	}
	return paths[0], nil
}

func gitWorktreePaths(ctx context.Context, prefix string, c vcs.Checkout, name string) ([]string, error) {
	legacy, err := mintWorktreePath(ctx, prefix, c, name)
	if err != nil {
		return nil, err
	}
	if c.Kind == vcs.JJ {
		return []string{legacy}, nil
	}
	home, err := canonicalHome(ctx, prefix)
	if err != nil {
		return nil, err
	}
	root, err := gitWorktreeRoot(ctx, prefix, home)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return []string{legacy}, nil
	}
	repo := filepath.Base(filepath.Dir(legacy))
	if filepath.Dir(filepath.Dir(c.MainRoot)) == root {
		repo = filepath.Base(filepath.Dir(c.MainRoot))
	}
	configured, err := canonicalNewWorktreePath(filepath.Join(root, repo, filepath.Base(legacy)))
	if err != nil {
		return nil, fmt.Errorf("%s: resolve Git worktree destination: %w", prefix, err)
	}
	if configured == legacy {
		return []string{legacy}, nil
	}
	return []string{configured, legacy}, nil
}

// gitWorktreeRoot is the pool new Git worktrees mint under: ccx.worktreeRoot
// when it is set, else "" for the legacy pool under ~/.claude.
func gitWorktreeRoot(ctx context.Context, prefix, home string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(home), "git", []string{"config", "--global", "--path", "--get", worktreeRootKey})
	if err != nil {
		return "", fmt.Errorf("%s: read %s: %w", prefix, worktreeRootKey, err)
	}
	if code == 1 {
		return "", nil
	}
	if code != 0 {
		return "", fmt.Errorf("%s: read %s: exit %d: %s", prefix, worktreeRootKey, code, strings.TrimSpace(stderr))
	}
	root := strings.TrimSuffix(out, "\n")
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s: %s must name an absolute Git worktree root, got %q", prefix, worktreeRootKey, root)
	}
	root, err = canonicalWorktreeRoot(root)
	if err != nil {
		return "", fmt.Errorf("%s: resolve %s: %w", prefix, worktreeRootKey, err)
	}
	return root, nil
}

// worktreePools is every pool a worktree ccx minted can sit in: the legacy
// pool under ~/.claude and the ccx.worktreeRoot pool.
func worktreePools(ctx context.Context, prefix, home string) ([]string, error) {
	root, err := gitWorktreeRoot(ctx, prefix, home)
	if err != nil {
		return nil, err
	}
	pools := []string{worktreePool(home)}
	if root != "" && root != pools[0] {
		pools = append(pools, root)
	}
	return pools, nil
}

func canonicalNewWorktreePath(path string) (string, error) {
	parent, err := canonicalWorktreeRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func canonicalWorktreeRoot(path string) (string, error) {
	ancestor, tail := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		tail = filepath.Join(filepath.Base(ancestor), tail)
		ancestor = filepath.Dir(ancestor)
	}
}
