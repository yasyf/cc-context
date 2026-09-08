package lookpath

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// GitEnv names the git ccx and its children run, overriding the search below
// outright: a user who names a binary has already decided.
const GitEnv = "CCX_GIT"

// stubDirs are skipped while another git is reachable. /usr/bin/git on macOS is
// a stub that execs the real git inside Xcode.app, and an endpoint-security
// agent scans every exec off the sealed system volume, so it pays that scan
// twice: ~0.5s an invocation against ~0.01s for a git installed elsewhere.
var stubDirs = []string{"/usr/bin", "/bin"}

// gitCache holds the resolved git per (GitEnv, PATH) pair, so a run walks PATH
// once rather than once per child.
var gitCache sync.Map

// Bin returns the executable to spawn for name: the preferred git for "git",
// and name itself otherwise, left for exec to resolve against PATH. git is the
// only binary ccx spawns with a stub in the system directories; gt, gh, jj, uv
// and rg install outside them and already resolve to a real one.
func Bin(name string) string {
	if name != "git" {
		return name
	}
	key := os.Getenv(GitEnv) + "\x00" + os.Getenv("PATH")
	if hit, ok := gitCache.Load(key); ok {
		return hit.(string)
	}
	git := resolveGit()
	gitCache.Store(key, git)
	return git
}

// GitPATH returns the PATH a child inherits: the resolved git's directory ahead
// of ccx's own, so a tool that spawns git itself — gt spawns around twenty per
// run — reaches the same git. That directory is already on PATH, so only git's
// resolution order changes. It returns "" when there is nothing to reorder.
func GitPATH() string {
	git := Bin("git")
	if !filepath.IsAbs(git) {
		return ""
	}
	dir := filepath.Dir(git)
	entries := filepath.SplitList(os.Getenv("PATH"))
	if len(entries) == 0 || entries[0] == dir || slices.Contains(stubDirs, dir) {
		return ""
	}
	return strings.Join(append([]string{dir}, entries...), string(os.PathListSeparator))
}

// resolveGit returns the git PATH names with the system stub passed over, and
// exec's own answer untouched whenever that answer is not the stub. It reads
// the filesystem and nothing else — a resolution that spawned anything would
// put an unbounded wait on every caller's exec path.
func resolveGit() string {
	if pinned := os.Getenv(GitEnv); pinned != "" {
		return pinned
	}
	found := Find("git")
	if found == "" {
		return "git"
	}
	if !slices.Contains(stubDirs, filepath.Dir(found)) {
		return found
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || slices.Contains(stubDirs, dir) {
			continue
		}
		if candidate := filepath.Join(dir, "git"); executable(candidate) {
			return candidate
		}
	}
	return found
}

// executable reports whether path is a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
