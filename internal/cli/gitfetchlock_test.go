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
func holdCommitGraphLock(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "config", "fetch.writeCommitGraph", "true")
	graphs := filepath.Join(f.Dir, ".git", "objects", "info", "commit-graphs")
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
	holdCommitGraphLock(t, f)

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
	holdCommitGraphLock(t, f)

	if _, _, err := runRestackCmd(t, f); err != nil {
		t.Fatalf("restack under a held commit-graph lock: %v", err)
	}
	if onTrunk := restackRun(t, f, f.Dir, "jj", "log", "-r", "trunk() & ::@", "--no-graph", "-T", "commit_id"); strings.TrimSpace(onTrunk) == "" {
		t.Error("@ does not descend from the fetched trunk()")
	}
}
