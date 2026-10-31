package relocate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestPausedRetirementFollowsCapturedDisplacedAdminCI(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	actual := filepath.Join(f.common, "retired-worktrees", "wt")
	if err := os.Mkdir(filepath.Dir(actual), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(job.AdminDir, actual); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(job.Original, ".git"), "gitdir: "+actual+"\n")
	if f.id(actual) != job.Admin || f.id(job.Original) != job.Tree {
		t.Fatal("fixture changed the captured identities")
	}
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("same captured admin outside native registry was not retired: %+v", job)
	}
	if got := f.read(filepath.Join(actual, "gitdir")); got != "" {
		t.Errorf("retired backlink = %q, want empty", got)
	}
	if f.id(job.Original) != job.Tree {
		t.Error("paused retirement changed the original tree")
	}
}
