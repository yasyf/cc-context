//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

func (f *gitFixture) startPaused(engine *Engine, clock *passClock) <-chan error {
	f.t.Helper()
	return f.startStopped(engine, clock, cleanup.Switch{Paused: true})
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
	unobserved := cleanup.Registration{Tree: cleanup.FileID{Dev: 1, Ino: 1}, AdminDir: f.adminDir, Admin: cleanup.FileID{Dev: 1, Ino: 1}}
	deferral := cleanup.DeferRequest{Worktree: f.worktree, CommonDir: f.common, Owner: "stack", Expected: unobserved, Git: f.wrapper}
	tests := []struct {
		name string
		ask  func() error
	}{
		{"defer", func() error { _, err := engine.Defer(ctx, deferral); return err }},
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

func (f *gitFixture) settled(id string) cleanup.Job {
	f.t.Helper()
	deadline := time.Now().Add(drainWait)
	for {
		jobs, damaged, err := f.journal.Load()
		if err != nil || len(damaged) != 0 {
			f.t.Fatalf("Load() = damaged %v, %v", damaged, err)
		}
		for _, job := range jobs {
			if job.ID == id && (job.Phase == cleanup.PhaseRetired || job.Blocked != nil) {
				return job
			}
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s neither retired nor blocked within %s", id, drainWait)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueuePauseRestsJournaledJobsAtTheirPhaseUntilResume(t *testing.T) {
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
			running := f.startPaused(engine, clock)

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

			f.finished(f.resumed(engine, running, job.ID))
		})
	}
}

func TestHaltedQueueRetiresAnUntouchedRemovalInPlaceUntilResume(t *testing.T) {
	hold := cleanup.Hold{ID: "host-maintenance", Reason: "disk repair", Since: time.Now()}
	resume := func(ctx context.Context, engine *Engine) error { return engine.Resume(ctx) }
	release := func(ctx context.Context, engine *Engine) error { return engine.Release(ctx, hold.ID) }
	tests := []struct {
		name   string
		sw     cleanup.Switch
		during bool
		lift   func(ctx context.Context, engine *Engine) error
	}{
		{"a deferral queued before the pause", cleanup.Switch{Paused: true}, false, resume},
		{"a removal asked for during the pause", cleanup.Switch{Paused: true}, true, resume},
		{"a removal asked for during the hold", cleanup.Switch{Hold: &hold}, true, release},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitFixture(t)
			tree := f.id(f.worktree)
			head := f.run(f.worktree, "rev-parse", "HEAD")
			index, err := os.ReadFile(filepath.Join(f.adminDir, "index"))
			if err != nil {
				t.Fatalf("read the index: %v", err)
			}
			var id string
			if !tt.during {
				id = f.intend().ID
			}
			clock := newPassClock()
			f.clock = clock
			engine := f.engine()
			running := f.startStopped(engine, clock, tt.sw)
			ctx, cancel := context.WithTimeout(context.Background(), drainWait)
			defer cancel()
			if tt.during {
				receipt, err := engine.Remove(ctx, f.request())
				if err != nil || receipt.State != cleanup.State(cleanup.PhasePending) || receipt.Original != f.worktree {
					t.Fatalf("Remove() while halted = %+v, %v; want it pending", receipt, err)
				}
				id = receipt.JobID
			}

			retired := f.settled(id)
			if retired.Phase != cleanup.PhaseRetired || retired.Blocked != nil {
				t.Fatalf("job while halted = phase %s, blockage %+v; want retired in place", retired.Phase, retired.Blocked)
			}
			gitDir := cleanup.RetiredGitDirFor(retired.Repo, id)
			reported := cleanup.PhasePending
			if retired.Deferred {
				reported = cleanup.PhaseQueued
			}
			if report, err := engine.Status(ctx, cleanup.Query{JobID: id}); err != nil || report.Jobs[0].Phase != reported {
				t.Errorf("Status(%s) while halted = %+v, %v; want it reported %s", id, report.Jobs, err, reported)
			}
			if got := f.id(f.worktree); got != tree {
				t.Errorf("tree identity at %s = %v, want the tree %v left where it was", f.worktree, got, tree)
			}
			if f.ran("worktree") {
				t.Errorf("git calls while halted = %q, want no worktree move or removal", f.gitCalls())
			}
			if got, want := f.listing(), f.mainEntry(); got != want {
				t.Errorf("worktree list while halted = %q, want only the main checkout %q", got, want)
			}
			f.absent(f.adminDir)
			if got := f.id(gitDir); got != retired.Admin {
				t.Errorf("retired admin identity = %v, want the registered admin %v moved, not copied", got, retired.Admin)
			}
			if kept, err := os.ReadFile(filepath.Join(gitDir, "index")); err != nil || !bytes.Equal(kept, index) {
				t.Errorf("retired index = %d bytes, %v; want the %d bytes git held", len(kept), err, len(index))
			}
			if got := f.run(f.worktree, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD in the retired checkout = %s, want %s", got, head)
			}
			if got := f.run(f.worktree, "rev-parse", "--absolute-git-dir"); got != gitDir {
				t.Errorf("git dir of the retired checkout = %s, want %s", got, gitDir)
			}
			if got := f.run(f.worktree, "status", "--porcelain"); got != "" {
				t.Errorf("status of the retired checkout = %q, want it clean", got)
			}
			if refs := f.run(f.repo, "for-each-ref", cleanup.RecoveryRefPrefix); refs != "" {
				t.Errorf("recovery refs while halted = %q, want none until the tree moves", refs)
			}

			if err := tt.lift(ctx, engine); err != nil {
				t.Fatalf("lift = %v", err)
			}
			done, err := engine.Wait(ctx, id)
			if err != nil || done.Phase != cleanup.PhaseDone {
				t.Fatalf("Wait() once running = phase %s, blockage %+v, %v; want done", done.Phase, done.Blocked, err)
			}
			if err := f.await(f.stop(engine), "Stop()"); err != nil {
				t.Errorf("Stop() = %v", err)
			}
			if err := f.await(running, "Run()"); err != nil {
				t.Errorf("Run() = %v", err)
			}
			f.finished(done)
			f.absent(gitDir)
		})
	}
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
