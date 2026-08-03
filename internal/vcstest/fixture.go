// Package vcstest stands up real git, jj, and gt repositories for tests and
// records every tool invocation through a passthrough shim, so no test ever
// feeds ccx a byte that was not produced by the real tool.
package vcstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fixture is a real repository built by Repo, with the recording shim
// installed at the head of PATH and the test's working directory inside Dir.
type Fixture struct {
	Dir       string
	RemoteDir string
	ShimBin   string
	ArgvLog   string
}

// WorktreePath returns the path at which Worktree, PrunableWorktree, or
// LockedWorktree placed the named worktree.
func (f *Fixture) WorktreePath(name string) string {
	return filepath.Join(filepath.Dir(f.Dir), "wt", name)
}

type config struct {
	jj                 bool
	gt                 bool
	remote             bool
	branch             string
	trunk              string
	detached           bool
	dirty              bool
	staged             bool
	conflicted         bool
	conflictedBookmark bool
	worktree           string
	prunableWorktree   bool
	lockedWorktree     bool
	indexLock          bool
	brokenGitDir       bool
	noOriginHead       bool
}

// Opt configures the repository Repo builds.
type Opt func(*config)

// JJ colocates a jj repository over the git repository; the initial commit is
// cut through jj and a bookmark named after the trunk points at it.
func JJ() Opt { return func(c *config) { c.jj = true } }

// GT runs gt init against the repository, tracking the trunk.
func GT() Opt { return func(c *config) { c.gt = true } }

// Remote adds a bare origin repository, pushes the trunk to it, and points
// refs/remotes/origin/HEAD at the trunk unless NoOriginHead is also given.
func Remote() Opt { return func(c *config) { c.remote = true } }

// Branch cuts and checks out the named branch (a bookmark at @- under JJ).
func Branch(name string) Opt { return func(c *config) { c.branch = name } }

// Trunk names the initial branch; the default is main.
func Trunk(name string) Opt { return func(c *config) { c.trunk = name } }

// Detached detaches HEAD from its branch.
func Detached() Opt { return func(c *config) { c.detached = true } }

// Dirty leaves an unstaged edit to f.txt in the working copy.
func Dirty() Opt { return func(c *config) { c.dirty = true } }

// Staged leaves a staged edit to f.txt in the index.
func Staged() Opt { return func(c *config) { c.staged = true } }

// Conflicted leaves the working copy mid-conflict: an unresolved merge under
// git, a conflicted @ merging two divergent edits under JJ.
func Conflicted() Opt { return func(c *config) { c.conflicted = true } }

// ConflictedBookmark leaves a jj bookmark named feat conflicted between two
// divergent commits, the state a concurrent bookmark move produces.
func ConflictedBookmark() Opt { return func(c *config) { c.conflictedBookmark = true } }

// Worktree adds a linked worktree under the named branch.
func Worktree(name string) Opt { return func(c *config) { c.worktree = name } }

// PrunableWorktree adds a linked worktree named prunable and deletes its
// directory, so git reports it prunable.
func PrunableWorktree() Opt { return func(c *config) { c.prunableWorktree = true } }

// LockedWorktree adds a linked worktree named locked and locks it.
func LockedWorktree() Opt { return func(c *config) { c.lockedWorktree = true } }

// IndexLock holds .git/index.lock, the state a crashed or concurrent git
// process leaves behind.
func IndexLock() Opt { return func(c *config) { c.indexLock = true } }

// BrokenGitDir makes Dir a checkout whose .git pointer names a repository
// that does not exist, so every git query there exits 128.
func BrokenGitDir() Opt { return func(c *config) { c.brokenGitDir = true } }

// NoOriginHead suppresses the refs/remotes/origin/HEAD symref Remote would
// set, the state a plain git remote add leaves.
func NoOriginHead() Opt { return func(c *config) { c.noOriginHead = true } }

// Repo builds a real repository per opts under an isolated HOME and git/jj
// config, installs the recording shim on a brew-free PATH, and chdirs into
// it. Real binaries are resolved before any PATH change; fixture construction
// runs by absolute path, so the shim log opens empty.
func Repo(t *testing.T, opts ...Opt) *Fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("vcstest fixtures are POSIX-only")
	}
	cfg := config{trunk: "main"}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.noOriginHead && !cfg.remote {
		t.Fatal("NoOriginHead requires Remote")
	}
	if cfg.conflictedBookmark && !cfg.jj {
		t.Fatal("ConflictedBookmark requires JJ")
	}

	tools := []string{"git"}
	if cfg.jj {
		tools = append(tools, "jj")
	}
	if cfg.gt {
		tools = append(tools, "gt")
	}
	resolved := resolveTools(t, tools)

	base := realTempDir(t)
	isolateEnv(t, base)

	dir := filepath.Join(base, "repo")
	mkdir(t, dir)
	f := &Fixture{Dir: dir}

	if cfg.brokenGitDir {
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: /nonexistent-repo\n")
		f.ShimBin, f.ArgvLog = installShim(t, resolved)
		t.Chdir(dir)
		return f
	}

	bin := map[string]string{}
	for _, tool := range resolved {
		bin[tool.name] = tool.path
	}
	git := func(args ...string) string { return run(t, dir, bin["git"], args...) }
	jj := func(args ...string) string { return run(t, dir, bin["jj"], args...) }

	git("init", "-q", "-b", cfg.trunk)
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	writeFile(t, filepath.Join(dir, "f.txt"), "base\n")
	if cfg.jj {
		jj("git", "init", "--colocate")
		jj("commit", "-m", "init")
		jj("bookmark", "create", cfg.trunk, "-r", "@-")
	} else {
		git("add", "f.txt")
		git("commit", "-qm", "init")
	}

	if cfg.remote {
		f.RemoteDir = filepath.Join(base, "remote.git")
		run(t, base, bin["git"], "init", "-q", "--bare", "--initial-branch="+cfg.trunk, f.RemoteDir)
		git("remote", "add", "origin", f.RemoteDir)
		if cfg.jj {
			jj("git", "push", "--bookmark", cfg.trunk)
		} else {
			git("push", "-q", "origin", cfg.trunk)
		}
		if !cfg.noOriginHead {
			git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+cfg.trunk)
		}
	}

	if cfg.gt {
		run(t, dir, bin["gt"], "init", "--trunk", cfg.trunk, "--no-interactive")
	}

	if cfg.branch != "" {
		if cfg.jj {
			jj("bookmark", "create", cfg.branch, "-r", "@-")
		} else {
			git("switch", "-qc", cfg.branch)
		}
	}
	if cfg.worktree != "" {
		addWorktree(t, dir, bin["git"], f.WorktreePath(cfg.worktree))
	}
	if cfg.prunableWorktree {
		path := f.WorktreePath("prunable")
		addWorktree(t, dir, bin["git"], path)
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("remove worktree %s: %v", path, err)
		}
	}
	if cfg.lockedWorktree {
		path := f.WorktreePath("locked")
		addWorktree(t, dir, bin["git"], path)
		git("worktree", "lock", path)
	}
	if cfg.conflicted {
		if cfg.jj {
			buildJJConflict(t, dir, jj)
		} else {
			buildGitConflict(t, dir, bin["git"], git)
		}
	}
	if cfg.conflictedBookmark {
		buildConflictedBookmark(t, dir, jj)
	}
	if cfg.detached {
		git("checkout", "-q", "--detach")
	}
	if cfg.dirty {
		writeFile(t, filepath.Join(dir, "f.txt"), "dirty\n")
	}
	if cfg.staged {
		writeFile(t, filepath.Join(dir, "f.txt"), "staged\n")
		git("add", "f.txt")
	}
	if cfg.indexLock {
		writeFile(t, filepath.Join(dir, ".git", "index.lock"), "")
	}

	f.ShimBin, f.ArgvLog = installShim(t, resolved)
	t.Chdir(dir)
	return f
}

// isolateEnv points HOME, config, and cache environment at base so no fixture
// reads or writes state outside its own temp tree.
func isolateEnv(t *testing.T, base string) {
	t.Helper()
	home := filepath.Join(base, "home")
	mkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	jjCfg := filepath.Join(base, "jjconfig.toml")
	writeFile(t, jjCfg, "user.name=\"t\"\nuser.email=\"t@t.t\"\n")
	t.Setenv("JJ_CONFIG", jjCfg)
	pluginData := filepath.Join(base, "plugin-data")
	mkdir(t, pluginData)
	t.Setenv("CLAUDE_PLUGIN_DATA", pluginData)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("GRAPHITE_AUTH_TOKEN", "")
	t.Setenv("PATH", strings.Join(systemPATH, string(os.PathListSeparator)))
}

// buildGitConflict leaves an unresolved merge of two edits to f.txt.
func buildGitConflict(t *testing.T, dir, gitBin string, git func(...string) string) {
	t.Helper()
	current := strings.TrimSpace(git("branch", "--show-current"))
	git("switch", "-qc", "conflict-side")
	writeFile(t, filepath.Join(dir, "f.txt"), "side\n")
	git("add", "f.txt")
	git("commit", "-qm", "side")
	git("switch", "-q", current)
	writeFile(t, filepath.Join(dir, "f.txt"), "ours\n")
	git("add", "f.txt")
	git("commit", "-qm", "ours")
	runExpectFail(t, dir, gitBin, "merge", "conflict-side")
}

// buildJJConflict leaves @ as a conflicted merge of two divergent edits.
func buildJJConflict(t *testing.T, dir string, jj func(...string) string) {
	t.Helper()
	init := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	writeFile(t, filepath.Join(dir, "f.txt"), "left\n")
	jj("commit", "-m", "left")
	left := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	jj("new", init)
	writeFile(t, filepath.Join(dir, "f.txt"), "right\n")
	jj("commit", "-m", "right")
	right := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	jj("new", left, right)
}

// buildConflictedBookmark creates the feat bookmark in two divergent
// operations, then settles the operation log so jj marks it conflicted.
func buildConflictedBookmark(t *testing.T, dir string, jj func(...string) string) {
	t.Helper()
	init := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	writeFile(t, filepath.Join(dir, "f.txt"), "a\n")
	jj("commit", "-m", "a")
	a := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	jj("new", init)
	writeFile(t, filepath.Join(dir, "f.txt"), "b\n")
	jj("commit", "-m", "b")
	b := strings.TrimSpace(jj("log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	op := strings.TrimSpace(jj("op", "log", "-n", "1", "--no-graph", "-T", "id.short()"))
	jj("bookmark", "create", "feat", "-r", a)
	jj("--at-op", op, "bookmark", "create", "feat", "-r", b)
	jj("bookmark", "list")
}

// addWorktree cuts a linked worktree at path, letting git name its branch
// after the path's basename.
func addWorktree(t *testing.T, repo, gitBin, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	run(t, repo, gitBin, "worktree", "add", "-q", path)
}

// run executes bin with args in dir and returns its stdout, failing the test
// on a nonzero exit.
func run(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // bin is a LookPath-resolved vcs binary and args are fixture-authored, never user input
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s %v: %v\n%s", bin, args, err, stderr)
	}
	return string(out)
}

// runExpectFail executes bin with args in dir and fails the test if the
// command succeeds — the fixture's claimed state requires the failure.
func runExpectFail(t *testing.T, dir, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // bin is a LookPath-resolved vcs binary and args are fixture-authored, never user input
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatalf("%s %v: succeeded, want failure", bin, args)
	}
}

// realTempDir returns a per-test temp dir with symlinks resolved, so paths
// git and jj report compare equal to the fixture's own.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
