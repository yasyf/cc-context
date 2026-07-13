package find

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/boyter/gocodewalker"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/yasyf/cc-context/internal/backend"
)

// match is one file the glob selected, carrying the slash-normalized path relative
// to the display root (for output and sorting), the absolute path (for probing),
// and its size in bytes.
type match struct {
	rel  string
	abs  string
	size int64
}

// walkConfig captures how collect filters a walk: escaped disables ignore-FILE
// processing (.gitignore/.ignore/.gitmodules), excludeVCS hard-skips the VCS
// stores, and matcher (rooted at gitRoot) applies the enclosing repo's ignore
// chain — the git-root info/exclude plus the .gitignore files down to the walk
// root — that a gocodewalker rooted below the git root never sees on its own.
type walkConfig struct {
	escaped    bool
	excludeVCS bool
	gitRoot    string
	matcher    gitignore.Matcher
}

// resolveAnchor applies the grep lane's escape-hatch contract to glob: when its
// literal prefix resolves to an existing path below the walk root, the walk
// re-roots there with ignore-FILE processing disabled. It returns the walk root,
// the glob to match relative to that root, whether the escape hatch fired, and
// whether the VCS stores stay excluded. An anchor that resolves to the walk root
// itself is a normal default walk, not an escape. A prefix that does not resolve
// leaves root and glob unchanged. The VCS stores stay excluded even under the
// escape hatch, unless a path element of the anchor is itself a VCS store —
// explicitly naming the store is the only way in.
func resolveAnchor(root, glob string) (walkRoot, matchGlob string, escaped, excludeVCS bool) {
	dir, rest := backend.SplitGlobAnchor(glob)
	if dir == "" {
		return root, glob, false, true
	}
	joined := dir
	if !filepath.IsAbs(dir) {
		joined = filepath.Join(root, dir)
	}
	info, err := os.Stat(joined)
	if err != nil {
		return root, glob, false, true
	}
	switch {
	case info.IsDir():
		if rest == "" {
			rest = "**/*"
		}
		if filepath.Clean(joined) == filepath.Clean(root) {
			return root, rest, false, true
		}
		return joined, rest, true, !anchorNamesVCSStore(dir)
	case rest == "" && info.Mode().IsRegular():
		return filepath.Dir(joined), filepath.Base(joined), true, !anchorNamesVCSStore(dir)
	}
	return root, glob, false, true
}

// anchorNamesVCSStore reports whether any path element of the anchor is a VCS
// store directory — the one case where the escape hatch keeps them in the walk.
func anchorNamesVCSStore(dir string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if slices.Contains(VCSStoreDirs, seg) {
			return true
		}
	}
	return false
}

// collect walks walkRoot and returns the files matching matchGlob (relative to
// walkRoot), each path also made relative to displayRoot for output. Under the
// default semantics it honors the per-directory .gitignore/.ignore/.gitmodules
// chain gocodewalker discovers, plus cfg.matcher for the enclosing repo's
// ancestor rules, and hard-skips the VCS stores; the escape hatch disables all
// ignore-FILE processing. It also returns the set of extensions seen anywhere
// under the walk, for the zero-match hint.
func collect(ctx context.Context, displayRoot, walkRoot, matchGlob string, cfg walkConfig) ([]match, map[string]bool, error) {
	queue := make(chan *gocodewalker.File, 256)
	w := newWalker(walkRoot, queue)
	w.IgnoreGitIgnore = cfg.escaped
	w.IgnoreIgnoreFile = cfg.escaped
	w.IgnoreGitModules = cfg.escaped
	if cfg.excludeVCS {
		w.ExcludeDirectory = VCSStoreDirs
	}

	errc := make(chan error, 1)
	go func() { errc <- w.Start() }()

	var matches []match
	seenExts := map[string]bool{}
	var stop error
	for f := range queue {
		if stop != nil {
			continue // keep draining so Start can close the queue
		}
		if ctx.Err() != nil {
			stop = fmt.Errorf("find: walk cancelled: %w", ctx.Err())
			w.Terminate()
			continue
		}
		if ext := extOf(f.Filename); ext != "" {
			seenExts[ext] = true
		}
		matchRel, err := filepath.Rel(walkRoot, f.Location)
		if err != nil {
			continue
		}
		ok, err := matchGlobFn(matchGlob, matchRel, cfg.escaped)
		if err != nil {
			stop = fmt.Errorf("find: match glob %q: %w", matchGlob, err)
			w.Terminate()
			continue
		}
		if !ok {
			continue
		}
		if ignoredByRepo(cfg, f.Location) {
			continue
		}
		info, err := os.Lstat(f.Location)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		outRel, err := filepath.Rel(displayRoot, f.Location)
		if err != nil {
			continue
		}
		matches = append(matches, match{rel: filepath.ToSlash(outRel), abs: f.Location, size: info.Size()})
	}
	if err := <-errc; err != nil {
		return nil, nil, fmt.Errorf("find: walk %q: %w", walkRoot, err)
	}
	if stop != nil {
		return nil, nil, stop
	}
	return matches, seenExts, nil
}

// ignoredByRepo reports whether the enclosing repo's ancestor ignore chain hides
// the file at abs. gocodewalker already applied the walk-root-and-below chain, so
// this only adds the rules above the walk root (and the git-root info/exclude);
// it is a no-op when the walk is not inside a git repo or the escape hatch fired.
func ignoredByRepo(cfg walkConfig, abs string) bool {
	if cfg.matcher == nil {
		return false
	}
	rel, err := filepath.Rel(cfg.gitRoot, abs)
	if err != nil {
		return false
	}
	return cfg.matcher.Match(strings.Split(filepath.ToSlash(rel), "/"), false)
}

// countHidden reports how many additional files matchGlob would match under root
// once the ignore chain is disabled — the files the default walk hid. VCS stores
// stay skipped so their internals never count. The result is clamped at zero
// against a concurrent tree mutation between the two walks.
func countHidden(ctx context.Context, root, matchGlob string, shown int) (int, error) {
	queue := make(chan *gocodewalker.File, 256)
	w := newWalker(root, queue)
	w.IgnoreGitIgnore = true
	w.IgnoreIgnoreFile = true
	w.IgnoreGitModules = true
	w.ExcludeDirectory = VCSStoreDirs

	errc := make(chan error, 1)
	go func() { errc <- w.Start() }()

	raw := 0
	var stop error
	for f := range queue {
		if stop != nil {
			continue // keep draining so Start can close the queue
		}
		if ctx.Err() != nil {
			stop = fmt.Errorf("find: count walk cancelled: %w", ctx.Err())
			w.Terminate()
			continue
		}
		matchRel, err := filepath.Rel(root, f.Location)
		if err != nil {
			continue
		}
		ok, err := matchGlobFn(matchGlob, matchRel, false)
		if err != nil {
			stop = fmt.Errorf("find: match glob %q: %w", matchGlob, err)
			w.Terminate()
			continue
		}
		if !ok {
			continue
		}
		info, err := os.Lstat(f.Location)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw++
	}
	if err := <-errc; err != nil {
		return 0, fmt.Errorf("find: count walk %q: %w", root, err)
	}
	if stop != nil {
		return 0, stop
	}
	if hidden := raw - shown; hidden > 0 {
		return hidden, nil
	}
	return 0, nil
}

// newWalker builds a gocodewalker rooted at root that emits dotfiles and tolerates
// per-file errors; the caller sets the ignore-processing flags before Start.
func newWalker(root string, queue chan *gocodewalker.File) *gocodewalker.FileWalker {
	w := gocodewalker.NewFileWalker(root, queue)
	w.IncludeHidden = true
	w.SetErrorHandler(func(error) bool { return true })
	return w
}

// matchGlobFn matches rel against pattern: a slash-less pattern the caller typed
// matches the basename at any depth (recursive-basename semantics); a slashed
// pattern — and any anchored remainder, so ".venv/*.py" stays direct-children —
// matches the full relative path via doublestar.
func matchGlobFn(pattern, rel string, anchored bool) (bool, error) {
	rel = filepath.ToSlash(rel)
	if !anchored && !strings.Contains(pattern, "/") {
		return doublestar.Match(pattern, path.Base(rel))
	}
	return doublestar.Match(pattern, rel)
}

// gitRootOf walks up from start returning the nearest directory that holds a .git
// entry — a directory (a normal repo) or a file (a worktree or submodule gitlink)
// — or "" when none is found up to the filesystem root.
func gitRootOf(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ancestorMatcher builds the enclosing repo's ignore matcher: the git-root
// .git/info/exclude plus every .gitignore from the git root down to walkRoot
// (inclusive), each pattern anchored at its own file's directory via its domain,
// in ascending priority (deeper wins). It matches paths relative to the git root,
// so an anchored ancestor rule like "sub/secret.go" applies even when the walk is
// scoped into sub. Returns nil when there is no git root or no patterns apply —
// gocodewalker's own per-directory chain then covers the walk root and below.
func ancestorMatcher(gitRoot, walkRoot string) gitignore.Matcher {
	if gitRoot == "" {
		return nil
	}
	ps := parsePatternFile(filepath.Join(gitRoot, ".git", "info", "exclude"), nil)
	ps = append(ps, parsePatternFile(filepath.Join(gitRoot, ".gitignore"), nil)...)

	rel, err := filepath.Rel(gitRoot, walkRoot)
	if err == nil && rel != "." {
		dir := gitRoot
		var domain []string
		for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
			dir = filepath.Join(dir, seg)
			domain = append(domain, seg)
			ps = append(ps, parsePatternFile(filepath.Join(dir, ".gitignore"), slices.Clone(domain))...)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	return gitignore.NewMatcher(ps)
}

// parsePatternFile reads one gitignore-format file and parses its non-blank,
// non-comment lines into patterns anchored at domain (the segments, relative to
// the git root, of the directory the file lives in). A missing file yields none.
func parsePatternFile(path string, domain []string) []gitignore.Pattern {
	data, err := os.ReadFile(path) //nolint:gosec // path is the caller's own repo ignore file; reading it is intended
	if err != nil {
		return nil
	}
	var ps []gitignore.Pattern
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	return ps
}

// extOf returns name's lowercase extension without the dot, or "" when it has none.
func extOf(name string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
}
