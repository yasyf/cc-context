package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestWorktreeProviderRefusalDoesNotRegisterAWorktree(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
		t.Fatal(err)
	}
	f.Out(t, "git", "config", "ccx.stack", "unknown-provider")
	before := f.Out(t, "git", "worktree", "list", "--porcelain")
	if _, err := runWorktreeCmd(t, f, "add", "invalid-provider"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid provider = %v", err)
	}
	if after := f.Out(t, "git", "worktree", "list", "--porcelain"); after != before {
		t.Fatalf("provider refusal changed worktree registrations:\n%s", after)
	}
}
