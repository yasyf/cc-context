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

// TestStackSubmitWarnsOfAChildOfAHeldParentThatConflictsWithTrunk is monorepo
// #42038, where submit called the child mergeable while the queue ejected it,
// and #42767, whose lane could not restack under a held parent it does not own.
func TestStackSubmitWarnsOfAChildOfAHeldParentThatConflictsWithTrunk(t *testing.T) {
	for _, args := range [][]string{{"submit"}, {"submit", "--restack"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "base", "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("first stack submit: %v", err)
			}
			base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane-base"), "base")
			stackAdvanceTrunk(t, f, "feature.txt", "upstream\n")
			pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")

			out, errOut, err := runStackCmd(t, f, args...)
			if err != nil {
				t.Fatalf("stack %v: %v\n%s%s", args, err, out, errOut)
			}
			if want := "warning: feature follows base's published head, and feature on it conflicts with main@" + pin[:12] + " in feature.txt — the merge queue ejects it until base moves onto main by ccx vcs stack submit from the working copy holding it, or by --include base here"; !strings.Contains(errOut, want) {
				t.Errorf("stderr = %q, want the warning %q", errOut, want)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
				t.Errorf("origin base = %.12s, want it left at its published %.12s", got, base)
			}
			feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", feature+"^"); parent != base {
				t.Errorf("origin feature sits on %.12s, want base's published head %.12s", parent, base)
			}

			if _, _, err := runStackCmd(t, f, append(args, "--include", "base")...); err == nil {
				t.Fatalf("stack %v --include base succeeded, want a conflict stop on feature", args)
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

func TestStackRebaseParentFollowsAHeldParentThatConflictsWithTrunk(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	writeShipFile(t, held, "base.txt", "base moved\n")
	mustRun(t, f.Env(), held, "git", "commit", "-qam", "base moves")
	mustRun(t, f.Env(), held, "git", "push", "-q", "origin", "base")
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stackAdvanceTrunk(t, f, "base.txt", "upstream\n")
	pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")

	out, errOut, err := runStackCmd(t, f, "rebase", "--parent", "feature=base")
	if err != nil {
		t.Fatalf("stack rebase --parent feature=base: %v\n%s%s", err, out, errOut)
	}
	if want := "warning: feature follows base's published head, and feature on it conflicts with main@" + pin[:12] + " in base.txt — the merge queue ejects it until base moves onto main by ccx vcs stack submit from the working copy holding it"; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want the warning %q", errOut, want)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("origin base = %.12s, want it left at its published %.12s", got, base)
	}
	feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", feature+"^"); parent != base {
		t.Errorf("origin feature sits on %.12s, want base's published head %.12s", parent, base)
	}
}
