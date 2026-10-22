package cli

import (
	"path/filepath"
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
		{name: "push", args: []string{"rebase"}, says: " · kept at its published head; --include base moves it"},
		{name: "no_push", args: []string{"rebase", "--no-push"}, says: " · kept at its local head; --include base moves it"},
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

// TestStackSubmitRefusesAChildOfAHeldParentThatConflictsWithTrunk is monorepo
// #42038: submit called it mergeable while the queue ejected it.
func TestStackSubmitRefusesAChildOfAHeldParentThatConflictsWithTrunk(t *testing.T) {
	for _, args := range [][]string{{"submit"}, {"submit", "--restack"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "base", "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("first stack submit: %v", err)
			}
			published := map[string]string{}
			for _, name := range []string{"base", "feature"} {
				published[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
			}
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane-base"), "base")
			stackAdvanceTrunk(t, f, "feature.txt", "upstream\n")
			pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
			shipResetLog(t, f)

			out, _, err := runStackCmd(t, f, args...)
			if err == nil {
				t.Fatalf("stack submit %v succeeded with output %q, want a refusal: feature conflicts with main under a held base", args, out)
			}
			for _, want := range []string{
				"base is another lane's, kept at its published head, and feature on it conflicts with main@" + pin[:12] + " in feature.txt",
				"pass --include base to take it into the run",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
			if n := pushCount(t, f); n != 0 {
				t.Errorf("pushes = %d, want none before the refusal", n)
			}
			for name, head := range published {
				if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != head {
					t.Errorf("origin %s = %.12s, want it untouched at %.12s", name, got, head)
				}
			}

			if _, _, err := runStackCmd(t, f, append(args, "--include", "base")...); err == nil {
				t.Fatalf("stack submit %v --include base succeeded, want a conflict stop on feature", args)
			}
			run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if run.Conflict == nil || run.Conflict.Branch != "feature" {
				t.Errorf("run conflict = %+v, want a stop on feature", run.Conflict)
			}
		})
	}
}
