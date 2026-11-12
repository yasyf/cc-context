package vcs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// RemoteCheckout returns the shallow clone ccx keeps of an https git URL under
// its cache at clones/<host>/<path>, cloning the remote's HEAD on first use and
// moving to it again on every later call. ok is false when raw is not an https
// URL, so the caller reads it as a path.
func RemoteCheckout(ctx context.Context, raw string) (dir string, ok bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return "", false, nil
	}
	segments := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if u.Host == "" || slices.ContainsFunc(segments, func(s string) bool { return s == "" || s == "." || s == ".." }) {
		return "", true, fmt.Errorf("clone %s: not a repository URL", raw)
	}
	parent, err := cache.Dir(ctx, append([]string{"clones", u.Host}, segments[:len(segments)-1]...)...)
	if err != nil {
		return "", true, err
	}
	name := segments[len(segments)-1]
	dir = filepath.Join(parent, name)
	err = cache.WithLock(ctx, parent, name, func() error {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return cloneInto(ctx, raw, dir)
		}
		if _, err := render.RunCLIEnv(ctx, render.Dir(dir), "git", []string{"fetch", "--quiet", "--depth=1", "--no-tags", "origin", "HEAD"}, noPrompt); err != nil {
			return err
		}
		_, err := render.RunCLI(ctx, render.Dir(dir), "git", []string{"reset", "--quiet", "--hard", "FETCH_HEAD"})
		return err
	})
	if err != nil {
		return "", true, fmt.Errorf("clone %s: %w", raw, err)
	}
	return dir, true, nil
}

// noPrompt fails a clone or fetch that would otherwise wait on a credential
// prompt nobody at an MCP server can answer.
var noPrompt = []string{"GIT_TERMINAL_PROMPT=0"}

func cloneInto(ctx context.Context, raw, dir string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := render.RunCLIEnv(ctx, render.Dir(tmp), "git", []string{"clone", "--quiet", "--depth=1", "--no-tags", "--", raw, tmp}, noPrompt); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}
