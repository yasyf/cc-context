package relocate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanupwatch"
	"github.com/yasyf/cc-context/internal/render"
)

type quarantineProbe struct {
	t     *testing.T
	roots []string
}

func (q *quarantineProbe) Run(_ context.Context, _ render.Dir, name string, args ...string) (cleanupwatch.Output, error) {
	q.t.Helper()
	if name != "watchman" || len(args) != 3 || args[0] != "--no-spawn" || args[1] != "--no-pretty" {
		q.t.Fatalf("unexpected external command: %s %v", name, args)
	}
	var reply any
	switch args[2] {
	case "get-sockname":
		reply = map[string]any{"sockname": "/unused-test-watchman.sock", "version": "test"}
	case "get-pid":
		reply = map[string]any{"pid": 4242}
	case "watch-list":
		reply = map[string]any{"roots": q.roots}
	default:
		q.t.Fatalf("watcher precondition fixture attempted %s", args[2])
	}
	data, err := json.Marshal(reply)
	return cleanupwatch.Output{Stdout: data, PID: 60000}, err
}

func (*quarantineProbe) FSMonitorDaemons(context.Context) ([]cleanupwatch.Process, error) {
	return nil, nil
}

func (*quarantineProbe) Processes(context.Context, []int) (map[int]cleanupwatch.Process, error) {
	return map[int]cleanupwatch.Process{}, nil
}

func existingDirectoryGuard(_ context.Context, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("existing-directory guard: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("existing-directory guard: %s is not a directory", dir)
	}
	return nil
}

func preconditionAdmin(t *testing.T) (*fixture, cleanup.Job, string, *quarantineProbe) {
	t.Helper()
	f := newFixture(t)
	probe := &quarantineProbe{t: t}
	deps := cleanupwatch.Deps{Run: probe, Procs: probe, Timeout: time.Second}
	f.watchers.quarantine = func(ctx context.Context, dir string) error {
		return cleanupwatch.CheckQuarantine(ctx, deps, dir)
	}
	f.guard = existingDirectoryGuard
	job := f.intend()
	admin := moveCapturedAdmin(t, f, job)
	retireFixture(t, f, &job)
	f.advance(&job)
	adminStage(t, f, &job)
	return f, job, admin, probe
}

func removePreconditionAdmin(t *testing.T, f *fixture, job *cleanup.Job) {
	t.Helper()
	root, err := f.relocator.Admit(context.Background(), job)
	if err != nil || job.Blocked != nil || root.ID != job.Admin {
		t.Fatalf("admin admission = %+v, %+v, %v", job, root, err)
	}
	deleteRoot(t, root)
}

func TestAbsentAdminRecoveryUsesExistingDirectoryGates(t *testing.T) {
	f, job, admin, _ := preconditionAdmin(t)
	removePreconditionAdmin(t, f, &job)
	f.absent(admin)
	f.open(f.layout.Root)
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 1 {
		t.Fatalf("Load() = %v, %v, %v", jobs, damaged, err)
	}
	job = jobs[0]
	checked, guarded := len(f.watchers.checked), len(f.guarded)
	root, err := f.relocator.Admit(context.Background(), &job)
	if err != nil || job.Blocked != nil || job.Phase != cleanup.PhaseDeletingRetiredPayload || root.ID != job.Tree {
		t.Fatalf("absent admin recovery = %+v, %+v, %v", job, root, err)
	}
	if slices.Contains(f.watchers.checked[checked:], admin) || slices.Contains(f.guarded[guarded:], admin) {
		t.Fatal("existing-directory API received the absent admin")
	}
	if !slices.Contains(f.watchers.checked[checked:], filepath.Dir(job.Payload)) || !slices.Contains(f.guarded[guarded:], job.Payload) {
		t.Fatal("recovery omitted an existing quarantine or payload activity gate")
	}
	f.stored(&job)
}

func TestPresentAdminStillUsesRealQuarantineGate(t *testing.T) {
	f, job, admin, probe := preconditionAdmin(t)
	probe.roots = []string{admin}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Blocked.Reason != "quarantine" || job.Phase != cleanup.PhaseDeletingAdmin {
		t.Fatalf("watched existing admin = %+v, want quarantine refusal", job)
	}
	if f.id(admin) != job.Admin || f.id(job.Payload) != job.Tree {
		t.Fatal("watcher refusal changed a captured root")
	}
	f.stored(&job)
}

func TestPresentAdminStillBlocksUnknownActivity(t *testing.T) {
	f, job, admin, _ := preconditionAdmin(t)
	f.guard = func(ctx context.Context, dir string) error {
		if dir == admin {
			return cleanup.ErrUnprobed
		}
		return existingDirectoryGuard(ctx, dir)
	}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Blocked.Reason != "activity" || job.Phase != cleanup.PhaseDeletingAdmin {
		t.Fatalf("unprobed existing admin = %+v, want activity refusal", job)
	}
	f.stored(&job)
}

func TestAbsentAdminStillBlocksUnknownPayloadActivity(t *testing.T) {
	f, job, _, _ := preconditionAdmin(t)
	removePreconditionAdmin(t, f, &job)
	f.guard = func(ctx context.Context, dir string) error {
		if dir == job.Payload {
			return cleanup.ErrUnprobed
		}
		return existingDirectoryGuard(ctx, dir)
	}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Blocked.Reason != "activity" || job.Phase != cleanup.PhaseDeletingAdmin {
		t.Fatalf("unprobed payload = %+v, want activity refusal before checkpoint", job)
	}
	f.stored(&job)
}

func TestAbsentAdminReappearanceStillBlocksCheckpoint(t *testing.T) {
	f, job, admin, _ := preconditionAdmin(t)
	replacement := filepath.Join(f.root, "replacement-admin")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(replacement, "owner.txt"), "new owner\n")
	replacementID := f.id(replacement)
	removePreconditionAdmin(t, f, &job)
	f.guard = func(ctx context.Context, dir string) error {
		if dir == job.Payload {
			if err := os.Rename(replacement, admin); err != nil {
				t.Fatal(err)
			}
		}
		return existingDirectoryGuard(ctx, dir)
	}
	if _, err := f.relocator.Admit(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Blocked == nil || job.Blocked.Reason != "reconcile" || job.Phase != cleanup.PhaseDeletingAdmin {
		t.Fatalf("reappeared admin = %+v, want blocked checkpoint", job)
	}
	if f.id(admin) != replacementID || f.read(filepath.Join(admin, "owner.txt")) != "new owner\n" || f.id(job.Payload) != job.Tree {
		t.Fatal("checkpoint refusal changed an owned or replacement root")
	}
	f.stored(&job)
}
