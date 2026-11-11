// Package lookpath resolves executables against a given environment's PATH.
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

// GhEnv names the gh ccx and its children run. A machine whose gh on PATH is a
// shim into ccx vcs gh names the real one here, or ccx would call itself.
const GhEnv = "CCX_GH"

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
	Gh   string
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
		case GhEnv:
			e.Gh = value
		case "PATH":
			e.PATH = value
		}
	}
	return e
}

// Bin returns the executable to spawn for name, resolved against e.PATH by the
// rule [Env.Find] states. Resolving here rather than leaving it to exec is what
// makes e.PATH govern at all — exec.Command searches the process's PATH, never
// the one on cmd.Env.
func (e Env) Bin(name string) string {
	if name == "gh" && e.Gh != "" {
		return e.Gh
	}
	if name != "git" {
		return e.find(name)
	}
	key := e.Git + "\x00" + e.PATH
	if hit, ok := gitCache.Load(key); ok {
		return hit.(string)
	}
	git := e.Git
	if git == "" {
		git = e.find("git")
	}
	gitCache.Store(key, git)
	return git
}

// Find returns name's first match on e.PATH, passing over a launcher while
// another match is reachable, or "" when PATH holds none. A launcher is a mise
// shim, which refuses to start in a directory whose mise.toml is untrusted, or
// a git in [stubDirs]. Find is the presence check matching what [Env.Bin] would
// spawn, for a caller that has to know a tool is absent before it runs it.
func (e Env) Find(name string) string {
	if name == "gh" && e.Gh != "" {
		return e.Gh
	}
	if resolved := e.find(name); resolved != name {
		return resolved
	}
	return ""
}

// find returns name resolved by the rule [Env.Find] states, or name itself
// when PATH holds no such executable, leaving exec to report the failure in its
// own words. It reads the filesystem and nothing else — a resolution that
// spawned anything would put an unbounded wait on every caller's exec path.
func (e Env) find(name string) string {
	if strings.ContainsRune(name, filepath.Separator) {
		return name
	}
	var launcher string
	for _, dir := range filepath.SplitList(e.PATH) {
		candidate := filepath.Join(dir, name)
		if dir == "" || !executable(candidate) {
			continue
		}
		if !miseShim(candidate) && (name != "git" || !slices.Contains(stubDirs, dir)) {
			return candidate
		}
		if launcher == "" {
			launcher = candidate
		}
	}
	if launcher != "" {
		return launcher
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

// miseShim reports whether path is a mise shim: a symlink to mise itself, which
// picks the tool's version from the working directory's mise.toml.
func miseShim(path string) bool {
	target, err := os.Readlink(path)
	return err == nil && filepath.Base(target) == "mise"
}

// executable reports whether path is a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path is a PATH entry joined with a binary name, not user input
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
