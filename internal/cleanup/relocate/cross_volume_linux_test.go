package relocate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func crossVolumeFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	root, err := os.MkdirTemp("/dev/shm", "ccx-retirement-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if sameVolume(f.id(root), f.id(f.worktree)) {
		t.Fatal("cross-volume fixture needs distinct filesystems")
	}
	f.open(filepath.Join(root, "state"))
	return f
}

func TestPausedCrossVolumeRetirementPreservesPayloadAndRefusesPhysicalResume(t *testing.T) {
	for _, displaced := range []bool{false, true} {
		name := "native"
		if displaced {
			name = "moved-admin"
		}
		t.Run(name, func(t *testing.T) {
			f := crossVolumeFixture(t)
			before := f.id(f.worktree)
			job, err := f.relocator.Pend(context.Background(), 1, f.request(false))
			if err != nil {
				t.Fatal(err)
			}
			admin := f.adminDir
			if displaced {
				admin = moveCapturedAdmin(t, f, job)
			}
			pointer := f.read(filepath.Join(f.worktree, ".git"))
			retireFixture(t, f, &job)
			if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete || job.Phase != cleanup.PhasePending {
				t.Fatalf("Retire() = %+v", job)
			}
			if got := f.id(f.worktree); got != before {
				t.Fatalf("payload identity = %+v, want %+v", got, before)
			}
			if got := f.listing(); strings.Contains(got, f.worktree) {
				t.Fatalf("retired registration remains listed: %s", got)
			}
			f.advance(&job)
			if job.Blocked == nil || job.Blocked.Reason != "volume" || job.Retirement != cleanup.RetirementComplete {
				t.Fatalf("Advance() = %+v, want retired registration blocked on physical volume", job)
			}
			if got := f.read(filepath.Join(admin, "gitdir")); got != "" {
				t.Errorf("blocked resume restored backlink %q", got)
			}
			if got := f.read(filepath.Join(admin, "locked")); got != retirementMarker(&job) {
				t.Errorf("blocked resume changed retirement marker %q", got)
			}
			if got := f.read(filepath.Join(f.worktree, ".git")); got != pointer {
				t.Errorf("blocked resume changed pointer %q", got)
			}
			if got := f.id(f.worktree); got != before {
				t.Errorf("blocked resume changed payload identity %+v", got)
			}
			if got := f.read(filepath.Join(f.worktree, "feature.txt")); got != "feature\n" {
				t.Errorf("retained payload = %q", got)
			}
			f.absent(job.Registered)
			f.absent(job.Payload)
		})
	}
}

func TestCrossVolumePhysicalAdmissionStillRefuses(t *testing.T) {
	for _, operation := range []string{"preview", "accept", "intend"} {
		t.Run(operation, func(t *testing.T) {
			f := crossVolumeFixture(t)
			before := f.listing()
			var err error
			switch operation {
			case "preview":
				_, err = f.relocator.Preview(context.Background(), f.request(false))
			case "accept":
				_, err = f.relocator.Accept(context.Background(), 1, f.request(false))
			case "intend":
				_, err = f.relocator.Intend(context.Background(), 1, f.deferral())
			}
			var refused *cleanup.RefusedError
			if !errors.As(err, &refused) || refused.Reason != "volume" {
				t.Fatalf("physical admission = %v, want volume refusal", err)
			}
			if got := f.listing(); got != before {
				t.Errorf("refused admission changed registrations: %s", got)
			}
		})
	}
}

func TestPausedCrossVolumeRetirementStillRefusesActivityAndReplacement(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		name := "active"
		if replaced {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			f := crossVolumeFixture(t)
			job, err := f.relocator.Pend(context.Background(), 1, f.request(false))
			if err != nil {
				t.Fatal(err)
			}
			if replaced {
				f.run(f.repo, "worktree", "move", f.worktree, filepath.Join(f.root, "kept"))
				f.run(f.repo, "worktree", "add", "-q", "--detach", f.worktree)
			} else {
				_ = f.holdAt(f.worktree)
			}
			before := f.listing()
			retireFixture(t, f, &job)
			if job.Blocked == nil || job.Retirement != "" {
				t.Fatalf("Retire() = %+v, want refusal without retirement", job)
			}
			if got := f.listing(); got != before {
				t.Errorf("refused retirement changed registrations: %s", got)
			}
		})
	}
}

func TestPausedCrossVolumePendingCannotMoveBeforeRetirement(t *testing.T) {
	f := crossVolumeFixture(t)
	before := f.id(f.worktree)
	listing := f.listing()
	job, err := f.relocator.Pend(context.Background(), 1, f.request(false))
	if err != nil {
		t.Fatal(err)
	}
	f.advance(&job)
	if job.Blocked == nil || job.Blocked.Reason != "volume" || job.Retirement != "" {
		t.Fatalf("Advance() = %+v, want physical volume block", job)
	}
	if got := f.listing(); got != listing {
		t.Errorf("physical refusal changed registrations: %s", got)
	}
	if got := f.id(f.worktree); got != before {
		t.Errorf("physical refusal changed payload identity %+v", got)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("physical refusal retired watchers: %v", f.watchers.retired)
	}
	if _, err := f.try(f.repo, "show-ref", "--verify", job.RecoveryRef); err == nil {
		t.Error("physical refusal created recovery ref")
	}
	f.absent(job.Registered)
	f.absent(job.Payload)
}

func TestCrossVolumePreparedCannotMoveAfterJournalChangesVolume(t *testing.T) {
	f := newFixture(t)
	job := f.accept()
	before := f.id(f.worktree)
	listing := f.listing()
	root, err := os.MkdirTemp("/dev/shm", "ccx-prepared-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if sameVolume(f.id(root), before) {
		t.Fatal("cross-volume fixture needs distinct filesystems")
	}
	f.open(filepath.Join(root, "state"))
	job.Registered = f.layout.Registered(job.ID)
	job.Payload = f.layout.Payload(job.ID)
	if err := f.journal.Create(job); err != nil {
		t.Fatal(err)
	}
	f.advance(&job)
	if job.Blocked == nil || job.Blocked.Reason != "volume" || job.Phase != cleanup.PhasePrepared {
		t.Fatalf("Advance() = %+v, want prepared volume block", job)
	}
	if got := f.listing(); got != listing {
		t.Errorf("physical refusal changed registrations: %s", got)
	}
	if got := f.id(f.worktree); got != before {
		t.Errorf("physical refusal changed payload identity %+v", got)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("physical refusal retired watchers: %v", f.watchers.retired)
	}
	if _, err := f.try(f.repo, "show-ref", "--verify", job.RecoveryRef); err == nil {
		t.Error("physical refusal created recovery ref")
	}
	f.absent(job.Registered)
	f.absent(job.Payload)
}
