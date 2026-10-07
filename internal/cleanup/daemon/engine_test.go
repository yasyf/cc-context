package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func bubble(t *testing.T, tuning Tuning, fn func(t *testing.T, h *harness)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) { fn(t, newHarness(t, tuning)) })
}

func TestEngineRunsJobsFirstInFirstOut(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.seed("a", 1, cleanup.PhasePrepared)
		h.seed("b", 2, cleanup.PhasePrepared)
		h.seed("c", 3, cleanup.PhasePrepared)
		for _, name := range []string{"a", "b", "c"} {
			h.deleter.put(name, &payload{entries: 50})
		}
		h.start()
		h.expectEvents("advance:a@prepared", "advance:b@prepared", "advance:c@prepared", "sample", "admit:a", "open:a", "step:a")
		h.expectTimers(200 * time.Millisecond)
		h.clock.Advance(200 * time.Millisecond)
		h.expectEvents("admit:b", "open:b", "step:b")
		h.expectTimers(200 * time.Millisecond)
		h.clock.Advance(200 * time.Millisecond)
		h.expectEvents("admit:c", "open:c", "step:c")
		h.expectTimers()

		report, err := h.engine.Status(context.Background(), cleanup.Query{})
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if got, want := h.names(report.Jobs), []string{"c", "b", "a"}; !slices.Equal(got, want) {
			t.Errorf("finished jobs = %v, want %v", got, want)
		}
		for _, job := range report.Jobs {
			if job.Phase != cleanup.PhaseDone || job.Removed != 50 {
				t.Errorf("job %s = phase %s, removed %d; want done, 50", h.rec.name(job.ID), job.Phase, job.Removed)
			}
		}
	})
}

func TestRemoveDuringDeletionWaitsAtMostOneSlice(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.seed("a", 1, cleanup.PhaseUnregistered)
		gated := &payload{entries: 300, entered: make(chan struct{}, 16)}
		h.deleter.put("a", gated)
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)

		gate := make(chan struct{})
		h.deleter.mu.Lock()
		gated.gate = gate
		h.deleter.mu.Unlock()
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("step:a")

		type outcome struct {
			receipt cleanup.Receipt
			err     error
		}
		removed := make(chan outcome, 1)
		go func() {
			receipt, err := h.engine.Remove(context.Background(), h.request("b"))
			removed <- outcome{receipt, err}
		}()
		h.expectEvents()

		h.deleter.mu.Lock()
		gated.gate = nil
		h.deleter.mu.Unlock()
		gate <- struct{}{}
		h.expectEvents("accept:b", "advance:b@prepared")
		got := <-removed
		if got.err != nil {
			t.Fatalf("Remove() = %v", got.err)
		}
		if got.receipt.State != cleanup.State(cleanup.PhaseUnregistered) || h.rec.name(got.receipt.JobID) != "b" {
			t.Errorf("Remove() receipt = %+v, want job b unregistered", got.receipt)
		}
		h.expectTimers(400 * time.Millisecond)

		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("step:a")
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:b", "open:b")
		if job := h.status(got.receipt.JobID); job.Phase != cleanup.PhaseDone {
			t.Errorf("job b phase = %s, want done", job.Phase)
		}
	})
}

func TestDeletionPace(t *testing.T) {
	tests := []struct {
		name       string
		payload    payload
		wantEvents []string
		wantTimers []time.Duration
	}{
		{"a full slice waits its share of a second", payload{entries: 200}, []string{"sample", "admit:a", "open:a", "step:a"}, []time.Duration{400 * time.Millisecond}},
		{"the slice's own duration is subtracted", payload{entries: 200, stepTime: 30 * time.Millisecond}, []string{"sample", "admit:a", "open:a", "step:a"}, []time.Duration{370 * time.Millisecond}},
		{"a short slice waits for what it removed", payload{entries: 200, perStep: 50, stepTime: 10 * time.Millisecond}, []string{"sample", "admit:a", "open:a", "step:a"}, []time.Duration{190 * time.Millisecond}},
		{"a slice slower than its pace is followed at once", payload{entries: 200, stepTime: 400 * time.Millisecond}, []string{"sample", "admit:a", "open:a", "step:a", "step:a"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(_ *testing.T, h *harness) {
				h.seed("a", 1, cleanup.PhaseUnregistered)
				h.deleter.put("a", &tt.payload)
				h.start()
				h.expectEvents(tt.wantEvents...)
				h.expectTimers(tt.wantTimers...)
			})
		})
	}
}

func TestGovernorSlowsDeletionToTheFloorOverTheAmbientLoad(t *testing.T) {
	tuning := paced()
	tuning.Rate, tuning.FloorRate, tuning.SampleEvery = 100, 25, 2*time.Second
	bubble(t, tuning, func(t *testing.T, h *harness) {
		governor := func() cleanup.Governor {
			t.Helper()
			synctest.Wait()
			report, err := h.engine.Status(context.Background(), cleanup.Query{})
			if err != nil {
				t.Fatalf("Status() = %v", err)
			}
			return report.Governor
		}
		expectGovernor := func(want cleanup.Governor) {
			t.Helper()
			if got := governor(); got != want {
				t.Fatalf("governor = %+v, want %+v", got, want)
			}
		}
		h.cpu.set(0, nil)
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 1000})
		h.start()
		h.expectEvents("sample")
		h.expectTimers(2 * time.Second)
		expectGovernor(cleanup.Governor{State: "sampling"})

		h.cpu.burn(2 * time.Second)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(time.Second)
		expectGovernor(cleanup.Governor{State: "clear", CPUPercent: 100})

		h.clock.Advance(time.Second)
		h.expectEvents("step:a")
		h.expectTimers(time.Second)

		h.cpu.burn(3500 * time.Millisecond)
		h.clock.Advance(time.Second)
		h.expectEvents("sample", "step:a")
		h.expectTimers(2 * time.Second)
		expectGovernor(cleanup.Governor{State: "throttled", CPUPercent: 175})

		h.cpu.burn(2 * time.Second)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("sample")
		h.expectTimers(2 * time.Second)
		expectGovernor(cleanup.Governor{State: "throttled", CPUPercent: 100})

		h.cpu.burn(2 * time.Second)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("sample", "step:a")
		h.expectTimers(2 * time.Second)
		expectGovernor(cleanup.Governor{State: "throttled", CPUPercent: 100})
		if got := h.status(job.ID); got.Removed != 400 || got.Phase != cleanup.PhaseDeleting {
			t.Errorf("job under the throttle = phase %s, removed %d; want deleting, 400", got.Phase, got.Removed)
		}

		h.cpu.burn(2 * time.Second)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("sample")
		h.expectTimers(2 * time.Second)
		expectGovernor(cleanup.Governor{State: "clear", CPUPercent: 100})

		if err := h.engine.Pause(context.Background()); err != nil {
			t.Fatalf("Pause() = %v", err)
		}
		h.expectEvents("close:a")
		h.expectTimers()
		expectGovernor(cleanup.Governor{State: "idle"})
	})
}

func TestAwaitedJobJumpsTheQueuePastTheThrottleAndThePace(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.cpu.set(0, nil)
		queued := h.seed("a", 1, cleanup.PhaseUnregistered)
		awaited := h.seed("b", 2, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 300})
		h.deleter.put("b", &payload{entries: 300})
		h.start()
		h.expectEvents("sample")
		h.expectTimers(5 * time.Second)

		h.clock.Advance(5 * time.Second)
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)

		await := cleanup.Query{JobID: awaited.ID, Await: true}
		if _, err := h.engine.Status(context.Background(), await); err != nil {
			t.Fatalf("Status(%+v) = %v", await, err)
		}
		if _, err := h.engine.Status(context.Background(), cleanup.Query{JobID: "0000000000000000-000000", Await: true}); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Status(await unknown) = %v, want ErrUnknownJob", err)
		}
		h.cpu.burn(5 * time.Second)
		h.clock.Advance(5 * time.Second)
		h.expectEvents("close:a", "sample", "admit:b", "open:b", "step:b", "step:b", "step:b", "admit:a", "open:a", "step:a")
		h.expectTimers(time.Second)

		if got := h.status(awaited.ID); got.Phase != cleanup.PhaseDone || got.Removed != 300 {
			t.Errorf("awaited job = phase %s, removed %d; want done, 300", got.Phase, got.Removed)
		}
		if _, err := h.engine.Status(context.Background(), await); err != nil {
			t.Fatalf("Status(%+v) after the finish = %v", await, err)
		}
		h.engine.mu.Lock()
		leases := len(h.engine.awaits)
		h.engine.mu.Unlock()
		if leases != 0 {
			t.Errorf("leases after awaiting a finished job = %d, want none", leases)
		}
		if got := h.status(queued.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 200 {
			t.Errorf("queued job = phase %s, removed %d; want deleting, 200 at the floor", got.Phase, got.Removed)
		}
		report, err := h.engine.Status(context.Background(), cleanup.Query{})
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if want := (cleanup.Governor{State: "throttled", CPUPercent: 100}); report.Governor != want {
			t.Errorf("governor = %+v, want %+v", report.Governor, want)
		}
	})
}

func TestAwaitLapsesWithoutAnotherAwaitingRead(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.cpu.set(0, nil)
		h.seed("a", 1, cleanup.PhaseUnregistered)
		lapsed := h.seed("b", 2, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 200})
		h.deleter.put("b", &payload{entries: 200})
		h.start()
		h.expectEvents("sample")
		h.expectTimers(5 * time.Second)

		if _, err := h.engine.Status(context.Background(), cleanup.Query{JobID: lapsed.ID, Await: true}); err != nil {
			t.Fatalf("Status(await) = %v", err)
		}
		h.clock.Advance(11 * time.Second)
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)
	})
}

func TestUnavailableSamplerProceedsAndIsReported(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 300})
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		report, err := h.engine.Status(context.Background(), cleanup.Query{})
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		want := cleanup.Governor{State: "unavailable", Detail: "no sampler in this test"}
		if report.Governor != want {
			t.Errorf("governor = %+v, want %+v", report.Governor, want)
		}
	})
}

func TestProgressIsJournaledAtMostOnceAMinute(t *testing.T) {
	tuning := paced()
	tuning.Rate = 100
	bubble(t, tuning, func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 100000})
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		h.expectTimers(time.Second)
		for range 59 {
			h.clock.Advance(time.Second)
			synctest.Wait()
		}
		if got := h.status(job.ID).Removed; got != 6000 {
			t.Fatalf("removed after 60 slices = %d, want 6000", got)
		}
		if got := h.journaled(job.ID); got.Removed != 0 || got.Phase != cleanup.PhaseDeleting {
			t.Fatalf("journal inside the first minute = phase %s, removed %d; want deleting, 0", got.Phase, got.Removed)
		}
		h.clock.Advance(time.Second)
		synctest.Wait()
		if got := h.journaled(job.ID).Removed; got != 6100 {
			t.Errorf("journaled removed at the minute = %d, want 6100", got)
		}
		h.clock.Advance(time.Second)
		synctest.Wait()
		if got, status := h.journaled(job.ID).Removed, h.status(job.ID).Removed; got != 6100 || status != 6200 {
			t.Errorf("after the next slice the journal holds %d and the engine %d; want 6100, 6200", got, status)
		}
	})
}

func TestQueuePauseRestsEveryJobAndRefusesNewWork(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 300})
		waiting := h.seed("w", 2, cleanup.PhaseWaiting)
		h.relocator.script("w", stayWaiting)
		retrying := h.seed("r", 3, cleanup.PhasePrepared, func(job *cleanup.Job) {
			job.Block(job.Created, "activity", "zsh (pid 7) in the tree")
		})
		h.start()
		h.expectEvents("advance:w@waiting", "sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)

		if err := h.engine.Pause(ctx); err != nil {
			t.Fatalf("Pause() = %v", err)
		}
		h.expectEvents("close:a")
		h.expectTimers()
		sw, err := h.journal.LoadSwitch()
		if err != nil || !sw.Paused {
			t.Fatalf("LoadSwitch() = %+v, %v; want paused", sw, err)
		}
		if got := h.journaled(job.ID).Removed; got != 100 {
			t.Errorf("journaled removed = %d, want 100", got)
		}
		h.clock.Advance(time.Hour)
		h.expectEvents()
		h.expectTimers()

		for _, tt := range []struct {
			name string
			ask  func() error
		}{
			{"remove", func() error { _, err := h.engine.Remove(ctx, h.request("b")); return err }},
			{"defer", func() error { _, err := h.engine.Defer(ctx, h.deferral("d")); return err }},
			{"adopt", func() error { _, err := h.engine.Adopt(ctx, h.adoption("q")); return err }},
		} {
			if err := tt.ask(); !errors.Is(err, cleanup.ErrPaused) {
				t.Errorf("%s under a queue pause = %v, want ErrPaused", tt.name, err)
			}
		}
		h.expectEvents()
		h.expectTimers()
		jobs, damaged, err := h.journal.Load()
		if err != nil || len(damaged) != 0 || len(jobs) != 3 {
			t.Fatalf("Load() = %d jobs, damaged %v, %v; want the three seeded and nothing new", len(jobs), damaged, err)
		}
		for _, seeded := range []cleanup.Job{waiting, retrying} {
			if got := h.journaled(seeded.ID); got.Phase != seeded.Phase || (got.Blocked == nil) != (seeded.Blocked == nil) {
				t.Errorf("%s rests at %s, blocked %t; want %s, blocked %t", h.rec.name(seeded.ID), got.Phase, got.Blocked != nil, seeded.Phase, seeded.Blocked != nil)
			}
		}
		report, err := h.engine.Status(ctx, cleanup.Query{})
		if err != nil || !report.Paused {
			t.Fatalf("Status() paused = %t, %v; want true", report.Paused, err)
		}

		if err := h.engine.Resume(ctx); err != nil {
			t.Fatalf("Resume() = %v", err)
		}
		h.expectEvents("advance:w@waiting", "advance:r@prepared", "sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)
		if sw, err := h.journal.LoadSwitch(); err != nil || sw.Paused {
			t.Errorf("LoadSwitch() after resume = %+v, %v; want unpaused", sw, err)
		}
		if got := h.deleter.wants("a"); !slices.Equal(got, []cleanup.FileID{job.Tree, job.Tree}) {
			t.Errorf("payload identities verified = %v, want %v before and again after the pause", got, job.Tree)
		}
		if got := h.status(job.ID); got.State() != cleanup.State(cleanup.PhaseDeleting) || got.Removed != 200 {
			t.Errorf("resumed job = state %s, removed %d; want deleting, 200", got.State(), got.Removed)
		}
		if got := h.status(retrying.ID); got.Phase != cleanup.PhaseUnregistered || got.Blocked != nil {
			t.Errorf("retried job = phase %s, blockage %+v; want unregistered and unblocked once the queue resumed", got.Phase, got.Blocked)
		}
		receipt, err := h.engine.Remove(ctx, h.request("b"))
		if err != nil || receipt.State != cleanup.State(cleanup.PhaseUnregistered) {
			t.Fatalf("Remove(b) after the resume = %+v, %v; want it unregistered", receipt, err)
		}
		h.expectEvents("accept:b", "advance:b@prepared")
	})
}

func TestWaitingJobIsRecheckedWhileLaterJobsProceed(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		waiting := h.seed("w", 1, cleanup.PhaseWaiting)
		later := h.seed("b", 2, cleanup.PhasePrepared)
		h.relocator.script("w", stayWaiting)
		h.start()
		h.expectEvents("advance:w@waiting", "advance:b@prepared", "sample", "admit:b", "open:b")
		h.expectTimers(30 * time.Second)
		if got := h.status(later.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("later job phase = %s, want done", got)
		}

		h.clock.Advance(29 * time.Second)
		h.expectEvents()
		h.expectTimers()
		h.clock.Advance(time.Second)
		h.expectEvents("advance:w@waiting")
		h.expectTimers(30 * time.Second)
		if got := h.status(waiting.ID).Phase; got != cleanup.PhaseWaiting {
			t.Errorf("waiting job phase = %s, want waiting", got)
		}

		h.relocator.script("w", nil)
		h.clock.Advance(30 * time.Second)
		h.expectEvents("advance:w@waiting", "sample", "admit:w", "open:w")
		h.expectTimers()
		if got := h.status(waiting.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("released job phase = %s, want done", got)
		}
	})
}

func TestBlockedJobWaitsForRetryAndResumesFromItsPhase(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseMoved)
		h.relocator.script("a", blockWith(h.clock, "git", "worktree move failed"))
		h.start()
		h.expectEvents("advance:a@moved")
		h.expectTimers()
		h.clock.Advance(time.Hour)
		h.expectEvents()
		h.expectTimers()

		blocked := h.status(job.ID)
		wantBlockage := cleanup.Blockage{Reason: "git", Detail: "worktree move failed", At: h.clock.Now().Add(-time.Hour)}
		if blocked.State() != cleanup.StateBlocked || blocked.Phase != cleanup.PhaseMoved || *blocked.Blocked != wantBlockage {
			t.Fatalf("blocked job = state %s, phase %s, blockage %+v; want blocked, moved, %+v", blocked.State(), blocked.Phase, blocked.Blocked, wantBlockage)
		}

		if _, err := h.engine.Retry(ctx, "0000000000000000-000000"); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Retry(unknown) = %v, want ErrUnknownJob", err)
		}
		h.relocator.script("a", nil)
		retried, err := h.engine.Retry(ctx, job.ID)
		if err != nil {
			t.Fatalf("Retry() = %v", err)
		}
		if retried.Blocked != nil || retried.Phase != cleanup.PhaseMoved || !retried.Updated.Equal(h.clock.Now()) {
			t.Errorf("Retry() = blocked %+v, phase %s, updated %s; want unblocked at moved, updated now", retried.Blocked, retried.Phase, retried.Updated)
		}
		h.expectEvents("advance:a@moved", "sample", "admit:a", "open:a")
		if got := h.status(job.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("retried job phase = %s, want done", got)
		}
	})
}

func TestWaitReturnsOnDoneAndOnBlock(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		done := h.seed("a", 1, cleanup.PhaseUnregistered)
		stuck := h.seed("b", 2, cleanup.PhaseUnregistered)
		held := h.seed("c", 3, cleanup.PhaseWaiting)
		h.relocator.script("c", stayWaiting)
		h.deleter.put("a", &payload{entries: 100})
		h.deleter.put("b", &payload{entries: 100, stepErr: errors.New("stuck at sub/dir")})
		h.start()

		type outcome struct {
			job cleanup.Job
			err error
		}
		wait := func(id string) chan outcome {
			result := make(chan outcome, 1)
			go func() {
				job, err := h.engine.Wait(ctx, id)
				result <- outcome{job, err}
			}()
			return result
		}
		doneWait, stuckWait, heldWait := wait(done.ID), wait(stuck.ID), wait(held.ID)
		h.expectEvents("advance:c@waiting", "sample", "admit:a", "open:a", "step:a")
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:b", "open:b", "step:b")

		got := <-doneWait
		if got.err != nil || got.job.Phase != cleanup.PhaseDone || got.job.Removed != 100 {
			t.Errorf("Wait(done) = phase %s, removed %d, err %v; want done, 100, nil", got.job.Phase, got.job.Removed, got.err)
		}
		got = <-stuckWait
		var blocked *cleanup.BlockedError
		if !errors.As(got.err, &blocked) {
			t.Fatalf("Wait(stuck) = %v, want a *BlockedError", got.err)
		}
		wantBlockage := cleanup.Blockage{Reason: "delete", Detail: "stuck at sub/dir", At: h.clock.Now()}
		if *blocked.Job.Blocked != wantBlockage || blocked.Job.ID != stuck.ID || !reflect.DeepEqual(got.job, blocked.Job) {
			t.Errorf("Wait(stuck) blockage = %+v on %s, want %+v on %s", blocked.Job.Blocked, blocked.Job.ID, wantBlockage, stuck.ID)
		}
		if _, err := h.engine.Wait(ctx, "0000000000000000-000000"); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Wait(unknown) = %v, want ErrUnknownJob", err)
		}

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := h.engine.Wait(cancelled, held.ID); !errors.Is(err, context.Canceled) {
			t.Errorf("Wait(waiting) under a cancelled context = %v, want context.Canceled", err)
		}
		h.stop()
		if got := <-heldWait; !errors.Is(got.err, ErrStopped) {
			t.Errorf("Wait(waiting) across a stop = %v, want ErrStopped", got.err)
		}
	})
}

func TestRestartResumesEveryPhaseAndReverifiesThePayload(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		deleting := h.seed("x", 1, cleanup.PhaseUnregistered)
		h.deleter.put("x", &payload{entries: 300})
		h.start()
		h.expectEvents("sample", "admit:x", "open:x", "step:x")
		h.stop()
		h.expectEvents("close:x")
		if got := h.journaled(deleting.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 100 {
			t.Fatalf("journal at shutdown = phase %s, removed %d; want deleting, 100", got.Phase, got.Removed)
		}
		h.clock.takeArmed()

		h.seed("p", 2, cleanup.PhasePrepared)
		h.seed("m", 3, cleanup.PhaseMoved)
		h.seed("d", 4, cleanup.PhaseDetached)
		h.start()
		h.expectEvents("advance:p@prepared", "advance:m@moved", "advance:d@detached", "sample", "admit:x", "open:x", "step:x")
		h.expectTimers(400 * time.Millisecond)
		if got := h.deleter.wants("x"); !slices.Equal(got, []cleanup.FileID{deleting.Tree, deleting.Tree}) {
			t.Errorf("payload identities verified = %v, want %v once per engine", got, deleting.Tree)
		}
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("step:x")
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:p", "open:p", "admit:m", "open:m", "admit:d", "open:d")
		if got := h.status(deleting.ID); got.Phase != cleanup.PhaseDone || got.Removed != 300 {
			t.Errorf("resumed job = phase %s, removed %d; want done, 300", got.Phase, got.Removed)
		}
	})
}

func TestStalledDeletionBlocksOnlyItsJob(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		stalled := h.seed("a", 1, cleanup.PhaseUnregistered)
		healthy := h.seed("b", 2, cleanup.PhaseUnregistered)
		swapped := h.seed("c", 3, cleanup.PhaseUnregistered)
		identity := fmt.Errorf("payload is another tree: %w", cleanup.ErrIdentity)
		h.deleter.put("a", &payload{entries: 100, stepErr: errors.New("stuck at sub/dir")})
		h.deleter.put("b", &payload{entries: 100})
		h.deleter.put("c", &payload{entries: 100, openErr: identity})
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a", "admit:b", "open:b", "step:b")
		h.expectTimers(400 * time.Millisecond)
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:c", "open:c")
		h.expectTimers()

		tests := []struct {
			name   string
			id     string
			phase  cleanup.Phase
			detail string
		}{
			{"a step error", stalled.ID, cleanup.PhaseDeleting, "stuck at sub/dir"},
			{"an identity mismatch at open", swapped.ID, cleanup.PhaseDeleting, identity.Error()},
		}
		for _, tt := range tests {
			job := h.journaled(tt.id)
			if job.Blocked == nil || job.Blocked.Reason != "delete" || job.Blocked.Detail != tt.detail || job.Phase != tt.phase {
				t.Errorf("%s: journaled job = phase %s, blockage %+v; want %s blocked on delete: %s", tt.name, job.Phase, job.Blocked, tt.phase, tt.detail)
			}
		}
		if got := h.status(healthy.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("the next job = phase %s, removed %d; want done, 100", got.Phase, got.Removed)
		}
	})
}

func TestRepeatRemoveJoinsTheJobStillAtItsPath(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		h.relocator.script("a", blockWith(h.clock, "watchers", "fsmonitor is still running"))
		_, err := h.engine.Remove(ctx, h.request("a"))
		var blocked *cleanup.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("Remove() = %v, want a *BlockedError", err)
		}
		if blocked.Job.Phase != cleanup.PhasePrepared || blocked.Job.Blocked.Reason != "watchers" {
			t.Fatalf("blocked job = phase %s, reason %s; want prepared, watchers", blocked.Job.Phase, blocked.Job.Blocked.Reason)
		}
		if at := blocked.Job.Blocked.RetryAt; at == nil || !at.Equal(h.clock.Now().Add(30*time.Second)) {
			t.Errorf("blocked job retries at %v, want %s", at, h.clock.Now().Add(30*time.Second))
		}
		first := blocked.Job.ID
		h.expectEvents("accept:a", "advance:a@prepared")

		h.relocator.script("a", nil)
		receipt, err := h.engine.Remove(ctx, h.request("a"))
		if err != nil {
			t.Fatalf("repeat Remove() = %v", err)
		}
		want := cleanup.Receipt{JobID: first, State: cleanup.State(cleanup.PhaseUnregistered), Original: h.tree("a")}
		if receipt != want {
			t.Errorf("repeat Remove() = %+v, want %+v", receipt, want)
		}
		h.expectEvents("advance:a@prepared", "sample", "admit:a", "open:a")

		receipt, err = h.engine.Remove(ctx, h.request("a"))
		if err != nil {
			t.Fatalf("Remove() of the freed path = %v", err)
		}
		if receipt.JobID == first {
			t.Errorf("Remove() of the freed path rejoined job %s, want a new job", first)
		}
		h.expectEvents("accept:a", "advance:a@prepared", "sample", "admit:a", "open:a")

		h.relocator.script("a", blockWith(h.clock, "watchers", "fsmonitor is still running"))
		if _, err = h.engine.Remove(ctx, h.request("a")); !errors.As(err, &blocked) {
			t.Fatalf("Remove() = %v, want a *BlockedError", err)
		}
		stranded := blocked.Job.ID
		h.expectEvents("accept:a", "advance:a@prepared")
		if err := os.Rename(h.tree("a"), filepath.Join(h.root, "trees", "a.old")); err != nil {
			t.Fatal(err)
		}
		h.relocator.script("a", nil)
		receipt, err = h.engine.Remove(ctx, h.request("a"))
		if err != nil {
			t.Fatalf("Remove() of a replacement tree = %v", err)
		}
		if receipt.JobID == stranded {
			t.Errorf("Remove() of a replacement tree rejoined job %s, want a new job", stranded)
		}
		h.expectEvents("accept:a", "advance:a@prepared", "sample", "admit:a", "open:a")
		if got := h.status(stranded).State(); got != cleanup.StateBlocked {
			t.Errorf("the job of the replaced tree = %s, want blocked", got)
		}
	})
}

func TestRemoveOfAWorkspaceStillWaitingIsRefused(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		h.relocator.held["d"] = "zsh (pid 7) in the tree"
		h.relocator.script("d", stayWaiting)
		deferred, err := h.engine.Defer(ctx, h.deferral("d"))
		if err != nil || deferred.State != cleanup.State(cleanup.PhaseWaiting) {
			t.Fatalf("Defer() = %+v, %v; want a waiting job", deferred, err)
		}
		h.expectEvents("intend:d")

		receipt, err := h.engine.Remove(ctx, h.request("d"))
		want := cleanup.RefusedError{
			Worktree: h.tree("d"),
			Reason:   "waiting",
			Detail:   "deferred removal " + deferred.JobID + " still waits for the tree: zsh (pid 7) in the tree",
		}
		var refused *cleanup.RefusedError
		if !errors.As(err, &refused) || *refused != want || receipt != (cleanup.Receipt{}) {
			t.Fatalf("Remove() of a workspace still held = %+v, %v; want no receipt and %+v", receipt, err, want)
		}
		h.expectEvents("advance:d@waiting")
		if got := h.status(deferred.JobID); got.Phase != cleanup.PhaseWaiting || got.Blocked != nil {
			t.Errorf("the deferred job = phase %s, blocked %v; want it still waiting", got.Phase, got.Blocked)
		}

		h.relocator.script("d", nil)
		receipt, err = h.engine.Remove(ctx, h.request("d"))
		freed := cleanup.Receipt{JobID: deferred.JobID, State: cleanup.State(cleanup.PhaseUnregistered), Original: h.tree("d")}
		if err != nil || receipt != freed {
			t.Errorf("Remove() once the workspace is free = %+v, %v; want %+v", receipt, err, freed)
		}
		h.expectEvents("advance:d@waiting", "sample", "admit:d", "open:d")
	})
}

func TestUnforcedRequestNeverJoinsAForcedJob(t *testing.T) {
	tests := []struct {
		name string
		ask  func(ctx context.Context, h *harness) error
		want string
	}{
		{"remove", func(ctx context.Context, h *harness) error {
			_, err := h.engine.Remove(ctx, h.request("a"))
			return err
		}, "accept:a"},
		{"defer", func(ctx context.Context, h *harness) error {
			_, err := h.engine.Defer(ctx, h.deferral("a"))
			return err
		}, "intend:a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				ctx := context.Background()
				h.start()
				forced := h.request("a")
				forced.Force = true
				h.relocator.script("a", blockWith(h.clock, "watchers", "fsmonitor is still running"))
				_, err := h.engine.Remove(ctx, forced)
				var blocked *cleanup.BlockedError
				if !errors.As(err, &blocked) || !blocked.Job.Force {
					t.Fatalf("forced Remove() = %v, want a forced job blocked at prepared", err)
				}
				h.expectEvents("accept:a", "advance:a@prepared")

				refusal := &cleanup.RefusedError{Worktree: h.tree("a"), Reason: "dirty", Detail: "2 uncommitted paths"}
				h.relocator.refuse["a"] = refusal
				h.relocator.script("a", nil)
				var refused *cleanup.RefusedError
				if err := tt.ask(ctx, h); !errors.As(err, &refused) || refused != refusal {
					t.Fatalf("unforced %s of the forced job's tree = %v, want its own preflight refusal", tt.name, err)
				}
				h.expectEvents(tt.want)
				if got := h.status(blocked.Job.ID); got.Phase != cleanup.PhasePrepared || got.Blocked == nil {
					t.Errorf("the forced job = phase %s, blocked %v; want it still blocked at prepared", got.Phase, got.Blocked)
				}

				delete(h.relocator.refuse, "a")
				receipt, err := h.engine.Remove(ctx, forced)
				if err != nil || receipt.JobID != blocked.Job.ID {
					t.Errorf("forced repeat Remove() = %+v, %v; want it to rejoin job %s", receipt, err, blocked.Job.ID)
				}
				h.expectEvents("advance:a@prepared", "sample", "admit:a", "open:a")
			})
		})
	}
}

func TestCommandsRunOutsideTheBackgroundBand(t *testing.T) {
	t.Run("a remove relocates in the foreground", func(t *testing.T) {
		bubble(t, paced(), func(t *testing.T, h *harness) {
			gate := make(chan struct{})
			h.relocator.gates["a"] = gate
			h.start()
			removed := make(chan error, 1)
			go func() {
				_, err := h.engine.Remove(context.Background(), h.request("a"))
				removed <- err
			}()
			if got := <-h.relocator.entered; got != "a" {
				t.Fatalf("the relocator entered %q, want a", got)
			}
			if !h.band.inForeground() {
				t.Error("the remove advanced in the background band, want the foreground")
			}
			gate <- struct{}{}
			if err := <-removed; err != nil {
				t.Fatalf("Remove() = %v", err)
			}
			if got, want := h.band.takeSwitches(), []string{"foreground", "background"}; !slices.Equal(got, want) {
				t.Errorf("band switches = %v, want %v", got, want)
			}
		})
	})
	t.Run("the worker's own pass stays in the background", func(t *testing.T) {
		bubble(t, paced(), func(t *testing.T, h *harness) {
			h.seed("a", 1, cleanup.PhaseMoved)
			gate := make(chan struct{})
			h.relocator.gates["a"] = gate
			h.start()
			if got := <-h.relocator.entered; got != "a" {
				t.Fatalf("the relocator entered %q, want a", got)
			}
			if h.band.inForeground() {
				t.Error("the worker's own pass advanced in the foreground band, want the background")
			}
			gate <- struct{}{}
			synctest.Wait()
			if got := h.band.takeSwitches(); len(got) != 0 {
				t.Errorf("band switches = %v, want none", got)
			}
		})
	})
	t.Run("a band the daemon cannot leave stops the worker", func(t *testing.T) {
		bubble(t, paced(), func(t *testing.T, h *harness) {
			h.band.refuseLeave = errors.New("operation not permitted")
			engine := h.build()
			running := make(chan error, 1)
			go func() { running <- engine.Run(context.Background()) }()
			if _, err := engine.Remove(context.Background(), h.request("a")); !errors.Is(err, ErrStopped) {
				t.Errorf("Remove() = %v, want ErrStopped", err)
			}
			if err := <-running; err == nil || !errors.Is(err, h.band.refuseLeave) {
				t.Errorf("Run() = %v, want the band's refusal", err)
			}
			h.expectEvents()
		})
	})
}

func TestRemovePassesRefusalsThrough(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		refusal := &cleanup.RefusedError{Worktree: h.tree("a"), Reason: "dirty", Detail: "2 uncommitted paths"}
		h.relocator.refuse["a"] = refusal
		_, err := h.engine.Remove(ctx, h.request("a"))
		var refused *cleanup.RefusedError
		if !errors.As(err, &refused) || refused != refusal {
			t.Errorf("Remove() = %v, want the relocator's refusal unchanged", err)
		}
		if _, err := h.engine.Remove(ctx, cleanup.Request{Worktree: "relative", Git: "/usr/bin/git"}); err == nil {
			t.Error("Remove() of a relative path = nil, want a validation error")
		}
		h.expectEvents("accept:a")
		report, err := h.engine.Status(ctx, cleanup.Query{})
		if err != nil || len(report.Jobs) != 0 {
			t.Errorf("Status() after refusals = %d jobs, %v; want none", len(report.Jobs), err)
		}
	})
}

func TestDeferWaitsWithTheHoldersAsDetail(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		h.relocator.held["d"] = "zsh (pid 7) in the tree"
		h.relocator.script("d", stayWaiting)
		receipt, err := h.engine.Defer(ctx, h.deferral("d"))
		if err != nil {
			t.Fatalf("Defer() = %v", err)
		}
		want := cleanup.Receipt{
			JobID:    receipt.JobID,
			State:    cleanup.State(cleanup.PhaseWaiting),
			Original: h.tree("d"),
			Detail:   "zsh (pid 7) in the tree",
		}
		if receipt != want || h.rec.name(receipt.JobID) != "d" {
			t.Fatalf("Defer() = %+v, want %+v", receipt, want)
		}
		h.expectEvents("intend:d")
		h.expectTimers(30 * time.Second)

		again, err := h.engine.Defer(ctx, h.deferral("d"))
		if err != nil || again != want {
			t.Errorf("repeat Defer() = %+v, %v; want %+v", again, err, want)
		}
		h.expectEvents()
		h.clock.takeArmed()
		h.clock.Advance(30 * time.Second)
		h.expectEvents("advance:d@waiting")

		queued, err := h.engine.Defer(ctx, h.deferral("q"))
		if err != nil || queued.State != cleanup.State(cleanup.PhaseQueued) || queued.Detail != "" {
			t.Errorf("Defer() of a free tree = %+v, %v; want queued with no detail", queued, err)
		}
		h.expectEvents("intend:q", "advance:q@queued", "sample", "admit:q", "open:q")

		h.relocator.script("x", blockWith(h.clock, "activity", "could not verify: pid 9: operation not permitted"))
		first, err := h.engine.Defer(ctx, h.deferral("x"))
		if wantFirst := (cleanup.Receipt{JobID: first.JobID, State: cleanup.State(cleanup.PhaseQueued), Original: h.tree("x")}); err != nil || first != wantFirst {
			t.Fatalf("Defer(x) = %+v, %v; want %+v", first, err, wantFirst)
		}
		h.expectEvents("intend:x", "advance:x@queued")
		if got := h.status(first.JobID).State(); got != cleanup.StateBlocked {
			t.Fatalf("job x = %s, want blocked", got)
		}
		h.relocator.script("x", nil)
		retried, err := h.engine.Defer(ctx, h.deferral("x"))
		if err != nil || retried != first {
			t.Errorf("repeat Defer(x) of a blocked job = %+v, %v; want it retried as %+v", retried, err, first)
		}
		h.expectEvents("advance:x@queued", "sample", "admit:x", "open:x")
	})
}

func TestDeferWithoutARegistrationNeverReachesTheRelocator(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		request := h.deferral("d")
		request.Expected = cleanup.Registration{}
		receipt, err := h.engine.Defer(ctx, request)
		if want := "cleanup daemon: defer request: expected: tree identity is unset"; err == nil || err.Error() != want || receipt != (cleanup.Receipt{}) {
			t.Fatalf("Defer() without a registration = %+v, %v; want no receipt and %q", receipt, err, want)
		}
		h.expectEvents()
		report, err := h.engine.Status(ctx, cleanup.Query{})
		if err != nil || len(report.Jobs) != 0 {
			t.Errorf("Status() after the rejected request = %d jobs, %v; want none", len(report.Jobs), err)
		}
	})
}

func TestDeferAfterALostReceiptRejoinsTheJobOfTheReplacedTree(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		h.relocator.held["d"] = "zsh (pid 7) in the tree"
		h.relocator.script("d", stayWaiting)
		request := h.deferral("d")
		first, err := h.engine.Defer(ctx, request)
		if err != nil || first.State != cleanup.State(cleanup.PhaseWaiting) {
			t.Fatalf("Defer() = %+v, %v; want a waiting job", first, err)
		}
		h.expectEvents("intend:d")

		h.replace("d")
		again, err := h.engine.Defer(ctx, request)
		if err != nil || again != first {
			t.Errorf("repeat Defer() once the path holds a replacement = %+v, %v; want the first job's receipt %+v", again, err, first)
		}
		h.expectEvents()
		jobs, damaged, err := h.journal.Load()
		if err != nil || len(damaged) != 0 || len(jobs) != 1 || jobs[0].ID != first.JobID {
			t.Fatalf("Journal.Load() = jobs %v, damaged %v, err %v; want only job %s", h.names(jobs), damaged, err, first.JobID)
		}
		if bound := (cleanup.Registration{Tree: jobs[0].Tree, AdminDir: jobs[0].AdminDir, Admin: jobs[0].Admin}); bound != request.Expected {
			t.Errorf("the journaled job is bound to %+v, want the original %+v", bound, request.Expected)
		}
		if got := h.status(first.JobID); got.Phase != cleanup.PhaseWaiting || got.Blocked != nil {
			t.Errorf("the rejoined job = phase %s, blocked %v; want it still waiting", got.Phase, got.Blocked)
		}
	})
}

func TestDeferRejoinsOnlyTheJobItsRegistrationBinds(t *testing.T) {
	tests := []struct {
		name    string
		forced  bool
		again   func(h *harness, first cleanup.DeferRequest) cleanup.DeferRequest
		rejoins bool
	}{
		{"the same registration", false, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			return first
		}, true},
		{"the same registration with force", false, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			first.Force = true
			return first
		}, true},
		{"the same registration without the job's force", true, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			first.Force = false
			return first
		}, false},
		{"the replacement's registration", false, func(h *harness, _ cleanup.DeferRequest) cleanup.DeferRequest {
			h.replace("d")
			return h.deferral("d")
		}, false},
		{"another tree", false, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			first.Expected.Tree.Ino++
			return first
		}, false},
		{"another admin identity", false, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			first.Expected.Admin.Ino++
			return first
		}, false},
		{"another admin directory", false, func(_ *harness, first cleanup.DeferRequest) cleanup.DeferRequest {
			first.Expected.AdminDir += "-moved"
			return first
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				ctx := context.Background()
				h.start()
				h.relocator.held["d"] = "zsh (pid 7) in the tree"
				h.relocator.script("d", stayWaiting)
				request := h.deferral("d")
				request.Force = tt.forced
				first, err := h.engine.Defer(ctx, request)
				if err != nil || first.State != cleanup.State(cleanup.PhaseWaiting) {
					t.Fatalf("Defer() = %+v, %v; want a waiting job", first, err)
				}
				h.expectEvents("intend:d")

				again, err := h.engine.Defer(ctx, tt.again(h, request))
				if err != nil {
					t.Fatalf("repeat Defer() = %v", err)
				}
				if tt.rejoins {
					if again != first {
						t.Errorf("repeat Defer() = %+v, want the first job's receipt %+v", again, first)
					}
					h.expectEvents()
					return
				}
				if again.JobID == first.JobID {
					t.Errorf("repeat Defer() rejoined job %s, want the relocator's own verdict", first.JobID)
				}
				h.expectEvents("intend:d")
			})
		})
	}
}

func TestCancelledAdvanceStopsTheWorkerAndLeavesTheJobRunnable(t *testing.T) {
	seedMoved := func(h *harness) { h.seed("a", 1, cleanup.PhaseMoved) }
	tests := []struct {
		name   string
		seed   func(h *harness)
		remove bool
		stop   bool
		phase  cleanup.Phase
	}{
		{"on the worker's own pass", seedMoved, false, false, cleanup.PhaseMoved},
		{"inside a remove command", func(*harness) {}, true, false, cleanup.PhasePrepared},
		{"on the worker's own pass, by Stop", seedMoved, false, true, cleanup.PhaseMoved},
		{"inside a remove command, by Stop", func(*harness) {}, true, true, cleanup.PhasePrepared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				tt.seed(h)
				gate := make(chan struct{})
				h.relocator.gates["a"] = gate
				engine := h.build()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				running := make(chan error, 1)
				go func() { running <- engine.Run(ctx) }()
				removed := make(chan error, 1)
				if tt.remove {
					go func() {
						_, err := engine.Remove(context.Background(), h.request("a"))
						removed <- err
					}()
				}
				if got := <-h.relocator.entered; got != "a" {
					t.Fatalf("the relocator entered %q, want a", got)
				}
				stopped := make(chan error, 1)
				if tt.stop {
					go func() { stopped <- engine.Stop(context.Background()) }()
					synctest.Wait()
					select {
					case err := <-stopped:
						t.Fatalf("Stop() = %v while the advance was still in flight", err)
					default:
					}
				} else {
					cancel()
				}
				gate <- struct{}{}
				if err := <-running; err != nil {
					t.Fatalf("Run() = %v, want nil: a cancelled advance is a stop, not a journal failure", err)
				}
				if tt.stop {
					if err := <-stopped; err != nil {
						t.Errorf("Stop() = %v", err)
					}
					if err := engine.Stop(context.Background()); err != nil {
						t.Errorf("a second Stop() = %v, want nil", err)
					}
				}
				if tt.remove {
					err := <-removed
					var blocked *cleanup.BlockedError
					if !errors.Is(err, ErrStopped) || !errors.Is(err, context.Canceled) || errors.As(err, &blocked) {
						t.Errorf("Remove() across the stop = %v, want ErrStopped wrapping context.Canceled", err)
					}
				}
				report, err := engine.Status(context.Background(), cleanup.Query{})
				if err != nil || len(report.Jobs) != 1 {
					t.Fatalf("Status() = %d jobs, %v; want the one job", len(report.Jobs), err)
				}
				id := report.Jobs[0].ID
				for source, job := range map[string]cleanup.Job{"engine": report.Jobs[0], "journal": h.journaled(id)} {
					if job.Phase != tt.phase || job.Blocked != nil || len(job.Errors) != 0 {
						t.Errorf("%s job = phase %s, blockage %+v, errors %v; want %s, unblocked, no history", source, job.Phase, job.Blocked, job.Errors, tt.phase)
					}
				}
				h.rec.take()

				h.relocator.mu.Lock()
				delete(h.relocator.gates, "a")
				h.relocator.mu.Unlock()
				h.start()
				h.expectEvents(fmt.Sprintf("advance:a@%s", tt.phase), "sample", "admit:a", "open:a")
				if got := h.status(id).Phase; got != cleanup.PhaseDone {
					t.Errorf("the next engine left the job at %s, want done", got)
				}
			})
		})
	}
}

func TestRequesterRidesOnlyTheCommandThatNamedIt(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		client := cleanup.WithRequester(ctx, cleanup.ProcessID{PID: 4242, Start: clientStart})
		h.seed("bg", 1, cleanup.PhasePrepared)
		h.relocator.held["d"] = "zsh (pid 7) in the tree"
		h.relocator.script("d", stayWaiting)
		h.start()
		synctest.Wait()
		if _, err := h.engine.Remove(client, h.request("a")); err != nil {
			t.Fatalf("Remove(a) = %v", err)
		}
		if _, err := h.engine.Defer(client, h.deferral("d")); err != nil {
			t.Fatalf("Defer(d) = %v", err)
		}
		if _, err := h.engine.Adopt(client, h.adoption("q")); err != nil {
			t.Fatalf("Adopt(q) = %v", err)
		}
		if _, err := h.engine.Remove(ctx, h.request("b")); err != nil {
			t.Fatalf("Remove(b) = %v", err)
		}
		h.clock.Advance(30 * time.Second)
		synctest.Wait()
		want := []string{
			"advance:bg=none",
			"accept:a=4242", "advance:a=4242",
			"intend:d=4242",
			"adopt:q=4242", "advance:q=4242",
			"accept:b=none", "advance:b=none",
			"advance:d=none",
		}
		if got := h.relocator.takeServed(); !slices.Equal(got, want) {
			t.Errorf("requesters = %q, want %q", got, want)
		}
		admitted := h.relocator.takeAdmitted()
		slices.Sort(admitted)
		if wantAdmitted := []string{"a=none", "b=none", "bg=none", "q=none"}; !slices.Equal(admitted, wantAdmitted) {
			t.Errorf("admissions = %q, want each payload admitted once on nobody's behalf: %q", admitted, wantAdmitted)
		}
	})
}

func TestRemovalKeepsItsRequesterForEveryRetry(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		requester := cleanup.ProcessID{PID: 4242, Start: clientStart}
		h.relocator.script("a", blockWith(h.clock, "activity", "ccx (pid 4242) started with the tree"))
		h.relocator.script("b", blockWith(h.clock, "activity", "zsh (pid 7) in the tree"))
		h.start()
		synctest.Wait()
		var blocked *cleanup.BlockedError
		if _, err := h.engine.Remove(cleanup.WithRequester(context.Background(), requester), h.request("a")); !errors.As(err, &blocked) {
			t.Fatalf("Remove(a) = %v, want blocked", err)
		}
		if got := h.status(blocked.Job.ID).Requester; got != requester {
			t.Errorf("job a keeps requester %+v, want %+v", got, requester)
		}
		if _, err := h.engine.Remove(context.Background(), h.request("b")); !errors.As(err, &blocked) {
			t.Fatalf("Remove(b) = %v, want blocked", err)
		}
		h.relocator.script("a", nil)
		h.relocator.script("b", nil)
		h.clock.Advance(30 * time.Second)
		synctest.Wait()
		want := []string{
			"accept:a=4242", "advance:a=4242",
			"accept:b=none", "advance:b=none",
			"advance:a=4242", "advance:b=none",
		}
		if got := h.relocator.takeServed(); !slices.Equal(got, want) {
			t.Errorf("requesters = %q, want %q", got, want)
		}
	})
}

func TestAdoptRelocatesAParkedTree(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		request := h.adoption("q")
		receipt, err := h.engine.Adopt(ctx, request)
		if err != nil {
			t.Fatalf("Adopt() = %v", err)
		}
		want := cleanup.Receipt{
			JobID:       receipt.JobID,
			State:       cleanup.State(cleanup.PhaseUnregistered),
			Original:    request.Original,
			RecoveryRef: request.RecoveryRef,
		}
		if receipt != want || h.rec.name(receipt.JobID) != "q" {
			t.Fatalf("Adopt() = %+v, want %+v", receipt, want)
		}
		h.expectEvents("adopt:q", "advance:q@prepared", "sample", "admit:q", "open:q")
		job := h.journaled(receipt.JobID)
		if !job.Adopted || job.Source != request.Source || job.Tree != request.Tree || job.Head != request.Head || job.Owner != request.Owner || job.Phase != cleanup.PhaseDone {
			t.Errorf("journaled adoption = %+v, want the done job of request %+v", job, request)
		}

		again, err := h.engine.Adopt(ctx, request)
		if err != nil || again.JobID == receipt.JobID {
			t.Errorf("Adopt() after the tree left its source = job %s, %v; want a fresh job, not %s", again.JobID, err, receipt.JobID)
		}
		h.expectEvents("adopt:q", "advance:q@prepared", "sample", "admit:q", "open:q")
	})
}

func TestRepeatAdoptJoinsTheJobStillAtItsSource(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		request := h.adoption("q")
		request.Original = request.Source
		h.relocator.script("q", blockWith(h.clock, "quarantine", "the job folder is watched"))
		_, err := h.engine.Adopt(ctx, request)
		var blocked *cleanup.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("Adopt() = %v, want a *BlockedError", err)
		}
		wantBlockage := cleanup.Blockage{Reason: "quarantine", Detail: "the job folder is watched", At: h.clock.Now()}
		if blocked.Job.Phase != cleanup.PhasePrepared || *blocked.Job.Blocked != wantBlockage || !blocked.Job.Adopted {
			t.Fatalf("blocked adoption = phase %s, blockage %+v; want prepared, %+v", blocked.Job.Phase, blocked.Job.Blocked, wantBlockage)
		}
		first := blocked.Job.ID
		h.expectEvents("adopt:q", "advance:q@prepared")

		if _, err := h.engine.Adopt(ctx, request); !errors.As(err, &blocked) || blocked.Job.ID != first {
			t.Fatalf("repeat Adopt() = %v, want job %s retried and blocked again", err, first)
		}
		h.expectEvents("advance:q@prepared")

		divergent := []struct {
			name   string
			adjust func(r *cleanup.AdoptRequest)
		}{
			{"another head", func(r *cleanup.AdoptRequest) { r.Head = "89abcdef0123456789abcdef0123456789abcdef" }},
			{"another identity", func(r *cleanup.AdoptRequest) { r.Tree.Ino++ }},
		}
		refusal := &cleanup.RefusedError{Worktree: request.Source, Reason: "identity", Detail: "the request names another tree"}
		h.relocator.refuse["q"] = refusal
		for _, tt := range divergent {
			other := request
			tt.adjust(&other)
			var refused *cleanup.RefusedError
			if _, err := h.engine.Adopt(ctx, other); !errors.As(err, &refused) || refused != refusal {
				t.Errorf("Adopt() naming %s = %v, want the relocator's verdict, not job %s", tt.name, err, first)
			}
			h.expectEvents("adopt:q")
		}

		parked := filepath.Base(request.Source)
		unregistered := &cleanup.RefusedError{Worktree: request.Source, Reason: "unregistered", Detail: "no worktree is registered there"}
		h.relocator.refuse[parked] = unregistered
		var refused *cleanup.RefusedError
		if _, err := h.engine.Remove(ctx, cleanup.Request{Worktree: request.Source, Git: "/usr/bin/git"}); !errors.As(err, &refused) || refused != unregistered {
			t.Errorf("Remove() of a parked tree = %v, want the relocator's refusal, not job %s", err, first)
		}
		h.expectEvents("accept:" + parked)
		if got := h.status(first); got.State() != cleanup.StateBlocked {
			t.Errorf("the adoption after requests that did not match it = %s, want still blocked", got.State())
		}

		delete(h.relocator.refuse, "q")
		h.relocator.script("q", nil)
		receipt, err := h.engine.Adopt(ctx, request)
		want := cleanup.Receipt{JobID: first, State: cleanup.State(cleanup.PhaseUnregistered), Original: request.Original, RecoveryRef: request.RecoveryRef}
		if err != nil || receipt != want {
			t.Errorf("repeat Adopt() once unblocked = %+v, %v; want %+v", receipt, err, want)
		}
		h.expectEvents("advance:q@prepared", "sample", "admit:q", "open:q")
	})
}

func TestAdoptOfAReplacedSourceStartsOver(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		request := h.adoption("q")
		h.relocator.script("q", blockWith(h.clock, "activity", "zsh (pid 7) in the tree"))
		_, err := h.engine.Adopt(ctx, request)
		var blocked *cleanup.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("Adopt() = %v, want a *BlockedError", err)
		}
		stranded := blocked.Job.ID
		h.expectEvents("adopt:q", "advance:q@prepared")

		if err := os.Rename(request.Source, request.Source+".old"); err != nil {
			t.Fatal(err)
		}
		if h.parked("q") != request.Source {
			t.Fatalf("the replacement was not parked at %s", request.Source)
		}
		refusal := &cleanup.RefusedError{Worktree: request.Source, Reason: "identity", Detail: "another directory sits at the source"}
		h.relocator.refuse["q"] = refusal
		var refused *cleanup.RefusedError
		if _, err := h.engine.Adopt(ctx, request); !errors.As(err, &refused) || refused != refusal {
			t.Errorf("Adopt() of a replaced source = %v, want the relocator's refusal unchanged", err)
		}
		h.expectEvents("adopt:q")
		if got := h.status(stranded); got.State() != cleanup.StateBlocked || got.Phase != cleanup.PhasePrepared {
			t.Errorf("the job of the replaced tree = %s at %s, want blocked at prepared", got.State(), got.Phase)
		}
	})
}

func TestAdoptPassesRefusalsThrough(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.start()
		refusal := &cleanup.RefusedError{Worktree: h.parked("r"), Reason: "registered", Detail: "the tree is still a worktree"}
		active := &cleanup.ActiveError{Worktree: h.parked("h"), Holders: []cleanup.Holder{
			{PID: 7, Name: "zsh", TTY: true, Evidence: cleanup.EvidenceCwd, Path: h.parked("h")},
		}}
		h.relocator.refuse["r"], h.relocator.refuse["h"] = refusal, active
		var refused *cleanup.RefusedError
		if _, err := h.engine.Adopt(ctx, h.adoption("r")); !errors.As(err, &refused) || refused != refusal {
			t.Errorf("Adopt() = %v, want the relocator's refusal unchanged", err)
		}
		var held *cleanup.ActiveError
		if _, err := h.engine.Adopt(ctx, h.adoption("h")); !errors.As(err, &held) || held != active {
			t.Errorf("Adopt() = %v, want the relocator's active verdict unchanged", err)
		}
		h.expectEvents("adopt:r", "adopt:h")

		unbound := h.adoption("u")
		unbound.RecoveryRef = cleanup.LegacyRecoveryPrefix + legacyDate + "/" + legacyID("other")
		if _, err := h.engine.Adopt(ctx, unbound); err == nil || errors.As(err, &refused) {
			t.Errorf("Adopt() of a tree its ref does not pin = %v, want a plain validation error", err)
		}
		h.expectEvents()
		report, err := h.engine.Status(ctx, cleanup.Query{})
		if err != nil || len(report.Jobs) != 0 {
			t.Errorf("Status() after refusals = %d jobs, %v; want none", len(report.Jobs), err)
		}
	})
}

func TestRestartResumesAnAdoption(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.start()
		h.relocator.script("q", blockWith(h.clock, "activity", "zsh (pid 7) in the tree"))
		_, err := h.engine.Adopt(context.Background(), h.adoption("q"))
		var blocked *cleanup.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("Adopt() = %v, want a *BlockedError", err)
		}
		h.stop()
		h.rec.take()

		h.relocator.script("q", nil)
		h.start()
		h.expectEvents()
		retried, err := h.engine.Retry(context.Background(), blocked.Job.ID)
		if err != nil || retried.Blocked != nil || !retried.Adopted || retried.Phase != cleanup.PhasePrepared {
			t.Fatalf("Retry() = %+v, %v; want the adoption unblocked at prepared", retried, err)
		}
		h.expectEvents("advance:q@prepared", "sample", "admit:q", "open:q")
		if got := h.status(blocked.Job.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("resumed adoption phase = %s, want done", got)
		}
	})
}

func TestFinishedRecordsArePrunedPastKeepDone(t *testing.T) {
	tuning := paced()
	tuning.KeepDone = 2
	bubble(t, tuning, func(t *testing.T, h *harness) {
		ids := []string{
			h.seed("a", 1, cleanup.PhaseUnregistered).ID,
			h.seed("b", 2, cleanup.PhaseUnregistered).ID,
			h.seed("c", 3, cleanup.PhaseUnregistered).ID,
			h.seed("d", 4, cleanup.PhaseUnregistered).ID,
		}
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "admit:b", "open:b", "admit:c", "open:c", "admit:d", "open:d")
		report, err := h.engine.Status(context.Background(), cleanup.Query{})
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if got, want := h.names(report.Jobs), []string{"d", "c"}; !slices.Equal(got, want) {
			t.Errorf("retained jobs = %v, want %v", got, want)
		}
		jobs, damaged, err := h.journal.Load()
		if err != nil || len(damaged) != 0 {
			t.Fatalf("Journal.Load() = damaged %v, err %v", damaged, err)
		}
		if got, want := h.names(jobs), []string{"c", "d"}; !slices.Equal(got, want) {
			t.Errorf("journaled jobs = %v, want %v", got, want)
		}
		for _, id := range ids[:2] {
			if _, err := os.Lstat(h.layout.JobDir(id)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("pruned job folder %s: Lstat = %v, want not exist", id, err)
			}
		}
		if _, err := h.engine.Wait(context.Background(), ids[0]); !errors.Is(err, cleanup.ErrUnknownJob) {
			t.Errorf("Wait(pruned) = %v, want ErrUnknownJob", err)
		}
	})
}

func TestPruneThatCannotDiscardStopsTheWorker(t *testing.T) {
	tuning := paced()
	tuning.KeepDone = 0
	bubble(t, tuning, func(t *testing.T, h *harness) {
		old := h.seed("f", 1, cleanup.PhaseDone)
		finishing := h.seed("a", 2, cleanup.PhaseUnregistered)
		next := h.seed("b", 3, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 100})
		h.deleter.put("b", &payload{entries: 100})
		if err := os.Chmod(h.layout.JobDir(old.ID), 0o500); err != nil { //nolint:gosec // a directory left searchable but unwritable, so the record cannot be discarded
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(h.layout.JobDir(old.ID), 0o700); err != nil { //nolint:gosec // restores a directory's search bit
				t.Errorf("restore the job folder: %v", err)
			}
		})
		h.start()
		running := h.running
		synctest.Wait()
		select {
		case err := <-running:
			h.engine = nil
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("Run() = %v, want the journal's failure to discard the record", err)
			}
		default:
			t.Fatal("Run() kept working past a finished record it could not prune")
		}
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		if got := h.journaled(finishing.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("the finished job = phase %s, removed %d; want done, 100", got.Phase, got.Removed)
		}
		if got := h.journaled(next.ID); got.Phase != cleanup.PhaseUnregistered || got.Removed != 0 {
			t.Errorf("the next job = phase %s, removed %d; want it untouched at unregistered", got.Phase, got.Removed)
		}
		if got := h.journaled(old.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("the record that could not be pruned = phase %s, want done", got)
		}
	})
}

func TestStatusIsBoundedAndOrdered(t *testing.T) {
	tuning := paced()
	tuning.StatusLimit = 2
	h := newHarness(t, tuning)
	finishedAt := func(after time.Duration) func(job *cleanup.Job) {
		return func(job *cleanup.Job) { job.Updated = job.Updated.Add(after) }
	}
	h.seed("u1", 1, cleanup.PhasePrepared)
	h.seed("u2", 2, cleanup.PhaseMoved)
	h.seed("u3", 3, cleanup.PhaseUnregistered)
	oldest := h.seed("f1", 4, cleanup.PhaseDone, finishedAt(time.Minute))
	h.seed("f2", 5, cleanup.PhaseDone, finishedAt(3*time.Minute))
	h.seed("f3", 6, cleanup.PhaseDone, finishedAt(2*time.Minute))
	torn := "00000000000000ff-abcdef"
	if err := os.Mkdir(h.layout.JobDir(torn), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.layout.RecordPath(torn), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
		t.Fatal(err)
	}
	h.start()
	engine := h.engine

	tests := []struct {
		name    string
		query   cleanup.Query
		want    []string
		omitted int
	}{
		{"the default limit", cleanup.Query{}, []string{"u1", "u2"}, 4},
		{"unfinished in queue order, then finished newest first", cleanup.Query{Limit: 5}, []string{"u1", "u2", "u3", "f2", "f3"}, 1},
		{"a limit past the queue omits nothing", cleanup.Query{Limit: 10}, []string{"u1", "u2", "u3", "f2", "f3", "f1"}, 0},
		{"one job", cleanup.Query{JobID: oldest.ID}, []string{"f1"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := engine.Status(context.Background(), tt.query)
			if err != nil {
				t.Fatalf("Status() = %v", err)
			}
			if got := h.names(report.Jobs); !slices.Equal(got, tt.want) || report.Omitted != tt.omitted {
				t.Errorf("Status() = jobs %v, omitted %d; want %v, %d", got, report.Omitted, tt.want, tt.omitted)
			}
			if report.Version != "v1.2.3" || report.PID != os.Getpid() || !report.Paused || report.Governor != (cleanup.Governor{State: "idle"}) {
				t.Errorf("Status() header = version %q, pid %d, paused %t, governor %+v", report.Version, report.PID, report.Paused, report.Governor)
			}
			if len(report.Damaged) != 1 || report.Damaged[0].ID != torn || report.Damaged[0].Error == "" {
				t.Errorf("Status() damaged = %+v, want the torn record %s", report.Damaged, torn)
			}
		})
	}
	if _, err := engine.Status(context.Background(), cleanup.Query{JobID: "0000000000000000-000000"}); !errors.Is(err, cleanup.ErrUnknownJob) {
		t.Errorf("Status(unknown) = %v, want ErrUnknownJob", err)
	}
}

func TestIdleEngineArmsNoTimer(t *testing.T) {
	tests := []struct {
		name string
		seed func(h *harness)
	}{
		{"an empty journal", func(*harness) {}},
		{"only finished, blocked, and queue-paused jobs", func(h *harness) {
			h.seed("f", 1, cleanup.PhaseDone)
			h.seed("bl", 2, cleanup.PhaseMoved, func(job *cleanup.Job) { job.Block(job.Created, "git", "worktree move failed") })
			h.seed("ph", 3, cleanup.PhaseUnregistered)
			h.deleter.put("ph", &payload{entries: 100})
			if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				ctx := context.Background()
				tt.seed(h)
				h.start()
				h.expectEvents()
				if _, err := h.engine.Retry(ctx, "0000000000000000-000000"); !errors.Is(err, cleanup.ErrUnknownJob) {
					t.Errorf("Retry(unknown) = %v, want ErrUnknownJob", err)
				}
				h.clock.Advance(24 * time.Hour)
				h.expectEvents()
				h.expectTimers()
			})
		})
	}
}

func TestJournalFailureStopsTheWorker(t *testing.T) {
	loseRecord := func(h *harness, id string) {
		if err := os.RemoveAll(h.layout.JobDir(id)); err != nil {
			h.t.Errorf("remove the record of %s: %v", id, err)
		}
	}
	tests := []struct {
		name    string
		arrange func(h *harness)
		provoke func(h *harness, id string)
		want    []string
	}{
		{
			"the engine's own record of the deleting phase",
			func(h *harness) { h.cpu.set(0, nil) },
			func(h *harness, id string) {
				synctest.Wait()
				loseRecord(h, id)
				h.clock.Advance(5 * time.Second)
			},
			[]string{"sample", "sample"},
		},
		{
			"an admission that cannot journal its verdict",
			func(h *harness) {
				h.relocator.script("admit:a", func(job *cleanup.Job) {
					loseRecord(h, job.ID)
					job.Block(h.clock.Now(), "activity", "zsh (pid 7) working in the payload")
				})
			},
			func(*harness, string) {},
			[]string{"sample", "admit:a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				job := h.seed("a", 1, cleanup.PhaseUnregistered)
				h.deleter.put("a", &payload{entries: 100})
				tt.arrange(h)
				engine := h.build()
				running := make(chan error, 1)
				go func() { running <- engine.Run(context.Background()) }()
				tt.provoke(h, job.ID)
				if err := <-running; !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("Run() = %v, want the journal's write failure", err)
				}
				h.expectEvents(tt.want...)
				if err := engine.Pause(context.Background()); !errors.Is(err, ErrStopped) {
					t.Errorf("Pause() after the worker died = %v, want ErrStopped", err)
				}
				if err := engine.Stop(context.Background()); err != nil {
					t.Errorf("Stop() after the worker died = %v, want nil", err)
				}
			})
		})
	}
}

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	h := newHarness(t, paced())
	complete := Config{Journal: h.journal, Relocator: h.relocator, Deleter: h.deleter, CPU: h.cpu, Band: h.band, Parent: h.parent.lookup, Identify: identify}
	tests := []struct {
		name   string
		adjust func(cfg *Config)
	}{
		{"no journal", func(cfg *Config) { cfg.Journal = nil }},
		{"no relocator", func(cfg *Config) { cfg.Relocator = nil }},
		{"no deleter", func(cfg *Config) { cfg.Deleter = nil }},
		{"no sampler", func(cfg *Config) { cfg.CPU = nil }},
		{"no band", func(cfg *Config) { cfg.Band = nil }},
		{"no parent lookup", func(cfg *Config) { cfg.Parent = nil }},
		{"no identity lookup", func(cfg *Config) { cfg.Identify = nil }},
		{"a partial tuning", func(cfg *Config) { cfg.Tuning = Tuning{Rate: 250} }},
		{"a throttle that resumes above where it pauses", func(cfg *Config) {
			cfg.Tuning = paced()
			cfg.Tuning.ResumeBelow = 80
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := complete
			tt.adjust(&cfg)
			if engine, err := New(cfg); err == nil {
				t.Errorf("New() = %v, nil; want an error", engine)
			}
		})
	}
	engine, err := New(complete)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if engine.tuning != DefaultTuning() {
		t.Errorf("zero tuning resolved to %+v, want DefaultTuning", engine.tuning)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Run = %v", err)
	}
	if err := engine.Run(context.Background()); err != nil {
		t.Errorf("Run() after Stop = %v, want nil at once", err)
	}
}

func TestTransientBlockageRetriesWithBackoffUpToTheCeiling(t *testing.T) {
	tuning := paced()
	tuning.RetryCeiling = time.Minute
	bubble(t, tuning, func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhasePrepared)
		h.relocator.script("a", blockWith(h.clock, "activity", "could not verify that the tree is idle: read its arguments: input/output error"))
		h.start()
		h.expectEvents("advance:a@prepared")
		h.expectTimers(30 * time.Second)

		h.clock.Advance(29 * time.Second)
		h.expectEvents()
		h.clock.Advance(time.Second)
		h.expectEvents("advance:a@prepared")
		h.expectTimers(time.Minute)
		if blocked := h.status(job.ID); blocked.State() != cleanup.StateBlocked || len(blocked.Errors) != 2 {
			t.Fatalf("after one retry = state %s with %d errors, want blocked again with both attempts noted", blocked.State(), len(blocked.Errors))
		}

		h.clock.Advance(time.Minute)
		h.expectEvents("advance:a@prepared")
		h.expectTimers(time.Minute)

		h.relocator.script("a", nil)
		h.clock.Advance(time.Minute)
		h.expectEvents("advance:a@prepared", "sample", "admit:a", "open:a")
		h.expectTimers()
		if got := h.status(job.ID).Phase; got != cleanup.PhaseDone {
			t.Errorf("job phase = %s, want done once the inspection cleared", got)
		}
	})
}

func TestTransientBlockageOnAPhysicalPhaseRetriesThroughAdmission(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhaseDeleting, func(job *cleanup.Job) {
			job.Block(h.clock.Now(), "activity", "could not verify that the payload is idle: native: inspect pid 81401 (bash): read its arguments: input/output error")
		})
		h.deleter.put("a", &payload{entries: 100})
		h.start()
		h.expectEvents()
		h.expectTimers(30 * time.Second)

		h.clock.Advance(30 * time.Second)
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		if got := h.status(job.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("job = phase %s, removed %d; want done, 100 once admission passed again", got.Phase, got.Removed)
		}
	})
}

func TestTransientBlockageWaitsForAnOperatorPastTheRetryLimit(t *testing.T) {
	tuning := paced()
	tuning.RetryLimit = 2
	bubble(t, tuning, func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhasePrepared)
		h.relocator.script("a", blockWith(h.clock, "activity", "could not verify that the tree is idle: read its arguments: input/output error"))
		h.start()
		h.expectEvents("advance:a@prepared")
		h.expectTimers(30 * time.Second)
		if at := h.status(job.ID).Blocked.RetryAt; at == nil || !at.Equal(h.clock.Now().Add(30*time.Second)) {
			t.Fatalf("reported retry = %v, want %s", at, h.clock.Now().Add(30*time.Second))
		}

		h.clock.Advance(30 * time.Second)
		h.expectEvents("advance:a@prepared")
		h.expectTimers(time.Minute)

		h.clock.Advance(time.Minute)
		h.expectEvents("advance:a@prepared")
		h.expectTimers()
		blocked := h.status(job.ID)
		if blocked.State() != cleanup.StateBlocked || blocked.Blocked.RetryAt != nil {
			t.Fatalf("after %d transient blocks = state %s, retry %v; want blocked for an operator", len(blocked.Errors), blocked.State(), blocked.Blocked.RetryAt)
		}
	})
}

func TestWaitFollowsATransientBlockageThroughItsRetry(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhasePrepared)
		h.relocator.script("a", blockWith(h.clock, "activity", "could not verify that the tree is idle: read its arguments: input/output error"))
		h.start()
		h.expectEvents("advance:a@prepared")

		type outcome struct {
			job cleanup.Job
			err error
		}
		waited := make(chan outcome, 1)
		go func() {
			job, err := h.engine.Wait(context.Background(), job.ID)
			waited <- outcome{job, err}
		}()
		synctest.Wait()
		select {
		case got := <-waited:
			t.Fatalf("Wait() returned %v during a self-retrying blockage, want it to keep waiting", got.err)
		default:
		}

		h.relocator.script("a", nil)
		h.clock.Advance(30 * time.Second)
		h.expectEvents("advance:a@prepared", "sample", "admit:a", "open:a")
		got := <-waited
		if got.err != nil || got.job.Phase != cleanup.PhaseDone {
			t.Errorf("Wait() = phase %s, err %v; want done once the retry cleared the blockage", got.job.Phase, got.err)
		}
	})
}
