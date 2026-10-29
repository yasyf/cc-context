package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestOrdinaryPauseRetiresRegistrationsOnceWithoutPhysicalDeletion(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		queued := h.seed("q", 1, cleanup.PhaseQueued)
		intent := h.seed("i", 2, cleanup.PhasePrepared, func(job *cleanup.Job) { job.Retirement = cleanup.RetirementIntent })
		complete := h.seed("c", 3, cleanup.PhasePrepared, func(job *cleanup.Job) { job.Retirement = cleanup.RetirementComplete })
		physical := h.seed("p", 4, cleanup.PhaseUnregistered)
		h.deleter.put("p", &payload{entries: 100})
		if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
			t.Fatal(err)
		}
		h.start()
		h.expectEvents("retire:q@queued", "retire:i@prepared")
		h.expectTimers()
		for _, job := range []cleanup.Job{queued, intent, complete} {
			if got := h.status(job.ID); got.Retirement != cleanup.RetirementComplete || got.Phase != job.Phase {
				t.Fatalf("job %s = %+v; want its phase retained and registration retired", job.ID, got)
			}
		}
		if got := h.status(physical.ID); got.Removed != 0 || got.Phase != cleanup.PhaseUnregistered {
			t.Fatalf("physical job = %+v; want untouched", got)
		}
		h.clock.Advance(time.Hour)
		h.expectEvents()
		h.expectTimers()
		if err := h.engine.Pause(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.expectEvents()
		h.expectTimers()
	})
}

func TestMaintenanceHoldExcludesRetirementUntilReleasedIntoPause(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		job := h.seed("q", 1, cleanup.PhaseQueued)
		hold := cleanup.Hold{ID: "repair", Reason: "registration repair", Since: h.clock.Now()}
		if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true, Hold: &hold}); err != nil {
			t.Fatal(err)
		}
		h.start()
		h.expectEvents()
		h.expectTimers()
		if got := h.status(job.ID); got.Retirement != "" {
			t.Fatalf("held job = %+v; want no retirement", got)
		}
		if err := h.engine.Release(context.Background(), hold.ID); err != nil {
			t.Fatal(err)
		}
		h.expectEvents("retire:q@queued")
		h.expectTimers()
		if got := h.status(job.ID); got.Retirement != cleanup.RetirementComplete {
			t.Fatalf("released job = %+v; want retired", got)
		}
		if state, err := h.engine.HoldState(context.Background()); err != nil || !state.Paused || state.Hold != nil {
			t.Fatalf("hold state = %+v, %v; want ordinary pause", state, err)
		}
	})
}
