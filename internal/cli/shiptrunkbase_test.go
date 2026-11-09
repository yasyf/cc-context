package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipGTKeepsAnUntrackedPublishedBranchOnItsTrunkBaseWhenItsCommitsWouldNotReplay(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, f *vcstest.Fixture)
		args    []string
	}{
		{
			name: "a raw amend shipped with --no-commit --tip-only",
			prepare: func(t *testing.T, f *vcstest.Fixture) {
				writeShipFile(t, f.Dir, "g.txt", "g\n")
				mustRun(t, f.Env(), f.Dir, "git", "add", "g.txt")
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
			},
			args: []string{"--no-commit", "--tip-only"},
		},
		{
			name: "a new commit shipped with --tip-only",
			prepare: func(t *testing.T, f *vcstest.Fixture) {
				writeShipFile(t, f.Dir, "g.txt", "g\n")
			},
			args: []string{"-m", "fix: frobnicate", "--tip-only", "g.txt"},
		},
		{
			name: "an amend shipped with --amend",
			prepare: func(t *testing.T, f *vcstest.Fixture) {
				writeShipFile(t, f.Dir, "g.txt", "g\n")
			},
			args: []string{"--amend", "g.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipGTRepo(t)
			pushCommit(t, f, "a.txt", "one\ntwo\nthree\n", "base")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
			base := shipHead(t, f)
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
			pushCommit(t, f, "a.txt", "one\nFEAT\nthree\n", "feature edits two")
			pushCommit(t, f, "a.txt", "one\ntwo\nthree\n", "feature puts two back")
			published := pushCommit(t, f, "f.txt", "f\n", "feature work")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
			stubStackPRs(t, f, map[string]*stackPR{"feature": {Number: 42, Title: "feature", State: "OPEN", Base: "main", Head: published, Mergeable: "MERGEABLE"}})
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			api.prs["feature"] = 42
			stackAdvanceTrunk(t, f, "a.txt", "one\nMAIN\nthree\n")
			mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "main", "origin/main")
			tt.prepare(t, f)
			shipResetLog(t, f)

			got, errOut, err := runShipCmdFull(f.Context(), t, append(tt.args, "--no-watch")...)
			if err != nil {
				t.Fatalf("ship = %v (stderr=%q), want the branch shipped on its own base", err, errOut)
			}
			if !strings.Contains(got, "submitted feature") {
				t.Errorf("summary = %q, want feature submitted", got)
			}
			mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "feature")
			if pushed, head := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/feature"), shipHead(t, f); pushed != head {
				t.Errorf("origin/feature = %s, want the local head %s", pushed, head)
			}
			if fork := gitAt(t, f.Env(), f.Dir, "merge-base", "origin/main", "origin/feature"); fork != base {
				t.Errorf("origin/feature forks from main at %s, want its own base %s", fork, base)
			}
		})
	}
}
