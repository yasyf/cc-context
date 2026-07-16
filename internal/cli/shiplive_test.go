package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
