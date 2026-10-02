//go:build !windows

package daemon

import (
	"context"
	"errors"
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
	if err := f.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
		f.t.Fatalf("SaveSwitch() = %v", err)
	}
	running := f.start(engine)
	select {
	case <-clock.passed:
	case <-time.After(drainWait):
		f.t.Fatalf("the paused worker made no pass within %s", drainWait)
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
		{"remove", func() error { _, err := engine.Remove(ctx, f.request()); return err }},
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

func TestQueuePauseRefusesNewRemovalsBeforeAnyGitOrJournalWork(t *testing.T) {
	f := newGitFixture(t)
	tree, listing := f.id(f.worktree), f.listing()
	clock := newPassClock()
	f.clock = clock
	engine := f.engine()
	running := f.startPaused(engine, clock)

	f.refusedWhilePaused(engine)
	if got := f.gitCalls(); got != "" {
		t.Errorf("git ran for a refused request: %q", got)
	}
	jobs, damaged, err := f.journal.Load()
	if err != nil || len(damaged) != 0 || len(jobs) != 0 {
		t.Errorf("Load() = %d jobs, damaged %v, %v; want an empty journal", len(jobs), damaged, err)
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

	ctx, cancel := context.WithTimeout(context.Background(), drainWait)
	defer cancel()
	if err := engine.Resume(ctx); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	receipt, err := engine.Remove(ctx, f.request())
	if err != nil || receipt.State != cleanup.State(cleanup.PhaseUnregistered) {
		t.Fatalf("Remove() after the resume = %+v, %v; want it unregistered", receipt, err)
	}
	done, err := engine.Wait(ctx, receipt.JobID)
	if err != nil || done.Phase != cleanup.PhaseDone {
		t.Fatalf("Wait() = phase %s, blockage %+v, %v; want done", done.Phase, done.Blocked, err)
	}
	if err := f.await(f.stop(engine), "Stop()"); err != nil {
		t.Errorf("Stop() = %v", err)
	}
	if err := f.await(running, "Run()"); err != nil {
		t.Errorf("Run() = %v", err)
	}
	f.finished(done)
}
