package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runRestackCmd(t *testing.T, f *vcstest.Fixture, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRestackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(f.Context())
	return strings.TrimSpace(out.String()), errOut.String(), err
}

// restackRun runs a real tool in dir under f's environment, failing the test
// on a nonzero exit.
func restackRun(t *testing.T, f *vcstest.Fixture, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // bin resolves through the fixture's own shim and args are test-authored
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), f.Env()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", bin, args, dir, err, stderr.String())
	}
	return stdout.String()
}

// restackRev resolves rev to a commit id under f's environment, or "" when it
// names nothing.
func restackRev(t *testing.T, f *vcstest.Fixture, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", rev+"^{commit}") //nolint:gosec // fixed git argv; rev is a test literal and dir a fixture TempDir
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), f.Env()...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func restackWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func restackRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixture's own temp tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// restackAdvanceRemote pushes one commit to the fixture's origin from a clone of
// it, so the fixture's own refs/remotes/<remote>/<trunk> stays behind until
// restack fetches — the state every "the remote moved" case needs, built without
// touching the working copy under test.
func restackAdvanceRemote(t *testing.T, f *vcstest.Fixture, trunk, file, content string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "upstream")
	restackRun(t, f, filepath.Dir(clone), "git", "clone", "-q", "--branch", trunk, f.RemoteDir, clone)
	restackRun(t, f, clone, "git", "config", "user.email", "t@t.t")
	restackRun(t, f, clone, "git", "config", "user.name", "t")
	restackWrite(t, filepath.Join(clone, file), content)
	restackRun(t, f, clone, "git", "add", file)
	restackRun(t, f, clone, "git", "commit", "-qm", "upstream")
	restackRun(t, f, clone, "git", "push", "-q", "origin", trunk)
}

// restackReset drops every argv record the test's own fixture work wrote, so an
// invocation assertion sees only what restack itself ran.
func restackReset(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	f.RotateLog(t)
}

// restackUndesignatedTrunk leaves the repository with no default branch a fetch
// can designate for it: origin/HEAD is unset, and the remote's own HEAD names a
// branch the remote does not have — which is what git 2.44 and later need,
// since they write refs/remotes/origin/HEAD during any fetch that finds the
// answer.
func restackUndesignatedTrunk(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	restackRun(t, f, f.Dir, "git", "--git-dir", f.RemoteDir, "symbolic-ref", "HEAD", "refs/heads/absent")
}

// restackSiblingPath mints a path for a sibling working copy with its symlinks
// resolved, the spelling git reports back out of worktree list and the one every
// summary naming a holder has to match.
func restackSiblingPath(t *testing.T, name string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return filepath.Join(base, name)
}

func restackInvocations(t *testing.T, f *vcstest.Fixture) [][]string {
	t.Helper()
	return vcstest.Invocations(t, f.ArgvLog)
}

// assertNoRestackMutation fails when restack moved a ref or replayed a commit.
// A refusal must leave the working copy exactly as it found it, which the
// surviving HEAD alone cannot prove: a rebase that landed and was aborted also
// ends where it started.
func assertNoRestackMutation(t *testing.T, invocations [][]string) {
	t.Helper()
	for _, inv := range invocations {
		if len(inv) < 2 {
			continue
		}
		switch inv[0] + " " + inv[1] {
		case "git merge", "git rebase", "git commit", "git switch", "git checkout", "jj rebase", "jj commit":
			t.Errorf("repository mutated before restack refused: %v", inv)
		}
	}
}

func TestRestackGitRebasesOntoTrunk(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
	installDropGH(t, f, nil)
	restackWrite(t, filepath.Join(f.Dir, "feature.txt"), "feature\n")
	restackRun(t, f, f.Dir, "git", "add", "feature.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if restackRev(t, f, f.Dir, "refs/remotes/origin/main") == "" {
		t.Fatal("refs/remotes/origin/main missing after a fetch restack claims to have made")
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "upstream.txt")); err != nil {
		t.Errorf("stat upstream.txt: %v — the rebase did not replay onto the fetched trunk", err)
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/main..HEAD")); got != "1" {
		t.Errorf("commits above trunk = %s, want 1 (the feature commit, replayed once)", got)
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "branch", "--show-current")); got != "feature" {
		t.Errorf("branch = %q, want feature", got)
	}
}

// TestRestackGitFetchesOnlyTheRefsItReads configures origin to fetch a branch
// the remote has since deleted, which fails a bare git fetch origin with
// "couldn't find remote ref" before restack reads anything.
func TestRestackGitFetchesOnlyTheRefsItReads(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
	installDropGH(t, f, nil)
	restackWrite(t, filepath.Join(f.Dir, "feature.txt"), "feature\n")
	restackRun(t, f, f.Dir, "git", "add", "feature.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "feature")
	restackRun(t, f, f.Dir, "git", "push", "-q", "origin", "main:gone")
	restackRun(t, f, f.Dir, "git", "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	restackRun(t, f, f.Dir, "git", "config", "--add", "remote.origin.fetch", "+refs/heads/gone:refs/remotes/origin/gone")
	restackRun(t, f, f.Dir, "git", "push", "-q", "origin", "--delete", "gone")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "upstream.txt")); err != nil {
		t.Errorf("stat upstream.txt: %v — the rebase did not replay onto the fetched trunk", err)
	}
}

// TestRestackGitAsksTheRemoteForAnUnsetTrunk leaves refs/remotes/origin/HEAD
// unset while the remote's HEAD names main: restack takes the remote's answer,
// which a fetch of explicit refspecs never records.
func TestRestackGitAsksTheRemoteForAnUnsetTrunk(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.NoOriginHead(), vcstest.Branch("feature"))
	f.Isolate(t)
	installDropGH(t, f, nil)
	restackWrite(t, filepath.Join(f.Dir, "feature.txt"), "feature\n")
	restackRun(t, f, f.Dir, "git", "add", "feature.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "upstream.txt")); err != nil {
		t.Errorf("stat upstream.txt: %v — the rebase did not replay onto the fetched trunk", err)
	}
}

func TestRestackGitFastForwardsTrunk(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	f.Isolate(t)
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · fast-forwarded main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if local, remote := restackRev(t, f, f.Dir, "refs/heads/main"), restackRev(t, f, f.Dir, "refs/remotes/origin/main"); local != remote {
		t.Errorf("main = %s, refs/remotes/origin/main = %s — the fast-forward did not land", local, remote)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "upstream.txt")); got != "upstream\n" {
		t.Errorf("upstream.txt = %q, want the fetched trunk's content", got)
	}
}

func TestRestackGitAlreadyUpToDate(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	f.Isolate(t)
	before := restackRev(t, f, f.Dir, "HEAD")
	restackReset(t, f)

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · already up to date"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD moved from %s to %s on an up-to-date restack", before, after)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

// TestRestackGitTargetsTheQualifiedTrunkRef is the decoy case. git resolves a
// short origin/main through refs/heads before refs/remotes, so a local branch
// literally named origin/main answers merge-base and merge --ff-only in place of
// the remote-tracking ref — measured on git 2.55.0, which warns on stderr and
// exits 0. Here the decoy sits on the commit the working copy already has, so a
// restack that consults it reports "already up to date" and leaves the fetched
// trunk on the floor.
func TestRestackGitTargetsTheQualifiedTrunkRef(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{name: "on trunk", want: "fetched · fast-forwarded main"},
		{name: "on a branch", branch: "feature", want: "fetched · rebased onto main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []vcstest.Opt{vcstest.Remote()}
			if tt.branch != "" {
				opts = append(opts, vcstest.Branch(tt.branch))
			}
			f := shipRepo(t, opts...)
			f.Isolate(t)
			installDropGH(t, f, nil)
			restackRun(t, f, f.Dir, "git", "branch", "origin/main", "HEAD")
			restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			decoy := restackRev(t, f, f.Dir, "refs/heads/origin/main")

			out, _, err := runRestackCmd(t, f)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q — the decoy refs/heads/origin/main answered in place of the remote-tracking ref", out, tt.want)
			}
			if got := restackRead(t, filepath.Join(f.Dir, "upstream.txt")); got != "upstream\n" {
				t.Errorf("upstream.txt = %q, want the fetched trunk's content", got)
			}
			if got := restackRev(t, f, f.Dir, "refs/heads/origin/main"); got != decoy {
				t.Errorf("decoy branch moved from %s to %s — restack wrote to it", decoy, got)
			}
		})
	}
}

// TestRestackGitRefusesUndesignatedTrunk pins the refusal that replaced the old
// main/master guess. A remote added by git remote add sets no origin/HEAD, and
// restack is a mutating command: merging onto a branch nobody designated is a
// wrong target, so it names the one command that designates one instead.
func TestRestackGitRefusesUndesignatedTrunk(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.NoOriginHead())
	f.Isolate(t)
	restackUndesignatedTrunk(t, f)
	before := restackRev(t, f, f.Dir, "HEAD")
	restackReset(t, f)

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded with no designated trunk, want a refusal")
	}
	if !errors.Is(err, vcs.ErrNoTrunk) {
		t.Errorf("error = %v, want it to reach vcs.ErrNoTrunk", err)
	}
	want := "restack: refs/remotes/origin/HEAD: " + vcs.ErrNoTrunk.Error() + " — run git remote set-head origin -a"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD moved from %s to %s on a refusal", before, after)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

// TestRestackGitRefusesTrunkOutsideTheRemote separates a miss from a
// misconfiguration: origin/HEAD may legally be pointed at any ref, and a target
// outside refs/remotes/origin/ is an answer git gave, not an absent one — so it
// must not reach ErrNoTrunk, which callers branch on.
func TestRestackGitRefusesTrunkOutsideTheRemote(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	f.Isolate(t)
	restackRun(t, f, f.Dir, "git", "tag", "v1")
	restackRun(t, f, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/tags/v1")
	restackReset(t, f)

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded with origin/HEAD pointing at a tag, want a refusal")
	}
	if errors.Is(err, vcs.ErrNoTrunk) {
		t.Errorf("error = %v, want a misconfiguration rather than a missing trunk", err)
	}
	if !strings.Contains(err.Error(), "refs/tags/v1") {
		t.Errorf("error = %q, want it to name the ref origin/HEAD points at", err)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

func TestRestackGitRefusesDetachedHEAD(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Detached())
	f.Isolate(t)
	restackReset(t, f)

	_, _, err := runRestackCmd(t, f)
	if !errors.Is(err, errRestackDetached) {
		t.Fatalf("error = %v, want errRestackDetached", err)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

// restackGitConflict leaves feature and the remote trunk editing the same line
// of f.txt, so a restack of feature stops mid-replay.
func restackGitConflict(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
	installDropGH(t, f, nil)
	restackWrite(t, filepath.Join(f.Dir, "f.txt"), "feature\n")
	restackWrite(t, filepath.Join(f.Dir, "g.txt"), "committed\n")
	restackRun(t, f, f.Dir, "git", "add", "f.txt", "g.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "feature")
	restackAdvanceRemote(t, f, "main", "f.txt", "upstream\n")
	return f
}

func TestRestackGitConflictStopsInAWorkspaceAndContinues(t *testing.T) {
	f := restackGitConflict(t)
	before := restackRev(t, f, f.Dir, "HEAD")

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want it to stop in a workspace")
	}
	for _, want := range []string{"feature does not rebase onto main cleanly", "f.txt", "ccx vcs stack continue", "ccx vcs stack abort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("brief = %v, want %q", err, want)
		}
	}
	ws := stackWorkspaceOf(t, err)
	if filepath.Base(ws) != "conflict-feature" {
		t.Fatalf("workspace = %q, want a pool entry named conflict-feature", ws)
	}
	if after := restackRev(t, f, f.Dir, "refs/heads/feature"); after != before {
		t.Errorf("feature moved to %s before the conflict was resolved", after)
	}
	if restackRev(t, f, f.Dir, "REBASE_HEAD") != "" {
		t.Error("the working copy restack ran from is mid-rebase; the rebase belongs in the workspace")
	}

	restackWrite(t, filepath.Join(ws, "f.txt"), "upstream\nfeature\n")
	restackRun(t, f, ws, "git", "add", "f.txt")
	if _, _, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !stackOnto(t, f, "refs/remotes/origin/main", "feature") {
		t.Error("feature is not on the fetched trunk after continue")
	}
	if got := restackRev(t, f, f.Dir, "HEAD"); got != restackRev(t, f, f.Dir, "refs/heads/feature") {
		t.Errorf("HEAD = %s, want the working copy reset onto the rebased feature", got)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "f.txt")); got != "upstream\nfeature\n" {
		t.Errorf("f.txt = %q, want the resolution", got)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace %s left behind: %v", ws, err)
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
	}
}

// TestRestackGitContinueWaitsForTheLiveRestack runs stack continue from a
// second process while the restack that stopped on the conflict still lives:
// the git lane's run must name its owner as precisely as the gt lane's does.
func TestRestackGitContinueWaitsForTheLiveRestack(t *testing.T) {
	f := restackGitConflict(t)
	if _, _, err := runRestackCmd(t, f); err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want it to stop in a workspace")
	}

	cmd := exec.Command(os.Args[0], "vcs", "stack", "continue") //nolint:gosec // the test binary itself, run as ccx through TestMain
	cmd.Dir = f.Dir
	cmd.Env = append(append(os.Environ(), f.Env()...), "CCX_TEST_STACK_CONTINUE=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a second process continued a restack whose owner still runs:\n%s", out)
	}
	for _, want := range []string{fmt.Sprintf("pid %d on ", os.Getpid()), "is still driving the stack rebase of feature"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("continue = %s, want %q", out, want)
		}
	}
}

// TestRestackGitRefusesUncommittedWork holds the git lane to the stack rebase
// rule: the working copy a rebased branch is reset in must be clean, and the
// refusal comes before any workspace opens or ref moves.
func TestRestackGitRefusesUncommittedWork(t *testing.T) {
	f := restackGitConflict(t)
	restackWrite(t, filepath.Join(f.Dir, "g.txt"), "uncommitted\n")
	before := restackRev(t, f, f.Dir, "HEAD")

	_, _, err := runRestackCmd(t, f)
	if err == nil || !strings.Contains(err.Error(), "has uncommitted work; no branches moved") {
		t.Fatalf("restack over uncommitted work = %v, want a refusal", err)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD moved from %s to %s on a refusal", before, after)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "g.txt")); got != "uncommitted\n" {
		t.Errorf("g.txt = %q, want the uncommitted edit untouched", got)
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
	}
}

func TestRestackGitConflictAbortLeavesTheBranch(t *testing.T) {
	f := restackGitConflict(t)
	before := restackRev(t, f, f.Dir, "HEAD")

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want it to stop in a workspace")
	}
	ws := stackWorkspaceOf(t, err)
	out, _, err := runStackCmd(t, f, "abort")
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if out != "aborted · no branch moved" {
		t.Errorf("abort output = %q", out)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD = %s, want the pre-restack %s", after, before)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace %s left behind: %v", ws, err)
	}
	if _, _, err := runRestackCmd(t, f); err == nil || !strings.Contains(err.Error(), "does not rebase onto main cleanly") {
		t.Errorf("restack after abort = %v, want the conflict again rather than a run in progress", err)
	}
}

// TestRestackGitConflictRunsWithRerereOff records a resolution for the same
// conflict, then restacks: a rebase with rerere on replays it into the file
// and records a new preimage, where the workspace must show the conflict.
func TestRestackGitConflictRunsWithRerereOff(t *testing.T) {
	f := restackGitConflict(t)
	restackRun(t, f, f.Dir, "git", "config", "rerere.enabled", "true")
	restackRun(t, f, f.Dir, "git", "config", "rerere.autoUpdate", "true")
	restackRun(t, f, f.Dir, "git", "fetch", "-q", "origin")
	rehearsal := restackSiblingPath(t, "rehearsal")
	restackRun(t, f, f.Dir, "git", "worktree", "add", "-q", "--detach", rehearsal, "feature")
	if err := exec.Command("git", "-C", rehearsal, "rebase", "origin/main").Run(); err == nil { //nolint:gosec // fixed git argv over a fixture TempDir
		t.Fatal("the rehearsal rebase applied cleanly, want the conflict it records a resolution for")
	}
	restackWrite(t, filepath.Join(rehearsal, "f.txt"), "stale resolution\n")
	restackRun(t, f, rehearsal, "git", "add", "f.txt")
	restackRun(t, f, rehearsal, "git", "rerere")
	restackRun(t, f, rehearsal, "git", "rebase", "--abort")
	restackRun(t, f, f.Dir, "git", "worktree", "remove", "--force", rehearsal)
	cache := filepath.Join(f.Dir, ".git", "rr-cache")
	recorded, err := os.ReadDir(cache)
	if err != nil || len(recorded) != 1 {
		t.Fatalf("rr-cache = %v (%v), want the one rehearsed resolution", recorded, err)
	}
	stamp := restackRun(t, f, f.Dir, "find", cache, "-type", "f")

	_, _, err = runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want it to stop in a workspace")
	}
	ws := stackWorkspaceOf(t, err)
	if got := restackRead(t, filepath.Join(ws, "f.txt")); !strings.Contains(got, "<<<<<<<") {
		t.Errorf("f.txt in the workspace = %q, want the conflict rather than the recorded resolution", got)
	}
	if after := restackRun(t, f, f.Dir, "find", cache, "-type", "f"); after != stamp {
		t.Errorf("rr-cache changed during the restack:\nbefore:\n%s\nafter:\n%s", stamp, after)
	}
}

// TestRestackGitFlattensABranchCarryingAMerge covers the span git replay
// refuses: a branch that merged trunk in rebases flat, as git rebase does.
func TestRestackGitFlattensABranchCarryingAMerge(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
	installDropGH(t, f, nil)
	restackWrite(t, filepath.Join(f.Dir, "feature.txt"), "feature\n")
	restackRun(t, f, f.Dir, "git", "add", "feature.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "feature")
	restackAdvanceRemote(t, f, "main", "first.txt", "first\n")
	restackRun(t, f, f.Dir, "git", "fetch", "-q", "origin")
	restackRun(t, f, f.Dir, "git", "merge", "-q", "--no-edit", "refs/remotes/origin/main")
	restackAdvanceRemote(t, f, "main", "second.txt", "second\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto main"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/main..HEAD")); got != "1" {
		t.Errorf("commits above trunk = %s, want the feature commit alone", got)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "second.txt")); got != "second\n" {
		t.Errorf("second.txt = %q, want the fetched trunk's content", got)
	}
}

// TestRestackGitConflictNeverAdvisesPushingTrunk pins the recovery a stopped
// restack names: the branch restacked is feature, so no line of it may tell
// the user to push main.
func TestRestackGitConflictNeverAdvisesPushingTrunk(t *testing.T) {
	f := restackGitConflict(t)

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want it to stop in a workspace")
	}
	if strings.Contains(err.Error(), "git push origin main") {
		t.Errorf("error = %q, want no advice to push trunk from feature", err)
	}
}

func restackGitStacked(t *testing.T, prs map[string]dropSeed) (*vcstest.Fixture, *dropGH) {
	t.Helper()
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("base"))
	f.Isolate(t)
	for _, name := range []string{"base", "feature"} {
		if name != "base" {
			restackRun(t, f, f.Dir, "git", "switch", "-qc", name)
		}
		restackWrite(t, filepath.Join(f.Dir, name+".txt"), name+"\n")
		restackRun(t, f, f.Dir, "git", "add", name+".txt")
		restackRun(t, f, f.Dir, "git", "commit", "-qm", name)
		restackRun(t, f, f.Dir, "git", "push", "-q", "origin", name)
	}
	gh := installDropGH(t, f, prs)
	restackReset(t, f)
	return f, gh
}

func TestRestackGitReplaysOntoThePullRequestBase(t *testing.T) {
	tests := []struct {
		name string
		prs  map[string]dropSeed
		onto string
	}{
		{name: "stacked on base", prs: map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "base"}}, onto: "base"},
		{name: "pull request on trunk", prs: map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "main"}}, onto: "main"},
		{name: "no pull request", onto: "main"},
		{name: "closed pull request", prs: map[string]dropSeed{"feature": {number: 2, state: "CLOSED", base: "base"}}, onto: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := restackGitStacked(t, tt.prs)
			restackAdvanceRemote(t, f, tt.onto, "upstream.txt", "upstream\n")

			out, _, err := runRestackCmd(t, f)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if want := "fetched · rebased onto " + tt.onto; out != want {
				t.Fatalf("output = %q, want %q", out, want)
			}
			dropStep(t, restackInvocations(t, f), "gh", "GET", "repos/yasyf/cc-context/pulls", "head=yasyf:feature", "state=open")
			if !stackOnto(t, f, "refs/remotes/origin/"+tt.onto, "feature") {
				t.Errorf("feature is not on the fetched %s", tt.onto)
			}
			if got := restackRead(t, filepath.Join(f.Dir, "upstream.txt")); got != "upstream\n" {
				t.Errorf("upstream.txt = %q, want the fetched %s's content", got, tt.onto)
			}
		})
	}
}

func TestRestackGitAlreadyOnThePullRequestBase(t *testing.T) {
	f, _ := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "base"}})
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	before := restackRev(t, f, f.Dir, "HEAD")
	restackReset(t, f)

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · already up to date"; out != want {
		t.Fatalf("output = %q, want %q — trunk moved, but feature's parent is base", out, want)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD moved from %s to %s while already on its parent", before, after)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

func TestRestackGitFailsWhenThePullRequestLookupFails(t *testing.T) {
	tests := []struct {
		name      string
		uncached  bool
		wantError string
	}{
		{name: "repository lookup", uncached: true, wantError: "restack: github metadata unavailable: gh repo view"},
		{name: "pull request lookup", wantError: "restack: list the open pull requests of feature"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := restackGitStacked(t, nil)
			if tt.uncached {
				clearRepoRecord(f.Context(), t, f.Dir)
			}
			writeShipExecutable(t, f.ShimBin, "gh", "#!/bin/sh\nprintf 'gh: Bad Gateway (HTTP 502)\\n' >&2\nexit 1\n")
			restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			before := restackRev(t, f, f.Dir, "HEAD")
			restackReset(t, f)

			_, _, err := runRestackCmd(t, f)
			if err == nil {
				t.Fatal("restack succeeded without knowing feature's parent, want the lookup failure")
			}
			for _, want := range []string{tt.wantError, "HTTP 502"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want %q", err, want)
				}
			}
			if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
				t.Errorf("HEAD moved from %s to %s on a failed lookup", before, after)
			}
			assertNoRestackMutation(t, restackInvocations(t, f))
		})
	}
}

func TestRestackGitLeavesARewrittenParentsCommitsBehind(t *testing.T) {
	f, _ := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "base"}})
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	clone := filepath.Join(t.TempDir(), "upstream")
	restackRun(t, f, filepath.Dir(clone), "git", "clone", "-q", "--branch", "base", f.RemoteDir, clone)
	restackRun(t, f, clone, "git", "config", "user.email", "t@t.t")
	restackRun(t, f, clone, "git", "config", "user.name", "t")
	restackWrite(t, filepath.Join(clone, "base.txt"), "base, revised\n")
	restackRun(t, f, clone, "git", "commit", "-qam", "base, revised")
	restackRun(t, f, clone, "git", "rebase", "-q", "origin/main")
	restackRun(t, f, clone, "git", "push", "-qf", "origin", "base")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto base"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/base..feature")); got != "1" {
		t.Errorf("commits above base = %s, want 1 — the old base's commits were replayed along with feature's", got)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "base.txt")); got != "base, revised\n" {
		t.Errorf("base.txt = %q, want the rewritten base's content", got)
	}
}

func TestRestackGitParentMovesThePullRequest(t *testing.T) {
	f, gh := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "main"}})
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	restackAdvanceRemote(t, f, "base", "base2.txt", "base, again\n")
	published := restackRev(t, f, f.Dir, "feature")

	out, _, err := runRestackCmd(t, f, "--parent", "base")
	if err != nil {
		t.Fatalf("restack --parent base: %v", err)
	}
	if want := "fetched · rebased onto base · force-pushed feature → origin"; !strings.HasPrefix(out, want) {
		t.Errorf("output = %q, want prefix %q", out, want)
	}
	if want := "retargeted PR #2 onto base (was main)"; !strings.HasSuffix(out, want) {
		t.Errorf("output = %q, want suffix %q", out, want)
	}
	if got := gh.pr(2); got != "OPEN base" {
		t.Errorf("PR #2 = %q, want it open on base", got)
	}
	if !stackOnto(t, f, "refs/remotes/origin/base", "feature") {
		t.Error("feature is not on the fetched base")
	}
	if stackOnto(t, f, "refs/remotes/origin/main", "feature") {
		t.Error("feature carries main's newer commits, which its new parent does not")
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/base..feature")); got != "1" {
		t.Errorf("commits above base = %s, want feature's own 1", got)
	}
	if local, remote := restackRev(t, f, f.Dir, "feature"), restackRev(t, f, f.Dir, "refs/remotes/origin/feature"); local != remote {
		t.Errorf("origin/feature = %s, want the rebased %s", remote, local)
	}
	invocations := restackInvocations(t, f)
	push := dropStep(t, invocations, "git", "push", "--force-with-lease=refs/heads/feature:"+published)
	if retarget := dropStep(t, invocations, "gh", "PATCH", "repos/yasyf/cc-context/pulls/2", "base=base"); retarget < push {
		t.Error("the pull request moved onto base before the branch it shows was pushed")
	}
}

func TestRestackGitParentAlreadyThereRetargetsOnly(t *testing.T) {
	f, gh := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "main"}})
	published := restackRev(t, f, f.Dir, "feature")

	out, _, err := runRestackCmd(t, f, "--parent", "base")
	if err != nil {
		t.Fatalf("restack --parent base: %v", err)
	}
	if want := "fetched · already on base · origin/feature already at " + published[:12] + " — nothing to push · retargeted PR #2 onto base (was main)"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if got := restackRev(t, f, f.Dir, "feature"); got != published {
		t.Errorf("feature moved from %s to %s while already on base", published, got)
	}
	if got := gh.pr(2); got != "OPEN base" {
		t.Errorf("PR #2 = %q, want it open on base", got)
	}
}

func TestRestackGitParentLeavesTheOldBaseBehind(t *testing.T) {
	f, gh := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "base"}})

	out, _, err := runRestackCmd(t, f, "--parent", "main")
	if err != nil {
		t.Fatalf("restack --parent main: %v", err)
	}
	if want := "fetched · rebased onto main"; !strings.HasPrefix(out, want) {
		t.Errorf("output = %q, want prefix %q — main already sits under feature, but base's commit still has to come out", out, want)
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/main..feature")); got != "1" {
		t.Errorf("commits above main = %s, want feature's own 1", got)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "base.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("base.txt stat = %v, want base's file gone with its commit", err)
	}
	if got := gh.pr(2); got != "OPEN main" {
		t.Errorf("PR #2 = %q, want it open on main", got)
	}
}

func TestRestackGitParentRefusesForkPointsOnSeparateLines(t *testing.T) {
	f, _ := restackGitStacked(t, map[string]dropSeed{"feature": {number: 2, state: "OPEN", base: "other"}})
	restackRun(t, f, f.Dir, "git", "switch", "-qc", "other", "main")
	restackWrite(t, filepath.Join(f.Dir, "other.txt"), "other\n")
	restackRun(t, f, f.Dir, "git", "add", "other.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "other")
	restackRun(t, f, f.Dir, "git", "push", "-q", "origin", "other")
	restackRun(t, f, f.Dir, "git", "switch", "-q", "feature")
	restackRun(t, f, f.Dir, "git", "merge", "-q", "--no-edit", "other")
	restackRun(t, f, f.Dir, "git", "push", "-q", "origin", "feature")
	before := restackRev(t, f, f.Dir, "feature")
	restackReset(t, f)

	_, _, err := runRestackCmd(t, f, "--parent", "base")
	if err == nil || !strings.Contains(err.Error(), "on separate lines of history, so no single replay leaves out both bases' commits") {
		t.Fatalf("error = %v, want the separate-lines refusal", err)
	}
	if after := restackRev(t, f, f.Dir, "feature"); after != before {
		t.Errorf("feature moved from %s to %s on a refusal", before, after)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

func TestRestackGitParentWithoutAPullRequestStaysLocal(t *testing.T) {
	f, _ := restackGitStacked(t, nil)
	restackAdvanceRemote(t, f, "base", "base2.txt", "base, again\n")
	published := restackRev(t, f, f.Dir, "refs/remotes/origin/feature")

	out, _, err := runRestackCmd(t, f, "--parent", "base")
	if err != nil {
		t.Fatalf("restack --parent base: %v", err)
	}
	if want := "fetched · rebased onto base"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if !stackOnto(t, f, "refs/remotes/origin/base", "feature") {
		t.Error("feature is not on the fetched base")
	}
	if got := restackRev(t, f, f.Dir, "refs/remotes/origin/feature"); got != published {
		t.Errorf("origin/feature moved from %s to %s with no pull request to retarget", published, got)
	}
}

func TestRestackParentRefusals(t *testing.T) {
	t.Run("graphite lane", func(t *testing.T) {
		f := restackGTRepo(t, "feature")
		_, _, err := runRestackCmd(t, f, "--parent", "main")
		if want := "restack: --parent on the graphite lane is ccx vcs stack rebase --parent <branch>=<parent>"; err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
	})
	t.Run("onto itself", func(t *testing.T) {
		f, _ := restackGitStacked(t, nil)
		_, _, err := runRestackCmd(t, f, "--parent", "feature")
		if want := "restack: --parent moves a branch onto another, and feature cannot sit on feature"; err == nil || err.Error() != want {
			t.Errorf("error = %v, want %q", err, want)
		}
		assertNoRestackMutation(t, restackInvocations(t, f))
	})
}

func TestRestackJJAlreadyUpToDate(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	f.Isolate(t)
	before := strings.TrimSpace(restackRun(t, f, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · already up to date"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	after := strings.TrimSpace(restackRun(t, f, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	if after != before {
		t.Errorf("@- moved from %s to %s on an up-to-date restack", before, after)
	}
}

func TestRestackJJRebasesOntoTrunk(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	f.Isolate(t)
	restackWrite(t, filepath.Join(f.Dir, "one.txt"), "one\n")
	restackRun(t, f, f.Dir, "jj", "commit", "-m", "one")
	restackWrite(t, filepath.Join(f.Dir, "two.txt"), "two\n")
	restackRun(t, f, f.Dir, "jj", "commit", "-m", "two")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased 3 commit(s) onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if onTrunk := restackRun(t, f, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) == "" {
		t.Error("@ does not descend from trunk() — the rebase did not land")
	}
	for _, name := range []string{"upstream.txt", "one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(f.Dir, name)); err != nil {
			t.Errorf("stat %s: %v", name, err)
		}
	}
}

// TestRestackJJConflictRollsBack drives a real conflicting jj rebase. jj records
// the conflict in the commit rather than stopping, so ccx has to detect it after
// the fact and revert the operation — a rollback that has to leave the working
// copy exactly where it was.
func TestRestackJJConflictRollsBack(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	f.Isolate(t)
	restackWrite(t, filepath.Join(f.Dir, "f.txt"), "local\n")
	restackRun(t, f, f.Dir, "jj", "commit", "-m", "local")
	restackAdvanceRemote(t, f, "main", "f.txt", "upstream\n")

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want a refusal")
	}
	for _, want := range []string{
		`restack: rebase onto "main" conflicts in`,
		"rolled back to the pre-rebase state",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
	if conflicts := restackRun(t, f, f.Dir, "jj", "log", "-r", "conflicts() & @::", "--no-graph", "-T", "commit_id"); strings.TrimSpace(conflicts) != "" {
		t.Errorf("conflicts remain at %q — the rollback did not run", strings.TrimSpace(conflicts))
	}
	if onTrunk := restackRun(t, f, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) != "" {
		t.Error("@ descends from trunk() — the conflicted rebase was left in place")
	}
	if got := restackRead(t, filepath.Join(f.Dir, "f.txt")); got != "local\n" {
		t.Errorf("f.txt = %q, want the pre-rebase content", got)
	}
}

// TestRestackRefusalsCarryRestackPrefix pins each lane's refusal to restack's
// own prefix: these helpers are shared with ship, and an error leading with
// ship: sends the reader to a command they never ran.
func TestRestackRefusalsCarryRestackPrefix(t *testing.T) {
	tests := []struct {
		name        string
		opts        []vcstest.Opt
		undesignate bool
	}{
		{name: "git lane", opts: []vcstest.Opt{vcstest.Remote(), vcstest.NoOriginHead()}, undesignate: true},
		{name: "jj lane", opts: []vcstest.Opt{vcstest.JJ()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t, tt.opts...)
			f.Isolate(t)
			if tt.undesignate {
				restackUndesignatedTrunk(t, f)
			}

			_, _, err := runRestackCmd(t, f)
			if err == nil {
				t.Fatal("restack succeeded with no trunk to restack onto, want a refusal")
			}
			if !strings.HasPrefix(err.Error(), "restack: ") {
				t.Errorf("error = %v, want it to lead with restack's own prefix", err)
			}
			if strings.Contains(err.Error(), "ship:") {
				t.Errorf("error = %v, want restack's prefix, not ship's", err)
			}
		})
	}
}

// restackGTRepo builds a real gt-tracked stack: one branch per name, each cut
// from the last with a commit of its own and tracked by the real gt, which is
// what answers gt state for the rest of the test. Graphite's API is the stub,
// reporting no pull request merged; a test that needs one installs its own.
func restackGTRepo(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.GT(), vcstest.GTStack(names...))
	f.Isolate(t)
	seedLaneRecords(f.Context(), t, f.Dir, laneSeed{})
	f.Decorate(newGTAPIStub(t).ctx)
	stubStackPRs(t, f, nil)
	return f
}

func TestRestackGTPerBranchVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		advance, freeze, onTrunk bool
	}{
		{name: "already current"}, {name: "behind trunk", advance: true}, {name: "frozen current", freeze: true}, {name: "frozen behind", advance: true, freeze: true}, {name: "on trunk", onTrunk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := restackGTRepo(t, "feat")
			if tc.advance {
				restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			}
			if tc.freeze {
				restackRun(t, f, f.Dir, "gt", "freeze", "feat", "--no-interactive")
			}
			if tc.onTrunk {
				restackRun(t, f, f.Dir, "git", "switch", "-q", "main")
			}
			before := restackRev(t, f, f.Dir, "main")
			out, _, err := runRestackCmd(t, f)
			if tc.freeze || tc.onTrunk {
				if err == nil {
					t.Fatal("expected refusal")
				}
				want := "frozen"
				if tc.onTrunk {
					want = "HEAD is not on a stack branch"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "onto main@") || !strings.Contains(out, "not pushed (--no-push)") {
				t.Errorf("output = %q", out)
			}
			if got := restackRev(t, f, f.Dir, "main"); got != before {
				t.Errorf("trunk moved to %s", got)
			}
			if !stackOnto(t, f, "origin/main", "feat") {
				t.Error("feat did not reach remote trunk")
			}
			if gitBranchExists(t, f.Env(), f.RemoteDir, "feat") {
				t.Error("restack pushed feat")
			}
		})
	}
}

func TestRestackGTMovesPastALandedParent(t *testing.T) {
	f := restackGTRepo(t, "a", "b")
	stubStackPRs(t, f, map[string]*stackPR{"a": {Number: 10, Head: restackRev(t, f, f.Dir, "a"), State: "CLOSED", Landed: true}})
	restackAdvanceRemote(t, f, "main", "a.txt", "a\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if !strings.Contains(out, "dropped a (#10 landed)") {
		t.Fatalf("output = %q, want landed parent dropped", out)
	}
	if got := restackGTParent(t, f, "b"); got != "main" {
		t.Errorf("b's gt parent = %q, want main — a landed", got)
	}
	if own := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/main..b")); own != "1" {
		t.Errorf("b carries %s commits over trunk, want only its own 1", own)
	}
	for _, inv := range restackInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "gt" && inv[1] == "sync" {
			t.Errorf("restack ran %v", inv)
		}
	}
}

func TestRestackGTKeepsChildrenOfAParentThatMergedElsewhere(t *testing.T) {
	f := restackGTRepo(t, "a", "b")
	stubStackPRs(t, f, map[string]*stackPR{"a": {Number: 10, Head: restackRev(t, f, f.Dir, "main"), State: "CLOSED", Landed: true}})
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	_, _, err := runRestackCmd(t, f)
	if err == nil || !strings.Contains(err.Error(), "holds commits past it") {
		t.Fatalf("restack = %v, want unlanded-work refusal", err)
	}
	if got := restackGTParent(t, f, "b"); got != "a" {
		t.Errorf("b's gt parent = %q, want a — a merged at another head", got)
	}
}

func restackGTParent(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	rows, err := gtmeta.Rows(t.Context(), commonDir)
	if err != nil {
		t.Fatalf("gtmeta.Rows: %v", err)
	}
	for _, row := range rows {
		if row.Branch == branch {
			return row.Parent
		}
	}
	t.Fatalf("gt tracks no %s", branch)
	return ""
}

func TestRestackGTRefusesAnotherWorkingCopyMover(t *testing.T) {
	f := restackGTRepo(t, "a", "b")
	held := restackSiblingPath(t, "held")
	restackRun(t, f, f.Dir, "git", "worktree", "add", "-q", held, "a")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	before := map[string]string{"a": restackRev(t, f, f.Dir, "a"), "b": restackRev(t, f, f.Dir, "b")}
	_, _, err := runRestackCmd(t, f)
	if err == nil || !strings.Contains(err.Error(), "checked out in "+held) {
		t.Fatalf("restack = %v, want holder refusal", err)
	}
	for branch, head := range before {
		if got := restackRev(t, f, f.Dir, branch); got != head {
			t.Errorf("%s moved to %s", branch, got)
		}
	}
	if dirt := strings.TrimSpace(restackRun(t, f, held, "git", "status", "--porcelain")); dirt != "" {
		t.Errorf("held dirty: %s", dirt)
	}
}

func TestRestackGTLeavesLocalTrunkUntouched(t *testing.T) {
	for _, heldTrunk := range []bool{false, true} {
		t.Run(fmt.Sprint(heldTrunk), func(t *testing.T) {
			f := restackGTRepo(t, "feat")
			before := restackRev(t, f, f.Dir, "main")
			held := ""
			if heldTrunk {
				held = restackSiblingPath(t, "trunk")
				restackRun(t, f, f.Dir, "git", "worktree", "add", "-q", held, "main")
			}
			restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			if _, _, err := runRestackCmd(t, f); err != nil {
				t.Fatal(err)
			}
			if got := restackRev(t, f, f.Dir, "main"); got != before {
				t.Errorf("trunk moved to %s", got)
			}
			if held != "" {
				if got := restackRev(t, f, held, "HEAD"); got != before {
					t.Errorf("holder HEAD moved to %s", got)
				}
				if dirt := strings.TrimSpace(restackRun(t, f, held, "git", "status", "--porcelain")); dirt != "" {
					t.Errorf("holder dirty: %s", dirt)
				}
			}
		})
	}
}

func TestRestackGTRepairsIncorrectRestackMetadata(t *testing.T) {
	f := restackGTRepo(t, "feat")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	restackRun(t, f, f.Dir, "git", "fetch", "-q", "origin")
	commonDir := gitAt(t, f.Env(), f.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main")
	if err := gtmeta.RecordRestacked(t.Context(), commonDir, map[string]string{"feat": remote}); err != nil {
		t.Fatalf("record feat as restacked: %v", err)
	}
	before := restackRev(t, f, f.Dir, "feat")

	_, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("repair stale metadata: %v", err)
	}
	if after := restackRev(t, f, f.Dir, "feat"); after == before {
		t.Error("feature remained stale")
	}
	if !stackOnto(t, f, "origin/main", "feat") {
		t.Error("feature is not on remote trunk")
	}
	if got := strings.TrimSpace(restackRun(t, f, f.Dir, "git", "rev-list", "--count", "origin/main..feat")); got != "1" {
		t.Errorf("feature has %s commits, want 1", got)
	}
}

func TestRestackGTPreservesDirtyTrunkWithoutStagingDeletions(t *testing.T) {
	f := restackGTRepo(t, "feat")
	held := restackSiblingPath(t, "trunk")
	restackRun(t, f, f.Dir, "git", "worktree", "add", "-q", held, "main")
	before := restackRev(t, f, held, "HEAD")
	restackWrite(t, filepath.Join(held, "wip.txt"), "work in progress\n")
	restackRun(t, f, held, "git", "add", "wip.txt")
	restackWrite(t, filepath.Join(held, "scratch.txt"), "untracked\n")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	restackRun(t, f, f.Dir, "git", "fetch", "-q", "origin")

	if _, _, err := runRestackCmd(t, f); err != nil {
		t.Fatalf("restack: %v", err)
	}
	if got := restackRev(t, f, held, "HEAD"); got != before {
		t.Errorf("trunk moved to %s", got)
	}
	want := "A  wip.txt\n?? scratch.txt"
	if dirt := strings.TrimSpace(restackRun(t, f, held, "git", "status", "--porcelain")); dirt != want {
		t.Errorf("the holder's status = %q, want %q — its own work, staged as it was staged, and no deletion of the incoming commit", dirt, want)
	}
	if _, err := os.Stat(filepath.Join(held, "upstream.txt")); !os.IsNotExist(err) {
		t.Errorf("upstream file reached untouched trunk: %v", err)
	}
}

func TestRestackGraphiteFirst(t *testing.T) {
	t.Run("colocated routes to gt", func(t *testing.T) {
		f := restackGTRepo(t, "feat")

		out, _, err := runRestackCmd(t, f)
		if err != nil {
			t.Fatalf("restack: %v", err)
		}
		if !strings.Contains(out, "onto main@") || !strings.Contains(out, "not pushed (--no-push)") {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("no gt routes to the vcs lane", func(t *testing.T) {
		f := restackGTRepo(t, "feat")
		installDropGH(t, f, nil)
		restackReset(t, f)

		out, _, err := runRestackCmd(t, f, "--no-gt")
		if err != nil {
			t.Fatalf("restack --no-gt: %v", err)
		}
		if want := "fetched · already up to date"; out != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
		assertNoGT(t, restackInvocations(t, f))
	})
}

func TestRestackRefusesMissingGT(t *testing.T) {
	f := restackGTRepo(t, "feat")
	restackReset(t, f)
	if err := os.Remove(filepath.Join(f.ShimBin, "gt")); err != nil {
		t.Fatalf("remove gt shim: %v", err)
	}

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded, want missing-gt refusal")
	}
	want := "restack: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

func TestStackRebaseIsItsOwnCommand(t *testing.T) {
	t.Parallel()
	cmd := newVcsCmd()
	for _, name := range []string{"restack", "rebase", "continue", "abort"} {
		found, args, err := cmd.Find([]string{"stack", name})
		if err != nil {
			t.Fatalf("find stack %s: %v", name, err)
		}
		if found.Name() != name || len(args) != 0 {
			t.Fatalf("find stack %s = %s %#v", name, found.Name(), args)
		}
	}
}

func TestRestackGTReportsDriftNoBranchLandsOn(t *testing.T) {
	f := restackGTRepo(t, "feat")
	restackRun(t, f, f.Dir, "git", "switch", "-q", "main")
	restackWrite(t, filepath.Join(f.Dir, "foreign.txt"), "another lane's work\n")
	restackRun(t, f, f.Dir, "git", "add", "foreign.txt")
	restackRun(t, f, f.Dir, "git", "commit", "-qm", "foreign")

	before := restackRev(t, f, f.Dir, "main")
	_, errOut, err := runRestackCmd(t, f)
	if err == nil || !strings.Contains(err.Error(), "HEAD is not on a stack branch") {
		t.Fatalf("restack = %v, want trunk refusal", err)
	}
	if strings.Contains(errOut, "holds") {
		t.Errorf("unexpected drift warning: %q", errOut)
	}
	if got := restackRev(t, f, f.Dir, "main"); got != before {
		t.Errorf("trunk moved to %s", got)
	}
}

// TestRestackMergedNamesTheBranch pins that a branch already in its parent is
// not reported as one sitting off it: no rebase moves it, and the submit that
// would follow is one Graphite refuses.
func TestRestackMergedNamesTheBranch(t *testing.T) {
	t.Parallel()
	got := (&errRestackMerged{Branch: "yasyf/ssql-replica-chase-debug"}).Error()
	if !strings.Contains(got, "already merged") || strings.Contains(got, "off its parent") {
		t.Errorf("errRestackMerged = %q, want it to name the merge rather than the parent", got)
	}
	if !strings.Contains(got, "gt untrack yasyf/ssql-replica-chase-debug") {
		t.Errorf("errRestackMerged = %q, want the step that clears it", got)
	}
}

func TestRestackGTConflictContinuesWithoutPushing(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stackConflicting(t, f)
	before := map[string]string{"base": restackRev(t, f, f.Dir, "base"), "feature": restackRev(t, f, f.Dir, "feature")}
	_, _, err := runRestackCmd(t, f)
	if err == nil || !strings.Contains(err.Error(), "ccx vcs stack continue") {
		t.Fatalf("restack = %v, want durable conflict", err)
	}
	for branch, head := range before {
		if got := restackRev(t, f, f.Dir, branch); got != head {
			t.Errorf("%s moved before conflict resolved", branch)
		}
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !run.NoPush {
		t.Error("restack continuation can push")
	}
	ws := run.Conflict.Workspace
	restackWrite(t, filepath.Join(ws, "c.txt"), "resolved\n")
	restackRun(t, f, ws, "git", "add", "c.txt")
	if _, _, err := runStackCmdIn(t, f, ws, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature not on remote trunk")
	}
	for branch := range before {
		if gitBranchExists(t, f.Env(), f.RemoteDir, branch) {
			t.Errorf("restack pushed %s", branch)
		}
	}
}

// restackSquashRemote lands names on origin's trunk as one squash commit
// carrying the subject the Graphite merge queue writes — what a queue landing
// leaves behind, with none of the branches' own commits reaching trunk.
func restackSquashRemote(t *testing.T, f *vcstest.Fixture, trunk, subject string, names ...string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "upstream")
	restackRun(t, f, filepath.Dir(clone), "git", "clone", "-q", "--branch", trunk, f.RemoteDir, clone)
	restackRun(t, f, clone, "git", "config", "user.email", "t@t.t")
	restackRun(t, f, clone, "git", "config", "user.name", "t")
	for _, name := range names {
		restackWrite(t, filepath.Join(clone, name+".txt"), name+"\n")
		restackRun(t, f, clone, "git", "add", name+".txt")
	}
	restackRun(t, f, clone, "git", "commit", "-qm", subject)
	restackRun(t, f, clone, "git", "push", "-q", "origin", trunk)
}
