package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func schedulerFixture(n int) (*Engine, time.Time) {
	now := time.Unix(10000, 0)
	e := &Engine{
		jobs: make(map[string]cleanup.Job, n), checked: make(map[string]time.Time),
		changed: make(chan struct{}), tuning: DefaultTuning(), booted: now,
		paused: true,
	}
	for seq := 1; seq <= n; seq++ {
		job := cleanup.Job{ID: fmt.Sprintf("job-%d", seq), Seq: uint64(seq), Phase: cleanup.PhaseUnregistered}
		switch seq % 5 {
		case 0:
			job.Phase = cleanup.PhaseDone
		case 2:
			job.Phase, job.Retirement = cleanup.PhasePending, cleanup.RetirementComplete
		case 3:
			job.Phase, job.Adopted = cleanup.PhaseQueued, true
		case 1:
			job.Phase = cleanup.PhaseQueued
			job.Blocked = &cleanup.Blockage{Reason: "identity", At: now}
		}
		if seq > n-4 {
			job.Phase, job.Blocked, job.Retirement, job.Adopted = cleanup.PhaseWaiting, nil, "", false
			e.checked[job.ID] = now
		}
		e.put(job)
	}
	return e, now
}

func BenchmarkSchedulerParkedQueue(b *testing.B) {
	for _, n := range []int{32, 4096, 65536} {
		b.Run(fmt.Sprintf("N%d", n), func(b *testing.B) {
			e, now := schedulerFixture(n)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if job, ok := e.logicalDue(now); ok {
					b.Fatalf("unexpected due job %s", job.ID)
				}
				if at, ok := e.wake(false); !ok || !at.Equal(now.Add(e.tuning.Recheck)) {
					b.Fatalf("wake = %s, %v", at, ok)
				}
			}
		})
	}
}
