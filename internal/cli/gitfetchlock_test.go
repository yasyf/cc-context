package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
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
