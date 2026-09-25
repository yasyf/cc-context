package vcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// trunkStateRepo builds the shape this whole check is about: a main checkout
// holding trunk, and a linked worktree standing in for a lane's.
func trunkStateRepo(t *testing.T) (f *vcstest.Fixture, lane string, trunk Trunk) {
	t.Helper()
	f = vcstest.Repo(t, vcstest.Remote(), vcstest.Worktree("lane"))
	resolved, err := ResolveTrunk(f.Context(), render.Dir(f.Dir), "origin")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	return f, f.WorktreePath("lane"), resolved
}

// advanceRemoteTrunk lands n commits on origin's trunk from the lane, leaving
// the local trunk ref behind by exactly n and the lane back where it stood.
func advanceRemoteTrunk(t *testing.T, f *vcstest.Fixture, lane, trunk string, n int) {
	t.Helper()
	for i := range n {
		runGit(t, f, lane, "commit", "-q", "--allow-empty", "-m", "remote "+strconv.Itoa(i))
	}
	runGit(t, f, lane, "push", "-q", "origin", "HEAD:"+trunk)
	runGit(t, f, lane, "fetch", "-q", "origin")
	runGit(t, f, lane, "reset", "-q", "--hard", "HEAD~"+strconv.Itoa(n))
}

func writeTrunkFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func gitOut(t *testing.T, f *vcstest.Fixture, dir string, args ...string) string {
	t.Helper()
	if dir == f.Dir {
		return f.Out(t, "git", args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), f.Env()...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// signCommitsWithSSH makes repo sign every commit with a throwaway ssh key, so
// a log.showSignature=true run emits real verification output. No allowed
// signers file is written: the unverifiable case is the one that prints the
// lines a per-line parse would read as commits.
func signCommitsWithSSH(t *testing.T, f *vcstest.Fixture, repo string) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "signing")
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "t@t.t", "-f", key) //nolint:gosec // fixed argv over a test TempDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen unavailable: %v\n%s", err, out)
	}
	runGit(t, f, repo, "config", "gpg.format", "ssh")
	runGit(t, f, repo, "config", "user.signingkey", key+".pub")
	runGit(t, f, repo, "config", "commit.gpgsign", "true")
}

func readTrunkStateAt(t *testing.T, f *vcstest.Fixture, dir string, trunk Trunk, holder string) TrunkState {
	t.Helper()
	state, err := ReadTrunkState(f.ContextIn(dir), render.Dir(dir), trunk, holder)
	if err != nil {
		t.Fatalf("ReadTrunkState: %v", err)
	}
	return state
}

// TestReadTrunkStateHealthyIsSilent is the case that must report nothing: a
// trunk equal to the remote's, however many working copies hold it.
func TestReadTrunkStateHealthyIsSilent(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)

	state := readTrunkStateAt(t, f, lane, trunk, f.Dir)
	if !state.Healthy() {
		t.Fatalf("state = %+v, want healthy", state)
	}
	if state.Contaminated() {
		t.Fatalf("Foreign = %v, want none", state.Foreign)
	}
}

// TestReadTrunkStateNamesTheDirtyHolder is the shared-checkout failure: trunk
// held by a working copy carrying somebody's in-progress work, which is why
// nothing has fast-forwarded the ref the whole pool rebases onto.
func TestReadTrunkStateNamesTheDirtyHolder(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	advanceRemoteTrunk(t, f, lane, trunk.Name(), 2)
	writeTrunkFile(t, filepath.Join(repo, "f.txt"), "someone's work\n")
	writeTrunkFile(t, filepath.Join(repo, "untracked.txt"), "more\n")

	state := readTrunkStateAt(t, f, lane, trunk, repo)
	if state.Healthy() {
		t.Fatalf("state = %+v, want unhealthy", state)
	}
	if state.Holder != repo {
		t.Errorf("Holder = %q, want %q", state.Holder, repo)
	}
	if state.Dirty != 2 {
		t.Errorf("Dirty = %d, want 2", state.Dirty)
	}
	if state.Behind != 2 {
		t.Errorf("Behind = %d, want 2", state.Behind)
	}
}

// TestReadTrunkStateNamesTheForeignCommits is the contamination the report
// exists for: a restack onto this ref splices exactly these into every branch.
func TestReadTrunkStateNamesTheForeignCommits(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	runGit(t, f, repo, "commit", "-q", "--allow-empty", "-m", "parked work one")
	runGit(t, f, repo, "commit", "-q", "--allow-empty", "-m", "parked work two")

	state := readTrunkStateAt(t, f, lane, trunk, repo)
	if !state.Contaminated() {
		t.Fatalf("state = %+v, want contaminated", state)
	}
	subjects := make([]string, 0, len(state.Foreign))
	for _, c := range state.Foreign {
		if c.SHA == "" {
			t.Errorf("commit %+v carries no sha", c)
		}
		subjects = append(subjects, c.Subject)
	}
	want := []string{"parked work two", "parked work one"}
	if strings.Join(subjects, "|") != strings.Join(want, "|") {
		t.Errorf("Foreign = %q, want %q newest first", subjects, want)
	}
}

// TestReadTrunkStateUnderASigningConfig pins the commit listing against
// log.showSignature=true, whose verification lines a line-per-commit parse
// reads as commits of their own.
func TestReadTrunkStateUnderASigningConfig(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	signCommitsWithSSH(t, f, repo)
	runGit(t, f, repo, "config", "log.showSignature", "true")
	runGit(t, f, repo, "commit", "-q", "--allow-empty", "-m", "parked work one")

	state := readTrunkStateAt(t, f, lane, trunk, repo)
	if len(state.Foreign) != 1 {
		t.Fatalf("Foreign = %+v, want exactly one commit", state.Foreign)
	}
	if state.Foreign[0].Subject != "parked work one" {
		t.Errorf("Foreign[0] = %+v, want the real commit", state.Foreign[0])
	}
}

// TestReadTrunkStateReportsAHolderWhoseTreeIsGone keeps a prunable
// registration an answer rather than a failure: it still pins the ref, and
// probing the directory it no longer has would abort the whole report.
func TestReadTrunkStateReportsAHolderWhoseTreeIsGone(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	advanceRemoteTrunk(t, f, lane, trunk.Name(), 1)
	gone := filepath.Join(filepath.Dir(repo), "gone")
	runGit(t, f, repo, "worktree", "add", "-q", "--force", "-f", gone, trunk.Name())
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove %s: %v", gone, err)
	}

	state := readTrunkStateAt(t, f, lane, trunk, gone)
	if !state.Stale {
		t.Fatalf("state = %+v, want the holder reported stale", state)
	}
	if state.Dirty != 0 {
		t.Errorf("Dirty = %d, want no dirt read from a tree that is gone", state.Dirty)
	}
	if state.Behind != 1 {
		t.Errorf("Behind = %d, want the ref still measured", state.Behind)
	}
}

// TestReadTrunkStateWithoutALocalTrunkBranch holds the miss to healthy: there
// is no local ref for a restack to pick anything up from.
func TestReadTrunkStateWithoutALocalTrunkBranch(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	runGit(t, f, repo, "checkout", "-q", "--detach")
	runGit(t, f, repo, "branch", "-q", "-D", trunk.Name())

	state := readTrunkStateAt(t, f, lane, trunk, "")
	if !state.Healthy() {
		t.Fatalf("state = %+v, want healthy", state)
	}
	if state.Holder != "" || state.Dirty != 0 {
		t.Errorf("state = %+v, want no holder read at all", state)
	}
}

// TestReadTrunkStateLeavesTheHolderUntouched is the premise of the whole check:
// it reads a working copy somebody else is using and writes nothing into it.
func TestReadTrunkStateLeavesTheHolderUntouched(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	advanceRemoteTrunk(t, f, lane, trunk.Name(), 1)
	writeTrunkFile(t, filepath.Join(repo, "f.txt"), "someone's work\n")
	before := gitOut(t, f, repo, "status", "--porcelain")
	head := gitOut(t, f, repo, "rev-parse", "HEAD")

	readTrunkStateAt(t, f, lane, trunk, repo)

	if after := gitOut(t, f, repo, "status", "--porcelain"); after != before {
		t.Errorf("status = %q, want the %q it was before", after, before)
	}
	if after := gitOut(t, f, repo, "rev-parse", "HEAD"); after != head {
		t.Errorf("HEAD = %q, want the %q it was before", after, head)
	}
	if branch := strings.TrimSpace(gitOut(t, f, repo, "branch", "--show-current")); branch != trunk.Name() {
		t.Errorf("branch = %q, want %q", branch, trunk.Name())
	}
}

// TestGitRefusesToFetchIntoAHeldTrunk pins the premise the report rests on: no
// lane can advance a trunk another working copy holds, so the ref stays stale
// until somebody frees it.
func TestGitRefusesToFetchIntoAHeldTrunk(t *testing.T) {
	f, lane, trunk := trunkStateRepo(t)
	repo := f.Dir
	advanceRemoteTrunk(t, f, lane, trunk.Name(), 1)

	cmd := exec.Command("git", "-C", lane, "fetch", "origin", trunk.Name()+":"+trunk.Name()) //nolint:gosec // fixed git argv over a test TempDir
	cmd.Env = append(os.Environ(), f.Env()...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("fetch into a held %s succeeded:\n%s", trunk.Name(), out)
	}
	if !strings.Contains(string(out), "refusing to fetch into branch") {
		t.Fatalf("fetch failed with %q, want git's held-branch refusal", out)
	}
	if state := readTrunkStateAt(t, f, lane, trunk, repo); state.Behind != 1 {
		t.Fatalf("Behind = %d, want the ref still 1 behind", state.Behind)
	}
}
