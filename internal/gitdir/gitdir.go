// Package gitdir locates a working tree's git root and the common git directory
// its linked worktrees share, by reading .git entries rather than running git.
package gitdir

import (
	"os"
	"path/filepath"
	"strings"
)

// Root walks up from start returning the nearest directory that holds a .git
// entry — a directory (a normal repo) or a file (a worktree or submodule gitlink)
// — or "" when none is found up to the filesystem root.
func Root(start string) string {
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

// CommonDir resolves the git directory holding the repo-wide info/, given a git
// root: <gitRoot>/.git when that is a directory, and — when it is the gitdir
// pointer file of a linked worktree — the common directory that worktree's
// administrative dir names. It returns "" when the pointer does not resolve.
func CommonDir(gitRoot string) string {
	dot := filepath.Join(gitRoot, ".git")
	info, err := os.Stat(dot)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return dot
	}
	data, err := os.ReadFile(dot) //nolint:gosec // path is the caller's own repo gitdir pointer; reading it is intended
	if err != nil {
		return ""
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(gitRoot, gitDir)
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // path is the caller's own repo git dir; reading it is intended
	if err != nil {
		return gitDir
	}
	commonDir := strings.TrimSpace(string(common))
	if filepath.IsAbs(commonDir) {
		return filepath.Clean(commonDir)
	}
	return filepath.Join(gitDir, commonDir)
}
