package daemon

import (
	"context"
	"slices"
	"testing/synctest"
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

func TestPausedRetirementUsesDefaultBandAndRestoresBackground(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "retired"
		if blocked {
			name = "blocked"
		}
		t.Run(name, func(t *testing.T) {
			bubble(t, paced(), func(t *testing.T, h *harness) {
				job := h.seed("q", 1, cleanup.PhaseQueued)
				h.relocator.scripts["retire:q"] = func(job *cleanup.Job) {
					if !h.band.inForeground() {
						t.Error("paused retirement ran in the background band")
					}
					if blocked {
						job.Block(h.clock.Now(), "activity", "active session")
					} else {
						job.Retirement = cleanup.RetirementComplete
					}
				}
				if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
					t.Fatal(err)
				}
				h.start()
				h.expectEvents("retire:q@queued")
				if h.band.inForeground() {
					t.Error("retirement left the worker in the default band")
				}
				if got, want := h.band.takeSwitches(), []string{"foreground", "background"}; !slices.Equal(got, want) {
					t.Errorf("band switches = %v, want %v", got, want)
				}
				got := h.status(job.ID)
				if got.Phase != cleanup.PhaseQueued || (got.Blocked != nil) != blocked {
					t.Errorf("retired job = %+v, want queued with blocked=%t", got, blocked)
				}
				if state, err := h.engine.HoldState(context.Background()); err != nil || !state.Paused {
					t.Errorf("hold state = %+v, %v; want still paused", state, err)
				}
			})
		})
	}
}

func TestResumeAfterPausedRetirementDeletesInBackground(t *testing.T) {
	bubble(t, paced(), func(t *testing.T, h *harness) {
		h.seed("q", 1, cleanup.PhaseQueued)
		h.seed("p", 2, cleanup.PhaseUnregistered)
		h.relocator.scripts["retire:q"] = blockWith(h.clock, "activity", "active session")
		gate, entered := make(chan struct{}), make(chan struct{}, 1)
		h.deleter.put("p", &payload{entries: 200, gate: gate, entered: entered})
		if err := h.journal.SaveSwitch(cleanup.Switch{Paused: true}); err != nil {
			t.Fatal(err)
		}
		h.start()
		h.expectEvents("retire:q@queued")
		if got, want := h.band.takeSwitches(), []string{"foreground", "background"}; !slices.Equal(got, want) {
			t.Errorf("retirement switches = %v, want %v", got, want)
		}
		if err := h.engine.Resume(context.Background()); err != nil {
			t.Fatal(err)
		}
		<-entered
		if h.band.inForeground() {
			t.Error("physical deletion after retirement ran in the default band")
		}
		gate <- struct{}{}
		synctest.Wait()
		h.expectEvents("sample", "admit:p", "open:p", "step:p")
		if got, want := h.band.takeSwitches(), []string{"foreground", "background"}; !slices.Equal(got, want) {
			t.Errorf("resume switches = %v, want %v", got, want)
		}
	})
}
