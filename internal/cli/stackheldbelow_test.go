package cli

import (
	"strings"
	"testing"
)

func TestStackRebaseFromAChildKeepsAParentAnotherWorktreeHolds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
		says string
	}{
		{name: "push", args: []string{"rebase"}, says: "not pushing base"},
		{name: "no_push", args: []string{"rebase", "--no-push"}, says: "kept at its local head"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := stackRebaseRepo(t, "base", "feature")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
			base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
			held := f.WorktreePath("held")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")

			out, errOut, err := runStackCmd(t, f, tt.args...)
			if err != nil {
				t.Fatalf("stack rebase from feature: %v", err)
			}
			if !strings.Contains(out+errOut, tt.says) {
				t.Errorf("output = %q, stderr = %q, want %q", out, errOut, tt.says)
			}
			for where, dir := range map[string]string{"local": f.Dir, "held": held, "origin": f.RemoteDir} {
				if got := gitAt(t, f.Env(), dir, "rev-parse", "base"); got != base {
					t.Errorf("%s base = %.12s, want it left at %.12s", where, got, base)
				}
			}
			if !stackOnto(t, f, "base", "feature") {
				t.Error("feature does not sit on base")
			}
		})
	}
}
