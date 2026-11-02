package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
		t.Errorf("backlink = %q, want it untouched %q", got, job.Links.AdminGitdir)
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

func TestAdmitResumesAPartlyDeletedTreeWithoutReprovingIt(t *testing.T) {
	f := newFixture(t)
	job := f.claim()
	f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
	if err := os.Remove(filepath.Join(f.worktree, "base.txt")); err != nil {
		t.Fatalf("remove base.txt: %v", err)
	}
	f.write(filepath.Join(f.worktree, "late.txt"), "late\n")

	f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree)
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
			return fmt.Sprintf("%s no longer carries job %s's lock", f.adminDir, job.ID)
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
			f.absent(f.layout.ScratchIndex(job.ID))
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
		})
	}
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

func TestAdmitRefusesARegistrationThatLostItsLockMidDeletion(t *testing.T) {
	f := newFixture(t)
	job := f.claim()
	deleteRoot(t, f.admitted(&job, cleanup.PhaseDeletingTree, f.worktree, job.Tree))
	if err := os.Remove(filepath.Join(f.adminDir, "locked")); err != nil {
		t.Fatalf("remove the lock: %v", err)
	}

	f.admit(&job)

	f.blocked(&job, cleanup.PhaseDeletingRegistration, "identity", fmt.Sprintf(
		"%s no longer carries job %s's lock; git worktree prune drops it once nothing holds it, then ccx vcs cleanup retry finishes the job",
		f.adminDir, job.ID,
	))
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

func TestSiteNamesTheVolumeOfTheNextRoot(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	for _, tt := range []struct {
		phase cleanup.Phase
		want  string
	}{
		{cleanup.PhasePending, filepath.Dir(job.Original)},
		{cleanup.PhaseClaimed, filepath.Dir(job.Original)},
		{cleanup.PhaseDeletingTree, filepath.Dir(job.Original)},
		{cleanup.PhaseDeletingRegistration, filepath.Dir(job.AdminDir)},
		{cleanup.PhaseDeletingAdmin, filepath.Dir(job.AdminDir)},
		{cleanup.PhaseDeleting, f.layout.JobDir(job.ID)},
		{cleanup.PhaseDeletingRetiredPayload, f.layout.JobDir(job.ID)},
	} {
		job.Phase = tt.phase
		if got := f.relocator.Site(job); got != tt.want {
			t.Errorf("Site() at %s = %s, want %s", tt.phase, got, tt.want)
		}
	}
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
		{"a commit landed after the intent", func(f *fixture, job cleanup.Job) string {
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
