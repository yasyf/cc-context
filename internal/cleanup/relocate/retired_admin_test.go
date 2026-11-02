package relocate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/rmtree"
)

func stageAdmin(t *testing.T, f *fixture, job *cleanup.Job) {
	t.Helper()
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("job = %+v, want a completed retirement to stage", job)
	}
	if err := os.Rename(job.Original, job.Payload); err != nil {
		t.Fatal(err)
	}
	job.Phase, job.Retirement, job.Updated = cleanup.PhaseDeletingAdmin, "", f.clock
	if err := f.journal.Save(*job); err != nil {
		t.Fatal(err)
	}
}

func moveCapturedAdmin(t *testing.T, f *fixture, job cleanup.Job) string {
	t.Helper()
	retired := filepath.Join(f.common, "retired-worktrees", filepath.Base(job.AdminDir))
	if err := os.Mkdir(filepath.Dir(retired), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(job.AdminDir, retired); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(job.Original, ".git"), "gitdir: "+retired+"\n")
	if got := f.id(retired); got != job.Admin {
		t.Fatalf("moved admin identity = %+v, want %+v", got, job.Admin)
	}
	if got := f.id(job.Original); got != job.Tree {
		t.Fatalf("retained tree identity = %+v, want %+v", got, job.Tree)
	}
	if got := f.read(filepath.Join(retired, "gitdir")); got != job.Links.AdminGitdir {
		t.Fatalf("moved backlink = %q, want %q", got, job.Links.AdminGitdir)
	}
	f.absent(job.AdminDir)
	if got, want := f.listing(), f.mainEntry(); got != want {
		t.Fatalf("native registry = %q, want %q", got, want)
	}
	return retired
}

func TestMovedCapturedAdminRetirementKeepsStrictActivityChecks(t *testing.T) {
	for _, unprobed := range []bool{false, true} {
		name := "active"
		if unprobed {
			name = "unprobed pushed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			retired := moveCapturedAdmin(t, f, job)
			f.run(f.repo, "update-ref", "refs/remotes/origin/feature", job.Head)
			if unprobed {
				f.guard = func(context.Context, string) error { return cleanup.ErrUnprobed }
			} else {
				_ = f.holdAt(job.Original)
			}
			retireFixture(t, f, &job)
			if job.Blocked == nil || job.Blocked.Reason != "activity" || job.Retirement != "" {
				t.Fatalf("Retire() = %+v, want activity block before intent", job)
			}
			if got := f.read(filepath.Join(retired, "gitdir")); got != job.Links.AdminGitdir {
				t.Errorf("blocked backlink = %q, want %q", got, job.Links.AdminGitdir)
			}
			f.absent(filepath.Join(retired, "locked"))
			f.absent(job.Payload)
			if got := f.id(job.Original); got != job.Tree {
				t.Errorf("blocked tree identity = %+v, want %+v", got, job.Tree)
			}
			f.stored(&job)
		})
	}
}

func TestMovedCapturedAdminRetirementRefusesAReplacedAdmin(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	retired := moveCapturedAdmin(t, f, job)
	kept := retired + "-kept"
	if err := os.Rename(retired, kept); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(retired, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HEAD", "commondir", "gitdir"} {
		f.write(filepath.Join(retired, name), f.read(filepath.Join(kept, name)))
	}
	replacement := f.id(retired)
	if replacement == job.Admin {
		t.Fatal("replacement unexpectedly kept the captured admin identity")
	}
	retireFixture(t, f, &job)
	if job.Blocked == nil || job.Blocked.Reason != "identity" || job.Retirement != "" {
		t.Fatalf("Retire() = %+v, want identity block before intent", job)
	}
	for _, admin := range []string{retired, kept} {
		if got := f.read(filepath.Join(admin, "gitdir")); got != job.Links.AdminGitdir {
			t.Errorf("%s backlink = %q, want %q", admin, got, job.Links.AdminGitdir)
		}
		f.absent(filepath.Join(admin, "locked"))
	}
	if got := f.id(retired); got != replacement {
		t.Errorf("replacement identity = %+v, want %+v", got, replacement)
	}
	f.absent(job.Payload)
	f.stored(&job)
}

func TestMovedCapturedAdminPhysicalAdmissionKeepsStrictActivityChecks(t *testing.T) {
	tests := []struct {
		name     string
		phase    cleanup.Phase
		unprobed bool
	}{
		{"admin active", cleanup.PhaseDeletingAdmin, false},
		{"admin unprobed pushed", cleanup.PhaseDeletingAdmin, true},
		{"payload active", cleanup.PhaseDeletingRetiredPayload, false},
		{"payload unprobed pushed without locator", cleanup.PhaseDeletingRetiredPayload, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			moveCapturedAdmin(t, f, job)
			f.run(f.repo, "update-ref", "refs/remotes/origin/feature", job.Head)
			retireFixture(t, f, &job)
			if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
				t.Fatalf("Retire() = %+v, want completed retirement", job)
			}
			stageAdmin(t, f, &job)
			if tt.phase == cleanup.PhaseDeletingRetiredPayload {
				root, err := f.relocator.Admit(context.Background(), &job)
				if err != nil || job.Blocked != nil {
					t.Fatalf("admin admission = %+v, %v", job, err)
				}
				deleteRoot(t, root)
				if _, err := f.relocator.Admit(context.Background(), &job); err != nil || job.Blocked != nil || job.Phase != tt.phase {
					t.Fatalf("admin completion = %+v, %v", job, err)
				}
				if err := os.Remove(filepath.Join(job.Payload, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if tt.unprobed {
				f.guard = func(context.Context, string) error { return cleanup.ErrUnprobed }
			} else {
				_ = f.holdAt(job.Payload)
			}
			if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
				t.Fatal(err)
			}
			if job.Blocked == nil || job.Blocked.Reason != "activity" || job.Phase != tt.phase {
				t.Fatalf("Admit() = %+v, want strict activity refusal", job)
			}
			if got := f.id(job.Payload); got != job.Tree {
				t.Errorf("blocked payload identity = %+v, want %+v", got, job.Tree)
			}
			f.stored(&job)
		})
	}
}

func deleteRoot(t *testing.T, root cleanup.DeletionRoot) {
	t.Helper()
	deletion, err := (rmtree.Deleter{}).Open(root.Dir, root.Name, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deletion.Close() }()
	for range 256 {
		_, done, err := deletion.Step(1, time.Second, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
	t.Fatal("fixture deletion did not finish within 256 entries")
}

func TestMovedCapturedAdminCrashRecoveryDeletesAdminBeforePayload(t *testing.T) {
	for _, removed := range []string{"locked", "HEAD", "commondir", "gitdir"} {
		t.Run(removed, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			retired := moveCapturedAdmin(t, f, job)
			retireFixture(t, f, &job)
			stageAdmin(t, f, &job)
			if err := os.Remove(filepath.Join(retired, removed)); err != nil {
				t.Fatal(err)
			}
			root, err := f.relocator.Admit(context.Background(), &job)
			if err != nil || job.Blocked != nil {
				t.Fatalf("partial admin admission = %+v, %v", job, err)
			}
			want := cleanup.DeletionRoot{Dir: filepath.Dir(retired), Name: filepath.Base(retired), ID: job.Admin}
			if root != want {
				t.Fatalf("admitted root = %+v, want %+v", root, want)
			}
			locator := f.read(filepath.Join(job.Payload, ".git"))
			deleteRoot(t, root)
			f.absent(retired)
			if got := f.read(filepath.Join(job.Payload, ".git")); got != locator {
				t.Fatalf("admin deletion changed payload locator to %q", got)
			}
			f.open(f.layout.Root)
			jobs, damaged, err := f.journal.Load()
			if err != nil || len(damaged) != 0 || len(jobs) != 1 {
				t.Fatalf("Load() = %d jobs, %v, %v", len(jobs), damaged, err)
			}
			job = jobs[0]
			root, err = f.relocator.Admit(context.Background(), &job)
			if err != nil || job.Blocked != nil || job.Phase != cleanup.PhaseDeletingRetiredPayload || root.ID != job.Tree {
				t.Fatalf("absent admin recovery = %+v, %+v, %v", job, root, err)
			}
			f.stored(&job)
			if err := os.Remove(filepath.Join(job.Payload, ".git")); err != nil {
				t.Fatal(err)
			}
			root, err = f.relocator.Admit(context.Background(), &job)
			if err != nil || job.Blocked != nil || root.ID != job.Tree {
				t.Fatalf("partial payload admission = %+v, %+v, %v", job, root, err)
			}
			deleteRoot(t, root)
			f.absent(job.Payload)
			f.absent(retired)
		})
	}
}

func TestMovedCapturedAdminRefreshesOnlyDeviceNumbers(t *testing.T) {
	for _, tree := range []bool{false, true} {
		name := "admin"
		if tree {
			name = "tree"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			originalTree, originalAdmin := job.Tree, job.Admin
			retired := moveCapturedAdmin(t, f, job)
			if tree {
				job.Tree.Dev++
			} else {
				job.Admin.Dev++
			}
			if err := f.journal.Save(job); err != nil {
				t.Fatal(err)
			}
			retireFixture(t, f, &job)
			if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete || job.Tree != originalTree || job.Admin != originalAdmin {
				t.Fatalf("Retire() = %+v, want refreshed devices and logical retirement", job)
			}
			if got := f.read(filepath.Join(retired, "gitdir")); got != "" {
				t.Errorf("retired backlink = %q, want empty", got)
			}
			f.stored(&job)
		})
	}
}

func TestMovedCapturedAdminMissingLocatorBlocksBeforePayloadStage(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	retired := moveCapturedAdmin(t, f, job)
	retireFixture(t, f, &job)
	stageAdmin(t, f, &job)
	if err := os.Remove(filepath.Join(job.Payload, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Phase != cleanup.PhaseDeletingAdmin {
		t.Fatalf("missing locator = %+v, want blocked admin stage", job)
	}
	if got := f.id(retired); got != job.Admin {
		t.Errorf("admin identity = %+v, want %+v", got, job.Admin)
	}
	f.stored(&job)
}

func TestMovedCapturedAdminRejectsChangedCapturedGitIdentity(t *testing.T) {
	tests := []struct {
		name   string
		change func(*fixture, cleanup.Job, string)
	}{
		{"HEAD", func(f *fixture, _ cleanup.Job, _ string) { f.run(f.repo, "update-ref", "refs/heads/feature", "main") }},
		{"branch", func(f *fixture, job cleanup.Job, admin string) {
			f.run(f.repo, "update-ref", "refs/heads/other", job.Head)
			f.write(filepath.Join(admin, "HEAD"), "ref: refs/heads/other\n")
		}},
		{"common", func(f *fixture, _ cleanup.Job, admin string) { f.write(filepath.Join(admin, "commondir"), f.root+"\n") }},
		{"backlink", func(f *fixture, _ cleanup.Job, admin string) {
			f.write(filepath.Join(admin, "gitdir"), filepath.Join(f.repo, ".git")+"\n")
		}},
	}
	for _, tt := range tests {
		for _, refresh := range []bool{false, true} {
			name := tt.name
			if refresh {
				name += " device-only"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				job := f.intend()
				admin := moveCapturedAdmin(t, f, job)
				if refresh {
					job.Tree.Dev++
					job.Admin.Dev++
					if err := f.journal.Save(job); err != nil {
						t.Fatal(err)
					}
				}
				capturedTree, capturedAdmin := job.Tree, job.Admin
				tt.change(f, job, admin)
				backlink := f.read(filepath.Join(admin, "gitdir"))
				retireFixture(t, f, &job)
				if job.Blocked == nil || job.Blocked.Reason != "identity" || job.Retirement != "" {
					t.Fatalf("Retire() = %+v, want captured identity refusal", job)
				}
				if got := f.read(filepath.Join(admin, "gitdir")); got != backlink {
					t.Errorf("refusal changed backlink to %q", got)
				}
				f.absent(filepath.Join(admin, "locked"))
				if job.Tree != capturedTree || job.Admin != capturedAdmin {
					t.Fatal("unproved identity refreshed captured devices")
				}
				f.stored(&job)
			})
		}
	}
}

func TestMovedCapturedAdminFinalGuardPreservesReplacements(t *testing.T) {
	for _, replaceAdmin := range []bool{false, true} {
		name := "backlink"
		if replaceAdmin {
			name = "admin"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			job := f.intend()
			admin := moveCapturedAdmin(t, f, job)
			kept := admin + "-kept"
			if !replaceAdmin {
				kept = filepath.Join(admin, "gitdir-kept")
			}
			calls := 0
			f.guard = func(_ context.Context, tree string) error {
				if tree != admin {
					return nil
				}
				calls++
				if calls != 2 {
					return nil
				}
				if replaceAdmin {
					if err := os.Rename(admin, kept); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(admin, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Rename(filepath.Join(admin, "gitdir"), kept); err != nil {
					t.Fatal(err)
				}
				f.write(filepath.Join(admin, "gitdir"), "replacement\n")
				return nil
			}
			retireFixture(t, f, &job)
			if job.Blocked == nil || job.Blocked.Reason != "identity" {
				t.Fatalf("Retire() = %+v, want final identity refusal", job)
			}
			if got := f.read(filepath.Join(admin, "gitdir")); got != "replacement\n" {
				t.Errorf("replacement backlink = %q", got)
			}
			if replaceAdmin {
				kept = filepath.Join(kept, "gitdir")
			}
			if got := f.read(kept); got != job.Links.AdminGitdir {
				t.Errorf("captured backlink = %q, want unchanged", got)
			}
			f.stored(&job)
		})
	}
}

func TestMovedCapturedAdminAbsentParentBlocksCheckpoint(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	admin := moveCapturedAdmin(t, f, job)
	retireFixture(t, f, &job)
	stageAdmin(t, f, &job)
	root, err := f.relocator.Admit(context.Background(), &job)
	if err != nil || job.Blocked != nil {
		t.Fatalf("Admit() = %+v, %v", job, err)
	}
	deleteRoot(t, root)
	if err := os.Rename(filepath.Dir(admin), filepath.Join(f.root, "parent-kept")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Phase != cleanup.PhaseDeletingAdmin || job.Blocked == nil {
		t.Fatalf("missing parent = %+v, want blocked admin checkpoint", job)
	}
	if got := f.id(job.Payload); got != job.Tree {
		t.Errorf("blocked payload = %+v, want %+v", got, job.Tree)
	}
	if got := f.read(filepath.Join(job.Payload, ".git")); got != "gitdir: "+admin+"\n" {
		t.Errorf("blocked locator = %q", got)
	}
	f.stored(&job)
}

func TestMovedCapturedAdminRechecksHeadAfterFinalGuard(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	admin := moveCapturedAdmin(t, f, job)
	calls := 0
	f.guard = func(_ context.Context, tree string) error {
		if tree == admin {
			calls++
			if calls == 2 {
				f.run(f.repo, "update-ref", "refs/heads/feature", "main")
			}
		}
		return nil
	}
	retireFixture(t, f, &job)
	if job.Blocked == nil || job.Blocked.Reason != "identity" {
		t.Fatalf("late HEAD change = %+v, want identity refusal", job)
	}
	if got := f.read(filepath.Join(admin, "gitdir")); got != job.Links.AdminGitdir {
		t.Errorf("late HEAD refusal changed backlink to %q", got)
	}
	f.stored(&job)
}
