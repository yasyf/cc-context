//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type passClock struct {
	once   sync.Once
	passed chan struct{}
}

func newPassClock() *passClock { return &passClock{passed: make(chan struct{})} }

func (c *passClock) Now() time.Time {
	c.once.Do(func() { close(c.passed) })
	return time.Now()
}

func (c *passClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (f *gitFixture) startMaintained(engine *Engine, clock *passClock) <-chan error {
	f.t.Helper()
	return f.startStopped(engine, clock, cleanup.Switch{Paused: true, Hold: &cleanup.Hold{ID: "repair", Reason: "registration repair", Since: time.Now()}})
}

func (f *gitFixture) startStopped(engine *Engine, clock *passClock, sw cleanup.Switch) <-chan error {
	f.t.Helper()
	if err := f.journal.SaveSwitch(sw); err != nil {
		f.t.Fatalf("SaveSwitch() = %v", err)
	}
	running := f.start(engine)
	select {
	case <-clock.passed:
	case <-time.After(drainWait):
		f.t.Fatalf("the stopped worker made no pass within %s", drainWait)
	}
	return running
}

func (f *gitFixture) refusedWhilePaused(engine *Engine) {
	f.t.Helper()
	ctx := context.Background()
	tests := []struct {
		name string
		ask  func() error
	}{
		{"adopt", func() error { _, err := engine.Adopt(ctx, f.adoption()); return err }},
	}
	for _, tt := range tests {
		if err := tt.ask(); !errors.Is(err, cleanup.ErrPaused) {
			f.t.Errorf("%s under a queue pause = %v, want ErrPaused", tt.name, err)
		}
	}
}

func (f *gitFixture) resumed(engine *Engine, running <-chan error, id string) cleanup.Job {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	if err := engine.Resume(ctx); err != nil {
		f.t.Fatalf("Resume() = %v", err)
	}
	done, err := engine.Wait(ctx, id)
	if err != nil || done.Phase != cleanup.PhaseDone {
		f.t.Fatalf("Wait() after the resume = phase %s, blockage %+v, %v; want done", done.Phase, done.Blocked, err)
	}
	if err := f.await(f.stop(engine), "Stop()"); err != nil {
		f.t.Errorf("Stop() = %v", err)
	}
	if err := f.await(running, "Run()"); err != nil {
		f.t.Errorf("Run() = %v", err)
	}
	return done
}

func TestMaintenanceHoldRestsJournaledJobsAtTheirPhaseUntilRelease(t *testing.T) {
	seedMoved := func(f *gitFixture) cleanup.Job {
		job := f.accept()
		f.park(&job, "rev-parse", cleanup.PhaseMoved, "identity")
		return job
	}
	atOriginal := func(f *gitFixture, _ cleanup.Job) string { return f.worktree }
	atRegistered := func(_ *gitFixture, job cleanup.Job) string { return job.Registered }
	tests := []struct {
		name  string
		seed  func(f *gitFixture) cleanup.Job
		phase cleanup.Phase
		tree  func(f *gitFixture, job cleanup.Job) string
	}{
		{"queued", (*gitFixture).intend, cleanup.PhaseQueued, atOriginal},
		{"prepared", (*gitFixture).accept, cleanup.PhasePrepared, atOriginal},
		{"moved", seedMoved, cleanup.PhaseMoved, atRegistered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitFixture(t)
			job := tt.seed(f)
			before, listing := f.gitCalls(), f.listing()
			clock := newPassClock()
			f.clock = clock
			engine := f.engine()
			running := f.startMaintained(engine, clock)

			f.refusedWhilePaused(engine)
			if got := f.gitCalls(); got != before {
				t.Errorf("git ran under the pause: %q", strings.TrimPrefix(got, before))
			}
			rested := f.rests(job, tt.phase)
			if got := f.id(tt.tree(f, rested)); got != job.Tree {
				t.Errorf("tree identity at %s = %v, want the tree %v", tt.tree(f, rested), got, job.Tree)
			}
			if got := f.listing(); got != listing {
				t.Errorf("worktree list under the pause = %q, want it untouched: %q", got, listing)
			}
			jobs, damaged, err := f.journal.Load()
			if err != nil || len(damaged) != 0 || len(jobs) != 1 {
				t.Errorf("Load() = %d jobs, damaged %v, %v; want the one seeded job", len(jobs), damaged, err)
			}
			report, err := engine.Status(context.Background(), cleanup.Query{})
			if err != nil || !report.Paused || len(report.Jobs) != 1 {
				t.Errorf("Status() = paused %t, %d jobs, %v; want paused with the one job", report.Paused, len(report.Jobs), err)
			}

			if err := engine.Release(context.Background(), "repair"); err != nil {
				t.Fatal(err)
			}
			f.finished(f.resumed(engine, running, job.ID))
		})
	}
}

func TestMaintenanceHoldKeepsANewRemovalPendingUntilRelease(t *testing.T) {
	f := newGitFixture(t)
	tree, listing := f.id(f.worktree), f.listing()
	clock := newPassClock()
	f.clock = clock
	engine := f.engine()
	running := f.startMaintained(engine, clock)

	f.refusedWhilePaused(engine)
	if got := f.gitCalls(); got != "" {
		t.Errorf("git ran for a refused request: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	receipt, err := engine.Remove(ctx, f.request())
	if err != nil || receipt.State != cleanup.State(cleanup.PhasePending) || receipt.Original != f.worktree {
		t.Fatalf("Remove() under the pause = %+v, %v; want it pending", receipt, err)
	}
	if got := f.journaled(receipt.JobID); got.Phase != cleanup.PhasePending || got.Blocked != nil {
		t.Errorf("journaled job = phase %s, blockage %+v; want pending", got.Phase, got.Blocked)
	}
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 1 {
		t.Errorf("Load() = %d jobs, damaged %v, %v; want the one pending job", len(jobs), damaged, err)
	}
	if f.ran("worktree") || f.ran("update-ref") {
		t.Errorf("git calls under the pause = %q, want no move and no recovery ref", f.gitCalls())
	}
	if got := f.id(f.worktree); got != tree {
		t.Errorf("tree identity at %s = %v, want the untouched tree %v", f.worktree, got, tree)
	}
	if got := f.listing(); got != listing {
		t.Errorf("worktree list = %q, want it untouched: %q", got, listing)
	}
	if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
		t.Errorf("recovery refs = %q, want none", refs)
	}

	if err := engine.Release(context.Background(), "repair"); err != nil {
		t.Fatal(err)
	}
	f.finished(f.resumed(engine, running, receipt.JobID))
}

func TestMaintenanceHoldRestsAJournaledJobUntilItsHolderReleasesIt(t *testing.T) {
	f := newGitFixture(t)
	job := f.accept()
	before, listing := f.gitCalls(), f.listing()
	clock := newPassClock()
	f.clock = clock
	engine := f.engine()
	hold := cleanup.Hold{ID: "host-maintenance", Reason: "disk repair", Since: time.Now()}
	running := f.startStopped(engine, clock, cleanup.Switch{Hold: &hold})

	f.refusedWhilePaused(engine)
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	if err := engine.Resume(ctx); !heldBy(err, hold) {
		t.Errorf("Resume() under the hold = %v, want a *HeldError naming %+v", err, hold)
	}
	if got := f.gitCalls(); got != before {
		t.Errorf("git ran under the hold: %q", strings.TrimPrefix(got, before))
	}
	rested := f.rests(job, cleanup.PhasePrepared)
	if got := f.id(f.worktree); got != rested.Tree {
		t.Errorf("tree identity at %s = %v, want the tree %v", f.worktree, got, rested.Tree)
	}
	if got := f.listing(); got != listing {
		t.Errorf("worktree list under the hold = %q, want it untouched: %q", got, listing)
	}

	if err := engine.Release(ctx, hold.ID); err != nil {
		t.Fatalf("Release() = %v", err)
	}
	done, err := engine.Wait(ctx, job.ID)
	if err != nil || done.Phase != cleanup.PhaseDone {
		t.Fatalf("Wait() after the release = phase %s, blockage %+v, %v; want done", done.Phase, done.Blocked, err)
	}
	if err := f.await(f.stop(engine), "Stop()"); err != nil {
		t.Errorf("Stop() = %v", err)
	}
	if err := f.await(running, "Run()"); err != nil {
		t.Errorf("Run() = %v", err)
	}
	f.finished(done)
}

func TestFreshPausedRemoveCapturesCurrentHeadAfterUntouchedIdentityBlock(t *testing.T) {
	f := newGitFixture(t)
	relocator := f.relocator()
	original, err := relocator.Pend(context.Background(), 1, f.request())
	if err != nil {
		t.Fatal(err)
	}
	tree := f.id(f.worktree)
	f.write(filepath.Join(f.worktree, "feature.txt"), "current feature\n")
	f.run(f.worktree, "add", "feature.txt")
	f.run(f.worktree, "commit", "-q", "-m", "current feature")
	head := f.run(f.worktree, "rev-parse", "HEAD")
	if head == original.Head {
		t.Fatal("fixture did not change HEAD")
	}
	if err := relocator.Retire(context.Background(), &original); err != nil {
		t.Fatal(err)
	}
	if original.Blocked == nil || original.Blocked.Reason != "identity" || original.Retirement != "" {
		t.Fatalf("original retirement = %+v, want untouched identity block", original)
	}
	before := f.journaled(original.ID)
	clock := newPassClock()
	f.clock = clock
	engine := f.engine()
	running := f.startMaintained(engine, clock)
	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	type outcome struct {
		receipt cleanup.Receipt
		err     error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			receipt, err := engine.Remove(ctx, f.request())
			results <- outcome{receipt, err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent fresh removals = %v, %v", first.err, second.err)
	}
	if first.receipt.JobID != second.receipt.JobID {
		t.Fatalf("concurrent requests created different jobs: %+v, %+v", first.receipt, second.receipt)
	}
	receipt := first.receipt
	if receipt.JobID == original.ID {
		t.Fatal("fresh removal rejoined the stale captured HEAD")
	}
	current := f.journaled(receipt.JobID)
	if current.Head != head || current.Tree != tree || current.Phase != cleanup.PhasePending || current.Retirement != "" {
		t.Fatalf("fresh removal = %+v, want current captured HEAD and original payload", current)
	}
	if got := f.journaled(original.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("fresh request rewrote immutable old job: %+v", got)
	}
	if err := f.await(f.stop(engine), "Stop()"); err != nil {
		t.Fatal(err)
	}
	if err := f.await(running, "Run()"); err != nil {
		t.Fatal(err)
	}
	if !engine.paused {
		t.Fatal("fresh removal resumed physical deletion")
	}
	if err := relocator.Retire(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Blocked != nil || current.Retirement != cleanup.RetirementComplete {
		t.Fatalf("fresh registration retirement = %+v", current)
	}
	if got := f.listing(); got != f.mainEntry() {
		t.Errorf("retired registration remains listed: %s", got)
	}
	if got := f.id(f.worktree); got != tree {
		t.Errorf("fresh retirement changed payload identity %+v", got)
	}
	body, err := os.ReadFile(filepath.Join(f.worktree, "feature.txt"))
	if err != nil || string(body) != "current feature\n" {
		t.Errorf("retained current payload = %q, %v", body, err)
	}
	if got := f.journaled(original.ID); !reflect.DeepEqual(got, before) {
		t.Errorf("retirement rewrote immutable old job: %+v", got)
	}
}
