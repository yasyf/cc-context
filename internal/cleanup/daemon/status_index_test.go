package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func legacyStatusAtFaae356(e *Engine, ctx context.Context, q cleanup.Query) (cleanup.Report, error) {
	if err := e.ready(ctx); err != nil {
		return cleanup.Report{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	report := cleanup.Report{Version: e.version, PID: os.Getpid(), Paused: e.halted(), Governor: e.governed, Jobs: []cleanup.Job{}, Damaged: slices.Clone(e.damaged)}
	if q.JobID != "" {
		job, ok := e.jobs[q.JobID]
		if !ok {
			return cleanup.Report{}, cleanup.ErrUnknownJob
		}
		if q.Await && job.Phase != cleanup.PhaseDone {
			e.awaits[q.JobID] = e.clock.Now()
		}
		report.Jobs = append(report.Jobs, e.reported(job))
		return report, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = e.tuning.StatusLimit
	}
	var finished []cleanup.Job
	for job := range e.ordered() {
		if job.Phase == cleanup.PhaseDone {
			finished = append(finished, job)
			continue
		}
		report.Jobs = append(report.Jobs, e.reported(job))
		if len(report.Jobs) == limit {
			report.Omitted = len(e.jobs) - len(report.Jobs)
			return report, nil
		}
	}
	slices.SortFunc(finished, func(a, b cleanup.Job) int { return legacyFinishAtFaae356(b, a) })
	for _, job := range finished[:min(len(finished), limit-len(report.Jobs))] {
		report.Jobs = append(report.Jobs, clone(job))
	}
	report.Omitted = len(e.jobs) - len(report.Jobs)
	return report, nil
}

func legacyFinishAtFaae356(a, b cleanup.Job) int {
	if order := a.Updated.Compare(b.Updated); order != 0 {
		return order
	}
	return cmp.Compare(a.Seq, b.Seq)
}

func legacyPutAtFaae356(e *Engine, job cleanup.Job) {
	e.mu.Lock()
	if _, known := e.jobs[job.ID]; !known {
		at, _ := slices.BinarySearchFunc(e.order, job.Seq, bySeqKey)
		e.order = slices.Insert(e.order, at, seqKey{seq: job.Seq, id: job.ID})
	}
	e.jobs[job.ID] = clone(job)
	e.schedule(job)
	e.broadcast()
	e.mu.Unlock()
}

func legacyDequeueAtFaae356(e *Engine, job cleanup.Job) {
	at, found := slices.BinarySearchFunc(e.order, job.Seq, bySeqKey)
	if !found || e.order[at].id != job.ID {
		panic(fmt.Sprintf("cleanup daemon: job %s at seq %d is missing from the queue order", job.ID, job.Seq))
	}
	e.order = slices.Delete(e.order, at, at+1)
	e.logicalOrder = scheduleKey(e.logicalOrder, e.order, job, false)
	e.retirementOrder = scheduleKey(e.retirementOrder, e.order, job, false)
	e.wakeOrder = scheduleKey(e.wakeOrder, e.order, job, false)
}

func legacyLoadAtFaae356(e *Engine) error {
	jobs, damaged, err := e.journal.Load()
	if err != nil {
		return err
	}
	sw, err := e.journal.LoadSwitch()
	if err != nil {
		return fmt.Errorf("cleanup daemon: load the pause switch: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, job := range jobs {
		e.jobs[job.ID] = job
		e.order = append(e.order, seqKey{seq: job.Seq, id: job.ID})
		e.seq = max(e.seq, job.Seq)
	}
	slices.SortFunc(e.order, func(a, b seqKey) int { return cmp.Compare(a.seq, b.seq) })
	e.damaged, e.paused, e.hold, e.booted = damaged, sw.Paused, sw.Hold, e.clock.Now()
	for _, key := range e.order {
		logical, retirement, wake := e.scheduled(e.jobs[key.id])
		if logical {
			e.logicalOrder = append(e.logicalOrder, key)
		}
		if retirement {
			e.retirementOrder = append(e.retirementOrder, key)
		}
		if wake {
			e.wakeOrder = append(e.wakeOrder, key)
		}
	}
	close(e.loaded)
	return nil
}

func statusWorkFixture(n int, shape string) *Engine {
	phases := []cleanup.Phase{cleanup.PhaseDone, cleanup.PhasePending, cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePrepared, cleanup.PhaseMoved, cleanup.PhaseDetached, cleanup.PhaseUnregistered, cleanup.PhaseDeleting, cleanup.PhaseDeletingAdmin, cleanup.PhaseDeletingRetiredPayload}
	jobs := make([]cleanup.Job, n)
	seq := uint64(1)
	for i := range jobs {
		job := cleanup.Job{ID: fmt.Sprintf("status-%d", seq), Seq: seq, Phase: cleanup.PhaseDone, Updated: time.Unix(10000+int64(i%17), 0), Owner: "retained-owner", Requester: cleanup.ProcessID{PID: 7, Start: clientStart}, Ignored: []cleanup.Ignored{{PID: 8, Name: "guard", Reason: "outside"}}, Errors: []cleanup.JobError{{At: time.Unix(10000, 0), Message: "activity: retained"}}}
		if shape == "mixed" {
			job.Phase = phases[i%len(phases)]
		}
		if shape == "prefix" && i == n-1 {
			job.Phase = cleanup.PhasePending
		}
		if job.Phase != cleanup.PhaseDone || i%3 == 0 {
			reason := "identity"
			if i%2 == 0 {
				reason = "activity"
			}
			job.Blocked = &cleanup.Blockage{Reason: reason, Detail: "retained detail", At: time.Unix(10000, 0)}
		}
		jobs[i] = job
		seq++
	}
	e := statusFixture(jobs, 7)
	e.version, e.paused = "status-reference", true
	e.governed = cleanup.Governor{State: "throttled", CPUPercent: 17, Detail: "retained governor"}
	e.hold = &cleanup.Hold{ID: "fixture", Reason: "retained hold", Since: time.Unix(9999, 0)}
	return e
}

func assertStatusOrder(t *testing.T, e *Engine) {
	t.Helper()
	var unfinished []seqKey
	var finished []cleanup.Job
	for job := range e.ordered() {
		if job.Phase == cleanup.PhaseDone {
			finished = append(finished, job)
		} else {
			unfinished = append(unfinished, seqKey{seq: job.Seq, id: job.ID})
		}
	}
	slices.SortStableFunc(finished, func(a, b cleanup.Job) int { return legacyFinishAtFaae356(b, a) })
	if !slices.Equal(e.unfinishedOrder, unfinished) || len(e.finishedOrder) != len(finished) {
		t.Fatalf("status indexes = %d unfinished/%d finished, want %d/%d", len(e.unfinishedOrder), len(e.finishedOrder), len(unfinished), len(finished))
	}
	for i, job := range finished {
		key := e.finishedOrder[i]
		if key.id != job.ID || key.seq != job.Seq || !key.updated.Equal(job.Updated) {
			t.Fatalf("finished key %d = %+v, want %s/%d/%s", i, key, job.ID, job.Seq, job.Updated)
		}
	}
}

func assertStatusReference(t *testing.T, e *Engine) {
	t.Helper()
	assertStatusOrder(t, e)
	queries := []cleanup.Query{{}, {Limit: -1}, {Limit: 1}, {Limit: 7}, {Limit: len(e.jobs) + 1}, {JobID: "missing"}, {JobID: "missing", Await: true}}
	if len(e.order) > 0 {
		queries = append(queries, cleanup.Query{JobID: e.order[0].id, Limit: -1, Await: true}, cleanup.Query{JobID: e.order[len(e.order)-1].id, Limit: 1, Await: true})
	}
	for _, query := range queries {
		before, existed := e.awaits[query.JobID]
		got, gotErr := e.Status(context.Background(), query)
		if query.Await {
			job, known := e.jobs[query.JobID]
			at, set := e.awaits[query.JobID]
			if known && job.Phase != cleanup.PhaseDone {
				if !set || !at.Equal(e.clock.Now()) {
					t.Fatal("selected unfinished read did not renew its lease")
				}
			} else if set != existed || !at.Equal(before) {
				t.Fatal("done or unknown read changed its lease")
			}
		}
		want, wantErr := legacyStatusAtFaae356(e, context.Background(), query)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotErr, wantErr) {
			t.Fatalf("Status(%+v) differs from immutable legacy reference: got %+v/%v, want %+v/%v", query, got, gotErr, want, wantErr)
		}
	}
}

func TestStatusIndexesMatchLegacyReports(t *testing.T) {
	for _, shape := range []string{"done", "prefix", "mixed"} {
		for _, n := range []int{0, 32, 7422} {
			t.Run(fmt.Sprintf("%s/N%d", shape, n), func(t *testing.T) {
				e := statusWorkFixture(n, shape)
				e.damaged = []cleanup.Damaged{{ID: "unreadable", Error: "retained read error"}}
				assertStatusReference(t, e)
				if n > 0 {
					report, err := e.Status(context.Background(), cleanup.Query{Limit: n + 1})
					if err != nil {
						t.Fatal(err)
					}
					job := report.Jobs[0]
					before := clone(e.jobs[job.ID])
					job.Errors[0].Message = "mutated report"
					job.Ignored[0].Name = "mutated report"
					if job.Blocked != nil {
						job.Blocked.Detail = "mutated report"
					}
					report.Damaged[0].Error = "mutated report"
					if !reflect.DeepEqual(before, e.jobs[job.ID]) || e.damaged[0].Error != "retained read error" {
						t.Fatal("a returned report mutated engine state")
					}
				}
			})
		}
	}
}

func TestStatusReadinessCancellationMatchesLegacy(t *testing.T) {
	h := newHarness(t, paced())
	e := h.build()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, gotErr := e.Status(ctx, cleanup.Query{Limit: 1})
	want, wantErr := legacyStatusAtFaae356(e, ctx, cleanup.Query{Limit: 1})
	if !errors.Is(gotErr, context.Canceled) || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotErr, wantErr) {
		t.Fatalf("unloaded canceled Status = %+v/%v, want %+v/%v", got, gotErr, want, wantErr)
	}
}

func TestStatusIndexesFollowUpdatesAndRemoval(t *testing.T) {
	e := statusWorkFixture(32, "mixed")
	phases := []cleanup.Phase{cleanup.PhaseDone, cleanup.PhaseWaiting, cleanup.PhasePending, cleanup.PhaseDeleting, cleanup.PhasePrepared}
	for step := range 96 {
		job := e.jobs[fmt.Sprintf("status-%d", 1+step%32)]
		job.Phase = phases[step%len(phases)]
		job.Updated = time.Unix(20000+int64(step%5), 0)
		e.put(job)
		assertStatusReference(t, e)
		if step%7 == 0 {
			delete(e.jobs, job.ID)
			e.dequeue(job)
			assertStatusReference(t, e)
			e.put(job)
			assertStatusReference(t, e)
		}
	}
}

func TestStatusAcceptedEqualFinishKeysUseQueueOrder(t *testing.T) {
	for _, n := range []int{2, 64} {
		t.Run(fmt.Sprintf("N%d", n), func(t *testing.T) {
			h := newHarness(t, paced())
			updated := h.clock.Now().Add(time.Hour)
			seq := uint64(1)
			for i := range n {
				if n == 2 {
					seq = 63
				}
				h.clock.Advance(time.Nanosecond)
				h.seed(fmt.Sprintf("equal-%d", i), seq, cleanup.PhaseDone, func(job *cleanup.Job) { job.Updated = updated })
				if i < n-2 {
					seq++
				}
			}
			e := h.build()
			if err := e.load(); err != nil {
				t.Fatal(err)
			}
			assertStatusOrder(t, e)
			a, b := e.order[n-2], e.order[n-1]
			if legacyFinishAtFaae356(e.jobs[a.id], e.jobs[b.id]) != 0 {
				t.Fatal("the accepted records do not exercise exact finish-key equality")
			}
			report, err := e.Status(context.Background(), cleanup.Query{Limit: 2})
			if err != nil || len(report.Jobs) != 2 || report.Jobs[0].ID != a.id || report.Jobs[1].ID != b.id {
				t.Fatalf("equal-key Status = %+v/%v, want queue order %s/%s", report, err, a.id, b.id)
			}
			legacy, err := legacyStatusAtFaae356(e, context.Background(), cleanup.Query{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("accepted equal keys N%d: legacy first %s; queue-order first %s", n, legacy.Jobs[0].ID, a.id)
			changed := e.jobs[b.id]
			changed.Updated = updated.Add(time.Second)
			e.put(changed)
			assertStatusOrder(t, e)
			changed.Updated = updated
			e.put(changed)
			assertStatusOrder(t, e)
			delete(e.jobs, changed.ID)
			e.dequeue(changed)
			assertStatusOrder(t, e)
		})
	}
}

func TestStatusIndexesSurviveCancelAndPruning(t *testing.T) {
	tuning := paced()
	tuning.KeepDone = 2
	h := newHarness(t, tuning)
	var old, newest cleanup.Job
	for seq := uint64(1); seq <= 6; seq++ {
		job := h.seed(fmt.Sprintf("finished-%d", seq), seq, cleanup.PhaseDone)
		if seq == 1 {
			old = job
		}
		newest = job
	}
	pending := h.seed("pending", 7, cleanup.PhasePending)
	e := h.build()
	if err := e.load(); err != nil {
		t.Fatal(err)
	}
	assertStatusReference(t, e)
	if _, err := e.cancel(context.Background(), pending.ID); err != nil {
		t.Fatal(err)
	}
	assertStatusReference(t, e)
	e.waiters[old.ID] = 1
	h.clock.Advance(time.Minute)
	if err := e.finish(newest); err != nil {
		t.Fatal(err)
	}
	assertStatusReference(t, e)
	if len(e.jobs) != 2 || e.kept[old.ID].ID != old.ID {
		t.Fatalf("pruning retained %d jobs/kept %v, want two jobs and active waiter snapshot", len(e.jobs), e.kept)
	}
	if _, err := e.Status(context.Background(), cleanup.Query{JobID: old.ID}); !errors.Is(err, cleanup.ErrUnknownJob) {
		t.Fatalf("Status(pruned) = %v", err)
	}
}

func TestStatusAllocationsDoNotGrowWithOmittedHistory(t *testing.T) {
	for _, shape := range []string{"done", "prefix"} {
		t.Run(shape, func(t *testing.T) {
			allocations := func(n int, legacy bool) float64 {
				e := statusWorkFixture(n, shape)
				return testing.AllocsPerRun(3, func() {
					var report cleanup.Report
					var err error
					if legacy {
						report, err = legacyStatusAtFaae356(e, context.Background(), cleanup.Query{Limit: 1})
					} else {
						report, err = e.Status(context.Background(), cleanup.Query{Limit: 1})
					}
					if err != nil || len(report.Jobs) != 1 || report.Omitted != n-1 {
						t.Fatalf("Status = %d/%d/%v", len(report.Jobs), report.Omitted, err)
					}
				})
			}
			page, queue := allocations(32, false), allocations(7422, false)
			oldPage, oldQueue := allocations(32, true), allocations(7422, true)
			t.Logf("%s Limit1 allocations: indexed32/7422 %.0f/%.0f; immutable legacy %.0f/%.0f", shape, page, queue, oldPage, oldQueue)
			if queue > page+1 || oldQueue <= oldPage+1 {
				t.Fatalf("allocation bound/baseline witness failed: indexed %.0f/%.0f, legacy %.0f/%.0f", page, queue, oldPage, oldQueue)
			}
		})
	}
}

func BenchmarkStatusRead(b *testing.B) {
	for _, shape := range []string{"done", "prefix", "mixed"} {
		for _, n := range []int{32, 7422, 65536} {
			for _, legacy := range []bool{true, false} {
				b.Run(fmt.Sprintf("%s/N%d/Legacy%v", shape, n, legacy), func(b *testing.B) {
					e := statusWorkFixture(n, shape)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						var err error
						if legacy {
							_, err = legacyStatusAtFaae356(e, context.Background(), cleanup.Query{Limit: 1})
						} else {
							_, err = e.Status(context.Background(), cleanup.Query{Limit: 1})
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkStatusUpdate(b *testing.B) {
	for _, n := range []int{32, 7422, 65536} {
		for _, legacy := range []bool{true, false} {
			b.Run(fmt.Sprintf("N%d/Legacy%v", n, legacy), func(b *testing.B) {
				e := statusWorkFixture(n, "done")
				job := e.jobs[e.order[n/2].id]
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if job.Phase == cleanup.PhaseDone {
						job.Phase = cleanup.PhasePending
					} else {
						job.Phase = cleanup.PhaseDone
					}
					if legacy {
						legacyPutAtFaae356(e, job)
					} else {
						e.put(job)
					}
				}
			})
		}
	}
}

func BenchmarkStatusPrune(b *testing.B) {
	for _, n := range []int{32, 7422, 65536} {
		for _, legacy := range []bool{true, false} {
			b.Run(fmt.Sprintf("N%d/Legacy%v", n, legacy), func(b *testing.B) {
				e := statusWorkFixture(n, "done")
				jobs := make([]cleanup.Job, 0, 32)
				for _, key := range e.order[:32] {
					jobs = append(jobs, e.jobs[key.id])
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					for _, job := range jobs {
						delete(e.jobs, job.ID)
						if legacy {
							legacyDequeueAtFaae356(e, job)
						} else {
							e.dequeue(job)
							if len(e.finishedOrder) != len(e.jobs) {
								b.Fatal("pruned status index count differs")
							}
						}
					}
					b.StopTimer()
					for _, job := range jobs {
						if legacy {
							legacyPutAtFaae356(e, job)
						} else {
							e.put(job)
						}
					}
					b.StartTimer()
				}
			})
		}
	}
}

func BenchmarkStatusLoad(b *testing.B) {
	for _, n := range []int{32, 7422} {
		for _, legacy := range []bool{true, false} {
			b.Run(fmt.Sprintf("N%d/Legacy%v", n, legacy), func(b *testing.B) {
				layout := cleanup.Layout{Root: filepath.Join(b.TempDir(), "journal")}
				journal, err := cleanup.OpenJournal(layout)
				if err != nil {
					b.Fatal(err)
				}
				seq := uint64(1)
				for range n {
					id := fmt.Sprintf("%016x-%06x", seq, seq)
					admin := filepath.Join(layout.Root, "repo.git", "worktrees", id)
					original := filepath.Join(layout.Root, "trees", id)
					job := cleanup.Job{Schema: cleanup.Schema, ID: id, Seq: seq, Phase: cleanup.PhaseDone, Repo: filepath.Join(layout.Root, "repo.git"), AdminDir: admin, Admin: cleanup.FileID{Dev: 1, Ino: seq}, Original: original, Registered: layout.Registered(id), Payload: layout.Payload(id), Tree: cleanup.FileID{Dev: 1, Ino: seq}, Git: "/usr/bin/git", Links: cleanup.Links{DotGit: "gitdir: " + admin + "\n", AdminGitdir: original + "/.git\n"}, Created: time.Unix(10000, 0), Updated: time.Unix(10000, 0)}
					data, err := durable.Marshal(job)
					if err != nil {
						b.Fatal(err)
					}
					if err := os.Mkdir(layout.JobDir(id), 0o700); err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(layout.RecordPath(id), data, 0o600); err != nil {
						b.Fatal(err)
					}
					seq++
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					e, err := New(Config{Journal: journal, Relocator: &fakeRelocator{}, Deleter: &fakeDeleter{}, CPU: &fakeCPU{}, Band: &fakeBand{}, Parent: (&fakeParent{}).lookup, Identify: identify})
					if err != nil {
						b.Fatal(err)
					}
					if legacy {
						err = legacyLoadAtFaae356(e)
					} else {
						err = e.load()
					}
					if err != nil || len(e.jobs) != n {
						b.Fatalf("load = %d/%v", len(e.jobs), err)
					}
				}
			})
		}
	}
}
