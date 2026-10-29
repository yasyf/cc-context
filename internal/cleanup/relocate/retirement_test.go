package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
