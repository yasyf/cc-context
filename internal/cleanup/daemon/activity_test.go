package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func gated() Tuning {
	t := paced()
	t.ActivityEvery, t.ProbeSettle = 2*time.Second, 500*time.Millisecond
	return t
}

var (
	busySessions  = cleanup.Activity{Sessions: []float64{50, 40, 30, 20}, Create: time.Millisecond}
	quietSessions = cleanup.Activity{Sessions: []float64{1, 2}, Create: time.Millisecond}
)

func withActivity(h *harness, activity cleanup.Activity) {
	h.activity = &fakeActivity{h: h}
	h.activity.set(activity, nil)
}

func (h *harness) expectGovernor(want cleanup.Governor) {
	h.t.Helper()
	report, err := h.engine.Status(context.Background(), cleanup.Query{})
	if err != nil {
		h.t.Fatalf("Status() = %v", err)
	}
	if want.State == governorHeld {
		want.Gate = want.Detail
	}
	if report.Governor != want {
		h.t.Errorf("governor = %+v, want %+v", report.Governor, want)
	}
	if want.State == governorHeld && report.Pace != (cleanup.Pace{}) {
		h.t.Errorf("pace = %+v while held, want none", report.Pace)
	}
}

func TestGate(t *testing.T) {
	errPriming := errors.New("first sample primes")
	type step struct {
		activity cleanup.Activity
		err      error
		held     bool
		detail   string
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"unsampled holds", nil},
		{"priming holds, two quiet samples open", []step{
			{err: errPriming, held: true, detail: "first sample primes"},
			{activity: quietSessions, held: true, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"},
			{activity: quietSessions, held: false, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms)"},
		}},
		{"three busy sessions are quiet, four hold", []step{
			{activity: cleanup.Activity{Sessions: []float64{11, 12, 13, 10}, Create: time.Millisecond}, held: true, detail: "3 of 4 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"},
			{activity: cleanup.Activity{Sessions: []float64{11, 12, 13, 10}, Create: time.Millisecond}, held: false, detail: "3 of 4 sessions busy (at most 3), creates take 1ms (at most 3ms)"},
			{activity: busySessions, held: true, detail: "4 of 4 sessions busy (at most 3), creates take 1ms (at most 3ms)"},
		}},
		{"a slow create holds and resets the quiet streak", []step{
			{activity: quietSessions, held: true, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"},
			{activity: cleanup.Activity{Sessions: []float64{1}, Create: 3010 * time.Microsecond}, held: true, detail: "0 of 1 sessions busy (at most 3), creates take 3.01ms (at most 3ms)"},
			{activity: quietSessions, held: true, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"},
		}},
		{"a failed sample holds an open gate", []step{
			{activity: quietSessions, held: true, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"},
			{activity: quietSessions, held: false, detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms)"},
			{err: errPriming, held: true, detail: "first sample primes"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGate(DefaultTuning())
			if !g.held || g.detail != detailUnsampled {
				t.Fatalf("new gate held %t, detail %q; want held, %q", g.held, g.detail, detailUnsampled)
			}
			at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			for i, s := range tt.steps {
				at = at.Add(15 * time.Second)
				g.observe(at, s.activity, s.err)
				if g.held != s.held || g.detail != s.detail {
					t.Errorf("sample %d: held %t, detail %q; want held %t, %q", i, g.held, g.detail, s.held, s.detail)
				}
			}
		})
	}
}

func TestBusySessionsStopDeletionUntilTwoQuietSamples(t *testing.T) {
	bubble(t, gated(), func(t *testing.T, h *harness) {
		withActivity(h, busySessions)
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 1000})
		h.start()
		h.expectEvents("activity")
		h.expectTimers(2 * time.Second)
		h.expectGovernor(cleanup.Governor{State: governorHeld, Detail: "4 of 4 sessions busy (at most 3), creates take 1ms (at most 3ms)"})

		h.activity.set(quietSessions, nil)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("activity")
		h.expectTimers(2 * time.Second)
		h.expectGovernor(cleanup.Governor{State: governorHeld, Detail: "0 of 2 sessions busy (at most 3), creates take 1ms (at most 3ms), quiet 1 of 2 samples"})

		h.clock.Advance(2 * time.Second)
		h.expectEvents("activity", "sample", "admit:a", "open:a", "step:a")
		h.expectTimers(400 * time.Millisecond)

		h.activity.set(busySessions, nil)
		for range 4 {
			h.clock.Advance(400 * time.Millisecond)
			h.expectEvents("step:a")
			h.expectTimers(400 * time.Millisecond)
		}
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents()
		h.expectTimers(100 * time.Millisecond)
		h.clock.Advance(100 * time.Millisecond)
		h.expectEvents("activity", "close:a")
		h.expectTimers(2 * time.Second)
		h.expectGovernor(cleanup.Governor{State: governorHeld, Detail: "4 of 4 sessions busy (at most 3), creates take 1ms (at most 3ms)"})
		if got := h.status(job.ID); got.Phase != cleanup.PhaseDeleting || got.Removed != 500 {
			t.Errorf("job a = phase %s, removed %d; want deleting, 500", got.Phase, got.Removed)
		}
	})
}

func TestBusySessionsHoldLogicalWork(t *testing.T) {
	bubble(t, gated(), func(_ *testing.T, h *harness) {
		withActivity(h, busySessions)
		h.seed("a", 1, cleanup.PhasePrepared)
		h.deleter.put("a", &payload{entries: 50})
		h.start()
		h.expectEvents("activity")
		h.activity.set(quietSessions, nil)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("activity")
		h.clock.Advance(2 * time.Second)
		h.expectEvents("activity", "advance:a@prepared", "sample", "admit:a", "open:a", "step:a")
	})
}

func TestHeldGateWaitsForTheNextSampleNotADueRetry(t *testing.T) {
	bubble(t, gated(), func(_ *testing.T, h *harness) {
		withActivity(h, busySessions)
		h.seed("a", 1, cleanup.PhaseUnregistered, func(job *cleanup.Job) {
			job.Block(h.clock.Now().Add(-time.Minute), "activity", "held by pid 7")
		})
		h.deleter.put("a", &payload{entries: 50})
		h.start()
		h.expectEvents("activity")
		h.expectTimers(2 * time.Second)
		h.clock.Advance(2 * time.Second)
		h.expectEvents("activity")
		h.expectTimers(2 * time.Second)
	})
}
