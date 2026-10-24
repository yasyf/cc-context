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
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then echo 'fatal: boom' >&2; exit 128; fi\n"))

	out, errOut, err := runStackCmd(t, f, "submit", "--restack")
	if err == nil || !strings.Contains(err.Error(), "exited 128: fatal: boom") {
		t.Fatalf("submit = %q, %v, want the replay error", out, err)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"saved stack rebase of feature remains in " + run.dir, "ccx vcs stack continue --stack feature", "ccx vcs stack abort --stack feature"} {
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
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remote {
		t.Errorf("remote = %s, want it untouched at %s", got, remote)
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
