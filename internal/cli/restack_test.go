package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runRestackCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRestackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return strings.TrimSpace(out.String()), errOut.String(), err
}

// restackRun runs a real tool in dir through the recording shim, failing the
// test on a nonzero exit.
func restackRun(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // bin resolves through the fixture's own shim and args are test-authored
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", bin, args, dir, err, stderr.String())
	}
	return stdout.String()
}

// restackRev resolves rev to a commit id, or "" when it names nothing.
func restackRev(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", rev+"^{commit}") //nolint:gosec // fixed git argv; rev is a test literal and dir a fixture TempDir
	cmd.Dir = dir
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
	restackRun(t, filepath.Dir(clone), "git", "clone", "-q", "--branch", trunk, f.RemoteDir, clone)
	restackRun(t, clone, "git", "config", "user.email", "t@t.t")
	restackRun(t, clone, "git", "config", "user.name", "t")
	restackWrite(t, filepath.Join(clone, file), content)
	restackRun(t, clone, "git", "add", file)
	restackRun(t, clone, "git", "commit", "-qm", "upstream")
	restackRun(t, clone, "git", "push", "-q", "origin", trunk)
}

// restackReset drops every argv record the test's own fixture work wrote, so an
// invocation assertion sees only what restack itself ran.
func restackReset(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	vcstest.Quiesce(t, f.ArgvLog)
	restackWrite(t, f.ArgvLog, "")
}

// restackUndesignatedTrunk leaves the repository with no default branch a fetch
// can designate for it: origin/HEAD is unset, and the remote's own HEAD names a
// branch the remote does not have — which is what git 2.44 and later need,
// since they write refs/remotes/origin/HEAD during any fetch that finds the
// answer.
func restackUndesignatedTrunk(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	restackRun(t, f.Dir, "git", "--git-dir", f.RemoteDir, "symbolic-ref", "HEAD", "refs/heads/absent")
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
	vcstest.Quiesce(t, f.ArgvLog)
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
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Branch("feature"))
	restackWrite(t, filepath.Join(f.Dir, "feature.txt"), "feature\n")
	restackRun(t, f.Dir, "git", "add", "feature.txt")
	restackRun(t, f.Dir, "git", "commit", "-qm", "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if restackRev(t, f.Dir, "refs/remotes/origin/main") == "" {
		t.Fatal("refs/remotes/origin/main missing after a fetch restack claims to have made")
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "upstream.txt")); err != nil {
		t.Errorf("stat upstream.txt: %v — the rebase did not replay onto the fetched trunk", err)
	}
	if got := strings.TrimSpace(restackRun(t, f.Dir, "git", "rev-list", "--count", "refs/remotes/origin/main..HEAD")); got != "1" {
		t.Errorf("commits above trunk = %s, want 1 (the feature commit, replayed once)", got)
	}
	if got := strings.TrimSpace(restackRun(t, f.Dir, "git", "branch", "--show-current")); got != "feature" {
		t.Errorf("branch = %q, want feature", got)
	}
}

func TestRestackGitFastForwardsTrunk(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · fast-forwarded main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if local, remote := restackRev(t, f.Dir, "refs/heads/main"), restackRev(t, f.Dir, "refs/remotes/origin/main"); local != remote {
		t.Errorf("main = %s, refs/remotes/origin/main = %s — the fast-forward did not land", local, remote)
	}
	if got := restackRead(t, filepath.Join(f.Dir, "upstream.txt")); got != "upstream\n" {
		t.Errorf("upstream.txt = %q, want the fetched trunk's content", got)
	}
}

func TestRestackGitAlreadyUpToDate(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	before := restackRev(t, f.Dir, "HEAD")
	restackReset(t, f)

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · already up to date"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if after := restackRev(t, f.Dir, "HEAD"); after != before {
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
			restackRun(t, f.Dir, "git", "branch", "origin/main", "HEAD")
			restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			decoy := restackRev(t, f.Dir, "refs/heads/origin/main")

			out, _, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q — the decoy refs/heads/origin/main answered in place of the remote-tracking ref", out, tt.want)
			}
			if got := restackRead(t, filepath.Join(f.Dir, "upstream.txt")); got != "upstream\n" {
				t.Errorf("upstream.txt = %q, want the fetched trunk's content", got)
			}
			if got := restackRev(t, f.Dir, "refs/heads/origin/main"); got != decoy {
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
	restackUndesignatedTrunk(t, f)
	before := restackRev(t, f.Dir, "HEAD")
	restackReset(t, f)

	_, _, err := runRestackCmd(t)
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
	if after := restackRev(t, f.Dir, "HEAD"); after != before {
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
	restackRun(t, f.Dir, "git", "tag", "v1")
	restackRun(t, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/tags/v1")
	restackReset(t, f)

	_, _, err := runRestackCmd(t)
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
	restackReset(t, f)

	_, _, err := runRestackCmd(t)
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
	restackWrite(t, filepath.Join(f.Dir, "f.txt"), "feature\n")
	restackRun(t, f.Dir, "git", "commit", "-qam", "feature")
	restackAdvanceRemote(t, f, "main", "f.txt", "upstream\n")
	before := restackRev(t, f.Dir, "HEAD")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded over a conflicting rebase, want a refusal")
	}
	if !strings.Contains(err.Error(), "conflicts in: f.txt; aborted back to the pre-rebase state") {
		t.Fatalf("error = %q, want it to name the conflicted file and the abort", err)
	}
	if !strings.HasPrefix(err.Error(), "restack: ") {
		t.Errorf("error = %q, want the restack prefix — gitRebaseOnto is shared with ship", err)
	}
	if after := restackRev(t, f.Dir, "HEAD"); after != before {
		t.Errorf("HEAD = %s, want the pre-rebase %s", after, before)
	}
	if restackRev(t, f.Dir, "REBASE_HEAD") != "" {
		t.Error("a rebase is still in progress — the abort did not run")
	}
	if got := restackRead(t, filepath.Join(f.Dir, "f.txt")); got != "feature\n" {
		t.Errorf("f.txt = %q, want the branch's own content back", got)
	}
	if status := restackRun(t, f.Dir, "git", "status", "--porcelain"); status != "" {
		t.Errorf("status = %q, want a clean working copy after the abort", status)
	}
}

func TestRestackJJAlreadyUpToDate(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	before := strings.TrimSpace(restackRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · already up to date"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	after := strings.TrimSpace(restackRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
	if after != before {
		t.Errorf("@- moved from %s to %s on an up-to-date restack", before, after)
	}
}

func TestRestackJJRebasesOntoTrunk(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	restackWrite(t, filepath.Join(f.Dir, "one.txt"), "one\n")
	restackRun(t, f.Dir, "jj", "commit", "-m", "one")
	restackWrite(t, filepath.Join(f.Dir, "two.txt"), "two\n")
	restackRun(t, f.Dir, "jj", "commit", "-m", "two")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "fetched · rebased 3 commit(s) onto main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if onTrunk := restackRun(t, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) == "" {
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
	restackWrite(t, filepath.Join(f.Dir, "f.txt"), "local\n")
	restackRun(t, f.Dir, "jj", "commit", "-m", "local")
	restackAdvanceRemote(t, f, "main", "f.txt", "upstream\n")

	_, _, err := runRestackCmd(t)
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
	if conflicts := restackRun(t, f.Dir, "jj", "log", "-r", "conflicts() & @::", "--no-graph", "-T", "commit_id"); strings.TrimSpace(conflicts) != "" {
		t.Errorf("conflicts remain at %q — the rollback did not run", strings.TrimSpace(conflicts))
	}
	if onTrunk := restackRun(t, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) != "" {
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
			if tt.undesignate {
				restackUndesignatedTrunk(t, f)
			}

			_, _, err := runRestackCmd(t)
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
// what answers gt state for the rest of the test.
func restackGTRepo(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.GT())
	seedLaneRecords(t, ".", laneSeed{})
	for _, name := range names {
		restackRun(t, f.Dir, "git", "switch", "-qc", name)
		restackWrite(t, filepath.Join(f.Dir, name+".txt"), name+"\n")
		restackRun(t, f.Dir, "git", "add", name+".txt")
		restackRun(t, f.Dir, "git", "commit", "-qm", name)
		restackRun(t, f.Dir, "gt", "track", "-f", "--no-interactive")
	}
	return f
}

// restackGTSync puts a gt ahead of the fixture's shim that answers gt sync from
// a recorded run and passes every other verb through to the real gt. sync is the
// one verb that cannot run here — it resolves the repository through Graphite's
// API — so its bytes come from the corpus rather than from a sentence anyone
// wrote. effect, when set, is the shell the recorded sync stands for: the local
// gt and git commands that leave behind the repository state the recorded run
// described. It returns the log of every sync argv served.
//
// The golden arrives already loaded because loadGTGolden reads a path relative
// to the package directory, which the fixture has already chdir'd out of.
func restackGTSync(t *testing.T, f *vcstest.Fixture, g gtGolden, effect string) string {
	t.Helper()
	dir := t.TempDir()
	stdout := filepath.Join(dir, "stdout")
	stderr := filepath.Join(dir, "stderr")
	syncLog := filepath.Join(dir, "sync.log")
	restackWrite(t, stdout, g.stdout)
	restackWrite(t, stderr, g.stderr)

	body := ""
	if effect != "" {
		body = "  if ! ( " + effect + " ) >/dev/null 2>&1; then printf 'restack test: sync effect failed\\n' >&2; exit 99; fi\n"
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = sync ]; then\n" +
		"  { for a in \"$@\"; do printf '%s\\0' \"$a\"; done; printf '\\0'; } >> '" + syncLog + "'\n" +
		body +
		"  cat '" + stdout + "'\n" +
		"  cat '" + stderr + "' >&2\n" +
		"  exit " + strconv.Itoa(g.exit) + "\n" +
		"fi\n" +
		"exec '" + filepath.Join(f.ShimBin, "gt") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(script), 0o700); err != nil { //nolint:gosec // the interceptor must be owner-executable to serve as a PATH entry
		t.Fatalf("write gt interceptor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return syncLog
}

// restackSyncArgv reads the argv of every gt sync the interceptor served.
func restackSyncArgv(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log) //nolint:gosec // log is the interceptor's own path under the test's TempDir
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read sync log: %v", err)
	}
	var records [][]string
	for _, record := range bytes.Split(data, []byte{0, 0}) {
		if len(record) == 0 {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(string(record), "\x00"), "\x00")
		records = append(records, fields)
	}
	return records
}

// TestRestackGTSyncArgv pins the one flag gt sync is given. --no-interactive has
// no observable of its own — it only keeps gt from prompting at a terminal no
// test has — so argv is the only place it can be held.
func TestRestackGTSyncArgv(t *testing.T) {
	g := loadGTGolden(t, "sync-quiet-exit0")
	f := restackGTRepo(t, "feat")
	syncLog := restackGTSync(t, f, g, "")

	if _, _, err := runRestackCmd(t); err != nil {
		t.Fatalf("restack: %v", err)
	}
	want := [][]string{{"sync", "--no-interactive"}}
	if got := restackSyncArgv(t, syncLog); !reflect.DeepEqual(got, want) {
		t.Fatalf("gt sync argv = %v, want %v", got, want)
	}
}

func TestRestackGTPerBranchVerdict(t *testing.T) {
	tests := []struct {
		name     string
		golden   string
		onTrunk  bool
		advance  bool
		want     string
		wantLead string
	}{
		{
			name:   "the stack landed on trunk",
			golden: "sync-quiet-exit0",
			want:   "restacked 1 of 1 · trunk main",
		},
		{
			name:    "a branch the sync left behind trunk",
			golden:  "sync-quiet-exit0",
			advance: true,
			want:    "restacked 0 of 1 · trunk main · skipped feat",
		},
		{
			name:   "gt declined a frozen branch already on trunk",
			golden: "restack-frozen",
			want:   "restacked 0 of 1 · trunk main · skipped feat (frozen; already on main)",
		},
		{
			name:    "gt declined a frozen branch that never reached trunk",
			golden:  "restack-frozen",
			advance: true,
			want:    "restacked 0 of 1 · trunk main · skipped feat (frozen)",
		},
		{
			name:     "gt named the working copy that blocked it",
			golden:   "restack-worktree-held",
			advance:  true,
			wantLead: "restacked 0 of 1 · trunk main · skipped feat (checked out in /",
		},
		{
			name:    "on trunk, nothing to restack",
			golden:  "sync-quiet-exit0",
			onTrunk: true,
			want:    "synced · trunk main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := loadGTGolden(t, tt.golden)
			f := restackGTRepo(t, "feat")
			if tt.advance {
				restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
				restackRun(t, f.Dir, "git", "fetch", "-q", "origin")
			}
			if tt.onTrunk {
				restackRun(t, f.Dir, "git", "switch", "-q", "main")
			}
			restackGTSync(t, f, g, "")

			out, _, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if tt.wantLead != "" {
				if !strings.HasPrefix(out, tt.wantLead) {
					t.Fatalf("output = %q, want it to lead with %q", out, tt.wantLead)
				}
				return
			}
			if out != tt.want {
				t.Fatalf("output = %q, want %q", out, tt.want)
			}
		})
	}
}

// TestRestackGTVerdictReadsTheSyncedStack pins the verdict to the stack gt sync
// left behind. Sync deletes the branches whose PRs landed and reparents their
// children, so a verdict over the pre-sync list asks git about a ref sync just
// deleted — merge-base exits 128 there, failing a restack that worked. The
// interceptor performs that deletion with gt's own local verbs, so the state the
// second gt state reads is one real gt wrote.
func TestRestackGTVerdictReadsTheSyncedStack(t *testing.T) {
	g := loadGTGolden(t, "sync-quiet-exit0")
	f := restackGTRepo(t, "a", "b")
	restackGTSync(t, f, g,
		"gt move --onto main --no-interactive && gt untrack a --no-interactive && git branch -D a")

	out, _, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "restacked 1 of 1 · trunk main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	for _, inv := range restackInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "merge-base" && inv[len(inv)-1] == "a" {
			t.Errorf("verdict probed %v — a is the branch the sync deleted", inv)
		}
	}
}

// TestRestackGTSurfacesSyncDiagnostics covers the half of a sync stdout alone
// cannot see. gt 1.8.6 splits one exit-0 sync across both streams — the phase
// banners on stdout, severity-led lines on stderr — so a restack it declined
// leaves the summary reporting the stack as behind with nothing saying why. The
// remediation rides out with the warnings: the unprefixed sentence gt puts a
// blank line below them is the only thing telling the user what to run. Tips are
// unprefixed stderr too, and are the gate's negative case — reporting them would
// make every fresh install noisy. Exit 0 stays a success, since the
// remote-trunk oracle already reports the stack correctly.
func TestRestackGTSurfacesSyncDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		golden  string
		wantErr []string
		denyErr []string
	}{
		{
			name:   "the warnings gt exited 0 on still reach the user",
			golden: "sync-decline-unstaged-exit0",
			wantErr: []string{
				"WARNING: Did not restack checked out branch feat due to conflicting unstaged changes.",
				"WARNING: feat could not be restacked cleanly.",
				"Please resolve conflicts in the current stack with gt restack.",
			},
		},
		{
			name:    "tips alone leave the report silent",
			golden:  "sync-tips-exit0",
			denyErr: []string{"tip:", "gt undo", "sync.explanation"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := loadGTGolden(t, tt.golden)
			f := restackGTRepo(t, "feat")
			restackGTSync(t, f, g, "")

			out, errOut, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if want := "restacked 1 of 1 · trunk main"; out != want {
				t.Fatalf("output = %q, want %q", out, want)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want it to carry %q", errOut, want)
				}
			}
			for _, deny := range tt.denyErr {
				if strings.Contains(errOut, deny) {
					t.Errorf("stderr = %q, want it to withhold %q", errOut, deny)
				}
			}
		})
	}
}

// TestRestackGTStreamedSyncPrintsDiagnosticsOnce guards the two arms against
// disagreeing the other way. The streaming arm already wires both of gt's
// streams to the writer as they are produced, so a diagnostic pass there would
// print every line the user just watched a second time.
func TestRestackGTStreamedSyncPrintsDiagnosticsOnce(t *testing.T) {
	g := loadGTGolden(t, "sync-decline-exit0")
	f := restackGTRepo(t, "feat")
	restackGTSync(t, f, g, "")
	old := shipStreamCI
	t.Cleanup(func() { shipStreamCI = old })
	shipStreamCI = func(io.Writer) bool { return true }

	out, errOut, err := runRestackCmd(t)
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if want := "restacked 1 of 1 · trunk main"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	line := "WARNING: feat could not be restacked cleanly."
	if got := strings.Count(errOut, line); got != 1 {
		t.Fatalf("diagnostic printed %d times in %q, want exactly 1", got, errOut)
	}
}

// TestRestackGTFailures pins the classifier to gt's own streams. The conflict
// banner rides stdout with stderr empty, while the auth refusal drives stderr
// instead, so the two together prove neither stream alone is enough. The last
// rows are sentences this package recognizes nothing in, where the default arm
// must carry gt's own words through verbatim. Every row also asserts gt's
// failure survives the advice that replaces its sentence, and that a failed sync
// leaves the working copy where it was.
func TestRestackGTFailures(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		line   string
		want   string
	}{
		{
			name:   "conflict",
			golden: "restack-conflict",
			line:   "Hit conflict restacking feat on main.",
			want:   "restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above",
		},
		{
			name:   "expired auth",
			golden: "sync-auth-invalid",
			line:   "ERROR: Your Graphite auth token is invalid/expired.",
			want:   "restack: graphite auth required — run gt auth",
		},
		{
			name:   "unrecognized failure carries gt's own words",
			golden: "sync-no-remote",
			line:   "ERROR: Could not determine the name of this repo",
		},
		{
			name:   "blocked during a rebase",
			golden: "restack-blocked-during-rebase",
			line:   "ERROR: This operation is blocked during a rebase.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := loadGTGolden(t, tt.golden)
			f := restackGTRepo(t, "feat")
			restackGTSync(t, f, g, "")
			before := restackRev(t, f.Dir, "HEAD")

			_, _, err := runRestackCmd(t)
			if err == nil {
				t.Fatal("restack succeeded on a failed sync, want failure")
			}
			var gerr *gtError
			if !errors.As(err, &gerr) {
				t.Fatalf("error = %q, want gt's own failure reachable through errors.As", err)
			}
			if !strings.Contains(gerr.Output, tt.line) {
				t.Fatalf("gtError.Output = %q, want it to carry gt's line %q", gerr.Output, tt.line)
			}
			switch {
			case tt.want != "":
				if err.Error() != tt.want {
					t.Fatalf("error = %q, want %q", err, tt.want)
				}
			default:
				if want := "restack: " + gerr.Error(); err.Error() != want {
					t.Fatalf("error = %q, want gt's failure wrapped verbatim as %q", err, want)
				}
			}
			if after := restackRev(t, f.Dir, "HEAD"); after != before {
				t.Errorf("HEAD moved from %s to %s on a failed sync", before, after)
			}
		})
	}
}

// TestRestackGTRefusesMissingRemoteTrunk pins the verdict's own precondition: it
// measures the stack against the remote-tracking trunk, so a repository that has
// none is a refusal rather than a verdict measured against whatever the name
// resolves to locally.
func TestRestackGTRefusesMissingRemoteTrunk(t *testing.T) {
	g := loadGTGolden(t, "sync-quiet-exit0")
	f := restackGTRepo(t, "feat")
	restackRun(t, f.Dir, "git", "update-ref", "-d", "refs/remotes/origin/main")
	restackGTSync(t, f, g, "")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded without a remote-tracking trunk, want a refusal")
	}
	if !errors.Is(err, vcs.ErrNoTrunk) {
		t.Errorf("error = %v, want it to reach vcs.ErrNoTrunk", err)
	}
	want := "restack: refs/remotes/origin/main: " + vcs.ErrNoTrunk.Error() + " — run git fetch origin main"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestRestackGTRefusesBranchHeldElsewhere pins the preflight. git will not move a
// branch a sibling checkout holds, so gt skips it and still exits 0; refusing
// first is the only way the user learns which working copy to go to. The sync
// log proves the refusal landed before gt ran, which the final state cannot
// show.
func TestRestackGTRefusesBranchHeldElsewhere(t *testing.T) {
	g := loadGTGolden(t, "sync-quiet-exit0")
	f := restackGTRepo(t, "a", "b")
	held := restackSiblingPath(t, "held")
	restackRun(t, f.Dir, "git", "worktree", "add", "-q", held, "a")
	syncLog := restackGTSync(t, f, g, "")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want a refusal naming the holder")
	}
	want := "restack: a is checked out in " + held + " — gt cannot restack a branch another working copy holds; restack from there, or release it first"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if got := restackSyncArgv(t, syncLog); got != nil {
		t.Errorf("gt sync ran %v — the preflight must refuse before it does", got)
	}
}

// TestRestackGTNamesTheWorkingCopyHoldingTrunk covers the reason the stale-trunk
// summary otherwise withholds. gt declines nothing when it cannot pull a held
// trunk, so the stack reads as behind with no cause attached; the holder is the
// cause, and BranchHolders already has it. It names only a holder git reports —
// an unheld trunk stays silent rather than assert a working copy that is not
// there.
func TestRestackGTNamesTheWorkingCopyHoldingTrunk(t *testing.T) {
	tests := []struct {
		name string
		held bool
	}{
		{name: "a sibling working copy holds trunk", held: true},
		{name: "git names no holder for trunk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := loadGTGolden(t, "sync-quiet-exit0")
			f := restackGTRepo(t, "feat")
			restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
			restackRun(t, f.Dir, "git", "fetch", "-q", "origin")
			want := "restacked 0 of 1 · trunk main · skipped feat"
			if tt.held {
				held := restackSiblingPath(t, "trunk")
				restackRun(t, f.Dir, "git", "worktree", "add", "-q", held, "main")
				want = "restacked 0 of 1 · trunk main (checked out in " + held + ") · skipped feat"
			}
			restackGTSync(t, f, g, "")

			out, _, err := runRestackCmd(t)
			if err != nil {
				t.Fatalf("restack: %v", err)
			}
			if out != want {
				t.Fatalf("output = %q, want %q", out, want)
			}
		})
	}
}

func TestRestackGraphiteFirst(t *testing.T) {
	t.Run("colocated routes to gt", func(t *testing.T) {
		g := loadGTGolden(t, "sync-quiet-exit0")
		f := restackGTRepo(t, "feat")
		restackGTSync(t, f, g, "")

		out, _, err := runRestackCmd(t)
		if err != nil {
			t.Fatalf("restack: %v", err)
		}
		if want := "restacked 1 of 1 · trunk main"; out != want {
			t.Fatalf("output = %q, want %q", out, want)
		}
	})

	t.Run("no gt routes to the vcs lane", func(t *testing.T) {
		f := restackGTRepo(t, "feat")
		restackReset(t, f)

		out, _, err := runRestackCmd(t, "--no-gt")
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
	vcstest.LinkPATH(t, "git")

	_, _, err := runRestackCmd(t)
	if err == nil {
		t.Fatal("restack succeeded, want missing-gt refusal")
	}
	want := "restack: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	assertNoRestackMutation(t, restackInvocations(t, f))
}

func TestRestackRegisteredWithRebaseAlias(t *testing.T) {
	t.Parallel()
	cmd := newVcsCmd()
	found, args, err := cmd.Find([]string{"rebase"})
	if err != nil {
		t.Fatalf("find rebase: %v", err)
	}
	if found.Name() != "restack" || len(args) != 0 {
		t.Fatalf("find rebase = %s %#v, want restack", found.Name(), args)
	}
}
