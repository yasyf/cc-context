package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestWorktreeAddAtCutsASoakBranchShipKeepsThere is the staging soak: a branch
// cut at the commit staging serves keeps that commit below its own after a
// ship with no pull request, though trunk moved on.
func TestWorktreeAddAtCutsASoakBranchShipKeepsThere(t *testing.T) {
	f := shipGTRepo(t)
	f.Isolate(t)
	cut := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")

	out, err := runWorktreeCmd(t, f, "add", "soak", "--at", cut[:12])
	if err != nil {
		t.Fatalf("worktree add soak --at %.12s: %v", cut, err)
	}
	path := worktreeSummaryPath(t, out)
	if want := strings.Join([]string{"added soak", "git worktree", "from origin/main " + shortOID(cut), path}, shipSep) + "\n"; out != want {
		t.Errorf("summary = %q, want %q", out, want)
	}
	if got := gitAt(t, f.Env(), path, "rev-parse", "HEAD"); got != cut {
		t.Errorf("soak HEAD = %s, want the cut %s", got, cut)
	}
	if got := dropGTParent(t, f, "soak"); got != "main" {
		t.Errorf("soak's gt parent = %q, want main", got)
	}

	writeShipFile(t, path, "soak.txt", "soak\n")
	if _, errOut, err := runShipCmdFull(f.ContextIn(path), t, "-m", "fix: soak", "--no-pr", "--no-watch"); err != nil {
		t.Fatalf("ship --no-pr = %v (stderr=%q)", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "soak^"); got != cut {
		t.Errorf("origin soak's commit sits on %s, want the cut %s", got, cut)
	}
}

func TestWorktreeAddAtRefuses(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, f *vcstest.Fixture) string
		want    string
	}{
		{
			name: "a commit trunk does not hold",
			prepare: func(t *testing.T, f *vcstest.Fixture) string {
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side")
				side := pushCommit(t, f, "side.txt", "side\n", "side")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return side
			},
			want: "is not on origin/main, so soak cannot be cut on trunk there",
		},
		{
			name: "a branch that already exists",
			prepare: func(t *testing.T, f *vcstest.Fixture) string {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "soak")
				return "main"
			},
			want: "--at cuts a new branch, and soak already exists",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			f.Isolate(t)
			at := tt.prepare(t, f)

			if _, err := runWorktreeCmd(t, f, "add", "soak", "--at", at); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("worktree add soak --at %s = %v, want it to carry %q", at, err, tt.want)
			}
		})
	}
}
