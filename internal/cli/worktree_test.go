package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func runWorktreeCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newWorktreeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// worktreeTempDir is t.TempDir() resolved symlink-free — the spelling git writes
// into its own registry and ResolveCheckout canonicalizes every root to, so a
// test can compare the two without a second canonicalization at every assertion.
func worktreeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// addPoolWorktree mints the working copy named name through ccx and returns the
// path its summary reports.
func addPoolWorktree(t *testing.T, name string) string {
	t.Helper()
	out, err := runWorktreeCmd(t, "add", name)
	if err != nil {
		t.Fatalf("add %s error = %v", name, err)
	}
	return worktreeSummaryPath(t, out)
}

// worktreeRegistered reports whether git's own registry still carries path.
func worktreeRegistered(t *testing.T, repo, path string) bool {
	t.Helper()
	return strings.Contains(mustRun(t, repo, "git", "worktree", "list", "--porcelain"), "worktree "+path+"\n")
}

// addLinkedWorktree cuts a linked worktree at path, creating the pool directory
// git itself will not. An empty commitish lets git name a new branch after the
// path's own basename.
func addLinkedWorktree(t *testing.T, repo, path, commitish string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir pool: %v", err)
	}
	argv := []string{"worktree", "add", "-q", path}
	if commitish != "" {
		argv = append(argv, commitish)
	}
	mustRun(t, repo, "git", argv...)
}

func TestWorktreeListHealthy(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("feat"))
	dir := f.Dir
	linked := f.WorktreePath("feat")

	out, err := runWorktreeCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	var got worktreeReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if got.Root != dir || got.RepoKey != filepath.Join(dir, ".git") {
		t.Errorf("root/repo_key = %q/%q, want %q/%q", got.Root, got.RepoKey, dir, filepath.Join(dir, ".git"))
	}
	want := []worktreeEntry{
		{Path: dir, Shape: "main", Branch: "main", Current: true},
		{Path: linked, Shape: "git worktree", Branch: "feat"},
	}
	if len(got.Worktrees) != len(want) {
		t.Fatalf("worktrees = %+v, want %+v", got.Worktrees, want)
	}
	for i, w := range want {
		if got.Worktrees[i] != w {
			t.Errorf("worktree %d = %+v, want %+v", i, got.Worktrees[i], w)
		}
	}

	human, err := runWorktreeCmd(t, "list")
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	wantHuman := "* " + dir + " · main · main\n  " + linked + " · git worktree · feat\n"
	if human != wantHuman {
		t.Errorf("listing =\n%s\nwant\n%s", human, wantHuman)
	}
}

// TestWorktreeListDanglingCheckout proves the listing survives the working copy
// whose own gitdir pointer resolves to nothing: that diagnosis is what someone
// runs list to get, so it lands in the report at exit 0 rather than as a failure.
func TestWorktreeListDanglingCheckout(t *testing.T) {
	vcstest.Repo(t, vcstest.BrokenGitDir())

	out, err := runWorktreeCmd(t, "list")
	if err != nil {
		t.Fatalf("list exited non-zero over a dangling pointer: %v", err)
	}
	for _, want := range []string{"checkout", "gitdir pointer resolves to nothing", "/nonexistent-repo"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing = %q, want it to contain %q", out, want)
		}
	}
}

// TestWorktreeListReportsBrokenSibling proves a defect in a *listed* worktree is
// reported inline against an otherwise healthy repository, still at exit 0: a
// relocated repository leaves every linked .git file naming the old location.
func TestWorktreeListReportsBrokenSibling(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("orphan"))
	linked := f.WorktreePath("orphan")
	relocated := filepath.Join(filepath.Dir(f.Dir), "relocated")
	if err := os.Rename(f.Dir, relocated); err != nil {
		t.Fatalf("relocate repository: %v", err)
	}
	t.Chdir(relocated)

	out, err := runWorktreeCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list exited non-zero over a broken sibling: %v", err)
	}
	var got worktreeReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, out)
	}
	if len(got.Worktrees) != 2 {
		t.Fatalf("worktrees = %+v, want the repository and its orphan", got.Worktrees)
	}
	orphan := got.Worktrees[1]
	if orphan.Path != linked {
		t.Fatalf("second entry = %+v, want %q", orphan, linked)
	}
	if !strings.Contains(orphan.Defect, "gitdir pointer resolves to nothing") {
		t.Errorf("defect = %q, want it to name the dangling pointer", orphan.Defect)
	}
	if orphan.Shape != "" {
		t.Errorf("shape = %q, want none — the checkout never resolved", orphan.Shape)
	}
}

func TestWorktreeAddRmRoundTrip(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	dir := f.Dir

	out, err := runWorktreeCmd(t, "add", "feat")
	if err != nil {
		t.Fatalf("add error = %v", err)
	}
	path := worktreeSummaryPath(t, out)
	pool := filepath.Dir(path)
	if want := filepath.Join(os.Getenv("HOME"), ".claude", "worktrees"); filepath.Dir(pool) != want {
		t.Errorf("minted %q, want it under %q", path, want)
	}
	if filepath.Base(path) != "feat" {
		t.Errorf("minted %q, want it to end in the name", path)
	}
	if got, want := filepath.Base(pool), filepath.Base(dir); got != want {
		t.Errorf("pool = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(path, "f.txt")); err != nil {
		t.Errorf("minted worktree has no checkout: %v", err)
	}
	if got := strings.TrimSpace(mustRun(t, dir, "git", "branch", "--list", "feat")); got == "" {
		t.Errorf("git branch --list feat is empty, want the branch add named after the worktree")
	}
	listed, err := runWorktreeCmd(t, "list")
	if err != nil {
		t.Fatalf("list error = %v", err)
	}
	if !strings.Contains(listed, path+" · git worktree · feat") {
		t.Errorf("listing = %q, want it to carry the added worktree", listed)
	}

	if _, err := runWorktreeCmd(t, "rm", "feat"); err != nil {
		t.Fatalf("rm error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s after rm = %v, want the tree gone", path, err)
	}
	if worktreeRegistered(t, dir, path) {
		t.Errorf("git still registers %s, want it deregistered", path)
	}
}

// TestWorktreeRmRefusesTrunkHolder proves the one removal that is never safe:
// the checkout holding trunk pins the branch every restack rebases onto, and the
// refusal names it rather than reporting a bare failure.
func TestWorktreeRmRefusesTrunkHolder(t *testing.T) {
	// Detached: git refuses to check a branch out twice, so trunk moves to the
	// pool worktree only once the repository's own working copy lets go of it.
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Detached())
	path := addPoolWorktree(t, "holder")
	mustRun(t, path, "git", "checkout", "-q", "main")

	_, err := runWorktreeCmd(t, "rm", "holder")
	if err == nil {
		t.Fatal("rm removed the checkout holding trunk")
	}
	for _, want := range []string{path, "holds trunk main"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(path, "f.txt")); statErr != nil {
		t.Errorf("the refused worktree was removed anyway: %v", statErr)
	}
	if !worktreeRegistered(t, f.Dir, path) {
		t.Errorf("git no longer registers %s, want the refusal to have changed nothing", path)
	}
}

// TestWorktreeRmSurfacesUnshapedRemoteHead proves the no-trunk skip is the
// provable miss alone, never a lookup that answered. A remote HEAD may legally
// be aimed at any ref, and one naming a local branch is a misconfiguration git
// answered — reporting it as "no trunk" would skip the guard over the very
// branch the pool worktree holds.
func TestWorktreeRmSurfacesUnshapedRemoteHead(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Detached())
	path := addPoolWorktree(t, "holder")
	mustRun(t, path, "git", "checkout", "-q", "main")
	mustRun(t, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/main")

	_, err := runWorktreeCmd(t, "rm", "holder")
	if err == nil {
		t.Fatal("rm removed the checkout holding trunk")
	}
	for _, want := range []string{"refs/remotes/origin/HEAD", `"refs/heads/main"`, "git remote set-head origin -a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(path, "f.txt")); statErr != nil {
		t.Errorf("the refused worktree was removed anyway: %v", statErr)
	}
	if !worktreeRegistered(t, f.Dir, path) {
		t.Errorf("git no longer registers %s, want the refusal to have changed nothing", path)
	}
}

// TestWorktreeRmNoTrunkSkipsHolderGuard proves the add → rm round trip closes
// in a repository where no trunk resolves: with no remote there is nothing
// restack could rebase onto, so the trunk-holder guard has nothing to protect
// and rm skips it instead of refusing over a branch that does not exist.
func TestWorktreeRmNoTrunkSkipsHolderGuard(t *testing.T) {
	f := vcstest.Repo(t)
	path := addPoolWorktree(t, "feat")

	if _, err := runWorktreeCmd(t, "rm", "feat"); err != nil {
		t.Fatalf("rm error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after rm = %v, want the tree gone", path, statErr)
	}
	if worktreeRegistered(t, f.Dir, path) {
		t.Errorf("git still registers %s, want it deregistered", path)
	}
}

// TestWorktreeRmNoOriginHeadSkipsHolderGuard proves the skip covers the
// repository that has a remote and refs under it but designates no default
// branch — the state git remote add leaves until set-head runs. There is no
// trunk to guard even though origin/main exists, so the checkout holding main
// removes rather than being refused over a branch nothing rebases onto.
func TestWorktreeRmNoOriginHeadSkipsHolderGuard(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.NoOriginHead(), vcstest.Detached())
	path := addPoolWorktree(t, "holder")
	mustRun(t, path, "git", "checkout", "-q", "main")

	if _, err := runWorktreeCmd(t, "rm", "holder"); err != nil {
		t.Fatalf("rm error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after rm = %v, want the tree gone", path, statErr)
	}
	if worktreeRegistered(t, f.Dir, path) {
		t.Errorf("git still registers %s, want it deregistered", path)
	}
	mustRun(t, f.Dir, "git", "rev-parse", "--verify", "refs/remotes/origin/main")
}

// TestWorktreeRmSurfacesTrunkLookupFailure proves the no-trunk skip never
// swallows a genuine failure: a git that cannot read its refs at all is not a
// repository without a trunk, and rm surfaces the failure rather than removing
// on a guess. A corrupt packed-refs file is the live lever — git worktree list
// survives it, so the rm reaches trunk resolution, where the origin/HEAD lookup
// falls through to packed-refs and dies. The same fixture uncorrupted is
// TestWorktreeRmNoTrunkSkipsHolderGuard, where the removal closes.
func TestWorktreeRmSurfacesTrunkLookupFailure(t *testing.T) {
	f := vcstest.Repo(t)
	path := addPoolWorktree(t, "feat")
	if err := os.WriteFile(filepath.Join(f.Dir, ".git", "packed-refs"), []byte("garbage not a packed-refs line\n"), 0o600); err != nil {
		t.Fatalf("corrupt packed-refs: %v", err)
	}

	_, err := runWorktreeCmd(t, "rm", "feat")
	if err == nil {
		t.Fatal("rm treated a failing trunk lookup as no trunk")
	}
	if errors.Is(err, vcs.ErrNoTrunk) {
		t.Errorf("error = %v, want a failure, not %v", err, vcs.ErrNoTrunk)
	}
	for _, want := range []string{"symbolic-ref", "packed-refs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to carry git's own verdict via %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(path, "f.txt")); statErr != nil {
		t.Errorf("the refused worktree was removed anyway: %v", statErr)
	}
}

// TestWorktreeRmSurfacesBrokenCheckout proves a git that cannot open the
// repository at all is never the clean miss rm reads as "no such name": the
// diagnosis surfaces instead, so nothing is removed on a checkout whose own
// gitdir pointer resolves to nothing.
func TestWorktreeRmSurfacesBrokenCheckout(t *testing.T) {
	f := vcstest.Repo(t, vcstest.BrokenGitDir())

	_, err := runWorktreeCmd(t, "rm", "feat")
	if err == nil {
		t.Fatal("rm claimed success in a checkout git cannot open")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want the resolution failure, not %v", err, ErrNotFound)
	}
	for _, want := range []string{"gitdir pointer resolves to nothing", "/nonexistent-repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(f.Dir, ".git")); statErr != nil {
		t.Errorf("the broken checkout was disturbed: %v", statErr)
	}
}

// TestWorktreeRmRefusesForeignCheckout proves rm never resolves a name to a
// checkout ccx did not mint: a hand-made worktree sharing the name's basename
// is refused — --force included, since force discards changes, not ownership —
// with the refusal naming the tree and pointing at git worktree remove.
func TestWorktreeRmRefusesForeignCheckout(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	dir := f.Dir
	foreign := filepath.Join(filepath.Dir(dir), "checkouts", "feat")
	addLinkedWorktree(t, dir, foreign, "")

	for _, args := range [][]string{{"rm", "feat"}, {"rm", "feat", "--force"}} {
		_, err := runWorktreeCmd(t, args...)
		if err == nil {
			t.Fatalf("%v removed a checkout ccx never minted", args)
		}
		for _, want := range []string{foreign, "git worktree remove"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v error = %v, want it to contain %q", args, err, want)
			}
		}
		if _, statErr := os.Stat(filepath.Join(foreign, "f.txt")); statErr != nil {
			t.Fatalf("%v removed the foreign checkout: %v", args, statErr)
		}
	}
	if got := mustRun(t, dir, "git", "worktree", "list", "--porcelain"); !strings.Contains(got, "worktree "+foreign) {
		t.Errorf("worktree list = %q, want the foreign checkout still registered", got)
	}
}

// TestWorktreeRmPoolBesideNameTwin proves rm resolves a name to the pool path
// alone: with a minted "feat" and a hand-made "feat" both registered, rm
// removes the pool one and never touches its twin.
func TestWorktreeRmPoolBesideNameTwin(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	dir := f.Dir
	sha := strings.TrimSpace(mustRun(t, dir, "git", "rev-parse", "HEAD"))
	twin := filepath.Join(filepath.Dir(dir), "checkouts", "feat")
	addLinkedWorktree(t, dir, twin, sha)
	path := addPoolWorktree(t, "feat")

	if _, err := runWorktreeCmd(t, "rm", "feat"); err != nil {
		t.Fatalf("rm error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after rm = %v, want the pool worktree gone", path, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(twin, "f.txt")); statErr != nil {
		t.Errorf("rm reached the name twin outside the pool: %v", statErr)
	}
	if !worktreeRegistered(t, dir, twin) {
		t.Errorf("git no longer registers %s, want the name twin untouched", twin)
	}
}

// TestWorktreeRmJJWorkspace proves the jj path carries git's dirty-tree
// semantics: a workspace holding a never-snapshotted file is refused with the
// change named, --force discards it, and a clean workspace removes plain.
func TestWorktreeRmJJWorkspace(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.JJ()).Dir

	out, err := runWorktreeCmd(t, "add", "feat", "--jj", "workspace")
	if err != nil {
		t.Fatalf("add error = %v", err)
	}
	path := worktreeSummaryPath(t, out)
	unsaved := filepath.Join(path, "unsaved.txt")
	if err := os.WriteFile(unsaved, []byte("precious\n"), 0o600); err != nil {
		t.Fatalf("write unsaved.txt: %v", err)
	}

	_, err = runWorktreeCmd(t, "rm", "feat")
	if err == nil {
		t.Fatal("rm destroyed a workspace holding unsnapshotted work")
	}
	for _, want := range []string{path, "uncommitted changes", "A unsaved.txt", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if got := readFileStr(t, unsaved); got != "precious\n" {
		t.Errorf("unsaved.txt = %q after the refusal, want it untouched", got)
	}

	if _, err := runWorktreeCmd(t, "rm", "feat", "--force"); err != nil {
		t.Fatalf("rm --force error = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after rm --force = %v, want the tree gone", path, statErr)
	}
	if got := mustRun(t, dir, "jj", "workspace", "list"); strings.Contains(got, "feat") {
		t.Errorf("jj workspace list = %q, want feat forgotten", got)
	}

	out, err = runWorktreeCmd(t, "add", "tidy", "--jj", "workspace")
	if err != nil {
		t.Fatalf("add error = %v", err)
	}
	tidy := worktreeSummaryPath(t, out)
	if _, err := runWorktreeCmd(t, "rm", "tidy"); err != nil {
		t.Fatalf("rm of a clean workspace error = %v", err)
	}
	if _, statErr := os.Stat(tidy); !os.IsNotExist(statErr) {
		t.Errorf("stat %s after rm = %v, want the tree gone", tidy, statErr)
	}
}

// TestWorktreeRmSurfacesBrokenWorkspacePointer proves the "not a jj workspace"
// answer is the clean miss alone, never a resolution that failed. A workspace
// whose .jj/repo was truncated mid-write is still registered with jj and still
// on disk, so reporting it as a name this repository never minted would send the
// user to delete by hand the tree rm would have forgotten from jj first.
func TestWorktreeRmSurfacesBrokenWorkspacePointer(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.JJ()).Dir

	_, err := runWorktreeCmd(t, "rm", "never-minted")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("rm of an unminted name error = %v, want %v", err, ErrNotFound)
	}
	if !strings.Contains(err.Error(), `no working copy named "never-minted"`) {
		t.Errorf("error = %v, want the clean miss to name the working copy", err)
	}

	out, err := runWorktreeCmd(t, "add", "feat", "--jj", "workspace")
	if err != nil {
		t.Fatalf("add error = %v", err)
	}
	path := worktreeSummaryPath(t, out)
	pointer := filepath.Join(path, ".jj", "repo")
	if err := os.WriteFile(pointer, nil, 0o600); err != nil {
		t.Fatalf("truncate workspace pointer: %v", err)
	}

	_, err = runWorktreeCmd(t, "rm", "feat")
	if err == nil {
		t.Fatal("rm claimed success over a workspace whose pointer resolves to nothing")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want the resolution failure, not %v", err, ErrNotFound)
	}
	for _, want := range []string{path, "jj workspace pointer is empty", pointer} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(path, "f.txt")); statErr != nil {
		t.Errorf("the unresolvable workspace was removed anyway: %v", statErr)
	}
	if got := mustRun(t, dir, "jj", "workspace", "list"); !strings.Contains(got, "feat") {
		t.Errorf("jj workspace list = %q, want feat still registered", got)
	}
}

// TestWorktreeRepairDryRun proves --dry-run prints the invocation and changes
// nothing, and that the invocation it printed is the one that actually re-points
// a worktree orphaned by a relocated repository.
func TestWorktreeRepairDryRun(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("moved"))
	linked := f.WorktreePath("moved")
	relocated := filepath.Join(filepath.Dir(f.Dir), "relocated")
	if err := os.Rename(f.Dir, relocated); err != nil {
		t.Fatalf("relocate repository: %v", err)
	}
	t.Chdir(relocated)

	pointer := filepath.Join(linked, ".git")
	before := readFileStr(t, pointer)
	if _, err := runGit(linked, "status", "--porcelain"); err == nil {
		t.Fatal("precondition: relocating the repository should have orphaned the worktree")
	}

	out, err := runWorktreeCmd(t, "repair", "--dry-run")
	if err != nil {
		t.Fatalf("repair --dry-run error = %v", err)
	}
	want := "dry-run · git -C " + relocated + " worktree repair " + relocated + " " + linked + "\n"
	if out != want {
		t.Errorf("repair --dry-run = %q, want %q", out, want)
	}
	if got := readFileStr(t, pointer); got != before {
		t.Errorf("--dry-run rewrote the pointer to %q, want it left at %q", got, before)
	}

	out, err = runWorktreeCmd(t, "repair")
	if err != nil {
		t.Fatalf("repair error = %v", err)
	}
	if !strings.Contains(out, linked) || !strings.Contains(out, "2 working copies checked") {
		t.Errorf("repair = %q, want it to name the repaired worktree and the checked count", out)
	}
	if got := readFileStr(t, pointer); got == before {
		t.Errorf("repair left the pointer at %q", got)
	}
	if _, err := runGit(linked, "status", "--porcelain"); err != nil {
		t.Errorf("worktree still orphaned after repair: %v", err)
	}
}

// TestWorktreeRepairFromBrokenCheckout proves repair run from inside the broken
// tree reaches the repository its own dangling pointer names, and reports git's
// verdict when the admin dir behind that pointer is gone rather than misplaced —
// the shape that leaves a checkout diagnosable but unrepairable in place.
func TestWorktreeRepairFromBrokenCheckout(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("orphan"))
	dir, linked := f.Dir, f.WorktreePath("orphan")
	if err := os.RemoveAll(filepath.Join(dir, ".git", "worktrees", "orphan")); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	t.Chdir(linked)

	out, err := runWorktreeCmd(t, "repair", "--dry-run")
	if err != nil {
		t.Fatalf("repair --dry-run error = %v", err)
	}
	want := "dry-run · git -C " + dir + " worktree repair " + linked + "\n"
	if out != want {
		t.Errorf("repair --dry-run = %q, want %q", out, want)
	}

	_, err = runWorktreeCmd(t, "repair")
	if err == nil {
		t.Fatal("repair claimed success with the admin dir deleted")
	}
	if !strings.Contains(err.Error(), "unable to locate repository") {
		t.Errorf("error = %v, want git's own verdict on the missing admin dir", err)
	}
}

func TestWorktreeAddColocateRefused(t *testing.T) {
	vcstest.Repo(t)

	_, err := runWorktreeCmd(t, "add", "feat", "--jj", "colocate")
	if err == nil {
		t.Fatal("--jj colocate was accepted")
	}
	for _, want := range []string{
		"Error: Cannot create a colocated jj repo inside a Git worktree.",
		"Hint: Run `jj git init` in the main Git repository instead, or use `jj workspace add` to create additional jj workspaces.",
		"--jj workspace",
		"--jj none",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(os.Getenv("HOME"), ".ccx")); !os.IsNotExist(statErr) {
		t.Errorf("stat the pool after a refused add = %v, want it never minted", statErr)
	}
}

func TestWorktreeMode(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		checkout  vcs.Checkout
		want      string
		wantErr   string
	}{
		{"default in a git repository", "", vcs.Checkout{Kind: vcs.Git, Shape: vcs.ShapeMain}, jjModeNone, ""},
		{"default in a colocated repository", "", vcs.Checkout{Kind: vcs.JJ, Shape: vcs.ShapeMain}, jjModeNone, ""},
		{"default in a jj workspace", "", vcs.Checkout{Kind: vcs.JJ, Shape: vcs.ShapeJJWorkspace}, jjModeWorkspace, ""},
		{"workspace off a colocated repository", jjModeWorkspace, vcs.Checkout{Kind: vcs.JJ, Shape: vcs.ShapeMain}, jjModeWorkspace, ""},
		{"workspace in a git repository", jjModeWorkspace, vcs.Checkout{Kind: vcs.Git, Shape: vcs.ShapeMain}, "", "--jj workspace needs a jj repository"},
		{"none in a jj workspace", jjModeNone, vcs.Checkout{Kind: vcs.JJ, Shape: vcs.ShapeJJWorkspace}, "", "--jj none needs a git working copy"},
		{"colocate anywhere", jjModeColocate, vcs.Checkout{Kind: vcs.JJ, Shape: vcs.ShapeMain}, "", "Cannot create a colocated jj repo inside a Git worktree."},
		{"unknown mode", "hybrid", vcs.Checkout{Kind: vcs.Git, Shape: vcs.ShapeMain}, "", `unknown --jj mode "hybrid"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := worktreeMode(tt.requested, tt.checkout)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("worktreeMode(%q) error = %v", tt.requested, err)
				}
				if got != tt.want {
					t.Errorf("worktreeMode(%q) = %q, want %q", tt.requested, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("worktreeMode(%q) = %q, want an error containing %q", tt.requested, got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("worktreeMode(%q) error = %v, want it to contain %q", tt.requested, err, tt.wantErr)
			}
		})
	}
}

// TestWorktreeMintPathPool proves the pool is keyed by the main root's
// basename: every sibling checkout of one repository mints into the same
// directory, a differently named repository gets its own, and a same-named
// repository elsewhere collides into that pool too — the accepted trade for
// dropping the repo-key suffix.
func TestWorktreeMintPathPool(t *testing.T) {
	home := worktreeTempDir(t)
	t.Setenv("HOME", home)
	main := vcs.Checkout{Root: "/w/cc-context", Shape: vcs.ShapeMain, MainRoot: "/w/cc-context", CommonDir: "/w/cc-context/.git"}
	sibling := vcs.Checkout{Root: "/w/wt", Shape: vcs.ShapeGitWorktree, MainRoot: "/w/cc-context", CommonDir: "/w/cc-context/.git"}
	elsewhere := vcs.Checkout{Root: "/o/other-repo", Shape: vcs.ShapeMain, MainRoot: "/o/other-repo", CommonDir: "/o/other-repo/.git"}
	sameName := vcs.Checkout{Root: "/o/cc-context", Shape: vcs.ShapeMain, MainRoot: "/o/cc-context", CommonDir: "/o/cc-context/.git"}

	got, err := mintWorktreePath("t", main, "feat")
	if err != nil {
		t.Fatalf("mintWorktreePath: %v", err)
	}
	pool := filepath.Dir(got)
	if want := filepath.Join(home, ".claude", "worktrees"); filepath.Dir(pool) != want {
		t.Errorf("minted %q, want it under %q", got, want)
	}
	if base := filepath.Base(pool); base != "cc-context" {
		t.Errorf("pool = %q, want %q", base, "cc-context")
	}
	sib, err := mintWorktreePath("t", sibling, "feat")
	if err != nil {
		t.Fatalf("mintWorktreePath: %v", err)
	}
	if sib != got {
		t.Errorf("sibling minted %q, want the repository's own pool %q", sib, got)
	}
	other, err := mintWorktreePath("t", elsewhere, "feat")
	if err != nil {
		t.Fatalf("mintWorktreePath: %v", err)
	}
	if other == got {
		t.Errorf("a differently named repository minted %q, want a distinct pool", other)
	}
	same, err := mintWorktreePath("t", sameName, "feat")
	if err != nil {
		t.Fatalf("mintWorktreePath: %v", err)
	}
	if same != got {
		t.Errorf("a same-named repository elsewhere minted %q, want the shared pool %q", same, got)
	}
}

func TestWorktreeMintPathRejectsName(t *testing.T) {
	t.Setenv("HOME", worktreeTempDir(t))
	c := vcs.Checkout{Root: "/w/cc-context", Shape: vcs.ShapeMain, MainRoot: "/w/cc-context", CommonDir: "/w/cc-context/.git"}
	tests := []struct {
		name  string
		given string
	}{
		{"empty", ""},
		{"self", "."},
		{"parent", ".."},
		{"nested", "a/b"},
		{"escaping", "../x"},
		{"absolute", "/tmp/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mintWorktreePath("t", c, tt.given)
			if err == nil {
				t.Fatalf("mintWorktreePath(%q) = %q, want a refusal", tt.given, got)
			}
			if !strings.Contains(err.Error(), "is one path element") {
				t.Errorf("mintWorktreePath(%q) error = %v, want the name rule", tt.given, err)
			}
		})
	}
}

// worktreeSummaryPath reads the path off an add/rm summary line, whose last
// segment is the working copy the verb acted on.
func worktreeSummaryPath(t *testing.T, summary string) string {
	t.Helper()
	segs := strings.Split(strings.TrimSpace(summary), shipSep)
	if len(segs) != 3 {
		t.Fatalf("summary = %q, want three %q-separated segments", summary, shipSep)
	}
	return segs[2]
}
