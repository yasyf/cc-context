package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain re-execs the test binary as the ccx-ship-select diff tool: jj's
// hunk-scoped lane wires the merge-tool program to os.Executable(), which under
// `go test` is this binary. Both guards are required so a normal `go test` run —
// whose os.Args[1] is a -test.* flag — never self-dispatches; the env var is set
// only by the live ship tests, so it reaches jj's tool child through ship.
func TestMain(m *testing.M) {
	if os.Getenv("CCX_TEST_APPLY_SELECTION") == "1" && len(os.Args) > 1 && os.Args[1] == "vcs" {
		root := NewRootCmd()
		root.SetArgs(os.Args[1:])
		if err := root.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// requireLiveVCS skips a live test on Windows or when any required binary is off
// PATH — the skip lane the ci.yml jj step closes.
func requireLiveVCS(t *testing.T, bins ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("live VCS tests are POSIX-only")
	}
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// mustRun runs name with args in dir, failing the test on a nonzero exit, and
// returns its stdout.
func mustRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // fixed argv; dir is a TempDir, args are literals
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s %v: %v\n%s", name, args, err, stderr)
	}
	return string(out)
}

// statSnapshot captures a file's mtime and size so a test can assert the worktree
// file is untouched across a hunk-scoped commit.
type statSnapshot struct {
	mtime time.Time
	size  int64
}

func statOf(t *testing.T, path string) statSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return statSnapshot{mtime: info.ModTime(), size: info.Size()}
}

func (s statSnapshot) equal(o statSnapshot) bool {
	return s.mtime.Equal(o.mtime) && s.size == o.size
}

// liveHunkRefs runs the real `ccx vcs hunks` command over path and returns each
// listed hunk's ref (the first tab-delimited field), so the live ship exercises
// the exact refs an operator would copy from the listing.
func liveHunkRefs(t *testing.T, path string) []string {
	t.Helper()
	out := runHunksCmd(t, path)
	var refs []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		refs = append(refs, strings.SplitN(line, "\t", 2)[0])
	}
	return refs
}

func readFileStr(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// setupLiveJJRepo stands up a config-isolated colocated jj repo, chdirs into it,
// commits base as f.txt (so @- carries it), then writes current to the worktree —
// the @ change ship partially commits. It arms the CCX_TEST_APPLY_SELECTION guard
// so jj's spawned diff tool re-execs into ccx.
func setupLiveJJRepo(t *testing.T, base, current string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("user.name=\"t\"\nuser.email=\"t@t.t\"\n"), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CCX_TEST_APPLY_SELECTION", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, dir, "jj", "git", "init", "--colocate")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(base), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, dir, "jj", "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(current), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write current: %v", err)
	}
	t.Chdir(dir)
	return dir
}

func jjFileShow(t *testing.T, dir, rev string) string {
	t.Helper()
	return mustRun(t, dir, "jj", "file", "show", "-r", rev, "--", "f.txt")
}

func TestShipJJPreflightRefusalAndEmptyGuardLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, "clone")
	cfg := filepath.Join(t.TempDir(), "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("user.name=\"t\"\nuser.email=\"t@t.t\"\n"), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, base, "git", "init", "--bare", "--initial-branch=main", remote)
	if err := os.Mkdir(seed, 0o750); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	mustRun(t, seed, "git", "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("base\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, seed, "git", "add", "f.txt")
	mustRun(t, seed, "git", "-c", "user.name=t", "-c", "user.email=t@t.t", "commit", "-m", "init")
	mustRun(t, seed, "git", "branch", "dev")
	mustRun(t, seed, "git", "remote", "add", "origin", remote)
	mustRun(t, seed, "git", "push", "origin", "main", "dev")
	mustRun(t, base, "jj", "git", "clone", "--colocate", remote, clone)
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("edited\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write edit: %v", err)
	}
	t.Chdir(clone)

	before, err := strconv.Atoi(strings.TrimSpace(mustRun(t, base, "git", "--git-dir="+remote, "rev-list", "--count", "main")))
	if err != nil {
		t.Fatalf("parse initial remote commit count: %v", err)
	}
	opBefore := strings.TrimSpace(mustRun(t, clone, "jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate))
	_, err = runShipCmd(t, "-m", "x", "--no-watch")
	if err == nil || !strings.Contains(err.Error(), "cannot resolve the trunk bookmark") {
		t.Fatalf("first ship error = %v, want trunk resolution refusal", err)
	}
	if diff := mustRun(t, clone, "jj", "diff", "--name-only"); !strings.Contains(diff, "f.txt") {
		t.Errorf("jj diff after refusal = %q, want f.txt edit", diff)
	}
	afterRefusal, err := strconv.Atoi(strings.TrimSpace(mustRun(t, base, "git", "--git-dir="+remote, "rev-list", "--count", "main")))
	if err != nil {
		t.Fatalf("parse post-refusal remote commit count: %v", err)
	}
	if afterRefusal != before {
		t.Errorf("remote main count after refusal = %d, want %d", afterRefusal, before)
	}
	opAfter := strings.TrimSpace(mustRun(t, clone, "jj", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate))
	if opAfter != opBefore {
		t.Errorf("jj operation after refusal = %q, want unchanged %q", opAfter, opBefore)
	}

	if _, err := runShipCmd(t, "-m", "x", "--no-watch", "--bookmark", "main"); err != nil {
		t.Fatalf("second ship error = %v", err)
	}
	afterPush, err := strconv.Atoi(strings.TrimSpace(mustRun(t, base, "git", "--git-dir="+remote, "rev-list", "--count", "main")))
	if err != nil {
		t.Fatalf("parse post-push remote commit count: %v", err)
	}
	if afterPush != before+1 {
		t.Errorf("remote main count after push = %d, want %d", afterPush, before+1)
	}

	_, err = runShipCmd(t, "-m", "y", "--no-watch")
	if err == nil || !strings.Contains(err.Error(), "nothing to commit — did a prior ship already land") {
		t.Fatalf("third ship error = %v, want empty ship refusal", err)
	}
	if !strings.Contains(err.Error(), "jj bookmark move exact:main --to @- && jj git push --bookmark exact:main") {
		t.Errorf("third ship error = %q, want exact:main push hint", err)
	}
}

// TestShipJJAutoTrackUntrackedLive proves ship auto-tracks an untracked trunk
// bookmark in a fresh colocated repo: a git clone imported by jj git init
// --colocate leaves a local main bookmark but an untracked main@origin, so jj git
// push refuses "Non-tracking remote bookmark" until ship tracks it. The push must
// land and the bookmark must end up tracked.
func TestShipJJAutoTrackUntrackedLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, "clone")
	cfg := filepath.Join(t.TempDir(), "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("user.name=\"t\"\nuser.email=\"t@t.t\"\n"), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, base, "git", "init", "--bare", "--initial-branch=main", remote)
	if err := os.Mkdir(seed, 0o750); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	mustRun(t, seed, "git", "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("base\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, seed, "git", "add", "f.txt")
	mustRun(t, seed, "git", "-c", "user.name=t", "-c", "user.email=t@t.t", "commit", "-m", "init")
	mustRun(t, seed, "git", "remote", "add", "origin", remote)
	mustRun(t, seed, "git", "push", "origin", "main")

	// A git clone imports origin/main as a git remote-tracking ref; jj git init
	// --colocate then sees a local main bookmark but leaves main@origin untracked.
	mustRun(t, base, "git", "clone", remote, clone)
	mustRun(t, clone, "jj", "git", "init", "--colocate")
	if before := mustRun(t, clone, "jj", "bookmark", "list", "--remote", "origin", "-T", jjRemoteBookmarkTemplate); !strings.Contains(before, "origin\tuntracked") {
		t.Fatalf("precondition: main@origin should be untracked, got %q", before)
	}
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("edited\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write edit: %v", err)
	}
	t.Chdir(clone)

	before, err := strconv.Atoi(strings.TrimSpace(mustRun(t, base, "git", "--git-dir="+remote, "rev-list", "--count", "main")))
	if err != nil {
		t.Fatalf("parse initial remote commit count: %v", err)
	}

	if _, err := runShipCmd(t, "-m", "x", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v, want auto-track then successful push", err)
	}

	after, err := strconv.Atoi(strings.TrimSpace(mustRun(t, base, "git", "--git-dir="+remote, "rev-list", "--count", "main")))
	if err != nil {
		t.Fatalf("parse post-push remote commit count: %v", err)
	}
	if after != before+1 {
		t.Errorf("remote main count after push = %d, want %d", after, before+1)
	}
	tracked := mustRun(t, clone, "jj", "bookmark", "list", "--remote", "origin", "--tracked", "-T", jjRemoteBookmarkTemplate)
	if !strings.Contains(tracked, "origin\ttracked") {
		t.Errorf("main@origin should be tracked after ship, got %q", tracked)
	}
}

// TestJJTrackUntrackedAtNameLive proves the auto-track path handles a bookmark name
// that needs jj string-pattern quoting: a git branch literally named "foo@bar",
// imported untracked by jj git init --colocate. jjTrackUntrackedTarget must find the
// untracked counterpart via the exact-name list and track it with the quoted
// argument plus --remote — the bare foo@bar@origin operand jj reads as a
// bookmark@remote symbol and refuses.
func TestJJTrackUntrackedAtNameLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	const branch = "foo@bar"
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, "clone")
	cfg := filepath.Join(t.TempDir(), "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("user.name=\"t\"\nuser.email=\"t@t.t\"\n"), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, base, "git", "init", "--bare", "--initial-branch=main", remote)
	if err := os.Mkdir(seed, 0o750); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	mustRun(t, seed, "git", "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("base\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, seed, "git", "add", "f.txt")
	mustRun(t, seed, "git", "-c", "user.name=t", "-c", "user.email=t@t.t", "commit", "-m", "init")
	mustRun(t, seed, "git", "branch", branch)
	mustRun(t, seed, "git", "remote", "add", "origin", remote)
	mustRun(t, seed, "git", "push", "origin", "main", branch)

	mustRun(t, base, "git", "clone", remote, clone)
	mustRun(t, clone, "jj", "git", "init", "--colocate")
	pat := jjExactPattern(branch)
	if before := mustRun(t, clone, "jj", "bookmark", "list", pat, "--all-remotes", "-T", jjRemoteBookmarkTemplate); !strings.Contains(before, "origin\tuntracked") {
		t.Fatalf("precondition: %s@origin should be untracked, got %q", branch, before)
	}
	t.Chdir(clone)

	if err := jjTrackUntrackedTarget(context.Background(), branch); err != nil {
		t.Fatalf("jjTrackUntrackedTarget(%q) = %v, want nil", branch, err)
	}
	after := mustRun(t, clone, "jj", "bookmark", "list", pat, "--all-remotes", "-T", jjRemoteBookmarkTemplate)
	if !strings.Contains(after, "origin\ttracked") {
		t.Errorf("%s@origin should be tracked after track, got %q", branch, after)
	}
}

func TestShipJJHunkScopedLive(t *testing.T) {
	requireLiveVCS(t, "jj", "git")
	tests := []struct {
		name          string
		base          string
		current       string
		flag          string
		hunkIdx       int
		wantCommitted string // @- content after the partial commit
	}{
		{
			name:          "skip first change hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nc\nd\nE\n",
			flag:          "--skip-hunk",
			hunkIdx:       0,
			wantCommitted: "a\nb\nc\nd\nE\n",
		},
		{
			name:          "only first change hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nc\nd\nE\n",
			flag:          "--only-hunk",
			hunkIdx:       0,
			wantCommitted: "A\nb\nc\nd\ne\n",
		},
		{
			name:          "skip pure-deletion hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nd\ne\n",
			flag:          "--skip-hunk",
			hunkIdx:       1, // hunk 0 is the a->A change, hunk 1 is the c deletion
			wantCommitted: "A\nb\nc\nd\ne\n",
		},
		{
			name:          "only first of identical deletions",
			base:          "gone\na\ngone\n",
			current:       "a\n",
			flag:          "--only-hunk",
			hunkIdx:       0,
			wantCommitted: "a\ngone\n",
		},
		{
			name:          "only second of identical deletions",
			base:          "gone\na\ngone\n",
			current:       "a\n",
			flag:          "--only-hunk",
			hunkIdx:       1,
			wantCommitted: "gone\na\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupLiveJJRepo(t, tt.base, tt.current)
			refs := liveHunkRefs(t, "f.txt")
			if tt.hunkIdx >= len(refs) {
				t.Fatalf("want a ref at index %d, hunks listed %d: %v", tt.hunkIdx, len(refs), refs)
			}
			before := statOf(t, "f.txt")

			if _, err := runShipCmd(t, "-m", "partial ship", "--no-push", tt.flag, refs[tt.hunkIdx], "f.txt"); err != nil {
				t.Fatalf("ship error = %v", err)
			}

			if got := jjFileShow(t, dir, "@-"); got != tt.wantCommitted {
				t.Errorf("committed (@-) = %q, want %q", got, tt.wantCommitted)
			}
			if got := jjFileShow(t, dir, "@"); got != tt.current {
				t.Errorf("remainder (@) = %q, want %q (the skipped hunk must stay in the working copy)", got, tt.current)
			}
			if got := readFileStr(t, "f.txt"); got != tt.current {
				t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, tt.current)
			}
			if after := statOf(t, "f.txt"); !after.equal(before) {
				t.Errorf("worktree stat changed: before=%+v after=%+v (mtime/size must be untouched)", before, after)
			}
		})
	}
}

// setupLiveGitRepo stands up a config-isolated git repo, chdirs into it, commits
// base as f.txt, stages an unrelated file (which must survive the hunk-scoped
// commit), then writes current to the worktree.
func setupLiveGitRepo(t *testing.T, base, current string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, dir, "git", "init", "-q")
	mustRun(t, dir, "git", "config", "user.email", "t@t.t")
	mustRun(t, dir, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(base), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, dir, "git", "add", "f.txt")
	mustRun(t, dir, "git", "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write staged: %v", err)
	}
	mustRun(t, dir, "git", "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(current), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write current: %v", err)
	}
	t.Chdir(dir)
	return dir
}

func TestShipGitHunkScopedLive(t *testing.T) {
	requireLiveVCS(t, "git")
	tests := []struct {
		name          string
		base          string
		current       string
		flag          string
		hunkIdx       int
		wantCommitted string // HEAD:f.txt after the partial commit
	}{
		{
			name:          "skip first change hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nc\nd\nE\n",
			flag:          "--skip-hunk",
			hunkIdx:       0,
			wantCommitted: "a\nb\nc\nd\nE\n",
		},
		{
			name:          "only first change hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nc\nd\nE\n",
			flag:          "--only-hunk",
			hunkIdx:       0,
			wantCommitted: "A\nb\nc\nd\ne\n",
		},
		{
			name:          "skip pure-deletion hunk",
			base:          "a\nb\nc\nd\ne\n",
			current:       "A\nb\nd\ne\n",
			flag:          "--skip-hunk",
			hunkIdx:       1,
			wantCommitted: "A\nb\nc\nd\ne\n",
		},
		{
			name:          "only first of identical deletions",
			base:          "gone\na\ngone\n",
			current:       "a\n",
			flag:          "--only-hunk",
			hunkIdx:       0,
			wantCommitted: "a\ngone\n",
		},
		{
			name:          "only second of identical deletions",
			base:          "gone\na\ngone\n",
			current:       "a\n",
			flag:          "--only-hunk",
			hunkIdx:       1,
			wantCommitted: "gone\na\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupLiveGitRepo(t, tt.base, tt.current)
			refs := liveHunkRefs(t, "f.txt")
			if tt.hunkIdx >= len(refs) {
				t.Fatalf("want a ref at index %d, hunks listed %d: %v", tt.hunkIdx, len(refs), refs)
			}
			before := statOf(t, "f.txt")

			if _, err := runShipCmd(t, "-m", "partial ship", "--no-push", tt.flag, refs[tt.hunkIdx], "f.txt"); err != nil {
				t.Fatalf("ship error = %v", err)
			}

			if got := mustRun(t, dir, "git", "show", "HEAD:f.txt"); got != tt.wantCommitted {
				t.Errorf("committed (HEAD:f.txt) = %q, want %q", got, tt.wantCommitted)
			}
			if got := readFileStr(t, "f.txt"); got != tt.current {
				t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, tt.current)
			}
			if after := statOf(t, "f.txt"); !after.equal(before) {
				t.Errorf("worktree stat changed: before=%+v after=%+v (temp-index lane must not touch the worktree)", before, after)
			}

			// staged.txt must stay staged and uncommitted; f.txt must show
			// modified-unstaged (index synced to new HEAD, worktree still the full current).
			status := statusSet(t, dir)
			want := map[string]bool{"A  staged.txt": true, " M f.txt": true}
			if !mapEqual(status, want) {
				t.Errorf("git status --porcelain = %v, want %v", status, want)
			}
			if _, err := runGit(dir, "show", "HEAD:staged.txt"); err == nil {
				t.Errorf("staged.txt must not be committed, but HEAD:staged.txt resolved")
			}
		})
	}
}

// initSubdirGitRepo stands up a config-isolated git repo with the tracked file at
// sub/f.txt and chdirs into sub/, so ship runs from a subdirectory against the
// repo-root object frame. It returns the repo root.
func initSubdirGitRepo(t *testing.T, base, current string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")
	mustRun(t, dir, "git", "init", "-q")
	mustRun(t, dir, "git", "config", "user.email", "t@t.t")
	mustRun(t, dir, "git", "config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte(base), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte(current), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write current: %v", err)
	}
	t.Chdir(filepath.Join(dir, "sub"))
	return dir
}

func TestShipGitHunkScopedSubdirLive(t *testing.T) {
	requireLiveVCS(t, "git")
	const base = "a\nb\nc\nd\ne\n"
	const current = "A\nb\nc\nd\nE\n"
	dir := initSubdirGitRepo(t, base, current)

	refs := liveHunkRefs(t, "f.txt")
	if len(refs) == 0 || !strings.HasPrefix(refs[0], "sub/f.txt:") {
		t.Fatalf("ccx vcs hunks from a subdir must emit a root-relative ref, got %v", refs)
	}
	before := statOf(t, "f.txt")

	// Skip hunk 0 (a->A); the commit keeps only the E change.
	if _, err := runShipCmd(t, "-m", "partial ship", "--no-push", "--skip-hunk", refs[0], "f.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}

	if got := mustRun(t, dir, "git", "show", "HEAD:sub/f.txt"); got != "a\nb\nc\nd\nE\n" {
		t.Errorf("committed (HEAD:sub/f.txt) = %q, want %q", got, "a\nb\nc\nd\nE\n")
	}
	if _, err := runGit(dir, "show", "HEAD:f.txt"); err == nil {
		t.Errorf("a spurious root-level f.txt was committed")
	}
	if got := readFileStr(t, "f.txt"); got != current {
		t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, current)
	}
	if after := statOf(t, "f.txt"); !after.equal(before) {
		t.Errorf("worktree stat changed: before=%+v after=%+v", before, after)
	}
}

// initSubdirJJRepo is initSubdirGitRepo for a colocated jj repo, arming the
// CCX_TEST_APPLY_SELECTION guard so jj's diff tool re-execs into ccx.
func initSubdirJJRepo(t *testing.T, base, current string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "jjconfig.toml")
	if err := os.WriteFile(cfg, []byte("user.name=\"t\"\nuser.email=\"t@t.t\"\n"), 0o600); err != nil {
		t.Fatalf("write jj config: %v", err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CCX_TEST_APPLY_SELECTION", "1")
	t.Setenv(envClaudeSessionKey, "")
	mustRun(t, dir, "jj", "git", "init", "--colocate")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte(base), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, dir, "jj", "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte(current), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write current: %v", err)
	}
	t.Chdir(filepath.Join(dir, "sub"))
	return dir
}

func TestShipJJHunkScopedSubdirLive(t *testing.T) {
	requireLiveVCS(t, "jj", "git")
	const base = "a\nb\nc\nd\ne\n"
	const current = "A\nb\nc\nd\nE\n"
	dir := initSubdirJJRepo(t, base, current)

	refs := liveHunkRefs(t, "f.txt")
	if len(refs) == 0 || !strings.HasPrefix(refs[0], "sub/f.txt:") {
		t.Fatalf("ccx vcs hunks from a subdir must emit a root-relative ref, got %v", refs)
	}
	before := statOf(t, "f.txt")

	if _, err := runShipCmd(t, "-m", "partial ship", "--no-push", "--skip-hunk", refs[0], "f.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}

	if got := mustRun(t, dir, "jj", "file", "show", "-r", "@-", "--", "sub/f.txt"); got != "a\nb\nc\nd\nE\n" {
		t.Errorf("committed (@-:sub/f.txt) = %q, want %q", got, "a\nb\nc\nd\nE\n")
	}
	if got := mustRun(t, dir, "jj", "file", "show", "-r", "@", "--", "sub/f.txt"); got != current {
		t.Errorf("remainder (@:sub/f.txt) = %q, want %q (the skipped hunk stays in the working copy)", got, current)
	}
	if got := readFileStr(t, "f.txt"); got != current {
		t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, current)
	}
	if after := statOf(t, "f.txt"); !after.equal(before) {
		t.Errorf("worktree stat changed: before=%+v after=%+v", before, after)
	}
}

// runGit runs git in dir and returns its stdout and error, for probes whose
// failure is the assertion.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) //nolint:gosec // fixed argv; dir is a TempDir, args are literals
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// statusSet returns the porcelain status lines as a set.
func statusSet(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := mustRun(t, dir, "git", "status", "--porcelain")
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set
}

func mapEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// requireLiveGT skips a live gt test when gt is off PATH, and isolates HOME and
// XDG_CONFIG_HOME so gt's own config lives under a fresh TempDir instead of the
// operator's real ~/.graphite. init/create/modify/state/track/restack all run
// unauthenticated against that isolated config; sync and submit demand auth and
// must never be invoked from a live test.
func requireLiveGT(t *testing.T) {
	t.Helper()
	requireLiveVCS(t, "git", "gt")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
}

// setupLiveGTRepo stands up a config-isolated git repo with a graphite trunk
// tracked on main, chdirs into it, and returns its path.
func setupLiveGTRepo(t *testing.T, base string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv(envClaudeSessionKey, "")

	mustRun(t, dir, "git", "init", "-q", "-b", "main")
	mustRun(t, dir, "git", "config", "user.email", "t@t.t")
	mustRun(t, dir, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(base), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write base: %v", err)
	}
	mustRun(t, dir, "git", "add", "f.txt")
	mustRun(t, dir, "git", "commit", "-qm", "init")
	mustRun(t, dir, "gt", "init", "--trunk", "main", "--no-interactive")
	t.Chdir(dir)
	return dir
}

// TestShipLiveGT exercises the gt lane against the real gt and git binaries: a
// path-scoped ship on trunk auto-creates and checks out a stacked branch,
// commits only the scoped file (an untracked sibling stays untouched), and gt
// state tracks the new branch with parent main; a second path-scoped ship on
// that branch appends a second commit via gt modify -c.
func TestShipLiveGT(t *testing.T) {
	requireLiveGT(t)
	dir := setupLiveGTRepo(t, "base\n")

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("scoped change\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write scoped change: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "excluded.txt"), []byte("excluded\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write excluded file: %v", err)
	}

	if _, err := runShipCmd(t, "-m", "first stacked commit", "--no-push", "f.txt"); err != nil {
		t.Fatalf("first ship error = %v", err)
	}

	branch := strings.TrimSpace(mustRun(t, dir, "git", "branch", "--show-current"))
	if branch == "main" {
		t.Fatal("gt create did not check out a new branch")
	}
	if got := mustRun(t, dir, "git", "show", "HEAD:f.txt"); got != "scoped change\n" {
		t.Errorf("HEAD:f.txt = %q, want %q", got, "scoped change\n")
	}
	if _, err := runGit(dir, "show", "HEAD:excluded.txt"); err == nil {
		t.Error("excluded.txt must not be committed, but HEAD:excluded.txt resolved")
	}
	status := statusSet(t, dir)
	if !status["?? excluded.txt"] {
		t.Errorf("git status --porcelain = %v, want excluded.txt untracked", status)
	}

	stateOut := mustRun(t, dir, "gt", "state")
	var state gtState
	if err := json.Unmarshal([]byte(stateOut), &state); err != nil {
		t.Fatalf("parse gt state: %v", err)
	}
	entry, ok := state[branch]
	if !ok {
		t.Fatalf("gt state has no entry for %s: %s", branch, stateOut)
	}
	if len(entry.Parents) != 1 || entry.Parents[0].Ref != "main" {
		t.Errorf("gt state parents for %s = %+v, want a single main parent", branch, entry.Parents)
	}

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("second change\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write second change: %v", err)
	}
	if _, err := runShipCmd(t, "-m", "second stacked commit", "--no-push", "f.txt"); err != nil {
		t.Fatalf("second ship error = %v", err)
	}
	if got := strings.TrimSpace(mustRun(t, dir, "git", "branch", "--show-current")); got != branch {
		t.Fatalf("second ship switched branches: now on %q, want %q", got, branch)
	}
	log := mustRun(t, dir, "git", "log", "--oneline", "main.."+branch)
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("commit count on %s = %d, want 2 (gt modify -c appends): %v", branch, len(lines), lines)
	}
}
