package relocate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func mountJob(t *testing.T, f *fixture, phase cleanup.Phase) cleanup.Job {
	t.Helper()
	var job cleanup.Job
	var err error
	switch phase {
	case cleanup.PhaseQueued:
		job = f.intend()
	case cleanup.PhaseWaiting:
		_ = f.holdAt(f.worktree)
		job = f.intend()
		f.release()
	case cleanup.PhasePending:
		job, err = f.relocator.Pend(t.Context(), 1, f.request(false))
	case cleanup.PhasePrepared:
		job = f.accept()
	case cleanup.PhaseMoved, cleanup.PhaseDetached:
		job = f.accept()
		f.park(&job)
		if phase == cleanup.PhaseDetached {
			err = os.Rename(job.Registered, job.Payload)
			job.Phase = phase
		}
	default:
		t.Fatalf("unsupported fixture phase %s", phase)
	}
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func reloadMountJob(t *testing.T, f *fixture, job cleanup.Job) cleanup.Job {
	t.Helper()
	if err := f.journal.Save(job); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := f.journal.Load()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Load = %v, %v; want one persisted job", jobs, err)
	}
	return jobs[0]
}

func TestAdvanceRevalidatesMountDeviceOnly(t *testing.T) {
	for _, phase := range []cleanup.Phase{cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePending, cleanup.PhasePrepared, cleanup.PhaseMoved, cleanup.PhaseDetached} {
		for _, devices := range []string{"tree", "admin", "both"} {
			t.Run(string(phase)+"/"+devices, func(t *testing.T) {
				f := newFixture(t)
				job := mountJob(t, f, phase)
				tree, admin := job.Tree, job.Admin
				if devices != "admin" {
					job.Tree.Dev++
				}
				if devices != "tree" {
					job.Admin.Dev++
				}
				job = reloadMountJob(t, f, job)

				f.advance(&job)

				f.finished(&job)
				if job.Tree != tree || job.Admin != admin {
					t.Errorf("normalized identities = %v/%v, want %v/%v", job.Tree, job.Admin, tree, admin)
				}
				if got := f.read(filepath.Join(job.Payload, "feature.txt")); got != "feature\n" {
					t.Errorf("payload = %q, want original contents", got)
				}
			})
		}
	}
}

func TestMountDeviceChangeRequiresCompleteCapturedProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture, *cleanup.Job)
	}{
		{"tree inode", func(_ *fixture, job *cleanup.Job) { job.Tree.Ino++ }},
		{"admin inode", func(_ *fixture, job *cleanup.Job) { job.Admin.Ino++ }},
		{"head", func(_ *fixture, job *cleanup.Job) { job.Head = "0123456789012345678901234567890123456789" }},
		{"missing head", func(_ *fixture, job *cleanup.Job) { job.Head, job.RecoveryRef = "", "" }},
		{"dotgit", func(_ *fixture, job *cleanup.Job) { job.Links.DotGit += "changed\n" }},
		{"backlink", func(_ *fixture, job *cleanup.Job) { job.Links.AdminGitdir += "changed\n" }},
		{"common directory", func(f *fixture, _ *cleanup.Job) {
			other := newFixture(f.t)
			f.write(filepath.Join(f.adminDir, "commondir"), other.common+"\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.accept()
			job.Tree.Dev++
			job.Admin.Dev++
			tc.change(f, &job)
			job = reloadMountJob(t, f, job)
			tree, admin := job.Tree, job.Admin

			f.advance(&job)

			if job.Blocked == nil || job.Tree != tree || job.Admin != admin {
				t.Fatalf("job = %+v, want blocked with captured identities", job)
			}
			if got := f.read(filepath.Join(f.worktree, "feature.txt")); got != "feature\n" {
				t.Errorf("original contents = %q", got)
			}
			f.absent(job.Registered)
			f.absent(job.Payload)
		})
	}
}

func TestMountRevalidationKeepsActiveTreesRegistered(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	job.Tree.Dev++
	job.Admin.Dev++
	job = reloadMountJob(t, f, job)
	_ = f.holdAt(f.worktree)

	f.advance(&job)

	if job.Phase != cleanup.PhaseWaiting {
		t.Fatalf("phase = %s, want waiting", job.Phase)
	}
	if got := f.observe(f.worktree); got.Tree != f.id(f.worktree) {
		t.Errorf("active registration = %+v", got)
	}
	if len(f.watchers.retired) != 0 {
		t.Errorf("retired active watchers %v", f.watchers.retired)
	}
	f.absent(job.Registered)
	f.absent(job.Payload)
}

func TestMountRevalidationNeverReadsReleasedOriginal(t *testing.T) {
	f := newFixture(t)
	job := mountJob(t, f, cleanup.PhaseMoved)
	job.Tree.Dev++
	job.Admin.Dev++
	job = reloadMountJob(t, f, job)
	if err := os.WriteFile(f.worktree, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	f.advance(&job)

	f.finished(&job)
	if got := f.read(f.worktree); got != "replacement" {
		t.Errorf("released original = %q, want replacement untouched", got)
	}
}
