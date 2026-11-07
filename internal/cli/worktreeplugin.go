package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

var pluginCloneKinds = []struct{ dir, label string }{
	{"marketplaces", "marketplace"},
	{"cache", "cache"},
}

func refusePluginClone(ctx context.Context, prefix string, c vcs.Checkout) error {
	configDir := render.Getenv(ctx, envClaudeConfigDir)
	if configDir == "" {
		home, err := canonicalHome(ctx, prefix)
		if err != nil {
			return err
		}
		configDir = filepath.Join(home, ".claude")
	}
	main := c.MainRoot
	if resolved, err := filepath.EvalSymlinks(main); err == nil {
		main = resolved
	}
	var plugins []string
	label := ""
	for _, kind := range pluginCloneKinds {
		root, err := filepath.EvalSymlinks(filepath.Join(configDir, "plugins", kind.dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: resolve Claude Code's plugin %s: %w", prefix, kind.label, err)
		}
		plugins = append(plugins, root)
		if pathInside(root, main) {
			label = kind.label
		}
	}
	if label == "" {
		return nil
	}
	stable, remote, err := pluginStableClone(ctx, prefix, c, plugins)
	if err != nil {
		return err
	}
	instead := fmt.Sprintf("run this from %s, a clone of the same remote", stable)
	if stable == "" {
		instead = fmt.Sprintf("clone %s outside %s and run this from that clone", cmp.Or(remote, "the repository"), filepath.Join(configDir, "plugins"))
	}
	return refuse("%s: %s is a Claude Code plugin %s clone, which Claude Code re-clones on a plugin update, deleting every worktree registered in it; %s", prefix, main, label, instead)
}

func pluginStableClone(ctx context.Context, prefix string, c vcs.Checkout, plugins []string) (string, string, error) {
	dir := render.Dir(c.Root)
	name, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", prefix, err)
	}
	remote, ok, err := thinRemoteOf(ctx, dir, name)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", prefix, err)
	}
	if !ok {
		return "", "", nil
	}
	home, err := canonicalHome(ctx, prefix)
	if err != nil {
		return "", "", err
	}
	pools, err := worktreePools(ctx, prefix, home)
	if err != nil {
		return "", "", err
	}
	seen := map[string]bool{}
	for _, pool := range pools {
		repos, err := os.ReadDir(pool)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("%s: read worktree pool %s: %w", prefix, pool, err)
		}
		for _, repo := range repos {
			main := poolMainRoot(filepath.Join(pool, repo.Name()))
			if main == "" || seen[main] || slices.ContainsFunc(plugins, func(root string) bool { return pathInside(root, main) }) {
				continue
			}
			seen[main] = true
			if slices.Contains(cloneRemotes(ctx, main), remote) {
				return main, remote, nil
			}
		}
	}
	return "", remote, nil
}

func poolMainRoot(dir string) string {
	lanes, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, lane := range lanes {
		c, err := vcs.ResolveCheckout(filepath.Join(dir, lane.Name()))
		if err == nil && c.Shape == vcs.ShapeGitWorktree && c.MainRoot != "" {
			return c.MainRoot
		}
	}
	return ""
}

func cloneRemotes(ctx context.Context, root string) []string {
	out, code, _, err := render.RunCLIExitCode(ctx, render.Dir(root), "git", []string{"config", "--get-regexp", `^remote\..*\.url$`})
	if err != nil || code != 0 {
		return nil
	}
	var remotes []string
	for line := range strings.Lines(out) {
		_, url, _ := strings.Cut(strings.TrimSpace(line), " ")
		if canonical, err := thinCanonicalRemote(url); err == nil {
			remotes = append(remotes, canonical)
		}
	}
	return remotes
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
