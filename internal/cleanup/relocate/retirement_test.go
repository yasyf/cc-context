package relocate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func retireFixture(t *testing.T, f *fixture, job *cleanup.Job) {
	t.Helper()
	if err := f.relocator.Retire(context.Background(), job); err != nil {
		t.Fatalf("Retire() = %v", err)
	}
}

func TestPausedRetirementLeavesEachLogicalTreeInPlace(t *testing.T) {
	for _, phase := range []cleanup.Phase{cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePending, cleanup.PhasePrepared, cleanup.PhaseMoved, cleanup.PhaseDetached} {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t)
			var job cleanup.Job
			switch phase {
			case cleanup.PhaseQueued, cleanup.PhaseWaiting:
				job = f.intend()
			case cleanup.PhasePending:
				var err error
				job, err = f.relocator.Pend(context.Background(), 1, f.request(false))
				if err != nil {
					t.Fatal(err)
				}
			default:
				job = f.accept()
			}
			tree, registered := f.worktree, f.worktree
			if phase == cleanup.PhaseMoved || phase == cleanup.PhaseDetached {
				f.run(f.repo, "worktree", "move", f.worktree, job.Registered)
				tree, registered = job.Registered, job.Registered
				if phase == cleanup.PhaseDetached {
					if err := os.Rename(job.Registered, job.Payload); err != nil {
						t.Fatal(err)
					}
					tree = job.Payload
				}
			}
			job.Phase = phase
			if err := f.journal.Save(job); err != nil {
				t.Fatal(err)
			}
			before := f.id(tree)
			retireFixture(t, f, &job)
			if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete || job.Phase != phase {
				t.Fatalf("Retire() left %+v, want retired at %s", job, phase)
			}
			if got := f.id(tree); got != before {
				t.Errorf("tree identity = %v, want %v", got, before)
			}
			if got := f.read(filepath.Join(tree, "feature.txt")); got != "feature\n" {
				t.Errorf("retained contents = %q", got)
			}
			if got := f.run(f.repo, "worktree", "list", "--porcelain"); strings.Contains(got, registered) {
				t.Errorf("retired registration remains listed: %s", got)
			}
			if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
				t.Errorf("recovery ref = %q, want %q", got, job.Head)
			}
			f.advance(&job)
			f.finished(&job)
			if job.Retirement != "" || f.id(job.Payload) != before {
				t.Errorf("resumed job = %+v, want original payload and cleared retirement", job)
			}
		})
	}
}

func TestPausedRetirementRecoversOnlyOwnedCrashStates(t *testing.T) {
	tests := []struct {
		name                 string
		state                cleanup.Retirement
		marker               string
		empty, resume, block bool
	}{
		{"intent before marker", cleanup.RetirementIntent, "", false, false, false},
		{"published marker", cleanup.RetirementIntent, "own", false, false, false},
		{"truncated backlink", cleanup.RetirementIntent, "own", true, false, false},
		{"completed retirement", cleanup.RetirementComplete, "own", true, true, false},
		{"restored backlink", cleanup.RetirementIntent, "own", false, true, false},
		{"removed marker", cleanup.RetirementIntent, "", false, true, false},
		{"repaused restored backlink", cleanup.RetirementIntent, "own", false, false, false},
		{"repaused removed marker", cleanup.RetirementIntent, "", false, false, false},
		{"foreign marker", cleanup.RetirementIntent, "operator\n", false, false, true},
		{"partial marker", cleanup.RetirementIntent, "partial", false, false, true},
		{"empty without marker", cleanup.RetirementIntent, "", true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			job.Retirement = tt.state
			if err := f.journal.Save(job); err != nil {
				t.Fatal(err)
			}
			if tt.marker != "" {
				marker := tt.marker
				if marker == "own" {
					marker = retirementMarker(&job)
				}
				f.write(filepath.Join(f.adminDir, "locked"), marker)
			}
			if tt.empty {
				f.write(filepath.Join(f.adminDir, "gitdir"), "")
			}
			if tt.resume {
				f.advance(&job)
				f.finished(&job)
				return
			}
			retireFixture(t, f, &job)
			if tt.block {
				if job.Blocked == nil || job.Blocked.Reason != "identity" {
					t.Fatalf("Retire() = %+v, want identity block", job)
				}
				if got := f.read(filepath.Join(f.adminDir, "gitdir")); !tt.empty && got != job.Links.AdminGitdir {
					t.Errorf("blocked backlink = %q, want unchanged %q", got, job.Links.AdminGitdir)
				}
				return
			}
			if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
				t.Errorf("recovered state = %+v", job)
			}
		})
	}
}

func TestPausedRetirementKeepsDirtyContentsButResumeRechecksDirt(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	marker := filepath.Join(f.worktree, "dirty.txt")
	f.write(marker, "uncommitted\n")
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("Retire() = %+v", job)
	}
	f.advance(&job)
	if job.Blocked == nil || job.Blocked.Reason != "dirty" || job.Retirement != "" {
		t.Fatalf("Advance() = %+v, want ordinary dirt refusal after restoring", job)
	}
	if got := f.read(marker); got != "uncommitted\n" {
		t.Errorf("dirty contents = %q", got)
	}
}

func TestPausedRetirementRefusesActiveOrReplacedTrees(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "active"
		if replaced {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			if replaced {
				f.run(f.repo, "worktree", "move", f.worktree, filepath.Join(f.root, "kept"))
				f.run(f.repo, "worktree", "add", "-q", "--detach", f.worktree)
			} else {
				_ = f.holdAt(f.worktree)
			}
			before := f.run(f.repo, "worktree", "list", "--porcelain")
			retireFixture(t, f, &job)
			if job.Blocked == nil || job.Retirement != "" {
				t.Fatalf("Retire() = %+v, want a block without intent", job)
			}
			if got := f.run(f.repo, "worktree", "list", "--porcelain"); got != before {
				t.Errorf("blocked registry changed: %s", got)
			}
		})
	}
}

func TestPausedRetirementReconcilesAnInterruptedMoveWithoutTouchingTheReleasedPath(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.run(f.repo, "worktree", "move", f.worktree, job.Registered)
	f.run(f.repo, "worktree", "add", "-q", "--detach", f.worktree)
	replacement := f.observe(f.worktree)
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("Retire() = %+v", job)
	}
	if got := f.observe(f.worktree); got != replacement {
		t.Errorf("released original = %+v, want untouched %+v", got, replacement)
	}
	f.advance(&job)
	f.finished(&job)
	if got := f.observe(f.worktree); got != replacement {
		t.Errorf("released original after resume = %+v, want %+v", got, replacement)
	}
}

func TestRetiredRegistrationRevalidatesDeviceOnlyChanges(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	retireFixture(t, f, &job)
	if job.Blocked != nil {
		t.Fatal(job.Blocked)
	}
	wantTree, wantAdmin := job.Tree, job.Admin
	job.Tree.Dev++
	job.Admin.Dev++
	if err := f.journal.Save(job); err != nil {
		t.Fatal(err)
	}
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Tree != wantTree || job.Admin != wantAdmin {
		t.Errorf("revalidated job = %+v, want tree %+v admin %+v", job, wantTree, wantAdmin)
	}
	f.stored(&job)
}

func TestRetiredNativePayloadKeepsItsDeviceOnlyRefresh(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	f.run(f.repo, "worktree", "move", job.Original, job.Registered)
	if err := os.Rename(job.Registered, job.Payload); err != nil {
		t.Fatal(err)
	}
	job.Phase = cleanup.PhaseDetached
	if err := f.journal.Save(job); err != nil {
		t.Fatal(err)
	}
	retireFixture(t, f, &job)
	if job.Blocked != nil {
		t.Fatal(job.Blocked)
	}
	want := job.Admin
	job.Admin.Dev++
	if err := f.journal.Save(job); err != nil {
		t.Fatal(err)
	}
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Admin != want || job.Phase != cleanup.PhaseDetached {
		t.Fatalf("native device refresh = %+v, want captured native admin %+v", job, want)
	}
	f.advance(&job)
	f.finished(&job)
}

func TestPausedRetirementIgnoresAnInterruptedMarkerTemporary(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	job.Retirement = cleanup.RetirementIntent
	if err := f.journal.Save(job); err != nil {
		t.Fatal(err)
	}
	stump := filepath.Join(f.adminDir, "ccx-retiring-"+job.ID)
	f.write(stump, "partial")
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("Retire() = %+v, want successful marker publication", job)
	}
	if got := f.read(stump); got != "partial" {
		t.Errorf("unowned temporary = %q, want preserved partial content", got)
	}
}

func TestPausedRetirementDoesNotMutateARegistrationReplacedDuringTheFinalGuard(t *testing.T) {
	for _, replaceAdmin := range []bool{false, true} {
		name := "backlink"
		if replaceAdmin {
			name = "admin"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			calls := 0
			kept := f.adminDir + "-kept"
			if !replaceAdmin {
				kept = filepath.Join(f.adminDir, "gitdir-kept")
			}
			f.guard = func(context.Context, string) error {
				calls++
				if calls != 2 {
					return nil
				}
				if replaceAdmin {
					if err := os.Rename(f.adminDir, kept); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(f.adminDir, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Rename(filepath.Join(f.adminDir, "gitdir"), kept); err != nil {
					t.Fatal(err)
				}
				f.write(filepath.Join(f.adminDir, "gitdir"), "replacement\n")
				return nil
			}
			retireFixture(t, f, &job)
			if job.Blocked == nil || job.Blocked.Reason != "identity" {
				t.Fatalf("Retire() = %+v, want identity refusal", job)
			}
			if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != "replacement\n" {
				t.Errorf("replacement backlink = %q", got)
			}
			captured := kept
			if replaceAdmin {
				captured = filepath.Join(kept, "gitdir")
			}
			if got := f.read(captured); got != job.Links.AdminGitdir {
				t.Errorf("captured backlink = %q, want unchanged %q", got, job.Links.AdminGitdir)
			}
		})
	}
}

func (f *fixture) headMovedAt(at time.Time) {
	f.t.Helper()
	for _, name := range []string{"HEAD", filepath.Join("logs", "HEAD")} {
		if err := os.Chtimes(filepath.Join(f.adminDir, name), at, at); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) commitLater() {
	f.t.Helper()
	f.write(filepath.Join(f.worktree, "later.txt"), "later\n")
	f.run(f.worktree, "add", "later.txt")
	f.run(f.worktree, "commit", "-q", "-m", "later")
}

func TestPausedRetirementWaitsForTheHeadToRest(t *testing.T) {
	f := newFixture(t)
	f.quiet = time.Hour
	f.over(f.journal)
	job := f.intend()
	moved := f.clock.Add(-10 * time.Minute)
	f.headMovedAt(moved)
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != "" || !job.RetireAfter.Equal(moved.Add(time.Hour)) {
		t.Fatalf("Retire() = %+v, want retirement deferred to %s", job, moved.Add(time.Hour))
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
		t.Errorf("deferred backlink = %q, want %q", got, job.Links.AdminGitdir)
	}
	if got := f.run(f.repo, "worktree", "list", "--porcelain"); !strings.Contains(got, f.worktree) {
		t.Errorf("deferred registration is no longer listed: %s", got)
	}
	f.clock = moved.Add(time.Hour)
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete || !job.RetireAfter.IsZero() {
		t.Fatalf("Retire() after an hour of rest = %+v, want retired", job)
	}
}

func TestPausedRetirementBlocksATreeWhoseLaneCommittedWhileItWaited(t *testing.T) {
	f := newFixture(t)
	f.quiet = time.Hour
	f.over(f.journal)
	job := f.intend()
	f.headMovedAt(f.clock)
	retireFixture(t, f, &job)
	if job.RetireAfter.IsZero() {
		t.Fatalf("Retire() = %+v, want a deferral", job)
	}
	f.commitLater()
	f.clock = f.clock.Add(24 * time.Hour)
	retireFixture(t, f, &job)
	if job.Blocked == nil || job.Blocked.Reason != "identity" || job.Retirement != "" {
		t.Fatalf("Retire() = %+v, want an identity block without retirement", job)
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
		t.Errorf("blocked backlink = %q, want %q", got, job.Links.AdminGitdir)
	}
}

func TestCancelRestoresARetiredRegistrationItsLaneKeptUsing(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	retireFixture(t, f, &job)
	if job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("Retire() = %+v", job)
	}
	f.commitLater()
	if err := f.relocator.Cancel(context.Background(), &job); err != nil {
		t.Fatalf("Cancel() = %v", err)
	}
	if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != job.Links.AdminGitdir {
		t.Errorf("restored backlink = %q, want %q", got, job.Links.AdminGitdir)
	}
	if _, err := os.Stat(filepath.Join(f.adminDir, "locked")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("retirement lock = %v, want removed", err)
	}
	if got := f.run(f.repo, "worktree", "list", "--porcelain"); !strings.Contains(got, f.worktree) {
		t.Errorf("cancelled registration is not listed: %s", got)
	}
	if got := f.run(f.repo, "rev-parse", job.RecoveryRef); got != job.Head {
		t.Errorf("recovery ref = %q, want kept at %q", got, job.Head)
	}
}

func TestCancelRefusesWhatItCannotProveItsOwn(t *testing.T) {
	tests := []struct {
		name   string
		mangle func(f *fixture, job *cleanup.Job)
		reason string
	}{
		{"foreign lock", func(f *fixture, _ *cleanup.Job) { f.write(filepath.Join(f.adminDir, "locked"), "operator\n") }, "identity"},
		{"repointed tree", func(f *fixture, _ *cleanup.Job) { f.write(filepath.Join(f.worktree, ".git"), "gitdir: /elsewhere\n") }, "identity"},
		{"moved tree", func(_ *fixture, job *cleanup.Job) { job.Phase = cleanup.PhaseMoved }, "phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			retireFixture(t, f, &job)
			tt.mangle(f, &job)
			err := f.relocator.Cancel(context.Background(), &job)
			var refused *cleanup.RefusedError
			if !errors.As(err, &refused) || refused.Reason != tt.reason {
				t.Fatalf("Cancel() = %v, want a %s refusal", err, tt.reason)
			}
			if got := f.read(filepath.Join(f.adminDir, "gitdir")); got != "" {
				t.Errorf("refused backlink = %q, want still retired", got)
			}
		})
	}
}
