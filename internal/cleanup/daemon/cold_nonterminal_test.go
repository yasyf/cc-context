package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type coldPendingRelocator struct {
	*fakeRelocator
}

func (r coldPendingRelocator) Pend(_ context.Context, seq uint64, request cleanup.Request) (cleanup.Job, error) {
	job := r.h.newJob(filepath.Base(request.Worktree), seq, cleanup.PhasePending)
	job.Head = legacyHead
	job.Branch = "refs/heads/" + filepath.Base(request.Worktree)
	job.RecoveryRef = cleanup.RecoveryRefFor(job.ID)
	job.Force = request.Force
	return job, r.h.journal.Create(job)
}

func TestCleanupColdPausedNonterminal(t *testing.T) {
	if os.Getenv("CCX_COLD_STARTUP_OUTPUT") == "" {
		t.Skip("the private paused-nonterminal CI job retains this fixture's evidence")
	}
	for _, count := range []int{32, 100, 7422} {
		t.Run(fmt.Sprintf("N%d", count), func(t *testing.T) {
			h := newHarness(t, paced())
			producer := h.build()
			producer.relocator = coldPendingRelocator{h.relocator}
			h.running = make(chan error, 1)
			go func() { h.running <- producer.Run(context.Background()) }()
			t.Cleanup(func() {
				if h.engine == producer {
					h.stop()
				}
			})
			ctx := cleanup.WithRequester(context.Background(), cleanup.ProcessID{PID: 4242, Start: clientStart})
			if err := producer.Pause(ctx); err != nil {
				t.Fatal(err)
			}
			receipts := make([]cleanup.Receipt, 0, count)
			ids := make(map[string]bool, count)
			started := time.Now()
			for i := range count {
				request := h.request(fmt.Sprintf("pending-%06d", i+1))
				request.Force = true
				receipt, err := producer.Remove(ctx, request)
				if err != nil {
					t.Fatalf("Remove(%d) = %v", i, err)
				}
				if receipt.JobID == "" || ids[receipt.JobID] || receipt.State != cleanup.State(cleanup.PhasePending) || receipt.Original != request.Worktree {
					t.Fatalf("Remove(%d) receipt = %+v, duplicate = %t", i, receipt, ids[receipt.JobID])
				}
				ids[receipt.JobID] = true
				receipts = append(receipts, receipt)
			}
			admission := time.Since(started)
			h.stop()
			entries, err := os.ReadDir(h.layout.JobsDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != count {
				t.Fatalf("persisted job directories = %d, want %d", len(entries), count)
			}
			var recordBytes int64
			for _, receipt := range receipts {
				info, err := os.Stat(h.layout.RecordPath(receipt.JobID))
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() == 0 {
					t.Fatalf("empty record for %s", receipt.JobID)
				}
				recordBytes += info.Size()
			}
			replay := h.build()
			started = time.Now()
			if err := replay.load(); err != nil {
				t.Fatal(err)
			}
			load := time.Since(started)
			if !replay.paused || replay.hold != nil || len(replay.jobs) != count || len(replay.order) != count || len(replay.damaged) != 0 {
				t.Fatalf("replay paused=%t hold=%+v jobs=%d order=%d damaged=%d", replay.paused, replay.hold, len(replay.jobs), len(replay.order), len(replay.damaged))
			}
			var seq uint64
			retirements := make(map[cleanup.Retirement]int)
			for i, receipt := range receipts {
				seq++
				job := replay.jobs[receipt.JobID]
				if job.Schema != cleanup.Schema || job.ID != receipt.JobID || job.Seq != seq || job.Phase != cleanup.PhasePending || replay.order[i].id != job.ID || replay.order[i].seq != seq {
					t.Fatalf("replay job %d = %+v, order = %+v", i, job, replay.order[i])
				}
				if job.Head != legacyHead || job.Branch != "refs/heads/"+filepath.Base(job.Original) || job.RecoveryRef != cleanup.RecoveryRefFor(job.ID) || !job.Force || job.Requester != (cleanup.ProcessID{PID: 4242, Start: clientStart}) {
					t.Fatalf("replay capture %d = %+v", i, job)
				}
				if job.Original != receipt.Original || job.Registered != h.layout.Registered(job.ID) || job.Payload != h.layout.Payload(job.ID) || job.Tree.Zero() || job.Admin.Zero() || job.Links.DotGit == "" || job.Links.AdminGitdir == "" || job.Created.IsZero() || job.Updated.IsZero() {
					t.Fatalf("replay paths and metadata %d = %+v", i, job)
				}
				retirements[job.Retirement]++
			}
			if replay.seq != seq {
				t.Fatalf("max sequence = %d, want %d", replay.seq, seq)
			}
			started = time.Now()
			report, err := replay.Status(context.Background(), cleanup.Query{Limit: 1})
			status := time.Since(started)
			if err != nil || !report.Paused || len(report.Damaged) != 0 || len(report.Jobs) != 1 || report.Jobs[0].ID != receipts[0].JobID || report.Jobs[0].Seq != 1 || report.Jobs[0].Phase != cleanup.PhasePending || report.Omitted != count-1 {
				t.Fatalf("Status(Limit:1) = %+v, %v", report, err)
			}
			all, err := replay.Status(context.Background(), cleanup.Query{Limit: count})
			if err != nil || !all.Paused || len(all.Jobs) != count || all.Omitted != 0 || len(all.Damaged) != 0 {
				t.Fatalf("Status(Limit:N) jobs=%d omitted=%d damaged=%d paused=%t, %v", len(all.Jobs), all.Omitted, len(all.Damaged), all.Paused, err)
			}
			for i, job := range all.Jobs {
				if job.ID != receipts[i].JobID || job.Phase != cleanup.PhasePending {
					t.Fatalf("Status job %d = %+v", i, job)
				}
			}
			coldStartupOutput(t, fmt.Sprintf("paused-nonterminal-%d", count), map[string]any{
				"records":                              count,
				"acknowledged":                         len(receipts),
				"nonterminal_records":                  len(all.Jobs),
				"done_records":                         0,
				"record_bytes":                         recordBytes,
				"max_seq":                              replay.seq,
				"phase":                                cleanup.PhasePending,
				"retirement_counts":                    retirements,
				"admission_with_private_path_setup_ns": int64(admission),
				"engine_load_ns":                       int64(load),
				"postload_status_limit1_ns":            int64(status),
				"returned_seq":                         report.Jobs[0].Seq,
				"omitted":                              report.Omitted,
				"paused":                               report.Paused,
				"keep_done":                            replay.tuning.KeepDone,
				"authority":                            "existing fake relocator with linked-job capture metadata; real service and Journal",
				"clock":                                "real wall timings; fixed engine clock; no synctest bubble",
				"producer_stopped_before_replay":       true,
				"os_cache_flushed":                     false,
				"historical_population_or_latency_claimed": false,
			})
		})
	}
}
