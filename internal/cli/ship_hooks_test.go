package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

			err := shipRefuseIndexLock(root)
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

	if err := shipRefuseIndexLock(root); err != nil {
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

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
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
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+shipRecordArgv("uvx", f.ArgvLog)+`{
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

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "GIT_DIR=<unset>\nGIT_WORK_TREE=<unset>\nSHIP_HOOK_CONTROL=kept\n"
	if got := readFileStr(t, dump); got != want {
		t.Errorf("hook child env = %q, want %q", got, want)
	}
	if committed := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); committed != "f1.go" {
		t.Errorf("commit holds %q, want the file the hook ran over", committed)
	}
}

func TestShipHookEnvKeepsEverythingElse(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/wrong/.git")
	t.Setenv("GIT_WORK_TREE", "/tmp/wrong")
	t.Setenv("GIT_INDEX_FILE", "/tmp/wrong/index")

	var sawIndexFile bool
	for _, entry := range shipHookEnv() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE":
			t.Errorf("shipHookEnv() carries %s", entry)
		case "GIT_INDEX_FILE":
			sawIndexFile = true
		}
	}
	if !sawIndexFile {
		t.Error("shipHookEnv() dropped GIT_INDEX_FILE; only GIT_DIR and GIT_WORK_TREE are scrubbed")
	}
}
