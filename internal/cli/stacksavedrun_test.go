package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestStackSubmitReportsSavedRunAfterReplayError(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	head := shipHead(t, f)
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then echo 'fatal: boom' >&2; exit 128; fi\n"))

	out, errOut, err := runStackCmd(t, f, "submit", "--restack")
	if err == nil || !strings.Contains(err.Error(), "exited 128: fatal: boom") {
		t.Fatalf("submit = %q, %v, want the replay error", out, err)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"saved stack rebase writing feature remains in " + run.dir, "re-run ccx vcs stack submit from this worktree", "ccx vcs stack continue --stack feature", "ccx vcs stack abort --stack feature"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want %q", errOut, want)
		}
	}
	if run.Conflict != nil || strings.Contains(out, "published") {
		t.Fatalf("run = %+v, stdout = %q, want an unpublished run without a conflict workspace", run, out)
	}
	if got := shipHead(t, f); got != head {
		t.Errorf("source = %s, want it untouched at %s", got, head)
	}
}

func TestStackSubmitReportsSavedRunAfterCancellation(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	common := filepath.Join(f.Dir, ".git")
	f.Decorate(func(ctx context.Context) context.Context {
		query := ctx.Value(stackPRKey{}).(stackPRQuery)
		ctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		return withStackPRs(ctx, func(ctx context.Context, dir render.Dir, trunk string, branches []string) (map[string]*stackPR, error) {
			if _, err := os.Stat(stackStatePath(stackRunDir(common, "feature"))); err == nil {
				cancel()
				return nil, ctx.Err()
			}
			return query(ctx, dir, trunk, branches)
		})
	})

	out, errOut, err := runStackCmd(t, f, "submit", "--restack")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submit = %q, %v, want cancellation after saving the run", out, err)
	}
	run, err := stackOnlyTestRun(common)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{run.dir, "ccx vcs stack continue --stack feature", "ccx vcs stack abort --stack feature"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want %q despite the cancelled context", errOut, want)
		}
	}
	if strings.Contains(out, "published") {
		t.Fatalf("stdout = %q, want cancellation before publication", out)
	}
}

func TestStackSubmitFromTheSameWorktreeResumesItsSavedRun(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "feature")
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	refuse := filepath.Join(t.TempDir(), "refuse")
	if err := os.WriteFile(refuse, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$2\" = replay ] && [ -e '"+refuse+"' ]; then echo 'fatal: boom' >&2; exit 128; fi\n"))
	if _, _, err := runStackCmd(t, f, "submit", "--restack"); err == nil {
		t.Fatal("first submit published, want the replay error that saves its run")
	}
	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}

	out, _, err := runStackCmd(t, f, "submit", "--restack")

	if err != nil || !strings.Contains(out, "resuming this worktree's saved stack rebase of feature") {
		t.Fatalf("re-run submit = %q, %v, want it to resume the saved run", out, err)
	}
	if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); !stackOnto(t, f, "origin/main", remote) {
		t.Errorf("published feature %s missed the fresh trunk", remote)
	}
	stackAssertNoRun(t, f)
}
