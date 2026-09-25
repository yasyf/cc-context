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
	f.Quiesce(t)
	restackWrite(t, f.ArgvLog, "")
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
	f.Quiesce(t)
	return vcstest.Invocations(t, f.ArgvLog)
}

func restackRecords(t *testing.T, f *vcstest.Fixture) []vcstest.Invocation {
	t.Helper()
	f.Quiesce(t)
	return vcstest.Records(t, f.ArgvLog)
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
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
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
			f := vcstest.Repo(t, opts...)
			f.Isolate(t)
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

// TestRestackGitConflictAbortsBackToTheStartingState drives a real conflicting
// rebase: the branch and the trunk edit the same line, so git stops mid-replay
// and ccx has to abort rather than leave the working copy in a rebase.
func TestRestackGitConflictAbortsBackToTheStartingState(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Branch("feature"))
	f.Isolate(t)
	restackWrite(t, filepath.Join(f.Dir, "f.txt"), "feature\n")
	restackRun(t, f, f.Dir, "git", "commit", "-qam", "feature")
	restackAdvanceRemote(t, f, "main", "f.txt", "upstream\n")
	before := restackRev(t, f, f.Dir, "HEAD")

	_, _, err := runRestackCmd(t, f)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want a refusal")
	}
	if !strings.Contains(err.Error(), "conflicts in: f.txt; aborted back to the pre-rebase state") {
		t.Fatalf("error = %q, want it to name the conflicted file and the abort", err)
	}
	if !strings.HasPrefix(err.Error(), "restack: ") {
		t.Errorf("error = %q, want the restack prefix — gitRebaseOnto is shared with ship", err)
	}
	if after := restackRev(t, f, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD = %s, want the pre-rebase %s", after, before)
	}
	if restackRev(t, f, f.Dir, "REBASE_HEAD") != "" {
		t.Error("a rebase is still in progress — the abort did not run")
	}
	if got := restackRead(t, filepath.Join(f.Dir, "f.txt")); got != "feature\n" {
		t.Errorf("f.txt = %q, want the branch's own content back", got)
	}
	if status := restackRun(t, f, f.Dir, "git", "status", "--porcelain"); status != "" {
		t.Errorf("status = %q, want a clean working copy after the abort", status)
	}
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
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.GT())
	f.Isolate(t)
	seedLaneRecords(t, f.Dir, laneSeed{})
	stubGTAPI(t)
	stubStackPRs(t, nil)
	for _, name := range names {
		restackRun(t, f, f.Dir, "git", "switch", "-qc", name)
		restackWrite(t, filepath.Join(f.Dir, name+".txt"), name+"\n")
		restackRun(t, f, f.Dir, "git", "add", name+".txt")
		restackRun(t, f, f.Dir, "git", "commit", "-qm", name)
		restackRun(t, f, f.Dir, "gt", "track", "-f", "--no-interactive")
	}
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

// restackPinned names the commit the summary reports the pass pinned — the
// remote trunk's head, which the pass fast-forwards the local branch onto.
func restackPinned(t *testing.T, f *vcstest.Fixture, want string) string {
	t.Helper()
	head := f.Out(t, "git", "rev-parse", "--short=12", "refs/remotes/origin/main")
	return strings.Replace(want, "trunk main", "trunk main@"+strings.TrimSpace(head), 1)
}

func TestRestackGTMovesPastALandedParent(t *testing.T) {
	f := restackGTRepo(t, "a", "b")
	stubStackPRs(t, map[string]*stackPR{"a": {Number: 10, Head: restackRev(t, f, f.Dir, "a"), State: "CLOSED", Landed: true}})
	restackAdvanceRemote(t, f, "main", "a.txt", "a\n")

	out, _, err := runRestackCmd(t, f)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if !strings.Contains(out, "dropped landed a") {
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
	stubStackPRs(t, map[string]*stackPR{"a": {Number: 10, Head: restackRev(t, f, f.Dir, "main"), State: "CLOSED", Landed: true}})
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
	f := shipGTRepo(t)
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
	run, err := stackLoadRun(filepath.Join(f.Dir, ".git"))
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
