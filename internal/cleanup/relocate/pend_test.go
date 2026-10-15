package relocate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func (f *fixture) pend() cleanup.Job {
	f.t.Helper()
	job, err := f.relocator.Pend(context.Background(), 1, f.request(false))
	if err != nil {
		f.t.Fatalf("Pend() error = %v", err)
	}
	return job
}

func TestPendJournalsAPendingJobWithoutLookingForHolders(t *testing.T) {
	f := newFixture(t)
	_ = f.holdAt(f.worktree)
	tree, listing := f.id(f.worktree), f.listing()

	job := f.pend()

	if job.Phase != cleanup.PhasePending || job.Blocked != nil || job.Deferred {
		t.Errorf("job = phase %s, blockage %+v, deferred %t; want pending, unblocked, not deferred", job.Phase, job.Blocked, job.Deferred)
	}
	if job.Original != f.worktree || job.Tree != tree || job.AdminDir != f.adminDir || job.Branch != "feature" {
		t.Errorf("job = original %s, tree %v, admin %s, branch %q; want %s, %v, %s, feature", job.Original, job.Tree, job.AdminDir, job.Branch, f.worktree, tree, f.adminDir)
	}
	if head := f.run(f.worktree, "rev-parse", "HEAD"); job.Head != head || job.RecoveryRef != cleanup.RecoveryRefFor(job.ID) {
		t.Errorf("job = head %s, recovery ref %s; want %s, %s", job.Head, job.RecoveryRef, head, cleanup.RecoveryRefFor(job.ID))
	}
	f.stored(&job)
	f.unconsulted()
	if got := f.id(f.worktree); got != tree {
		t.Errorf("worktree identity = %v, want %v", got, tree)
	}
	if got := f.listing(); got != listing {
		t.Errorf("worktree list = %q, want %q", got, listing)
	}
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		t.Errorf("recovery refs = %q, want none", refs)
	}
}

func TestPendRefusesADirtyTree(t *testing.T) {
	f := newFixture(t)
	f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")

	_, err := f.relocator.Pend(context.Background(), 1, f.request(false))

	f.refused(err, cleanup.RefusedError{Worktree: f.worktree, Reason: "dirty", Detail: "uncommitted changes: scratch.txt"})
	f.unjournaled()
	f.unconsulted()
}

func TestAdvancePromotesAPendingJob(t *testing.T) {
	f := newFixture(t)
	job := f.pend()

	f.advance(&job)

	f.finished(&job)
	if len(f.guarded) == 0 || f.guarded[0] != f.worktree {
		t.Errorf("guard consulted for %v, want the original %s first", f.guarded, f.worktree)
	}
	f.absent(f.worktree)
}

func TestAdvanceBlocksAPendingJobItsPreflightNoLongerPasses(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		arrange func(f *fixture, job cleanup.Job) string
	}{
		{"held", "activity", func(f *fixture, _ cleanup.Job) string {
			return f.holdAt(f.worktree).Error()
		}},
		{"dirty", "dirty", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return "uncommitted changes: scratch.txt"
		}},
		{"locked", "locked", func(f *fixture, _ cleanup.Job) string {
			f.run(f.repo, "worktree", "lock", "--reason", "in use by ci", f.worktree)
			return "worktree is locked: in use by ci"
		}},
		{"head moved", "identity", func(f *fixture, job cleanup.Job) string {
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
			return fmt.Sprintf("HEAD changed: the job captured %q, %s now reads %q", job.Head, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}},
		{"branch switched", "identity", func(f *fixture, job cleanup.Job) string {
			f.run(f.worktree, "switch", "-q", "-c", "other")
			return fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", job.Branch, f.worktree, "other")
		}},
		{"replaced", "identity", func(f *fixture, job cleanup.Job) string {
			f.run(f.repo, "worktree", "remove", "--force", f.worktree)
			f.run(f.repo, "worktree", "add", "-q", "-b", "other", f.worktree)
			named := cleanup.Registration{Tree: job.Tree, AdminDir: job.AdminDir, Admin: job.Admin}
			return replacedDetail(f.worktree, f.observe(f.worktree), named)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.pend()
			detail := tt.arrange(f, job)

			f.advance(&job)

			f.blocked(&job, cleanup.PhasePending, tt.reason, detail)
			if _, err := os.Lstat(f.worktree); err != nil {
				t.Errorf("lstat %s = %v, want the tree left at its path", f.worktree, err)
			}
			if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
				t.Errorf("recovery refs = %q, want none", refs)
			}
		})
	}
}

func TestAdvanceSettlesAPendingJobWhoseTreeWasRemovedByHand(t *testing.T) {
	f := newFixture(t)
	job := f.pend()
	f.run(f.repo, "worktree", "remove", "--force", f.worktree)

	f.advance(&job)

	if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
		t.Fatalf("job = phase %s, blockage %+v; want done", job.Phase, job.Blocked)
	}
	f.stored(&job)
	f.unconsulted()
}
