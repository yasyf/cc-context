package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestTrunkFetchWaitsOutAnotherWorktreesRefLock is breakglass-land: a fetch in
// another worktree of the shared clone held refs/remotes/origin/dev, and stack
// rebase refused "cannot lock ref 'refs/remotes/origin/dev'".
func TestTrunkFetchWaitsOutAnotherWorktreesRefLock(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	lock := filepath.Join(f.Dir, ".git", "refs", "remotes", "origin", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released := time.AfterFunc(1500*time.Millisecond, func() { _ = os.Remove(lock) })
	defer released.Stop()

	tr, err := gtTrunkRef(f.Context(), render.Dir(f.Dir), "stack rebase", "main")
	if err != nil {
		t.Fatalf("trunk fetch under a held ref lock = %v", err)
	}
	if got, want := gitAt(t, f.Env(), f.Dir, "rev-parse", string(tr.Ref())), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"); got != want {
		t.Errorf("%s = %s, want the remote trunk %s", tr.Ref(), got, want)
	}
}

// holdCommitGraphLock is another worktree's fetch of a shared clone, mid-write
// of the commit-graph under fetch.writeCommitGraph, which the monorepo's clone
// sets: concurrent stack submits refused "Unable to create
// '.git/objects/info/commit-graphs/commit-graph-chain.lock': File exists".
func holdCommitGraphLock(t *testing.T, f *vcstest.Fixture, dir string) {
	t.Helper()
	mustRun(t, f.Env(), dir, "git", "config", "fetch.writeCommitGraph", "true")
	graphs := filepath.Join(dir, ".git", "objects", "info", "commit-graphs")
	if err := os.MkdirAll(graphs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphs, "commit-graph-chain.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrunkFetchSkipsAnotherWorktreesCommitGraphLock(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	holdCommitGraphLock(t, f, f.Dir)

	tr, err := gtTrunkRef(f.Context(), render.Dir(f.Dir), "stack rebase", "main")
	if err != nil {
		t.Fatalf("trunk fetch under a held commit-graph lock = %v", err)
	}
	if got, want := gitAt(t, f.Env(), f.Dir, "rev-parse", string(tr.Ref())), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"); got != want {
		t.Errorf("%s = %s, want the remote trunk %s", tr.Ref(), got, want)
	}
}

func TestRestackJJFetchSkipsAnotherWorktreesCommitGraphLock(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ(), vcstest.Remote())
	f.Isolate(t)
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	holdCommitGraphLock(t, f, f.Dir)

	if _, _, err := runRestackCmd(t, f); err != nil {
		t.Fatalf("restack under a held commit-graph lock: %v", err)
	}
	if onTrunk := restackRun(t, f, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) == "" {
		t.Error("@ does not descend from the fetched trunk()")
	}
}

func TestAlignInAPromisorCloneSkipsAnotherWorktreesCommitGraphLock(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	upstream := filepath.Join(t.TempDir(), "upstream")
	mustRun(t, f.Env(), filepath.Dir(upstream), "git", "clone", "-q", "--branch", "main", f.RemoteDir, upstream)
	mustRun(t, f.Env(), upstream, "git", "config", "user.email", "t@t.t")
	mustRun(t, f.Env(), upstream, "git", "config", "user.name", "t")
	writeShipFile(t, upstream, "gone.txt", "gone\n")
	writeShipFile(t, upstream, "kept.txt", "old\n")
	mustRun(t, f.Env(), upstream, "git", "add", "gone.txt", "kept.txt")
	mustRun(t, f.Env(), upstream, "git", "commit", "-qm", "previous")
	mustRun(t, f.Env(), upstream, "git", "push", "-q", "origin", "main")
	mustRun(t, f.Env(), upstream, "git", "rm", "-q", "gone.txt")
	writeShipFile(t, upstream, "kept.txt", "new\n")
	mustRun(t, f.Env(), upstream, "git", "commit", "-qam", "published")
	mustRun(t, f.Env(), upstream, "git", "push", "-q", "origin", "HEAD:refs/heads/published")
	head := gitAt(t, f.Env(), upstream, "rev-parse", "HEAD")
	mustRun(t, f.Env(), f.RemoteDir, "git", "config", "uploadpack.allowFilter", "true")
	mustRun(t, f.Env(), f.RemoteDir, "git", "config", "uploadpack.allowAnySHA1InWant", "true")

	holder := filepath.Join(t.TempDir(), "partial")
	mustRun(t, f.Env(), filepath.Dir(holder), "git", "clone", "-q", "--no-local", "--filter=blob:none", "--branch", "main", f.RemoteDir, holder)
	mustRun(t, f.Env(), holder, "git", "-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "outside the commit-graph")
	previous := gitAt(t, f.Env(), holder, "rev-parse", "HEAD")
	holdCommitGraphLock(t, f, holder)

	if err := gtRestackAlignHolder(f.Context(), holder, restackMove{branch: "main", previous: previous, head: head}); err != nil {
		t.Fatalf("align under a held commit-graph lock = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(holder, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt = %v, want it removed", err)
	}
	if got, err := os.ReadFile(filepath.Join(holder, "kept.txt")); err != nil || string(got) != "new\n" {
		t.Errorf("kept.txt = %q, %v, want the published head's new", got, err)
	}
}
