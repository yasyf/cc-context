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

// Env is the environment a resolution reads. A child's own environment governs
// which git it gets, so a caller that hands its child a PATH of its own
// resolves against that one rather than the process's.
type Env struct {
	Git  string
	PATH string
}

// For reads the resolution inputs out of a child environment in exec's own
// form, where a later entry overrides an earlier one.
func For(env []string) Env {
	var e Env
	for _, kv := range env {
		switch key, value, _ := strings.Cut(kv, "="); key {
		case GitEnv:
			e.Git = value
		case "PATH":
			e.PATH = value
		}
	}
	return e
}

// Bin returns the executable to spawn for name, resolved against e.PATH: the
// preferred git for "git", and the first match on PATH otherwise. git is the
// only binary ccx spawns with a stub in the system directories; gt, gh, jj, uv
// and rg install outside them and already resolve to a real one. Resolving
// here rather than leaving it to exec is what makes e.PATH govern at all —
// exec.Command searches the process's PATH, never the one on cmd.Env.
func (e Env) Bin(name string) string {
	if name != "git" {
		return e.find(name)
	}
	key := e.Git + "\x00" + e.PATH
	if hit, ok := gitCache.Load(key); ok {
		return hit.(string)
	}
	git := e.resolveGit()
	gitCache.Store(key, git)
	return git
}

// find returns name resolved against e.PATH, or name itself when PATH holds no
// such executable, leaving exec to report the failure in its own words.
func (e Env) find(name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	for _, dir := range filepath.SplitList(e.PATH) {
		if candidate := filepath.Join(dir, name); dir != "" && executable(candidate) {
			return candidate
		}
	}
	return name
}

// GitPATH returns the PATH a child inherits: the resolved git's directory ahead
// of ccx's own, so a tool that spawns git itself — gt spawns around twenty per
// run — reaches the same git. That directory is already on PATH, so only git's
// resolution order changes. It returns "" when there is nothing to reorder.
func (e Env) GitPATH() string {
	git := e.Bin("git")
	if !filepath.IsAbs(git) {
		return ""
	}
	dir := filepath.Dir(git)
	entries := filepath.SplitList(e.PATH)
	if len(entries) == 0 || entries[0] == dir || slices.Contains(stubDirs, dir) {
		return ""
	}
	return strings.Join(append([]string{dir}, entries...), string(os.PathListSeparator))
}

// resolveGit returns the first git on PATH, passing over one in the system
// directories while another is reachable and falling back to it when none is.
// It reads the filesystem and nothing else — a resolution that spawned
// anything would put an unbounded wait on every caller's exec path.
func (e Env) resolveGit() string {
	if e.Git != "" {
		return e.Git
	}
	var stub string
	for _, dir := range filepath.SplitList(e.PATH) {
		candidate := filepath.Join(dir, "git")
		if dir == "" || !executable(candidate) {
			continue
		}
		if !slices.Contains(stubDirs, dir) {
			return candidate
		}
		if stub == "" {
			stub = candidate
		}
	}
	if stub != "" {
		return stub
	}
	return "git"
}

// executable reports whether path is a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path is a PATH entry joined with "git", not user input
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
