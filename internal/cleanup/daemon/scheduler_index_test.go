package daemon

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestSchedulerIndexesExcludeParkedJobs(t *testing.T) {
	for _, n := range []int{32, 4096, 65536} {
		t.Run(fmt.Sprintf("N%d", n), func(t *testing.T) {
			e, now := schedulerFixture(n)
			if len(e.retirementOrder) != 4 || len(e.wakeOrder) != 4 {
				t.Fatalf("indexes for %d records = retirement %d, wake %d; want 4 each", n, len(e.retirementOrder), len(e.wakeOrder))
			}
			if _, ok := e.logicalDue(now); ok {
				t.Fatal("checked waiting job ran early")
			}
			if job, ok := e.logicalDue(now.Add(e.tuning.Recheck)); !ok || job.ID != fmt.Sprintf("job-%d", n-3) {
				t.Fatalf("due job = %v, %v; want seq %d", job, ok, n-3)
			}
		})
	}
}

func TestSchedulerIndexesMatchFullScanAcrossTransitions(t *testing.T) {
	phases := []cleanup.Phase{cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePending, cleanup.PhasePrepared, cleanup.PhaseMoved, cleanup.PhaseDetached, cleanup.PhaseUnregistered, cleanup.PhaseDeleting, cleanup.PhaseDeletingAdmin, cleanup.PhaseDeletingRetiredPayload, cleanup.PhaseDone}
	for _, n := range []int{32, 4096, 65536} {
		t.Run(fmt.Sprintf("N%d", n), func(t *testing.T) {
			e, now := schedulerFixture(n)
			random := rand.New(rand.NewPCG(19, 71))
			for step := range 96 {
				id := fmt.Sprintf("job-%d", 1+random.IntN(n))
				job := e.jobs[id]
				job.Phase = phases[random.IntN(len(phases))]
				job.Adopted = random.IntN(2) == 0
				job.Retirement = []cleanup.Retirement{"", cleanup.RetirementIntent, cleanup.RetirementComplete}[random.IntN(3)]
				job.Blocked, job.Errors = nil, nil
				if random.IntN(3) != 0 {
					reason := []string{"activity", "watchers", "timeout", "identity", "delete"}[random.IntN(5)]
					job.Blocked = &cleanup.Blockage{Reason: reason, At: now.Add(-time.Duration(random.IntN(120)) * time.Second)}
					for range random.IntN(e.tuning.RetryLimit + 3) {
						job.Errors = append(job.Errors, cleanup.JobError{At: now, Message: reason + ": refused"})
					}
				}
				e.put(job)
				e.checked[id] = now.Add(-time.Duration(random.IntN(120)) * time.Second)
				if step%13 == 0 {
					delete(e.checked, id)
				}
				assertSchedulerIndexes(t, e)
				for _, paused := range []bool{false, true} {
					e.paused = paused
					for _, held := range []bool{false, true} {
						e.hold = nil
						if held {
							e.hold = &cleanup.Hold{ID: "hold"}
						}
						for _, offset := range []time.Duration{0, time.Minute, 20 * time.Minute} {
							at := now.Add(offset)
							got, found := e.logicalDue(at)
							want, wanted := scanLogicalDue(e, at)
							if found != wanted || !reflect.DeepEqual(got, want) {
								t.Fatalf("step %d paused=%v held=%v at=%s: logical got %v/%v, want %v/%v", step, paused, held, at, got, found, want, wanted)
							}
						}
						got, timed := e.wake(false)
						want, wanted := scanWake(e)
						if timed != wanted || !got.Equal(want) {
							t.Fatalf("step %d paused=%v held=%v: wake got %v/%v, want %v/%v", step, paused, held, got, timed, want, wanted)
						}
					}
				}
			}
		})
	}
}

func TestSchedulerIndexesLoadAndDequeue(t *testing.T) {
	h := newHarness(t, paced())
	for i, seq := range []uint64{9, 2, 5, 5} {
		h.clock.Advance(time.Nanosecond)
		h.seed(fmt.Sprintf("t%d-%d", seq, i), seq, cleanup.PhaseWaiting)
	}
	e := h.build()
	if err := e.load(); err != nil {
		t.Fatal(err)
	}
	assertSchedulerIndexes(t, e)
	if len(e.logicalOrder) != 4 || len(e.wakeOrder) != 4 {
		t.Fatalf("loaded indexes = %v, %v", e.logicalOrder, e.wakeOrder)
	}
	for _, key := range slices.Clone(e.logicalOrder) {
		job := e.jobs[key.id]
		job.Blocked = &cleanup.Blockage{Reason: "identity"}
		e.put(job)
		assertSchedulerIndexes(t, e)
		job.Blocked = nil
		e.put(job)
		assertSchedulerIndexes(t, e)
		got, ok := e.logicalDue(e.clock.Now())
		want, wanted := scanLogicalDue(e, e.clock.Now())
		if ok != wanted || got.ID != want.ID {
			t.Fatalf("tied priority got %s/%v, want %s/%v", got.ID, ok, want.ID, wanted)
		}
	}
	job := e.jobs[e.logicalOrder[0].id]
	delete(e.jobs, job.ID)
	e.dequeue(job)
	assertSchedulerIndexes(t, e)
}

func assertSchedulerIndexes(t *testing.T, e *Engine) {
	t.Helper()
	var logical, retirement, wake []seqKey
	for _, key := range e.order {
		job := e.jobs[key.id]
		_, retries := e.retryAt(job)
		if job.Phase.Logical() && (job.Blocked == nil || retries) {
			logical = append(logical, key)
			if e.retirementDue(job) {
				retirement = append(retirement, key)
			}
		}
		if (job.Phase == cleanup.PhaseWaiting && job.Blocked == nil) || retries {
			wake = append(wake, key)
		}
	}
	if !slices.Equal(e.logicalOrder, logical) || !slices.Equal(e.retirementOrder, retirement) || !slices.Equal(e.wakeOrder, wake) {
		t.Fatalf("indexes logical=%v want=%v, retirement=%v want=%v, wake=%v want=%v", e.logicalOrder, logical, e.retirementOrder, retirement, e.wakeOrder, wake)
	}
}

func scanLogicalDue(e *Engine, now time.Time) (cleanup.Job, bool) {
	if e.hold != nil {
		return cleanup.Job{}, false
	}
	for job := range e.ordered() {
		if !job.Phase.Logical() || (e.paused && !e.retirementDue(job)) {
			continue
		}
		if job.Blocked != nil {
			if at, ok := e.retryAt(job); ok && !now.Before(at) {
				return clone(job), true
			}
			continue
		}
		if at, ok := e.checked[job.ID]; ok && job.Phase == cleanup.PhaseWaiting && now.Before(at.Add(e.tuning.Recheck)) {
			continue
		}
		return clone(job), true
	}
	return cleanup.Job{}, false
}

func scanWake(e *Engine) (time.Time, bool) {
	var wake time.Time
	timed := false
	consider := func(at time.Time) {
		if !timed || at.Before(wake) {
			wake, timed = at, true
		}
	}
	for job := range e.ordered() {
		if e.hold != nil {
			break
		}
		if e.paused && !e.retirementDue(job) {
			continue
		}
		if job.Phase == cleanup.PhaseWaiting && job.Blocked == nil {
			consider(e.checked[job.ID].Add(e.tuning.Recheck))
		}
		if at, ok := e.retryAt(job); ok {
			consider(at)
		}
	}
	return wake, timed
}
