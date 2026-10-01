package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestBlockedAdmissionSkipsTheJobAndTheNextProceeds(t *testing.T) {
	bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
		held := h.seed("a", 1, cleanup.PhaseUnregistered)
		next := h.seed("b", 2, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 100})
		h.deleter.put("b", &payload{entries: 100})
		h.relocator.script("admit:a", blockWith(h.clock, "reconcile", "a tree sits at the registered path"))
		h.start()
		h.expectEvents("sample", "admit:a", "admit:b", "open:b", "step:b")
		h.expectTimers()

		wantBlockage := cleanup.Blockage{Reason: "reconcile", Detail: "a tree sits at the registered path", At: h.clock.Now()}
		for source, job := range map[string]cleanup.Job{"engine": h.status(held.ID), "journal": h.journaled(held.ID)} {
			if job.Phase != cleanup.PhaseDeleting || job.Blocked == nil || *job.Blocked != wantBlockage || job.Removed != 0 {
				t.Errorf("%s job a = phase %s, blockage %+v, removed %d; want deleting, %+v, 0", source, job.Phase, job.Blocked, job.Removed, wantBlockage)
			}
		}
		if got := h.deleter.wants("a"); len(got) != 0 {
			t.Errorf("the payload of a blocked admission was opened %d times, want never", len(got))
		}
		if got := h.status(next.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("the next job = phase %s, removed %d; want done, 100", got.Phase, got.Removed)
		}

		h.relocator.script("admit:a", nil)
		if _, err := h.engine.Retry(context.Background(), held.ID); err != nil {
			t.Fatalf("Retry() = %v", err)
		}
		h.expectEvents("sample")
		h.expectTimers(400 * time.Millisecond)
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("admit:a", "open:a", "step:a")
		if got := h.status(held.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("the retried job = phase %s, removed %d; want done, 100", got.Phase, got.Removed)
		}
	})
}

func TestResumeAdmitsThePayloadAfreshAndHonorsABlock(t *testing.T) {
	bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 300})
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")

		if err := h.engine.Pause(ctx); err != nil {
			t.Fatalf("Pause() = %v", err)
		}
		h.expectEvents("close:a")
		h.clock.Advance(time.Second)
		h.relocator.script("admit:a", blockWith(h.clock, "reconcile", "a tree sits at the registered path"))
		if err := h.engine.Resume(ctx); err != nil {
			t.Fatalf("Resume() = %v", err)
		}
		h.expectEvents("sample", "admit:a")
		h.clock.Advance(time.Hour)
		h.expectEvents()

		wantBlockage := cleanup.Blockage{Reason: "reconcile", Detail: "a tree sits at the registered path", At: h.clock.Now().Add(-time.Hour)}
		got := h.journaled(job.ID)
		if got.Phase != cleanup.PhaseDeleting || got.Blocked == nil || *got.Blocked != wantBlockage || got.Removed != 100 {
			t.Errorf("journal after the resume = phase %s, blockage %+v, removed %d; want deleting, %+v, 100", got.Phase, got.Blocked, got.Removed, wantBlockage)
		}
		if opened := h.deleter.wants("a"); !slices.Equal(opened, []cleanup.FileID{job.Tree}) {
			t.Errorf("payload opens = %v, want the one before the pause and none after the blocked admission", opened)
		}
	})
}

func TestCancelledAdmitStopsTheWorkerAndLeavesTheJobRunnable(t *testing.T) {
	bubble(t, DefaultTuning(), func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 100})
		gate := make(chan struct{})
		h.relocator.gates["admit:a"] = gate
		engine := h.build()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		running := make(chan error, 1)
		go func() { running <- engine.Run(ctx) }()
		if got := <-h.relocator.entered; got != "admit:a" {
			t.Fatalf("the relocator entered %q, want admit:a", got)
		}
		cancel()
		gate <- struct{}{}
		if err := <-running; err != nil {
			t.Fatalf("Run() = %v, want nil: a cancelled admission is a stop, not a journal failure", err)
		}
		h.expectEvents("sample", "admit:a")
		for source, got := range map[string]cleanup.Job{"engine": h.status(job.ID), "journal": h.journaled(job.ID)} {
			if got.Phase != cleanup.PhaseDeleting || got.Blocked != nil || len(got.Errors) != 0 || got.Removed != 0 {
				t.Errorf("%s job = phase %s, blockage %+v, errors %v, removed %d; want deleting, unblocked, no history, 0", source, got.Phase, got.Blocked, got.Errors, got.Removed)
			}
		}
		if opened := h.deleter.wants("a"); len(opened) != 0 {
			t.Errorf("the payload was opened %d times behind a cancelled admission, want never", len(opened))
		}

		h.relocator.mu.Lock()
		delete(h.relocator.gates, "admit:a")
		h.relocator.mu.Unlock()
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		if got := h.status(job.ID); got.Phase != cleanup.PhaseDone || got.Removed != 100 {
			t.Errorf("the next engine left the job at %s with %d removed, want done, 100", got.Phase, got.Removed)
		}
	})
}
