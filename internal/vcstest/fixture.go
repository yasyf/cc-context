// Package vcstest stands up real git, jj, and gt repositories for tests and
// records every tool invocation through a passthrough shim, so no test ever
// feeds ccx a byte that was not produced by the real tool.
package vcstest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/lookpath"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/workspace"
)

// Fixture is a real repository built by Repo, with the recording shim
// installed at the head of PATH and the test's working directory inside Dir.
type Fixture struct {
	Dir       string
	RemoteDir string
	ShimBin   string
	ArgvLog   string

	// env is the fixture's environment in exec's "K=V" form, carried rather
	// than installed: a fixture that replaced the process's would decide HOME
	// and PATH for every other test in the binary, which is what stops the
	// package running in parallel.
	env []string

	// settles is set when the shim holds a tool that writes to the log past
	// its own exit, which only gt does. See Quiesce.
	settles bool
}

// Context returns the context a test drives ccx with: every child spawned
// through it gets the fixture's environment, and every command that resolves
// the project root gets the fixture's repository. It replaces the HOME, PATH
// and chdir a fixture used to impose on the whole test binary.
func (f *Fixture) Context() context.Context { return f.ContextIn(f.Dir) }

// ContextIn is [Fixture.Context] rooted at dir, for a test driving ccx from a
// worktree the fixture cut rather than from the repository itself.
func (f *Fixture) ContextIn(dir string) context.Context {
	return workspace.WithRoot(render.WithEnv(context.Background(), f.env...), dir)
}

// Env returns the fixture's environment, for a test that spawns a tool itself
// rather than through ccx.
func (f *Fixture) Env() []string { return slices.Clone(f.env) }

// Quiesce blocks until every invocation the fixture's shim recorded has
// landed. It is [Quiesce] over the fixture's own log, skipped when no tool in
// the shim leaves a detached writer behind — the wait is a settle window, so
// paying it where nothing can still be writing is dead time on every read.
func (f *Fixture) Quiesce(t *testing.T) {
	t.Helper()
	if !f.settles {
		return
	}
	Quiesce(t, f.ArgvLog)
}

// Isolate installs the fixture's environment over the process's and chdirs
// into its repository, for a test whose subject reads the environment in
// process rather than handing it to a child — CLAUDE_PLUGIN_DATA through
// cache.Dir, or HOME through os.UserCacheDir. Everything else should take
// [Fixture.Context] instead: this is what keeps a test out of t.Parallel(),
// since t.Setenv panics there and the process has only one working directory.
func (f *Fixture) Isolate(t *testing.T) {
	t.Helper()
	for _, kv := range f.env {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}
	t.Chdir(f.Dir)
}

// Out runs bin in the fixture's repository under the fixture's environment and
// returns its stdout, failing the test on a nonzero exit. It is how a test
// spawns a tool the way ccx does, now that a fixture carries its PATH rather
// than installing it over the process's.
func (f *Fixture) Out(t *testing.T, bin string, args ...string) string {
	t.Helper()
	return run(t, f.Dir, f.env, bin, args...)
}

// PATH is the PATH the fixture's children resolve against.
func (f *Fixture) PATH() string { return lookpath.For(f.env).PATH }

// WorktreePath returns the path at which Worktree, PrunableWorktree, or
// LockedWorktree placed the named worktree.
func (f *Fixture) WorktreePath(name string) string {
	return filepath.Join(filepath.Dir(f.Dir), "wt", name)
}

// OnlyShimPATH narrows PATH to the fixture's own shim directory, dropping the
// brew-free system directories Repo leaves behind it. A test that needs a tool
// ABSENT has to say so this way: systemPATH keeps /usr/bin, which is where CI
// installs git and gh, while a developer's live in Homebrew's — so relying on
// the brew-free PATH to hide a tool passes locally and fails on CI.
func (f *Fixture) OnlyShimPATH(t *testing.T) {
	t.Helper()
	f.env = append(f.env, "PATH="+f.ShimBin)
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
// it. Real binaries resolve against the host PATH, so a second Repo in the
// same test finds a tool the first did not request and keeps reaching the
// ones it did; fixture construction runs by absolute path, so the shim log
// opens empty.
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
	dir := filepath.Join(base, "repo")
	f := &Fixture{Dir: dir}

	if cfg.brokenGitDir {
		f.env = fixtureEnv(t, base, detachedHome(t), resolved)
		mkdir(t, dir)
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: /nonexistent-repo\n")
		f.installShim(t)
		return f
	}

	tmpl := templateFor(t, cfg.base(), resolved)
	home := detachedHome(t)
	copyTree(t, tmpl.base, base)
	copyTree(t, tmpl.home, home)
	f.env = fixtureEnv(t, base, home, resolved)
	if cfg.remote {
		f.RemoteDir = filepath.Join(base, "remote.git")
		pinOrigin(t, base)
	}

	bin := map[string]string{}
	for _, tool := range resolved {
		bin[tool.name] = tool.path
	}
	git := func(args ...string) string { return run(t, dir, f.env, bin["git"], args...) }
	jj := func(args ...string) string { return run(t, dir, f.env, bin["jj"], args...) }

	if cfg.branch != "" {
		if cfg.jj {
			jj("bookmark", "create", cfg.branch, "-r", "@-")
		} else {
			git("switch", "-qc", cfg.branch)
		}
	}
	if cfg.worktree != "" {
		addWorktree(t, dir, f.env, bin["git"], f.WorktreePath(cfg.worktree))
	}
	if cfg.prunableWorktree {
		path := f.WorktreePath("prunable")
		addWorktree(t, dir, f.env, bin["git"], path)
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("remove worktree %s: %v", path, err)
		}
	}
	if cfg.lockedWorktree {
		path := f.WorktreePath("locked")
		addWorktree(t, dir, f.env, bin["git"], path)
		git("worktree", "lock", path)
	}
	if cfg.conflicted {
		if cfg.jj {
			buildJJConflict(t, dir, jj)
		} else {
			buildGitConflict(t, dir, f.env, bin["git"], git)
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

	f.installShim(t)
	return f
}

// fixtureEnv builds the environment that points HOME, config, and cache at a
// temp tree of the caller's own, so no fixture reads or writes state outside
// it. Fixture construction runs the tools by absolute path but under the
// brew-free PATH, so their interpreters are linked where that PATH reaches
// them. It is returned rather than installed: see [Fixture.env].
func fixtureEnv(t *testing.T, base, home string, tools []resolvedTool) []string {
	t.Helper()
	jjCfg := filepath.Join(base, "jjconfig.toml")
	writeFile(t, jjCfg, "user.name=\"t\"\nuser.email=\"t@t.t\"\n")
	pluginData := filepath.Join(base, "plugin-data")
	mkdir(t, pluginData)
	interp := filepath.Join(base, "interp")
	linkInterpreters(t, interp, tools)
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "xdg-config"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"JJ_CONFIG=" + jjCfg,
		"CLAUDE_PLUGIN_DATA=" + pluginData,
		"CLAUDE_CODE_SESSION_ID=",
		"GRAPHITE_AUTH_TOKEN=",
		"PATH=" + toolPATH(interp),
	}
}

// buildGitConflict leaves an unresolved merge of two edits to f.txt.
func buildGitConflict(t *testing.T, dir string, env []string, gitBin string, git func(...string) string) {
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
	runExpectFail(t, dir, env, gitBin, "merge", "conflict-side")
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
func addWorktree(t *testing.T, repo string, env []string, gitBin, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	run(t, repo, env, gitBin, "worktree", "add", "-q", path)
}

// run executes bin with args in dir and returns its stdout, failing the test
// on a nonzero exit.
func run(t *testing.T, dir string, env []string, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(lookpath.For(env).Bin(bin), args...) //nolint:gosec // bin is a LookPath-resolved vcs binary and args are fixture-authored, never user input
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
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
func runExpectFail(t *testing.T, dir string, env []string, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(lookpath.For(env).Bin(bin), args...) //nolint:gosec // bin is a LookPath-resolved vcs binary and args are fixture-authored, never user input
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err == nil {
		t.Fatalf("%s %v: succeeded, want failure", bin, args)
	}
}

// detachedHome returns the test's HOME, removed best-effort rather than through
// t.TempDir: gt leaves a refresher writing under HOME for seconds after it
// exits, and t.TempDir's RemoveAll reports that race as a cleanup failure.
func detachedHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ccx-home")
	if err != nil {
		t.Fatalf("create home dir: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve home dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(resolved) })
	return resolved
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
