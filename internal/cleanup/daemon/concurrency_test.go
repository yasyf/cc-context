package daemon

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestPauseParksTheDeletionBeforeItAnswers(t *testing.T) {
	bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		gated := &payload{entries: 300, entered: make(chan struct{}, 16)}
		h.deleter.put("a", gated)
		h.start()
		engine := h.engine
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)

		gate := make(chan struct{})
		h.deleter.mu.Lock()
		gated.gate = gate
		h.deleter.mu.Unlock()
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("step:a")

		paused, resumed := make(chan error, 1), make(chan error, 1)
		go func() { paused <- engine.Pause(ctx) }()
		synctest.Wait()
		go func() { resumed <- engine.Resume(ctx) }()
		synctest.Wait()
		h.deleter.mu.Lock()
		gated.gate = nil
		h.deleter.mu.Unlock()
		gate <- struct{}{}
		if err := <-paused; err != nil {
			t.Fatalf("Pause() = %v", err)
		}
		if err := <-resumed; err != nil {
			t.Fatalf("Resume() = %v", err)
		}
		h.expectEvents("close:a")
		h.expectTimers(400 * time.Millisecond)
		if got := h.journaled(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 200 {
			t.Errorf("journal after the pause = phase %s, removed %d; want deleting, 200", got.Phase, got.Removed)
		}
		if sw, err := h.journal.LoadSwitch(); err != nil || sw.Paused {
			t.Errorf("LoadSwitch() after the resume = %+v, %v; want unpaused", sw, err)
		}

		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:a", "open:a", "step:a")
		if got := h.deleter.wants("a"); !slices.Equal(got, []cleanup.FileID{job.Tree, job.Tree}) {
			t.Errorf("payload identities verified = %v, want %v before the pause and again after it", got, job.Tree)
		}
		if got := h.status(job.ID); got.Phase != cleanup.PhaseDone || got.Removed != 300 {
			t.Errorf("job after the resume = phase %s, removed %d; want done, 300", got.Phase, got.Removed)
		}
	})
}

func TestAdmissionThatCannotJournalStopsTheWorker(t *testing.T) {
	tests := []struct {
		name  string
		admit func(ctx context.Context, h *harness, engine *Engine) error
		event string
	}{
		{"remove", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Remove(ctx, h.request("b"))
			return err
		}, "accept:b"},
		{"defer", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Defer(ctx, h.deferral("b"))
			return err
		}, "intend:b"},
		{"adopt", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Adopt(ctx, h.adoption("b"))
			return err
		}, "adopt:b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
				ctx := context.Background()
				job := h.seed("a", 1, cleanup.PhaseUnregistered)
				h.deleter.put("a", &payload{entries: 300})
				h.start()
				engine, running := h.engine, h.running
				h.expectEvents("sample", "admit:a", "open:a", "step:a")

				if err := os.Chmod(h.layout.JobsDir(), 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(h.layout.JobsDir(), 0o700); err != nil {
						t.Errorf("restore the jobs directory: %v", err)
					}
				})
				if err := tt.admit(ctx, h, engine); !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("%s with an unwritable journal = %v, want the journal's write failure", tt.name, err)
				}
				h.engine = nil
				if err := <-running; !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("Run() = %v, want the journal's write failure", err)
				}
				h.expectEvents(tt.event, "close:a")
				h.clock.Advance(time.Minute)
				h.expectEvents()
				if got := h.journaled(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 100 {
					t.Errorf("journal after the stop = phase %s, removed %d; want deleting, 100", got.Phase, got.Removed)
				}
				if err := engine.Pause(ctx); !errors.Is(err, ErrStopped) {
					t.Errorf("Pause() after the worker died = %v, want ErrStopped", err)
				}
			})
		})
	}
}

func TestStopOutranksACommandStillQueued(t *testing.T) {
	bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		gated := &payload{entries: 300, gate: make(chan struct{}), entered: make(chan struct{}, 16)}
		h.deleter.put("a", gated)
		h.start()
		<-gated.entered
		engine, running := h.engine, h.running
		h.engine = nil

		request := h.request("b")
		removed := make(chan error, 1)
		go func() {
			_, err := engine.Remove(ctx, request)
			removed <- err
		}()
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- engine.Stop(ctx) }()
		synctest.Wait()
		gated.gate <- struct{}{}

		if err := <-stopped; err != nil {
			t.Errorf("Stop() = %v", err)
		}
		if err := <-running; err != nil {
			t.Errorf("Run() = %v", err)
		}
		if err := <-removed; !errors.Is(err, ErrStopped) {
			t.Errorf("Remove() queued behind the stop = %v, want ErrStopped", err)
		}
		h.expectEvents("sample", "admit:a", "open:a", "step:a", "close:a")
		if got := h.journaled(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 100 {
			t.Errorf("journal after the stop = phase %s, removed %d; want deleting, 100", got.Phase, got.Removed)
		}
	})
}

func TestCommandTakenWhileStoppingIsRefused(t *testing.T) {
	ctx := context.Background()
	engine := newHarness(t, DefaultTuning()).build()
	if err := engine.Stop(ctx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	refused := 0
	cmd := command{
		run: func(context.Context) error {
			t.Error("the command ran after Stop")
			return nil
		},
		refuse: func() { refused++ },
	}
	if err := engine.execute(ctx, cmd); err != nil || refused != 1 {
		t.Errorf("execute() after Stop = %v with %d refusals, want nil with 1", err, refused)
	}
}

func TestWaiterOutlivesThePruningOfItsJob(t *testing.T) {
	tuning := DefaultTuning()
	tuning.KeepDone = 0
	bubble(t, tuning, func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		gated := &payload{entries: 100, gate: make(chan struct{}), entered: make(chan struct{}, 16)}
		h.deleter.put("a", gated)
		h.start()
		engine := h.engine
		<-gated.entered

		type outcome struct {
			job cleanup.Job
			err error
		}
		waited := make(chan outcome, 1)
		go func() {
			finished, err := engine.Wait(ctx, job.ID)
			waited <- outcome{finished, err}
		}()
		synctest.Wait()
		gated.gate <- struct{}{}
		got := <-waited
		if got.err != nil || got.job.ID != job.ID || got.job.Phase != cleanup.PhaseDone || got.job.Removed != 100 {
			t.Errorf("Wait() across the pruning = job %s at %s, removed %d, %v; want %s done, 100", got.job.ID, got.job.Phase, got.job.Removed, got.err, job.ID)
		}

		synctest.Wait()
		if _, err := engine.Wait(ctx, job.ID); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Wait() after the pruning = %v, want ErrUnknownJob", err)
		}
		report, err := engine.Status(ctx, cleanup.Query{})
		if err != nil || len(report.Jobs) != 0 {
			t.Errorf("Status() after the pruning = %d jobs, %v; want none", len(report.Jobs), err)
		}
		if _, err := os.Lstat(h.layout.JobDir(job.ID)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lstat(pruned job folder) = %v, want not exist", err)
		}
		engine.mu.Lock()
		kept, waiters := len(engine.kept), len(engine.waiters)
		engine.mu.Unlock()
		if kept != 0 || waiters != 0 {
			t.Errorf("the engine still keeps %d results for %d waiters, want none", kept, waiters)
		}
	})
}

func TestStopBeforeRunRefusesEveryCall(t *testing.T) {
	tests := []struct {
		name string
		call func(ctx context.Context, h *harness, engine *Engine) error
	}{
		{"remove", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Remove(ctx, h.request("a"))
			return err
		}},
		{"defer", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Defer(ctx, h.deferral("a"))
			return err
		}},
		{"adopt", func(ctx context.Context, h *harness, engine *Engine) error {
			_, err := engine.Adopt(ctx, h.adoption("a"))
			return err
		}},
		{"pause", func(ctx context.Context, _ *harness, engine *Engine) error { return engine.Pause(ctx) }},
		{"resume", func(ctx context.Context, _ *harness, engine *Engine) error { return engine.Resume(ctx) }},
		{"retry", func(ctx context.Context, _ *harness, engine *Engine) error {
			_, err := engine.Retry(ctx, unknownJob)
			return err
		}},
		{"status", func(ctx context.Context, _ *harness, engine *Engine) error {
			_, err := engine.Status(ctx, cleanup.Query{})
			return err
		}},
		{"wait", func(ctx context.Context, _ *harness, engine *Engine) error {
			_, err := engine.Wait(ctx, unknownJob)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
				engine := h.build()
				if err := engine.Stop(context.Background()); err != nil {
					t.Fatalf("Stop() before Run = %v", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := tt.call(ctx, h, engine); !errors.Is(err, ErrStopped) {
					t.Errorf("%s after a Stop that Run never followed = %v, want ErrStopped", tt.name, err)
				}
				h.expectEvents()
			})
		})
	}
}

func TestStopDuringASampleRunsNoFurtherSlice(t *testing.T) {
	tests := []struct {
		name string
		stop func(t *testing.T, engine *Engine, cancel context.CancelFunc)
	}{
		{"its context ends", func(_ *testing.T, _ *Engine, cancel context.CancelFunc) { cancel() }},
		{"Stop is called", func(t *testing.T, engine *Engine, _ context.CancelFunc) {
			go func() {
				if err := engine.Stop(context.Background()); err != nil {
					t.Errorf("Stop() = %v", err)
				}
			}()
		}},
	}
	tuning := DefaultTuning()
	tuning.SampleEvery = 400 * time.Millisecond
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, tuning, func(t *testing.T, h *harness) {
				job := h.seed("a", 1, cleanup.PhaseUnregistered)
				h.deleter.put("a", &payload{entries: 300})
				h.cpu.set(0, nil)
				engine := h.build()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				running := make(chan error, 1)
				go func() { running <- engine.Run(ctx) }()
				h.expectEvents("sample")
				h.clock.Advance(400 * time.Millisecond)
				h.expectEvents("sample", "admit:a", "open:a", "step:a")

				gate := h.cpu.hold()
				h.clock.Advance(400 * time.Millisecond)
				<-h.cpu.entered
				tt.stop(t, engine, cancel)
				synctest.Wait()
				gate <- struct{}{}
				if err := <-running; err != nil {
					t.Fatalf("Run() = %v", err)
				}
				h.expectEvents("sample", "close:a")
				if got := h.journaled(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 100 {
					t.Errorf("journal after the stop = phase %s, removed %d; want deleting, 100", got.Phase, got.Removed)
				}
				report, err := engine.Status(context.Background(), cleanup.Query{JobID: job.ID})
				if want := (cleanup.Governor{State: "clear"}); err != nil || report.Governor != want {
					t.Errorf("Status() after the stop = governor %+v, %v; want %+v", report.Governor, err, want)
				}
			})
		})
	}
}
