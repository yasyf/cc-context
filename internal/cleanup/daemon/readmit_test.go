package daemon

import (
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestActiveDeletionIsReadmittedEveryReadmit(t *testing.T) {
	tuning := paced()
	tuning.Readmit = time.Second
	bubble(t, tuning, func(t *testing.T, h *harness) {
		job := h.seed("a", 1, cleanup.PhaseUnregistered)
		h.deleter.put("a", &payload{entries: 1000})
		h.start()
		h.expectEvents("sample", "admit:a", "open:a", "step:a")
		for _, want := range [][]string{{"step:a"}, {"step:a"}, {"readmit:a", "step:a"}, {"step:a"}, {"step:a"}} {
			h.clock.Advance(400 * time.Millisecond)
			h.expectEvents(want...)
		}

		h.relocator.script("readmit:a", blockWith(h.clock, "activity", "held by pid 7"))
		h.clock.Advance(400 * time.Millisecond)
		h.expectEvents("readmit:a", "close:a")
		got := h.status(job.ID)
		if got.Blocked == nil || got.Blocked.Reason != "activity" || got.Removed != 600 {
			t.Errorf("job a = blocked %+v, removed %d; want blocked on activity after 600", got.Blocked, got.Removed)
		}
	})
}
