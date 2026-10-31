package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
)

type adminAdmitter struct {
	*fakeRelocator
	root cleanup.DeletionRoot
}

func (a *adminAdmitter) Admit(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	root, err := a.fakeRelocator.Admit(ctx, job)
	if err != nil || job.Blocked != nil {
		return root, err
	}
	if job.Phase == cleanup.PhaseDeletingAdmin {
		return a.root, nil
	}
	return root, nil
}

func TestEngineDeletesAdminBeforeRetiredPayloadAcrossRestarts(t *testing.T) {
	h := newHarness(t, paced())
	h.tuning.SliceEntries = 1
	h.tuning.KeepDone = 1
	admin := filepath.Join(h.root, "external-admin")
	if err := os.Mkdir(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"locked", "HEAD", "commondir", "gitdir"} {
		if err := os.WriteFile(filepath.Join(admin, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	adminID, _, err := cleanup.LstatID(admin)
	if err != nil {
		t.Fatal(err)
	}
	job := h.seed("ordered", 1, cleanup.PhaseDeletingAdmin, func(job *cleanup.Job) {
		job.Head, job.RecoveryRef, job.Admin = legacyHead, cleanup.RecoveryRefFor(job.ID), adminID
	})
	if err := os.Rename(job.Original, job.Payload); err != nil {
		t.Fatal(err)
	}
	locator := "gitdir: " + admin + "\n"
	if err := os.WriteFile(filepath.Join(job.Payload, ".git"), []byte(locator), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job.Payload, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func() *Engine {
		e := h.build()
		e.relocator = &adminAdmitter{fakeRelocator: h.relocator, root: cleanup.DeletionRoot{Dir: filepath.Dir(admin), Name: filepath.Base(admin), ID: adminID}}
		e.deleter = rmtree.Deleter{}
		if err := e.load(); err != nil {
			t.Fatal(err)
		}
		e.gov.observe(h.clock.Now(), 0, errUnsampled, true)
		return e
	}
	e := build()
	restartedAdmin, restartedPayload := false, false
	for range 32 {
		job = e.jobs[job.ID]
		if job.Phase == cleanup.PhaseDone {
			break
		}
		if err := e.slice(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		job = e.jobs[job.ID]
		if job.Blocked != nil {
			t.Fatalf("job blocked: %+v", job)
		}
		if job.Phase == cleanup.PhaseDeletingAdmin {
			data, err := os.ReadFile(filepath.Join(job.Payload, ".git"))
			if err != nil || string(data) != locator || job.Removed != 0 {
				t.Fatalf("admin deletion changed locator or payload count: %q, %v, %+v", data, err, job)
			}
			if !restartedAdmin {
				if err := e.park(); err != nil {
					t.Fatal(err)
				}
				e, restartedAdmin = build(), true
			}
		}
		if job.Phase == cleanup.PhaseDeletingRetiredPayload && !restartedPayload {
			if _, err := os.Lstat(admin); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("admin remains at payload checkpoint: %v", err)
			}
			stored, damaged, err := h.journal.Load()
			if err != nil || len(damaged) != 0 || len(stored) != 1 || stored[0].Phase != cleanup.PhaseDeletingRetiredPayload {
				t.Fatalf("payload checkpoint = %v, %v, %v", stored, damaged, err)
			}
			if err := os.Remove(filepath.Join(job.Payload, ".git")); err != nil {
				t.Fatal(err)
			}
			e, restartedPayload = build(), true
		}
	}
	job = e.jobs[job.ID]
	if job.Phase != cleanup.PhaseDone || job.Removed != 2 || !restartedAdmin || !restartedPayload {
		t.Fatalf("finished job = %+v, restarts %v/%v", job, restartedAdmin, restartedPayload)
	}
	for _, path := range []string{admin, job.Payload} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("owned root remains at %s: %v", path, err)
		}
	}
	if err := h.journal.Discard(job.ID); err != nil {
		t.Fatalf("Discard() = %v", err)
	}
}
