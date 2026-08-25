package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx", f.ArgvLog)+`{
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
	t.Setenv("SHIP_PREK_MARKER", marker)
	writeShipExecutable(t, f.ShimBin, "uvx", "#!/bin/sh\n"+vcstest.RecordArgv("uvx", f.ArgvLog)+`printf 'prek: checking every hook\n'
count=$(cat "$SHIP_PREK_MARKER")
if [ "$count" -gt 0 ]; then
  printf '%s' "$((count - 1))" > "$SHIP_PREK_MARKER"
  printf 'files were modified by this hook\n' >&2
  exit 1
fi
exit 0
`)
}

// TestShipHooksStreamingSeam pins which prek pass reaches the caller's stderr.
// The retry pass is the verdict and always reports; the first pass reports only
// on a terminal, where a human watches both go by in order. Off one the only
// reader is a capture, which would take the first pass's pre-fix output for a
// verdict the retry may already have overturned.
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
		{"an auto-fix streams both passes on a terminal", true, 1, "hooks fixed", 2, true, 2},
		{"an auto-fix shows a capture only the verdict pass", false, 1, "hooks fixed", 1, false, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")
			writeShipLoudUvx(t, f, tt.fail)
			shipResetLog(t, f)

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			out, errStr, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
			}
			if !strings.Contains(out, tt.wantSeg) {
				t.Errorf("summary = %q, want it to carry %q", out, tt.wantSeg)
			}
			if got := strings.Count(errStr, "prek: checking every hook"); got != tt.wantMarkers {
				t.Errorf("prek passes on stderr = %d, want %d (stderr=%q)", got, tt.wantMarkers, errStr)
			}
			if got := strings.Contains(errStr, shipHookRetryLead); got != tt.wantLead {
				t.Errorf("retry lead-in present = %v, want %v (stderr=%q)", got, tt.wantLead, errStr)
			}
			if got := len(shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx")); got != tt.wantRuns {
				t.Errorf("uvx runs = %d, want %d", got, tt.wantRuns)
			}
		})
	}
}
