package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// shipLockHeldAt is the fixed mtime every index-lock fixture is stamped with, so
// the refusal's rendering of it is asserted against a specific instant.
var shipLockHeldAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// shipCheckoutFixture builds a repository whose main working copy is <base>/main
// and, when linked, a git worktree at <base>/wt attached to it through the same
// gitdir and commondir pointer files git writes. Every path returned is
// symlink-free, the form ResolveCheckout reports.
func shipCheckoutFixture(t *testing.T, linked bool) (root, gitDir, commonDir string) {
	t.Helper()
	base := shipCanonical(t, t.TempDir())
	commonDir = filepath.Join(base, "main", ".git")
	if err := os.MkdirAll(commonDir, 0o750); err != nil {
		t.Fatalf("mkdir common dir: %v", err)
	}
	if !linked {
		return filepath.Dir(commonDir), commonDir, commonDir
	}
	gitDir = filepath.Join(commonDir, "worktrees", "wt")
	if err := os.MkdirAll(gitDir, 0o750); err != nil {
		t.Fatalf("mkdir worktree admin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	root = filepath.Join(base, "wt")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatalf("mkdir worktree root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatalf("write gitdir pointer: %v", err)
	}
	return root, gitDir, commonDir
}

func shipCanonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", path, err)
	}
	return resolved
}

func shipWriteIndexLock(t *testing.T, dir string) string {
	t.Helper()
	lock := filepath.Join(dir, "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatalf("write index lock: %v", err)
	}
	if err := os.Chtimes(lock, shipLockHeldAt, shipLockHeldAt); err != nil {
		t.Fatalf("stamp index lock: %v", err)
	}
	return lock
}

func shipLockRefusal(lock string) string {
	return fmt.Sprintf(
		"ship: hooks: another process holds the git index lock: %s (held since %s) — wait for it to finish, or delete the lock if the process that took it is gone",
		lock, shipLockHeldAt.Local().Format(time.RFC3339))
}

// TestShipRefuseIndexLock pins which lock a commit contends over: sibling
// worktrees share one common dir but each keeps its own index there, so the
// per-worktree admin dir's lock refuses and the common dir's does not.
func TestShipRefuseIndexLock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		linked bool
		// lockIn selects which admin dir gets the lock: "gitdir", "commondir", or
		// "" for an unlocked repository.
		lockIn  string
		wantErr bool
	}{
		{name: "main checkout unlocked", linked: false, lockIn: ""},
		{name: "main checkout locked", linked: false, lockIn: "gitdir", wantErr: true},
		{name: "linked worktree unlocked", linked: true, lockIn: ""},
		{name: "linked worktree locked", linked: true, lockIn: "gitdir", wantErr: true},
		{name: "linked worktree with only a common dir lock", linked: true, lockIn: "commondir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, gitDir, commonDir := shipCheckoutFixture(t, tt.linked)
			var lock string
			switch tt.lockIn {
			case "gitdir":
				lock = shipWriteIndexLock(t, gitDir)
			case "commondir":
				shipWriteIndexLock(t, commonDir)
			}

			err := shipRefuseIndexLock(render.Dir(root))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("shipRefuseIndexLock() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("shipRefuseIndexLock() = nil, want a refusal naming %s", lock)
			}
			if got, want := err.Error(), shipLockRefusal(lock); got != want {
				t.Errorf("refusal = %q, want %q", got, want)
			}
		})
	}
}

// TestShipRefuseIndexLockPassesJJWithoutGit proves a jj working copy with no git
// backing is passed rather than probed: it has no admin dir, so the lock path
// would collapse to a bare "index.lock" resolved against the process's own
// directory, and any file by that name there would refuse every ship.
func TestShipRefuseIndexLockPassesJJWithoutGit(t *testing.T) {
	root := shipCanonical(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(root, ".jj"), 0o750); err != nil {
		t.Fatalf("mkdir .jj: %v", err)
	}
	shipWriteIndexLock(t, root)
	t.Chdir(root)

	if err := shipRefuseIndexLock(render.Dir(root)); err != nil {
		t.Errorf("shipRefuseIndexLock() = %v, want nil — a jj repo with no git has no index", err)
	}
}

// TestShipHooksRefuseHeldIndexLock proves a lock a sibling session holds costs
// nothing: the ship refuses naming the lock, no hook runs, and nothing moves.
//
// shipRefuseIndexLock is not what refuses here, and on a real repository it
// cannot be: every lane reads its changed set through a tool that wants the
// index first — git add -A on the git lane, jj diff's HEAD reset on the
// colocated jj one — so a lock already held stops the run before shipRunHooks
// reaches its own check. That check narrows the race window against a lock
// taken mid-run; TestShipRefuseIndexLock covers it directly.
func TestShipHooksRefuseHeldIndexLock(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		jj   bool
	}{{name: "git"}, {name: "jj", jj: true}} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, shipOptsFor(tt.jj, vcstest.Remote())...)
			shipHookRepo(t, f, shipKind(tt.jj), 0, "", "f1.go")
			head := shipHead(t, f)
			shipResetLog(t, f)
			lock := shipWriteIndexLock(t, filepath.Join(f.Dir, ".git"))

			_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatalf("ship error = nil, want a refusal naming %s", lock)
			}
			if !strings.Contains(err.Error(), lock) {
				t.Errorf("ship error = %q, want it to name %s", err, lock)
			}
			invocations := vcstest.Invocations(t, f.ArgvLog)
			for _, inv := range invocations {
				if inv[0] == "uvx" {
					t.Errorf("uvx invoked while the index lock was held: %v", inv)
				}
			}
			assertNoShipMutation(t, invocations)
			if got := shipHead(t, f); got != head {
				t.Errorf("HEAD moved to %s, want the pre-ship %s", got, head)
			}
		})
	}
}

// TestShipHooksScrubGitEnv asserts on the environment prek's child actually
// receives: GIT_DIR and GIT_WORK_TREE would otherwise point a hook at whichever
// checkout invoked ccx, which from a linked worktree is the wrong one.
func TestShipHooksScrubGitEnv(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")

	dump := filepath.Join(t.TempDir(), "hook.env")
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx")+`{
  printf 'GIT_DIR=%s\n' "${GIT_DIR-<unset>}"
  printf 'GIT_WORK_TREE=%s\n' "${GIT_WORK_TREE-<unset>}"
  printf 'SHIP_HOOK_CONTROL=%s\n' "${SHIP_HOOK_CONTROL-<unset>}"
} > "$SHIP_HOOK_ENV_DUMP"
exit 0
`)
	t.Setenv("SHIP_HOOK_ENV_DUMP", dump)
	t.Setenv("SHIP_HOOK_CONTROL", "kept")
	// Naming this checkout rather than another one keeps ship's own git calls
	// working; the scrub is what the dump proves either way.
	t.Setenv("GIT_DIR", filepath.Join(f.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", f.Dir)

	if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "GIT_DIR=<unset>\nGIT_WORK_TREE=<unset>\nSHIP_HOOK_CONTROL=kept\n"
	if got := readFileStr(t, dump); got != want {
		t.Errorf("hook child env = %q, want %q", got, want)
	}
	if committed := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); committed != "f1.go" {
		t.Errorf("commit holds %q, want the file the hook ran over", committed)
	}
}

// writeShipLoudUvx installs a uvx that prints on every run, not only a failing
// one. writeShipUvx speaks only when it fails, so a test driving it cannot tell
// a pass that was discarded from a pass that had nothing to say — the whole
// distinction the streaming seam draws.
func writeShipLoudUvx(t *testing.T, f *vcstest.Fixture, n int) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "prek.marker")
	if err := os.WriteFile(marker, []byte(strconv.Itoa(n)), 0o600); err != nil {
		t.Fatalf("write prek marker: %v", err)
	}
	f.Setenv("SHIP_PREK_MARKER", marker)
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx")+`printf 'prek: checking every hook\n'
count=$(cat "$SHIP_PREK_MARKER")
if [ "$count" -gt 0 ]; then
  printf '%s' "$((count - 1))" > "$SHIP_PREK_MARKER"
  printf 'files were modified by this hook\n' >&2
  exit 1
fi
exit 0
`)
}

// TestShipHooksStreamingSeam pins what the single prek pass shows the caller.
// A clean pass streams on a terminal and stays silent in a capture. A pass that
// reports something is surfaced either way — streamed live on a terminal, and
// replayed behind the findings lead-in off one — because ship commits regardless
// and this is the only place a finding is ever shown.
func TestShipHooksStreamingSeam(t *testing.T) {
	for _, tt := range []struct {
		name        string
		stream      bool
		fail        int
		wantSeg     string
		wantMarkers int
		wantLead    bool
		wantRuns    int
	}{
		{"a clean pass streams on a terminal", true, 0, "hooks ok", 1, false, 1},
		{"a clean pass stays out of a capture", false, 0, "hooks ok", 0, false, 1},
		{"a finding streams once on a terminal, with no replay", true, 1, "hooks failed", 1, false, 1},
		{"a finding is replayed into a capture behind the lead-in", false, 1, "hooks failed", 1, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")
			writeShipLoudUvx(t, f, tt.fail)
			shipResetLog(t, f)

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
			}
			if !strings.Contains(out, tt.wantSeg) {
				t.Errorf("summary = %q, want it to carry %q", out, tt.wantSeg)
			}
			if got := strings.Count(errStr, "prek: checking every hook"); got != tt.wantMarkers {
				t.Errorf("prek passes on stderr = %d, want %d (stderr=%q)", got, tt.wantMarkers, errStr)
			}
			if got := strings.Contains(errStr, shipHookFindingsLead); got != tt.wantLead {
				t.Errorf("findings lead-in present = %v, want %v (stderr=%q)", got, tt.wantLead, errStr)
			}
			if got := len(shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx")); got != tt.wantRuns {
				t.Errorf("uvx runs = %d, want %d", got, tt.wantRuns)
			}
		})
	}
}

func TestShipHooksSummaryNamesEachFailedHook(t *testing.T) {
	t.Parallel()
	f := shipRepo(t, vcstest.Remote())
	report := `printf '%s\n' 'Check files with Biome....Failed' '- hook id: biome' '- exit code: 1' '' '  a finding' 'Format YAML.....Failed' '- hook id: yamlfmt' '- exit code: 1' 'Warn on ports....Passed' '- hook id: no-released-port' '- duration: 0.02s'`
	shipHookRepo(t, f, vcs.Git, 1, report, "f1.go")

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "hooks failed: biome, yamlfmt ") {
		t.Errorf("summary = %q, want it to name both failed hooks and not the one that passed", out)
	}
	if !strings.Contains(errStr, "a finding") {
		t.Errorf("stderr = %q, want the hook's output", errStr)
	}
}

func TestShipHookPassLocksMiseOnlyInARepositoryWithALockfile(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		lockfile     bool
		effect       string
		wantUnlocked bool
	}{
		{"a tool install leaves a committed lockfile out of the commit", true, `[ -n "$MISE_LOCKED" ] || echo rewritten >> mise.lock`, false},
		{"a repository with no lockfile can still install its tools", false, `[ -n "$MISE_LOCKED" ] || echo seen > unlocked`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipRepo(t, vcstest.Remote())
			if tt.lockfile {
				writeShipFile(t, f.Dir, "mise.lock", "pinned\n")
			}
			shipHookRepo(t, f, vcs.Git, 1, tt.effect, "f1.go")

			if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "f1.go"); err != nil {
				t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
			}
			if got := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); got != "f1.go" {
				t.Errorf("HEAD touched %q, want only f1.go", got)
			}
			if tt.lockfile {
				if got := gitAt(t, f.Env(), f.Dir, "status", "--short", "--", "mise.lock"); got != "" {
					t.Errorf("mise.lock status = %q, want it untouched", got)
				}
			}
			if _, err := os.Stat(filepath.Join(f.Dir, "unlocked")); (err == nil) != tt.wantUnlocked {
				t.Errorf("hook ran unlocked = %v, want %v", err == nil, tt.wantUnlocked)
			}
		})
	}
}

const shipLintConfig = "repos:\n- repo: local\n  hooks:\n  - id: golangci-lint\n    entry: golangci-lint run\n"

func shipHookFailure(id string, exit int, output string) string {
	return fmt.Sprintf("printf '%%s\\n' '- hook id: %s' '- exit code: %d' '' '  %s'\nexit 1", id, exit, output)
}

func shipLintFailure(exit int, output string) string {
	return shipHookFailure("golangci-lint", exit, output)
}

type shipWriteFunc func([]byte) (int, error)

func (f shipWriteFunc) Write(p []byte) (int, error) { return f(p) }

// shipLintRepo commits a prek config that runs golangci-lint and leaves name
// pending, with uvx scripted through passes in order, the last one repeating.
func shipLintRepo(t *testing.T, name string, passes ...string) *vcstest.Fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	delay := shipHookLintRetryDelay
	t.Cleanup(func() { shipHookLintRetryDelay = delay })
	shipHookLintRetryDelay = time.Millisecond

	f := shipRepo(t, vcstest.Remote())
	f.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	writeShipFile(t, f.Dir, ".pre-commit-config.yaml", shipLintConfig)
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "hooks")
	writeShipFile(t, f.Dir, name, "x")

	marker := filepath.Join(t.TempDir(), "prek.marker")
	if err := os.WriteFile(marker, []byte("0"), 0o600); err != nil {
		t.Fatalf("write prek marker: %v", err)
	}
	f.Setenv("SHIP_PREK_MARKER", marker)
	var steps strings.Builder
	for i, pass := range passes {
		label := strconv.Itoa(i)
		if i == len(passes)-1 {
			label = "*"
		}
		fmt.Fprintf(&steps, "%s)\n%s\n;;\n", label, pass)
	}
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx")+`n=$(cat "$SHIP_PREK_MARKER")
printf '%s' "$((n + 1))" > "$SHIP_PREK_MARKER"
case "$n" in
`+steps.String()+`esac
`)
	shipResetLog(t, f)
	return f
}

func shipUvxRuns(t *testing.T, f *vcstest.Fixture) int {
	t.Helper()
	return len(shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx"))
}

// TestShipHooksLintVerdict pins which golangci-lint outcomes let the commit
// proceed. A finding is a verdict CI can grade; a lint that never ran is not.
func TestShipHooksLintVerdict(t *testing.T) {
	busy := shipLintFailure(3, "Error: "+shipHookLintBusy)
	for _, tt := range []struct {
		name     string
		passes   []string
		wantSeg  string
		wantErr  string
		wantOut  string
		wantRuns int
	}{
		{name: "a finding is a verdict, so the commit proceeds", passes: []string{shipLintFailure(1, "f1.go:1:1: finding")}, wantSeg: "hooks failed", wantOut: "f1.go:1:1: finding", wantRuns: 1},
		{name: "another hook's finding is a verdict too", passes: []string{shipHookFailure("biome", 1, "a finding")}, wantSeg: "hooks failed", wantOut: "a finding", wantRuns: 1},
		{name: "a busy lock is retried until the lint runs", passes: []string{busy, "exit 0"}, wantSeg: "hooks ok", wantOut: "attempt 1 of 6", wantRuns: 2},
		{name: "a lock that stays busy commits nothing", passes: []string{busy}, wantErr: "held its lock through 6 attempts", wantOut: shipHookLintBusy, wantRuns: shipHookLintAttempts},
		{name: "a timeout commits nothing", passes: []string{shipLintFailure(4, "Timeout exceeded")}, wantErr: "its hook exited 4", wantOut: "Timeout exceeded", wantRuns: 1},
		{name: "a load failure commits nothing", passes: []string{shipLintFailure(3, "context loading failed")}, wantErr: "its hook exited 3", wantOut: "context loading failed", wantRuns: 1},
		{name: "a prek that reached no hook commits nothing", passes: []string{"printf 'error: Failed to parse config\\n'\nexit 2"}, wantErr: "prek reported no hook", wantOut: "Failed to parse config", wantRuns: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipLintRepo(t, "f1.go", tt.passes...)
			head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")

			out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			if !strings.Contains(errStr, tt.wantOut) {
				t.Errorf("stderr = %q, want it to carry %q", errStr, tt.wantOut)
			}
			if got := shipUvxRuns(t, f); got != tt.wantRuns {
				t.Errorf("uvx runs = %d, want %d", got, tt.wantRuns)
			}
			if tt.wantErr != "" {
				if !errors.Is(err, errShipHookLintNoVerdict) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ship error = %v, want a no-verdict refusal carrying %q", err, tt.wantErr)
				}
				if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got != head {
					t.Errorf("HEAD moved to %s, want it left at %s", got, head)
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
			}
			if !strings.Contains(out, tt.wantSeg) {
				t.Errorf("summary = %q, want it to carry %q", out, tt.wantSeg)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got == head {
				t.Errorf("HEAD stayed at %s, want a new commit", got)
			}
		})
	}
}

// TestShipHooksLintRetryKeepsFixes pins that a fix made by a pass whose lint
// could not run is committed even though the retry that follows comes back clean.
func TestShipHooksLintRetryKeepsFixes(t *testing.T) {
	f := shipLintRepo(t, "f1.go", "printf fixed > f1.go\n"+shipLintFailure(3, "Error: "+shipHookLintBusy), "exit 0")

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "hooks fixed") {
		t.Errorf("summary = %q, want it to carry %q", out, "hooks fixed")
	}
	if strings.Contains(errStr, shipHookFindingsLead) {
		t.Errorf("stderr = %q, want no findings lead-in for a clean last pass", errStr)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "HEAD:f1.go"); got != "fixed" {
		t.Errorf("committed f1.go = %q, want the fix", got)
	}
}

// TestShipHooksLintRetryAdoptsOtherFiles pins that a retry whose clean pass
// leaves the pending file alone still stages a fix made elsewhere in the module
// by the attempt before it, as a whole-module hook does.
func TestShipHooksLintRetryAdoptsOtherFiles(t *testing.T) {
	f := shipLintRepo(t, "f1.go", "printf fixed > f2.go\n"+shipLintFailure(3, "Error: "+shipHookLintBusy), "exit 0")

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "hooks fixed") {
		t.Errorf("summary = %q, want it to carry %q", out, "hooks fixed")
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "HEAD:f2.go"); got != "fixed" {
		t.Errorf("committed f2.go = %q, want the fix", got)
	}
}

// TestShipHooksLintLock pins the machine-wide queue: a pass that runs the lint
// waits for the one in front of it, gives up with nothing committed when that
// one never finishes, and a commit with no Go file never joins the queue.
func TestShipHooksLintLock(t *testing.T) {
	hold := func(t *testing.T) (release func()) {
		t.Helper()
		dir, err := cache.UserDir(shipHookLockDir)
		if err != nil {
			t.Fatalf("lock dir: %v", err)
		}
		held, freed := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- cache.WithLock(context.Background(), dir, shipHookLintLock, func() error {
				close(held)
				<-freed
				return nil
			})
		}()
		<-held
		var once sync.Once
		release = func() {
			once.Do(func() {
				close(freed)
				if err := <-done; err != nil {
					t.Errorf("holder: %v", err)
				}
			})
		}
		t.Cleanup(release)
		return release
	}
	const waiting = "another ship is running golangci-lint"

	t.Run("a pass waits for the lint in front of it", func(t *testing.T) {
		f := shipLintRepo(t, "f1.go", "exit 0")
		release := hold(t)
		announced := false
		errW := shipWriteFunc(func(p []byte) (int, error) {
			if strings.Contains(string(p), waiting) {
				announced = true
				release()
			}
			return len(p), nil
		})

		var buf bytes.Buffer
		if _, err := shipRunLintPass(f.Context(), errW, render.Dir(f.Dir), []string{"prek", "run"}, &buf, &buf); err != nil {
			t.Fatalf("lint pass error = %v", err)
		}
		if !announced {
			t.Error("lint pass ran without announcing the wait")
		}
		if got := shipUvxRuns(t, f); got != 1 {
			t.Errorf("uvx runs = %d, want 1", got)
		}
	})

	t.Run("a holder that never finishes commits nothing", func(t *testing.T) {
		f := shipLintRepo(t, "f1.go", "exit 0")
		head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
		hold(t)
		wait := shipHookLintWait
		t.Cleanup(func() { shipHookLintWait = wait })
		shipHookLintWait = 10 * time.Millisecond

		_, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
		if !errors.Is(err, errShipHookLintNoVerdict) {
			t.Fatalf("ship error = %v (stderr=%q), want a no-verdict refusal", err, errStr)
		}
		if !strings.Contains(errStr, waiting) {
			t.Errorf("stderr = %q, want it to announce the wait", errStr)
		}
		if got := shipUvxRuns(t, f); got != 0 {
			t.Errorf("uvx runs = %d, want 0", got)
		}
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got != head {
			t.Errorf("HEAD moved to %s, want it left at %s", got, head)
		}
	})

	t.Run("a commit with no Go file skips the queue", func(t *testing.T) {
		f := shipLintRepo(t, "notes.md", "exit 0")
		hold(t)

		out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "docs: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
		}
		if !strings.Contains(out, "hooks ok") {
			t.Errorf("summary = %q, want it to carry %q", out, "hooks ok")
		}
		if strings.Contains(errStr, waiting) {
			t.Errorf("stderr = %q, want no wait", errStr)
		}
	})
}

func TestShipHookEntryPaths(t *testing.T) {
	t.Parallel()
	yaml := `repos:
- repo: local
  hooks:
  - id: biome
    entry: ./tools/hooks/hook-biome.sh
  - entry: "bash scripts/lint/run.sh --fix"
    id: lint
  - id: gofmt
    entry: gofmt -w
  - id: env
    entry: $HOME/bin/x.sh ../outside/y.sh /abs/z.sh https://example.com/a
`
	if got, want := shipHookEntryPaths([]byte(yaml)), []string{"tools/hooks/hook-biome.sh", "scripts/lint/run.sh"}; !slices.Equal(got, want) {
		t.Errorf("yaml entries = %q, want %q", got, want)
	}
	toml := "[[repos.hooks]]\nid = \"protect\"\nentry = \"./tools/hooks/hook-protect-dev.sh\"\n"
	if got, want := shipHookEntryPaths([]byte(toml)), []string{"tools/hooks/hook-protect-dev.sh"}; !slices.Equal(got, want) {
		t.Errorf("toml entries = %q, want %q", got, want)
	}
}

func TestShipHookIDs(t *testing.T) {
	t.Parallel()
	yaml := `repos:
- repo: local
  hooks:
  - id: biome
    entry: ./tools/hooks/hook-biome.sh
  - entry: gofmt -w
    id: "gofmt"
- repo: https://github.com/pre-commit/pre-commit-hooks
  hooks:
  - id: check-added-large-files # big blobs
`
	if got, want := shipHookIDs([]byte(yaml)), []string{"biome", "gofmt", "check-added-large-files"}; !slices.Equal(got, want) {
		t.Errorf("yaml ids = %q, want %q", got, want)
	}
	toml := "[[repos.hooks]]\nid = \"golangci-lint\"\nentry = \"golangci-lint run\"\n"
	if got, want := shipHookIDs([]byte(toml)), []string{"golangci-lint"}; !slices.Equal(got, want) {
		t.Errorf("toml ids = %q, want %q", got, want)
	}
}

func TestShipMaterializeHookEntriesWidensASparseLane(t *testing.T) {
	t.Parallel()
	f := vcstest.Repo(t)
	config := filepath.Join(f.Dir, ".pre-commit-config.yaml")
	files := map[string]string{
		".pre-commit-config.yaml":   "repos:\n- repo: local\n  hooks:\n  - id: biome\n    entry: ./tools/hooks/hook-biome.sh\n",
		"tools/hooks/hook-biome.sh": "#!/bin/sh\n",
		"other/x.txt":               "x\n",
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.Dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "hooks")
	mustRun(t, f.Env(), f.Dir, "git", "sparse-checkout", "set", "--cone")
	hook := filepath.Join(f.Dir, "tools", "hooks", "hook-biome.sh")
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatalf("hook script present before the ship: %v", err)
	}

	if err := shipMaterializeHookEntries(f.Context(), render.Dir(f.Dir), config); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(hook); err != nil {
		t.Errorf("hook script not checked out: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "other")); !os.IsNotExist(err) {
		t.Errorf("other/ checked out: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "sparse-checkout", "list"); got != "tools/hooks" {
		t.Errorf("sparse-checkout list = %q, want tools/hooks", got)
	}
}

func TestShipMaterializeHookEntriesFillsANonConeLane(t *testing.T) {
	t.Parallel()
	f := vcstest.Repo(t)
	config := filepath.Join(f.Dir, ".pre-commit-config.yaml")
	files := map[string]string{
		".pre-commit-config.yaml":   "repos:\n- repo: local\n  hooks:\n  - id: biome\n    entry: ./tools/hooks/hook-biome.sh\n",
		"biome.json":                "{}\n",
		"tools/hooks/hook-biome.sh": "#!/bin/sh\n",
		"tools/ci":                  "#!/bin/sh\n",
		"other/x.txt":               "x\n",
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.Dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", "-A")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "hooks")
	mustRun(t, f.Env(), f.Dir, "git", "sparse-checkout", "set", "--no-cone", "/tools/ci", "/.pre-commit-config.yaml")
	if _, err := os.Stat(filepath.Join(f.Dir, "biome.json")); !os.IsNotExist(err) {
		t.Fatalf("biome.json present before the ship: %v", err)
	}

	if err := shipMaterializeHookEntries(f.Context(), render.Dir(f.Dir), config); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"biome.json", "tools/hooks/hook-biome.sh", "tools/ci"} {
		if _, err := os.Stat(filepath.Join(f.Dir, want)); err != nil {
			t.Errorf("%s not checked out: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "other")); !os.IsNotExist(err) {
		t.Errorf("other/ checked out: %v", err)
	}
}

// TestShipNewBranchRunsHooksOnTheBranchItCuts ships --new-branch from trunk on
// the native stack lane under a hook that refuses a commit on trunk. The lane
// ran prek on main and cut the branch after, so the hook reported a failure on
// every such ship though the commit landed on the new branch.
func TestShipNewBranchRunsHooksOnTheBranchItCuts(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
		t.Fatal(err)
	}
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx")+`if [ "$(git symbolic-ref --short HEAD)" = main ]; then
  printf 'Direct commits to main are not allowed\n' >&2
  exit 1
fi
exit 0
`)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--new-branch", "feat", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "hooks ok") || strings.Contains(errStr, "Direct commits to main") {
		t.Errorf("summary = %q, stderr = %q, want the hooks passing on feat", out, errStr)
	}
	if branch := gitAt(t, f.Env(), f.Dir, "symbolic-ref", "--short", "HEAD"); branch != "feat" {
		t.Errorf("working copy on %q, want feat", branch)
	}
	if committed := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "feat"); committed != "f1.go" {
		t.Errorf("feat's commit holds %q, want the file the hooks ran over", committed)
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if s := state["feat"]; len(s.Parents) != 1 || s.Parents[0].Ref != "main" {
		t.Errorf("feat = %+v, want it recorded on main", s)
	}
}

func TestShipHooksStopOnAPublicHook(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		id          string
		gt          bool
		wantRefused bool
	}{
		{"a teammate's name in the diff commits nothing", "public-names", false, true},
		{"a teammate's name in the diff commits nothing on the graphite lane", "public-names", true, true},
		{"another hook's finding still commits", "biome", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var f *vcstest.Fixture
			if tt.gt {
				f = shipGTRepo(t, vcstest.GTStack("base"))
			} else {
				f = shipRepo(t, vcstest.Remote())
			}
			report := `printf '%s\n' 'Check....Failed' '- hook id: ` + tt.id + `' '- exit code: 1' '' '  public-check: f1.go:1 names Rowan (Rowan Vale)'`
			shipHookRepo(t, f, vcs.Git, 1, report, "f1.go")
			head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")

			_, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			if !strings.Contains(errStr, "names Rowan (Rowan Vale)") {
				t.Errorf("stderr = %q, want the hook's finding", errStr)
			}
			committed := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD") != head
			if !tt.wantRefused {
				if err != nil || !committed {
					t.Fatalf("ship error = %v, committed = %v, want a commit (stderr=%q)", err, committed, errStr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "public-names refused what this commit would make public") {
				t.Fatalf("ship error = %v, want the public-names refusal", err)
			}
			if committed {
				t.Errorf("HEAD moved off %s, want nothing committed", head)
			}
			if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "?? f1.go" {
				t.Errorf("status = %q, want the index as ship found it, f1.go unstaged", status)
			}

			writeShipFile(t, f.Dir, "f1.go", "reworded")
			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
				t.Fatalf("ship after the rewording = %v", err)
			}
			if body := gitAt(t, f.Env(), f.Dir, "show", "HEAD:f1.go"); body != "reworded" {
				t.Errorf("committed f1.go = %q, want the reworded copy", body)
			}
			if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "" {
				t.Errorf("status = %q, want nothing left uncommitted", status)
			}
		})
	}
}

func TestShipHookRefusalKeepsWhatYouStagedOfANamedPath(t *testing.T) {
	t.Parallel()
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 1, `printf '%s\n' 'Check....Failed' '- hook id: public-names' '- exit code: 1' '' '  public-check: f1.go:1 names Rowan (Rowan Vale)'`)
	writeShipFile(t, f.Dir, "f1.go", "base")
	mustRun(t, f.Env(), f.Dir, "git", "add", "f1.go")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "base")
	writeShipFile(t, f.Dir, "f1.go", "staged")
	mustRun(t, f.Env(), f.Dir, "git", "add", "f1.go")
	writeShipFile(t, f.Dir, "f1.go", "working")
	writeShipFile(t, f.Dir, "f2.go", "new")

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "f1.go", "f2.go")
	if err == nil || !strings.Contains(err.Error(), "public-names refused what this commit would make public") {
		t.Fatalf("ship error = %v, want the public-names refusal", err)
	}
	if staged := gitAt(t, f.Env(), f.Dir, "show", ":f1.go"); staged != "staged" {
		t.Errorf("index f1.go = %q, want the copy staged before the ship", staged)
	}
	if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "MM f1.go\n?? f2.go" {
		t.Errorf("status = %q, want f1.go staged as before and f2.go untracked", status)
	}
}

func TestShipCheckPRBodies(t *testing.T) {
	t.Parallel()
	public := "repos:\n- repo: https://github.com/yasyf/cc-skills\n  rev: abc\n  hooks:\n  - id: public-names\n  - id: public-pr-body\n"
	for _, tt := range []struct {
		name        string
		config      string
		noVerify    bool
		fail        int
		wantRefused bool
		wantRuns    int
	}{
		{"a body naming a teammate stops the ship", public, false, 1, true, 1},
		{"a clean body passes", public, false, 0, false, 1},
		{"a repository without the hook ships as before", "repos: []\n", false, 1, false, 0},
		{"--no-verify skips it as it skips every hook", public, true, 1, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipRepo(t, vcstest.Remote())
			writeShipFile(t, f.Dir, ".pre-commit-config.yaml", tt.config)
			writeShipUvx(t, f, tt.fail, `printf '%s\n' 'Public body....Failed' '- hook id: public-pr-body' '' '  public-check: body.md: names Rowan (Rowan Vale)'`)
			shipResetLog(t, f)
			bodies := t.TempDir()
			writeShipFile(t, bodies, "body.md", "Rowan asked.\n")
			body := filepath.Join(bodies, "body.md")

			var errW bytes.Buffer
			err := shipCheckPRBodies(f.Context(), &errW, render.Dir(f.Dir), shipOpts{prBodyFile: []string{"feature=" + body}, noVerify: tt.noVerify})
			runs := shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx")
			if len(runs) != tt.wantRuns {
				t.Fatalf("uvx runs = %v, want %d", runs, tt.wantRuns)
			}
			if want := []string{"uvx", "prek", "run", "public-pr-body", "--hook-stage", "manual", "--cd", f.Dir, "--files", body}; tt.wantRuns > 0 && !slices.Equal(runs[0], want) {
				t.Errorf("uvx argv = %q, want %q", runs[0], want)
			}
			if (err != nil) != tt.wantRefused {
				t.Fatalf("error = %v, want refused = %v", err, tt.wantRefused)
			}
			if tt.wantRefused && !strings.Contains(errW.String(), "names Rowan (Rowan Vale)") {
				t.Errorf("stderr = %q, want the hook's finding", errW.String())
			}
		})
	}
}
