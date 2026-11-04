package relocate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestAdmitMovesAProvedTreeOutOfAMoveRoot(t *testing.T) {
	f := newFixture(t)
	f.relocator.cfg.Move = []string{filepath.Dir(f.worktree)}
	job := f.accept()
	f.advance(&job)
	f.claimed(&job)
	tree := f.id(job.Original)

	root, err := f.relocator.Admit(t.Context(), &job)
	if err != nil {
		t.Fatal(err)
	}
	if job.Blocked != nil || job.Phase != cleanup.PhaseDeletingTree || !job.Moved {
		t.Fatalf("job = phase %s, blocked %+v, moved %t; want deleting_tree, moved", job.Phase, job.Blocked, job.Moved)
	}
	if got := filepath.Join(root.Dir, root.Name); got != job.Payload {
		t.Errorf("deletion root = %s, want %s", got, job.Payload)
	}
	if got := f.id(job.Payload); got != tree {
		t.Errorf("payload identity = %v, want %v", got, tree)
	}
	if _, err := os.Lstat(job.Original); !os.IsNotExist(err) {
		t.Errorf("Lstat(original) = %v, want it gone", err)
	}
	if got := f.run(f.repo, "worktree", "list", "--porcelain"); strings.Contains(got, job.Original) {
		t.Errorf("moved registration remains listed: %s", got)
	}
	f.stored(&job)

	if _, err := f.relocator.Admit(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked != nil || f.id(job.Payload) != tree {
		t.Errorf("readmitted job = %+v, want the moved tree proved where it sits", job.Blocked)
	}
}

func TestAdmitNeverMovesATreeThatFailsItsProof(t *testing.T) {
	f := newFixture(t)
	f.relocator.cfg.Move = []string{filepath.Dir(f.worktree)}
	job := f.accept()
	f.advance(&job)
	f.claimed(&job)
	tree := f.id(job.Original)
	f.write(filepath.Join(job.Original, "feature.txt"), "edited\n")

	if _, err := f.relocator.Admit(t.Context(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Moved {
		t.Fatalf("job = blocked %+v, moved %t; want blocked, not moved", job.Blocked, job.Moved)
	}
	if got := f.id(job.Original); got != tree {
		t.Errorf("tree identity = %v, want %v still at its path", got, tree)
	}
	if _, err := os.Lstat(job.Payload); !os.IsNotExist(err) {
		t.Errorf("Lstat(payload) = %v, want it absent", err)
	}
}

func TestAdmitLeavesATreeOutsideEveryMoveRoot(t *testing.T) {
	f := newFixture(t)
	f.relocator.cfg.Move = []string{filepath.Join(filepath.Dir(f.worktree), "elsewhere")}
	job := f.accept()
	f.advance(&job)
	f.claimed(&job)

	root, err := f.relocator.Admit(t.Context(), &job)
	if err != nil {
		t.Fatal(err)
	}
	if job.Moved || filepath.Join(root.Dir, root.Name) != job.Original {
		t.Errorf("moved %t, deletion root %s/%s; want it deleted at %s", job.Moved, root.Dir, root.Name, job.Original)
	}
}
