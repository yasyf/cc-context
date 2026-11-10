package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func (f *fixture) claim() cleanup.Job {
	f.t.Helper()
	job := f.accept()
	f.advance(&job)
	f.claimed(&job)
	return job
}

func (f *fixture) admitted(job *cleanup.Job, phase cleanup.Phase, path string, id cleanup.FileID) cleanup.DeletionRoot {
	f.t.Helper()
	root, err := f.relocator.Admit(context.Background(), job)
	if err != nil {
		f.t.Fatalf("Admit() error = %v", err)
	}
	if job.Phase != phase || job.Blocked != nil {
		f.t.Fatalf("Admit() left the job at %s with blockage %+v, want %s and unblocked", job.Phase, job.Blocked, phase)
	}
	if want := (cleanup.DeletionRoot{Dir: filepath.Dir(path), Name: filepath.Base(path), ID: id}); root != want {
		f.t.Errorf("Admit() root = %+v, want %+v", root, want)
	}
	f.stored(job)
	return root
}

func (f *fixture) orphan(job cleanup.Job) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(job.Original, ".git")); err != nil {
		f.t.Fatalf("drop the .git link: %v", err)
	}
	if err := os.RemoveAll(job.AdminDir); err != nil {
		f.t.Fatalf("drop the admin directory: %v", err)
	}
}

func (f *fixture) lockOf() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.adminDir, "locked"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestAdvanceClaimsATreeWhereItSits(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	tree, admin := job.Tree, job.Admin

	f.advance(&job)

	f.claimed(&job)
	if got := f.id(f.worktree); got != tree {
		t.Errorf("tree identity = %v, want the tree %v left at its path", got, tree)
	}
	if got := f.id(f.adminDir); got != admin {
		t.Errorf("admin identity = %v, want the admin %v left in place", got, admin)
	}
	if got := f.lockOf(); got != retirementMarker(&job) {
		t.Errorf("registration lock = %q, want %q", got, retirementMarker(&job))
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != "" {
		t.Errorf("backlink = %q, want it emptied so git stops listing the tree", got)
	}
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want only %q", got, want)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
	f.absent(job.Registered)
	f.absent(job.Payload)
	if want := []string{f.worktree}; !reflect.DeepEqual(f.watchers.retired, want) {
		t.Errorf("watchers retired %v, want %v", f.watchers.retired, want)
	}
	if want := []string{f.worktree}; !reflect.DeepEqual(f.watchers.checked, want) {
		t.Errorf("quarantine checked %v, want %v", f.watchers.checked, want)
	}
	if out, err := f.try(f.repo, "worktree", "remove", f.worktree); err == nil {
		t.Errorf("git worktree remove of the claimed tree succeeded: %s", out)
	}
}

func TestAdmitDeletesTheTreeThenTheRegistration(t *testing.T) {
	f := newFixture(t)
	job := f.claim()

	deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
	f.absent(f.worktree)
	deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingRegistration, f.adminDir, job.Admin))
	f.absent(f.adminDir)
	root, err := f.relocator.Admit(context.Background(), &job)

	if err != nil || job.Phase != cleanup.PhaseDone || job.Blocked != nil || root != (cleanup.DeletionRoot{}) {
		t.Fatalf("Admit() = %+v, %+v, %v; want the job done with no root", job, root, err)
	}
	f.stored(&job)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want it kept at %s", got, job.Head)
	}
	if err := f.journal.Discard(job); err != nil {
		t.Errorf("Discard() error = %v, want the job folder to hold nothing but its record", err)
	}
}

func TestAdmitReprovesAPartlyDeletedTree(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) (reason, detail string)
	}{
		{"a deletion part way through", func(*fixture, cleanup.Job) (string, string) { return "", "" }},
		{"a file written since", func(f *fixture, _ cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
			return "dirty", "uncommitted changes: late.txt"
		}},
		{"a tracked file edited since", func(f *fixture, _ cleanup.Job) (string, string) {
			f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n")
			return "dirty", "uncommitted changes: feature.txt"
		}},
		{"another registration claims the tree", func(f *fixture, _ cleanup.Job) (string, string) {
			other := f.enroll("other", filepath.Join(f.worktree, ".git"))
			return "reconcile", fmt.Sprintf("%s registers %s", other, filepath.Join(f.worktree, ".git"))
		}},
		{"the lock is gone", func(f *fixture, job cleanup.Job) (string, string) {
			if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
				f.t.Fatal(err)
			}
			return "identity", fmt.Sprintf("%s has no backlink and no lock of job %s", f.adminDir, job.ID)
		}},
		{"a process holds it", func(f *fixture, _ cleanup.Job) (string, string) {
			return "activity", f.holdAt(f.worktree).Error()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.claim()
			f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
			if err := os.Remove(filepath.Join(f.worktree, "base.txt")); err != nil {
				t.Fatalf("remove base.txt: %v", err)
			}
			reason, detail := tt.arrange(f, job)

			if reason == "" {
				f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
				return
			}
			f.admit(&job)
			f.blocked(&job, cleanup.PhaseDeletingTree, reason, detail)
		})
	}
}

func TestReadmitRechecksAnOpenDeletionWithoutGit(t *testing.T) {
	t.Run("the tree", func(t *testing.T) {
		f := newFixture(t)
		job := f.claim()
		root := f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
		git, log := f.recorder()
		job.Git = git
		f.write(filepath.Join(f.worktree, "late.txt"), "late\n")

		got, err := f.relocator.Readmit(context.Background(), &job)

		if err != nil || got != root || job.Blocked != nil || job.Phase != cleanup.PhaseDeletingTree {
			t.Fatalf("Readmit() = %+v, %v with the job at %s, blockage %+v; want the same root", got, err, job.Phase, job.Blocked)
		}
		f.absent(log)
		active := f.holdAt(f.worktree)
		if got, err := f.relocator.Readmit(context.Background(), &job); err != nil || got != (cleanup.DeletionRoot{}) {
			t.Fatalf("Readmit() of a held tree = %+v, %v; want no root", got, err)
		}
		f.blocked(&job, cleanup.PhaseDeletingTree, "activity", active.Error())
		f.absent(log)
	})

	t.Run("the registration", func(t *testing.T) {
		f := newFixture(t)
		job := f.claim()
		stranger := f.worktree + "-stranger"
		if err := os.Mkdir(stranger, 0o700); err != nil {
			t.Fatal(err)
		}
		deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
		root := f.admitted(&job, cleanup.PhaseDeletingRegistration, f.adminDir, job.Admin)
		if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
			t.Fatal(err)
		}

		if got, err := f.relocator.Readmit(context.Background(), &job); err != nil || got != root || job.Blocked != nil {
			t.Fatalf("Readmit() = %+v, %v, blockage %+v; want the admin root again", got, err, job.Blocked)
		}
		if err := os.Rename(stranger, f.worktree); err != nil {
			t.Fatal(err)
		}
		if got, err := f.relocator.Readmit(context.Background(), &job); err != nil || got != root || job.Phase != cleanup.PhaseDeletingRegistration {
			t.Fatalf("Readmit() beside a new directory at the path = %+v, %v at %s; want the admin root", got, err, job.Phase)
		}
	})
}

func TestAdmitReprovesAClaimedTree(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) string
		reason  string
	}{
		{"an edit since the claim", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
			return "uncommitted changes: late.txt"
		}, "dirty"},
		{"a commit since the claim", func(f *fixture, job cleanup.Job) string {
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
			return headChanged(&job, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}, "identity"},
		{"the lock is gone", func(f *fixture, job cleanup.Job) string {
			if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
				f.t.Fatalf("remove the lock: %v", err)
			}
			return fmt.Sprintf("%s has no backlink and no lock of job %s", f.adminDir, job.ID)
		}, "identity"},
		{"the recovery ref moved", func(f *fixture, job cleanup.Job) string {
			base := f.run(f.repo, "rev-parse", "refs/heads/main")
			f.run(f.repo, "update-ref", job.RecoveryRef, base)
			return fmt.Sprintf("%s resolves to %s in %s, not the saved head %s", job.RecoveryRef, base, f.common, job.Head)
		}, "identity"},
		{"a process holds the tree", func(f *fixture, _ cleanup.Job) string {
			return f.holdAt(f.worktree).Error()
		}, "activity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.claim()
			detail := tt.arrange(f, job)

			f.admit(&job)

			f.blocked(&job, cleanup.PhaseClaimed, tt.reason, detail)
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("tree identity = %v, want the tree %v left at its path", got, job.Tree)
			}
		})
	}
}

func TestAdvanceBlocksAClaimItCannotProve(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) string
		reason  string
	}{
		{"dirty", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return "uncommitted changes: scratch.txt"
		}, "dirty"},
		{"locked by another hand", func(f *fixture, _ cleanup.Job) string {
			f.run(f.repo, "worktree", "lock", "--reason", "in use by ci", f.worktree)
			return "worktree is locked: in use by ci"
		}, "locked"},
		{"a worktree registered inside", func(f *fixture, _ cleanup.Job) string {
			inner := filepath.Join(f.worktree, "inner")
			f.run(f.repo, "worktree", "add", "-q", "-b", "inner", inner)
			return fmt.Sprintf("%s registers %s", filepath.Join(f.common, "worktrees", "inner"), filepath.Join(inner, ".git"))
		}, "reconcile"},
		{"the tree is watched from above", func(f *fixture, _ cleanup.Job) string {
			f.watchers.quarantine = func(context.Context, string) error { return fmt.Errorf("%s sits under a watched root", f.worktree) }
			return f.worktree + " sits under a watched root"
		}, "quarantine"},
		{"the .git link names another admin", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, ".git"), "gitdir: "+filepath.Join(f.root, "elsewhere")+"\n")
			return fmt.Sprintf("%s no longer names %s", filepath.Join(f.worktree, ".git"), f.adminDir)
		}, "identity"},
		{"the registration names another tree", func(f *fixture, _ cleanup.Job) string {
			other := filepath.Join(f.root, "other")
			if err := os.Mkdir(other, 0o700); err != nil {
				f.t.Fatalf("mkdir: %v", err)
			}
			f.write(filepath.Join(f.adminDir, "gitdir"), filepath.Join(other, ".git")+"\n")
			return fmt.Sprintf("%s registers %s, not the tree at %s", f.adminDir, filepath.Join(other, ".git"), f.worktree)
		}, "identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			detail := tt.arrange(f, job)

			f.advance(&job)

			f.blocked(&job, cleanup.PhasePrepared, tt.reason, detail)
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("tree identity = %v, want the tree %v left at its path", got, job.Tree)
			}
			if got := f.lockOf(); got == retirementMarker(&job) {
				t.Errorf("registration lock = %q, want none of the job's", got)
			}
			if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
				t.Errorf("recovery refs = %q, want none", refs)
			}
		})
	}
}

func TestAdvanceProvesAnOrphanAgainstItsSavedHead(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
		force   bool
		dirt    string
	}{
		{"clean", func(*fixture) {}, false, ""},
		{"files the tree no longer holds", func(f *fixture) {
			if err := os.Remove(filepath.Join(f.worktree, "feature.txt")); err != nil {
				f.t.Fatalf("remove: %v", err)
			}
		}, false, ""},
		{"an edit", func(f *fixture) { f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n") }, false, "uncommitted changes: feature.txt"},
		{"an untracked file", func(f *fixture) { f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n") }, false, "uncommitted changes: scratch.txt"},
		{"an edit under force", func(f *fixture) { f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n") }, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job, err := f.relocator.Accept(context.Background(), 1, f.request(tt.force))
			if err != nil {
				t.Fatalf("Accept() error = %v", err)
			}
			f.orphan(job)
			tt.arrange(f)

			f.advance(&job)

			if tt.dirt != "" {
				f.blocked(&job, cleanup.PhasePrepared, "dirty", tt.dirt)
			} else {
				f.claimed(&job)
			}
			f.absent(f.layout.ScratchIndex(job.ID) + ".lock")
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("tree identity = %v, want the tree %v left at its path", got, job.Tree)
			}
			if tt.dirt != "" {
				return
			}
			if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
				t.Errorf("recovery ref = %s, want %s", got, job.Head)
			}
			deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
			if _, err := f.relocator.Admit(context.Background(), &job); err != nil || job.Phase != cleanup.PhaseDone {
				t.Fatalf("Admit() after the deletion = %+v, %v, want the job done", job, err)
			}
			f.stored(&job)
			f.absent(f.layout.ScratchIndex(job.ID))
		})
	}
}

func (f *fixture) ignoreBuildOutput() {
	f.t.Helper()
	f.write(filepath.Join(f.worktree, ".gitignore"), "build/\n")
	f.run(f.worktree, "add", ".gitignore")
	f.run(f.worktree, "commit", "-m", "ignore build")
	if err := os.Mkdir(filepath.Join(f.worktree, "build"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.worktree, "build", "out.bin"), "out\n")
}

func (f *fixture) ageTrackedFiles() {
	f.t.Helper()
	earlier := time.Now().Add(-time.Hour)
	for _, name := range []string{"base.txt", "feature.txt"} {
		if err := os.Chtimes(filepath.Join(f.worktree, name), earlier, earlier); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) retireTree(job cleanup.Job) string {
	f.t.Helper()
	parent := filepath.Join(job.Repo, cleanup.RetiredWorktrees)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		f.t.Fatal(err)
	}
	retired := filepath.Join(parent, filepath.Base(job.AdminDir))
	if err := os.Rename(job.AdminDir, retired); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(job.Original, ".git")); err != nil {
		f.t.Fatal(err)
	}
	return retired
}

func (f *fixture) spyOnCleanFilter() string {
	f.t.Helper()
	cleaned := filepath.Join(f.t.TempDir(), "cleaned")
	f.run(f.repo, "config", "filter.spy.clean", "echo read >>"+cleaned+"; cat")
	if err := os.MkdirAll(filepath.Join(f.common, "info"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.common, "info", "attributes"), "*.txt filter=spy\n")
	return cleaned
}

func TestAdvanceProvesATreeFromTheStatDataOfItsLastIndex(t *testing.T) {
	retiredTree := func(f *fixture, job cleanup.Job) string {
		f.t.Helper()
		f.ageTrackedFiles()
		f.run(f.worktree, "update-index", "--refresh")
		return f.retireTree(job)
	}
	t.Run("reads no file the index holds as unchanged", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		retiredTree(f, job)
		cleaned := f.spyOnCleanFilter()

		f.advance(&job)

		f.claimed(&job)
		if _, err := os.Stat(cleaned); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the proof read a file through its clean filter: %v", err)
		}
	})
	t.Run("still finds an edit", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		retiredTree(f, job)
		f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n")

		f.advance(&job)

		f.blocked(&job, cleanup.PhasePrepared, "dirty", "uncommitted changes: feature.txt")
	})
	for _, mark := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run("finds an edit an entry marked "+mark+" would hide", func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			f.run(f.worktree, "update-index", mark, "feature.txt")
			f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n")
			f.retireTree(job)

			f.advance(&job)

			f.blocked(&job, cleanup.PhasePrepared, "dirty", "uncommitted changes: feature.txt")
		})
	}
	t.Run("falls back to the saved HEAD past an index git refuses", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		retired := retiredTree(f, job)
		f.write(filepath.Join(retired, "index"), "not an index")

		f.advance(&job)

		f.claimed(&job)
	})
}

func TestAdvanceFindsAnEditThatKeepsSizeAndMtimeUnderRelaxedStatSettings(t *testing.T) {
	f := newFixture(t)
	f.run(f.repo, "config", "core.trustctime", "false")
	f.run(f.repo, "config", "core.checkStat", "minimal")
	job := f.accept()
	f.ageTrackedFiles()
	f.orphan(job)
	feature := filepath.Join(f.worktree, "feature.txt")
	f.advance(&job)
	f.claimed(&job)

	info, err := os.Stat(feature)
	if err != nil {
		t.Fatal(err)
	}
	// Git compares ctime in whole seconds, so only a later second can show this edit.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 50*time.Millisecond)))
	f.write(feature, strings.Repeat("x", int(info.Size())))
	if err := os.Chtimes(feature, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	f.admit(&job)

	f.blocked(&job, cleanup.PhaseClaimed, "dirty", "uncommitted changes: feature.txt")
}

func TestAdvanceReadsEachFileOnceAcrossAJobsProofs(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.ageTrackedFiles()
	f.orphan(job)
	cleaned := f.spyOnCleanFilter()

	f.advance(&job)

	f.claimed(&job)
	if got := strings.Count(f.read(cleaned), "read"); got != 2 {
		t.Errorf("the clean filter read %d files, want each of the 2 tracked files once", got)
	}
	f.absent(f.layout.ScratchIndex(job.ID) + ".lock")
}

func TestAdvanceReadsIgnoreRulesThePartialTreeLost(t *testing.T) {
	f := newFixture(t)
	f.ignoreBuildOutput()
	job, err := f.relocator.Accept(context.Background(), 1, f.request(false))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	f.orphan(job)
	if err := os.Remove(filepath.Join(f.worktree, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.claimed(&job)
}

func TestAdmitReadsIgnoreRulesTheDeletionRemoved(t *testing.T) {
	f := newFixture(t)
	f.ignoreBuildOutput()
	job := f.claim()
	root := f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
	if err := os.Remove(filepath.Join(f.worktree, ".gitignore")); err != nil {
		t.Fatal(err)
	}

	if got := f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree); got != root {
		t.Errorf("Admit() = %+v, want the same root %+v", got, root)
	}
	f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
	f.admit(&job)
	f.blocked(&job, cleanup.PhaseDeletingTree, "dirty", "uncommitted changes: late.txt")
}

func TestAdvanceClaimsATreeWhoseBranchWasDeleted(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
		dirt    string
	}{
		{"clean", func(*fixture) {}, ""},
		{"an edit", func(f *fixture) { f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n") }, "uncommitted changes: feature.txt"},
		{"an untracked file", func(f *fixture) { f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n") }, "uncommitted changes: scratch.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			if job.Branch == "" {
				t.Fatal("fixture worktree has no branch")
			}
			f.run(f.repo, "update-ref", "-d", "refs/heads/"+job.Branch)
			tt.arrange(f)

			f.advance(&job)

			if tt.dirt != "" {
				f.blocked(&job, cleanup.PhasePrepared, "dirty", tt.dirt)
				return
			}
			f.claimed(&job)
			if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
				t.Errorf("recovery ref = %s, want %s", got, job.Head)
			}
			f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
		})
	}
}

func TestAdvanceFollowsAMovedTreeARefHolds(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
		branch  string
		dirt    string
	}{
		{"a commit on its branch", func(f *fixture) {
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
		}, "feature", ""},
		{"another branch", func(f *fixture) {
			f.run(f.worktree, "switch", "-q", "-c", "other")
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
		}, "other", ""},
		{"detached at the commit its branch holds", func(f *fixture) {
			f.run(f.worktree, "switch", "-q", "--detach")
		}, "", ""},
		{"a commit and an edit", func(f *fixture) {
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
			f.write(filepath.Join(f.worktree, "feature.txt"), "edited\n")
		}, "feature", "uncommitted changes: feature.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			tt.arrange(f)
			head := f.run(f.worktree, "rev-parse", "HEAD")

			f.advance(&job)

			if job.Head != head || job.Branch != tt.branch {
				t.Fatalf("job follows %s on %q, want %s on %q", job.Head, job.Branch, head, tt.branch)
			}
			if tt.dirt != "" {
				f.blocked(&job, cleanup.PhasePrepared, "dirty", tt.dirt)
				return
			}
			f.claimed(&job)
			if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != head {
				t.Errorf("recovery ref = %s, want %s", got, head)
			}
			f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
		})
	}
}

func TestAdvanceClaimsAPartialTreeStillRegistered(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	if err := os.Remove(filepath.Join(f.worktree, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.worktree, "feature.txt")); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.claimed(&job)
	if got := f.lockOf(); got == "" {
		t.Error("the registration carries no lock of the job")
	}
}

func TestAdvanceDeletesTheAdminOldRetirementLeftBehind(t *testing.T) {
	retire := func(f *fixture, job cleanup.Job) string {
		f.t.Helper()
		parent := filepath.Join(job.Repo, "retired-worktrees")
		if err := os.MkdirAll(parent, 0o700); err != nil {
			f.t.Fatal(err)
		}
		retired := filepath.Join(parent, filepath.Base(job.AdminDir))
		if err := os.Rename(job.AdminDir, retired); err != nil {
			f.t.Fatal(err)
		}
		return retired
	}
	t.Run("once its tree is gone", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		retired := retire(f, job)
		if err := os.RemoveAll(f.worktree); err != nil {
			t.Fatal(err)
		}

		f.advance(&job)

		if job.Phase != cleanup.PhaseDeletingRegistration || job.Blocked != nil {
			t.Fatalf("job = phase %s, blockage %+v; want deleting_registration", job.Phase, job.Blocked)
		}
		deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingRegistration, retired, job.Admin))
		if _, err := f.relocator.Admit(context.Background(), &job); err != nil || job.Phase != cleanup.PhaseDone {
			t.Fatalf("Admit() after the deletion = %+v, %v, want the job done", job, err)
		}
	})
	t.Run("never one whose backlink still names a tree", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		retired := retire(f, job)
		moved := f.worktree + "-moved"
		if err := os.Rename(f.worktree, moved); err != nil {
			t.Fatal(err)
		}
		f.write(filepath.Join(retired, "gitdir"), filepath.Join(moved, ".git")+"\n")

		f.advance(&job)

		if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
			t.Fatalf("job = phase %s, blockage %+v; want done", job.Phase, job.Blocked)
		}
		if got := f.id(retired); got != job.Admin {
			t.Errorf("retired admin = %v, want it left as %v", got, job.Admin)
		}
	})
}

func TestAdvanceRefusesAnOrphanSomeoneRegisteredAgain(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.orphan(job)
	admin := f.enroll("again", filepath.Join(f.worktree, ".git"))

	f.advance(&job)

	f.blocked(&job, cleanup.PhasePrepared, "reconcile", fmt.Sprintf("%s registers %s", admin, filepath.Join(f.worktree, ".git")))
}

func TestAdvanceSettlesATreeGoneFromEveryPath(t *testing.T) {
	t.Run("with its registration gone too", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		f.run(f.repo, "worktree", "remove", "--force", f.worktree)

		f.advance(&job)

		if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
			t.Fatalf("job = phase %s, blockage %+v; want done", job.Phase, job.Blocked)
		}
		f.stored(&job)
	})

	t.Run("with a registration the job never locked", func(t *testing.T) {
		f := newFixture(t)
		job := f.accept()
		aside := f.setAside(f.worktree)

		f.advance(&job)

		f.blocked(&job, cleanup.PhasePrepared, "activity", fmt.Sprintf(
			"%s is gone from every path job %s names, but %s still registers it without the job's lock; the tree may only have moved away, so the job waits for it to return or for git worktree prune to drop the registration",
			f.worktree, job.ID, f.adminDir,
		))
		if got := f.id(f.adminDir); got != job.Admin {
			t.Errorf("admin identity = %v, want the admin %v kept", got, job.Admin)
		}
		if got := f.id(aside); got != job.Tree {
			t.Errorf("set-aside identity = %v, want the tree %v", got, job.Tree)
		}
		if err := os.Rename(aside, f.worktree); err != nil {
			t.Fatalf("return the tree: %v", err)
		}
		job.Blocked = nil
		f.advance(&job)
		f.claimed(&job)
	})

	t.Run("with a registration the job locked", func(t *testing.T) {
		f := newFixture(t)
		job := f.intend()
		retireFixture(t, f, &job)
		f.setAside(f.worktree)

		f.advance(&job)

		if job.Phase != cleanup.PhaseDeletingRegistration || job.Blocked != nil || job.Retirement != "" {
			t.Fatalf("job = %+v, want its locked registration up for deletion", job)
		}
		f.stored(&job)
		deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingRegistration, f.adminDir, job.Admin))
		f.absent(f.adminDir)
	})
}

func TestAdmitHoldsADeletedTreeWhoseRegistrationLostItsLock(t *testing.T) {
	f := newFixture(t)
	job := f.claim()
	deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
	if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
		t.Fatalf("remove the lock: %v", err)
	}

	f.admit(&job)

	f.blocked(&job, cleanup.PhaseDeletingTree, "activity", fmt.Sprintf(
		"%s is gone from every path job %s names, but %s still registers it without the job's lock; the tree may only have moved away, so the job waits for it to return or for git worktree prune to drop the registration",
		f.worktree, job.ID, f.adminDir,
	))
	if got := f.id(f.adminDir); got != job.Admin {
		t.Errorf("admin identity = %v, want the admin %v kept", got, job.Admin)
	}
}

func TestAdmitResumesAnAdminDeletionThatRemovedItsLock(t *testing.T) {
	f := newFixture(t)
	job := f.claim()
	deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
	f.admitted(&job, cleanup.PhaseDeletingRegistration, f.adminDir, job.Admin)
	if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
		t.Fatalf("remove the lock: %v", err)
	}

	f.admitted(&job, cleanup.PhaseDeletingRegistration, f.adminDir, job.Admin)

	f.write(filepath.Join(f.adminDir, "locked"), "someone else\n")
	f.admit(&job)
	f.blocked(&job, cleanup.PhaseDeletingRegistration, "identity", fmt.Sprintf("%s: registration has a lock not owned by job %s", f.adminDir, job.ID))
	if got := f.id(f.adminDir); got != job.Admin {
		t.Errorf("admin identity = %v, want the admin %v kept", got, job.Admin)
	}
}

func TestAdvanceAndAdmitMatchByInodeUnderARenumberedVolume(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	current := job.Tree
	job.Tree.Dev++
	job.Admin.Dev++
	if err := f.journal.Save(job); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	f.advance(&job)
	f.claimed(&job)

	f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, current)
}

var errListingTimedOut = fmt.Errorf("list processes: %w: ps: %w", cleanup.ErrUnprobed, context.DeadlineExceeded)

var errArgumentsUnread = fmt.Errorf("%w: native: inspect pid 97627 (binrun): read its arguments: input/output error", cleanup.ErrUnprobed)

func TestAdvanceClaimsAPushedTreeWhoseProcessListingTimedOut(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
	}{
		{"naming the watchers", func(f *fixture) {
			f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) { return nil, errListingTimedOut }
		}},
		{"retiring the watchers", func(f *fixture) {
			f.watchers.retire = func(context.Context, string) error { return errListingTimedOut }
		}},
		{"checking the quarantine", func(f *fixture) {
			f.watchers.quarantine = func(context.Context, string) error { return errListingTimedOut }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
			job := f.accept()
			tt.arrange(f)

			f.advance(&job)

			f.claimed(&job)
			for i, discount := range f.discounts {
				if len(discount) != 0 {
					t.Errorf("guard %d discounted %v, want nothing discounted", i, discount)
				}
			}
		})
	}
}

func TestAcceptAndAdvanceTreatACleanPushedTreeAsIdlePastAFailedProbe(t *testing.T) {
	f := newFixture(t)
	f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
	f.guard = func(context.Context, string) error { return errArgumentsUnread }

	job := f.accept()
	f.advance(&job)

	f.claimed(&job)
}

func TestAcceptRefusesAProbeFailureOnAnUnpushedOrDirtyTree(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
	}{
		{"unpushed", func(*fixture) {}},
		{"dirty", func(f *fixture) {
			f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
			f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.arrange(f)
			f.guard = func(context.Context, string) error { return errArgumentsUnread }

			_, err := f.relocator.Accept(context.Background(), 1, f.request(tt.name == "dirty"))

			if err == nil || !strings.Contains(err.Error(), "read its arguments: input/output error") {
				t.Fatalf("Accept() error = %v, want the failed probe named", err)
			}
			if got := f.id(f.worktree); got.Ino == 0 {
				t.Errorf("worktree identity = %v, want the tree left in place", got)
			}
		})
	}
}

func TestAdvanceBlocksAnUnpushedTreeWhoseProcessListingTimedOut(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.watchers.retire = func(context.Context, string) error { return errListingTimedOut }

	f.advance(&job)

	f.blocked(&job, cleanup.PhasePrepared, timeoutReason, fmt.Sprintf("%v; no remote-tracking ref holds %s's head %s", errListingTimedOut, f.worktree, job.Head))
	if got := f.id(f.worktree); got != job.Tree {
		t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
	}
}

func TestUnprobedQuarantineBlocksAsATimeout(t *testing.T) {
	unpushed := func(f *fixture, job cleanup.Job) string {
		return fmt.Sprintf("%v; no remote-tracking ref holds %s's head %s", errListingTimedOut, f.worktree, job.Head)
	}
	tests := []struct {
		name    string
		arrange func(f *fixture) cleanup.Job
		run     func(f *fixture, job *cleanup.Job)
		phase   cleanup.Phase
		detail  func(f *fixture, job cleanup.Job) string
	}{
		{"before the claim", func(f *fixture) cleanup.Job { return f.accept() }, (*fixture).advance, cleanup.PhasePrepared, unpushed},
		{"claimed admission", func(f *fixture) cleanup.Job { return f.claim() }, (*fixture).admit, cleanup.PhaseClaimed, unpushed},
		{"payload admission", func(f *fixture) cleanup.Job { return f.unregistered() }, (*fixture).admit, cleanup.PhaseUnregistered, nil},
		{"admin admission", func(f *fixture) cleanup.Job {
			job := f.intend()
			moveCapturedAdmin(f.t, f, job)
			retireFixture(f.t, f, &job)
			stageAdmin(f.t, f, &job)
			return job
		}, (*fixture).admit, cleanup.PhaseDeletingAdmin, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := tt.arrange(f)
			f.watchers.quarantine = func(context.Context, string) error { return errListingTimedOut }
			detail := errListingTimedOut.Error()
			if tt.detail != nil {
				detail = tt.detail(f, job)
			}

			tt.run(f, &job)

			f.blocked(&job, tt.phase, timeoutReason, detail)
		})
	}
}

func TestAdvanceReprovesADeferredTreeWhoseProcessListingTimedOut(t *testing.T) {
	f := newFixture(t)
	f.run(f.repo, "update-ref", "refs/remotes/origin/feature", "feature")
	job := f.intend()
	f.watchers.retire = func(context.Context, string) error {
		f.write(filepath.Join(f.worktree, "late.txt"), "late\n")
		return errListingTimedOut
	}

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseQueued, "dirty", "uncommitted changes: late.txt")
	if got := f.read(filepath.Join(f.worktree, "late.txt")); got != "late\n" {
		t.Errorf("late.txt = %q, want it left in place", got)
	}
}

func TestAdvanceStartsNoGitRewriteUnderACancelledContext(t *testing.T) {
	f := newFixture(t)
	job, err := f.relocator.Accept(context.Background(), 1, f.request(true))
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	accepted := job
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.guard = func(context.Context, string) error {
		cancel()
		return nil
	}

	if err := f.relocator.Advance(ctx, &job); !errors.Is(err, context.Canceled) {
		t.Fatalf("Advance() error = %v, want context.Canceled", err)
	}

	if !reflect.DeepEqual(job, accepted) {
		t.Fatalf("job = %+v, want it untouched at %+v", job, accepted)
	}
	f.stored(&job)
	if got := f.id(f.worktree); got != job.Tree {
		t.Errorf("identity at %s = %v, want the tree %v still there", f.worktree, got, job.Tree)
	}
	if got, err := f.try(f.repo, "rev-parse", "--verify", "--quiet", job.RecoveryRef); err == nil {
		t.Errorf("recovery ref = %s, want none written", got)
	}
	if got := f.lockOf(); got != "" {
		t.Errorf("registration lock = %q, want none", got)
	}
}

func TestAdvanceDeferredWaitsThenClaims(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	if job.Phase != cleanup.PhaseQueued {
		t.Fatalf("Intend() phase = %s, want queued", job.Phase)
	}
	active := f.holdAt(f.worktree)

	f.advance(&job)

	if job.Phase != cleanup.PhaseWaiting || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want waiting and unblocked", job.Phase, job.Blocked)
	}
	if want := []cleanup.JobError{{At: f.clock, Phase: cleanup.PhaseWaiting, Message: active.Error()}}; !reflect.DeepEqual(job.Errors, want) {
		t.Errorf("errors = %+v, want %+v", job.Errors, want)
	}
	f.stored(&job)
	if len(f.watchers.retired) != 0 {
		t.Errorf("watchers retired %v while waiting, want none", f.watchers.retired)
	}

	record := f.read(f.layout.RecordPath(job.ID))
	f.advance(&job)
	if got := f.read(f.layout.RecordPath(job.ID)); got != record {
		t.Errorf("record after a still-held recheck = %q, want it unchanged %q", got, record)
	}
	if job.Phase != cleanup.PhaseWaiting || len(job.Errors) != 1 {
		t.Errorf("job is at %s with %d errors, want waiting with 1", job.Phase, len(job.Errors))
	}

	f.release()
	f.advance(&job)

	f.claimed(&job)
	if got := f.id(f.worktree); got != job.Tree {
		t.Errorf("worktree identity = %v, want the tree %v left at its path", got, job.Tree)
	}
	if got := f.lockOf(); got != retirementMarker(&job) {
		t.Errorf("registration lock = %q, want the job's", got)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %s, want %s", got, job.Head)
	}
}

func TestAdvanceDeferredBlocks(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture, job cleanup.Job) (detail string)
		reason  string
	}{
		{"a commit no ref holds landed after the intent", func(f *fixture, job cleanup.Job) string {
			f.run(f.worktree, "switch", "-q", "--detach")
			f.run(f.worktree, "commit", "-q", "--allow-empty", "-m", "late")
			return fmt.Sprintf("HEAD changed: the job captured %q, %s now reads %q", job.Head, f.worktree, f.run(f.worktree, "rev-parse", "HEAD"))
		}, "identity"},
		{"the tree is dirty", func(f *fixture, _ cleanup.Job) string {
			f.write(filepath.Join(f.worktree, "scratch.txt"), "scratch\n")
			return "uncommitted changes: scratch.txt"
		}, "dirty"},
		{"activity is unclear", func(f *fixture, _ cleanup.Job) string {
			f.guard = func(context.Context, string) error { return errors.New("process table unreadable") }
			return "could not verify that " + f.worktree + " is idle: process table unreadable"
		}, "activity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			detail := tt.arrange(f, job)

			f.advance(&job)

			f.blocked(&job, cleanup.PhaseQueued, tt.reason, detail)
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("worktree identity = %v, want the tree %v", got, job.Tree)
			}
			if got := f.lockOf(); got != "" {
				t.Errorf("registration lock = %q, want none", got)
			}
			if len(f.watchers.retired) != 0 {
				t.Errorf("watchers retired %v, want none", f.watchers.retired)
			}
		})
	}
}

func TestAdvanceReturnsTheContextErrorWithoutBlocking(t *testing.T) {
	interrupted := func(f *fixture, cancel context.CancelFunc) {
		f.guard = func(ctx context.Context, _ string) error {
			cancel()
			return ctx.Err()
		}
	}
	cancelledUnderGit := func(f *fixture, cancel context.CancelFunc) {
		f.guard = func(context.Context, string) error {
			cancel()
			return nil
		}
	}
	tests := []struct {
		name     string
		deferred bool
		arrange  func(f *fixture, cancel context.CancelFunc)
	}{
		{"cancelled before the call", false, func(_ *fixture, cancel context.CancelFunc) { cancel() }},
		{"the guard is interrupted before the claim", false, interrupted},
		{"git status is interrupted before the claim", false, cancelledUnderGit},
		{"the guard is interrupted while deferred", true, interrupted},
		{"git rev-parse is interrupted while deferred", true, cancelledUnderGit},
		{"a holder is reported as the context is cancelled", true, func(f *fixture, cancel context.CancelFunc) {
			active := f.holdAt(f.worktree)
			f.guard = func(context.Context, string) error {
				cancel()
				return active
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			begin := f.accept
			if tt.deferred {
				begin = f.intend
			}
			job := begin()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.arrange(f, cancel)
			record := f.read(f.layout.RecordPath(job.ID))
			want := job

			err := f.relocator.Advance(ctx, &job)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Advance() error = %v, want context.Canceled", err)
			}
			if !reflect.DeepEqual(job, want) {
				t.Errorf("job = %+v, want it unchanged %+v", job, want)
			}
			if job.Blocked != nil {
				t.Errorf("blockage = %+v, want none for a cancellation", job.Blocked)
			}
			if got := f.read(f.layout.RecordPath(job.ID)); got != record {
				t.Errorf("record = %q, want it unchanged %q", got, record)
			}
			if got := f.id(f.worktree); got != job.Tree {
				t.Errorf("tree identity = %v, want the tree %v", got, job.Tree)
			}
			if got := f.lockOf(); got != "" {
				t.Errorf("registration lock = %q, want none", got)
			}
		})
	}
}

func TestAdvanceDeferredWhoseTreeVanished(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	if err := os.RemoveAll(f.worktree); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.blocked(&job, cleanup.PhaseQueued, "activity", fmt.Sprintf(
		"%s is gone from every path job %s names, but %s still registers it without the job's lock; the tree may only have moved away, so the job waits for it to return or for git worktree prune to drop the registration",
		f.worktree, job.ID, f.adminDir,
	))

	f.run(f.repo, "worktree", "prune")
	job.Blocked = nil
	f.advance(&job)

	if job.Phase != cleanup.PhaseDone || job.Blocked != nil {
		t.Fatalf("job is at %s with blockage %+v, want done", job.Phase, job.Blocked)
	}
	f.stored(&job)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Errorf("worktree list = %q, want %q", got, want)
	}
}

func TestGitChildrenRunTheConfiguredLFSCleanFilter(t *testing.T) {
	for _, tt := range []struct {
		name   string
		filter string
		want   bool
	}{
		{"configured", "ccx vcs cleanup lfs-clean", true},
		{"unset", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _ := New(Config{LFSClean: tt.filter}).command(t.Context(), "git", "status")
			got := slices.Contains(cmd.Args, "filter.lfs.process="+tt.filter)
			if got != tt.want {
				t.Errorf("args %q carry the filter = %v, want %v", cmd.Args, got, tt.want)
			}
		})
	}
}
