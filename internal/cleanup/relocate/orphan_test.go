package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func (f *fixture) orphanRequest(tree string) cleanup.OrphanRequest {
	f.t.Helper()
	return cleanup.OrphanRequest{
		Path:      tree,
		Tree:      f.id(tree),
		Parent:    f.id(filepath.Dir(tree)),
		CommonDir: f.common,
		Git:       f.git,
	}
}

func (f *fixture) orphaned(tree string) cleanup.Job {
	f.t.Helper()
	job, err := f.relocator.Orphan(context.Background(), 1, f.orphanRequest(tree))
	if err != nil {
		f.t.Fatalf("Orphan() error = %v", err)
	}
	return job
}

func (f *fixture) dropAdmin() {
	f.t.Helper()
	if err := os.RemoveAll(f.adminDir); err != nil {
		f.t.Fatal(err)
	}
}

func TestOrphanDeletesAnOrphanedTreeWhereItSits(t *testing.T) {
	f := newFixture(t)
	f.dropAdmin()
	tree := f.id(f.worktree)
	refs := f.run(f.repo, "for-each-ref")

	job := f.orphaned(f.worktree)

	if job.Phase != cleanup.PhasePrepared || !job.Orphaned || job.Tree != tree || job.Original != f.worktree {
		t.Fatalf("Orphan() = %+v, want a prepared orphaned job for %s", job, f.worktree)
	}
	if job.Head != "" || job.RecoveryRef != "" || job.AdminDir != "" {
		t.Errorf("Orphan() head %q, ref %q, admin %q; want none", job.Head, job.RecoveryRef, job.AdminDir)
	}
	f.stored(&job)

	f.advance(&job)

	if job.Phase != cleanup.PhaseClaimed || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want claimed", job.Phase, job.Blocked)
	}
	f.stored(&job)
	if got := f.id(f.worktree); got != tree {
		t.Errorf("worktree identity = %v, want the tree %v left at its path", got, tree)
	}
	if want := []string{f.worktree, f.worktree}; !reflect.DeepEqual(f.guarded, want) {
		t.Errorf("guard consulted for %v, want %v", f.guarded, want)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("watchers retired %v, want none", f.watchers.retired)
	}

	root, err := f.relocator.Admit(context.Background(), &job)
	if err != nil || job.Blocked != nil || job.Phase != cleanup.PhaseDeletingTree {
		t.Fatalf("Admit() = %+v, %v, want the tree admitted", job, err)
	}
	if want := (cleanup.DeletionRoot{Dir: f.root, Name: filepath.Base(f.worktree), ID: tree}); root != want {
		t.Errorf("root = %+v, want %+v", root, want)
	}
	deleteRoot(t, root)
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil || job.Phase != cleanup.PhaseDone {
		t.Fatalf("Admit() after the deletion = %+v, %v, want the job done", job, err)
	}
	f.stored(&job)
	f.absent(f.worktree)
	if got := f.run(f.repo, "for-each-ref"); got != refs {
		t.Errorf("refs = %q, want them unchanged %q", got, refs)
	}
}

func TestOrphanRefusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, req *cleanup.OrphanRequest) (detail string)
		reason  string
	}{
		{"the admin directory still exists", func(f *fixture, _ *cleanup.OrphanRequest) string {
			return fmt.Sprintf("%s is still a registered worktree: %s links back to %s", f.worktree, f.adminDir, filepath.Join(f.worktree, ".git"))
		}, "registered"},
		{"another tree sits at the path", func(f *fixture, req *cleanup.OrphanRequest) string {
			f.dropAdmin()
			req.Tree.Ino++
			return fmt.Sprintf("%s is not the tree %s the request names", f.worktree, label(req.Tree))
		}, "identity"},
		{"the parent is another directory", func(f *fixture, req *cleanup.OrphanRequest) string {
			f.dropAdmin()
			req.Parent.Ino++
			return fmt.Sprintf("%s is no longer the directory %s the job captured", f.root, label(req.Parent))
		}, "identity"},
		{"another registration names the tree", func(f *fixture, _ *cleanup.OrphanRequest) string {
			f.dropAdmin()
			f.enroll("other", filepath.Join(f.worktree, ".git"))
			return fmt.Sprintf("%s still registers a worktree at %s", f.common, f.worktree)
		}, "registered"},
		{"the .git points into another repository", func(f *fixture, _ *cleanup.OrphanRequest) string {
			f.dropAdmin()
			elsewhere := filepath.Join(f.root, "other.git", "worktrees", "wt")
			f.write(filepath.Join(f.worktree, ".git"), "gitdir: "+elsewhere+"\n")
			return fmt.Sprintf("%s names %s, outside %s", filepath.Join(f.worktree, ".git"), elsewhere, filepath.Join(f.common, "worktrees"))
		}, "mismatch"},
		{"the tree has no .git", func(f *fixture, _ *cleanup.OrphanRequest) string {
			f.dropAdmin()
			if err := os.Remove(filepath.Join(f.worktree, ".git")); err != nil {
				f.t.Fatal(err)
			}
			return fmt.Sprintf("%s has no .git naming an admin directory", f.worktree)
		}, "mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			req := f.orphanRequest(f.worktree)
			detail := tt.arrange(f, &req)

			_, err := f.relocator.Orphan(context.Background(), 1, req)

			var refused *cleanup.RefusedError
			if !errors.As(err, &refused) || refused.Reason != tt.reason || refused.Detail != detail {
				t.Fatalf("Orphan() error = %#v, want a %s refusal %q", err, tt.reason, detail)
			}
			if jobs, _, err := f.journal.Load(); err != nil || len(jobs) != 0 {
				t.Errorf("Load() = %d jobs, %v; want nothing journaled", len(jobs), err)
			}
			if got := f.id(f.worktree); got.Ino == 0 {
				t.Errorf("worktree identity = %v, want the tree left in place", got)
			}
		})
	}
}

func TestOrphanRefusesAHeldTree(t *testing.T) {
	f := newFixture(t)
	f.dropAdmin()
	active := f.holdAt(f.worktree)

	_, err := f.relocator.Orphan(context.Background(), 1, f.orphanRequest(f.worktree))

	var got *cleanup.ActiveError
	if !errors.As(err, &got) || got.Error() != active.Error() {
		t.Fatalf("Orphan() error = %v, want %v", err, active)
	}
	if jobs, _, err := f.journal.Load(); err != nil || len(jobs) != 0 {
		t.Errorf("Load() = %d jobs, %v; want nothing journaled", len(jobs), err)
	}
}

func TestOrphanGetsNoPushedExcuseForAFailedProbe(t *testing.T) {
	tests := []struct {
		name   string
		run    func(f *fixture, job *cleanup.Job)
		phase  cleanup.Phase
		reason string
		detail func(f *fixture) string
		probe  func(f *fixture)
	}{
		{"the claim's guard", (*fixture).advance, cleanup.PhasePrepared, "activity", func(f *fixture) string {
			return unclear(f.worktree, errArgumentsUnread)
		}, func(f *fixture) {
			f.guard = func(context.Context, string) error { return errArgumentsUnread }
		}},
		{"the admission's quarantine", func(f *fixture, job *cleanup.Job) {
			f.advance(job)
			f.watchers.quarantine = func(context.Context, string) error { return errListingTimedOut }
			f.admit(job)
		}, cleanup.PhaseClaimed, timeoutReason, func(*fixture) string { return errListingTimedOut.Error() }, func(*fixture) {}},
		{"the deletion's guard", func(f *fixture, job *cleanup.Job) {
			f.advance(job)
			f.admit(job)
			f.guard = func(context.Context, string) error { return errArgumentsUnread }
			f.admit(job)
		}, cleanup.PhaseDeletingTree, "activity", func(f *fixture) string {
			return unclear(f.worktree, errArgumentsUnread)
		}, func(*fixture) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.dropAdmin()
			job := f.orphaned(f.worktree)
			tt.probe(f)

			tt.run(f, &job)

			f.blocked(&job, tt.phase, tt.reason, tt.detail(f))
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
			}
		})
	}
}

func TestAdvanceBlocksAnOrphanWhoseParentWasReplaced(t *testing.T) {
	f := newFixture(t)
	pool := filepath.Join(f.root, "pool")
	lane := filepath.Join(pool, "lane")
	f.run(f.repo, "worktree", "add", "-q", "-b", "lane", lane)
	if err := os.RemoveAll(filepath.Join(f.common, "worktrees", "lane")); err != nil {
		t.Fatal(err)
	}
	job := f.orphaned(lane)
	parent := f.id(pool)
	old := filepath.Join(f.root, "pool.old")
	if err := os.Rename(pool, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(old, "lane"), lane); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.blocked(&job, cleanup.PhasePrepared, "identity", fmt.Sprintf("%s is no longer the directory %s the job captured", pool, label(parent)))
	if got := f.id(lane); got != job.Tree {
		t.Errorf("lane identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestAdvanceSettlesAnOrphanWhoseTreeIsGone(t *testing.T) {
	f := newFixture(t)
	f.dropAdmin()
	job := f.orphaned(f.worktree)
	if err := os.RemoveAll(f.worktree); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want done", job.Phase, job.Blocked)
	}
	f.stored(&job)
}
