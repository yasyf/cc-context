package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func statusFixture(jobs []cleanup.Job, limit int) *Engine {
	loaded := make(chan struct{})
	close(loaded)
	e := &Engine{
		loaded: loaded,
		jobs:   make(map[string]cleanup.Job, len(jobs)),
		tuning: DefaultTuning(),
	}
	e.tuning.StatusLimit = limit
	for _, job := range jobs {
		e.jobs[job.ID] = job
		e.order = append(e.order, seqKey{seq: job.Seq, id: job.ID})
	}
	return e
}

func TestStatusAllocationIsBoundedByReturnedJobs(t *testing.T) {
	allocations := func(count int) float64 {
		jobs := make([]cleanup.Job, count)
		for i := range jobs {
			jobs[i] = cleanup.Job{
				ID:      fmt.Sprintf("job-%d", i),
				Seq:     uint64(i + 1),
				Phase:   cleanup.PhasePending,
				Errors:  []cleanup.JobError{{Message: "identity: replaced"}},
				Blocked: &cleanup.Blockage{Reason: "identity", Detail: "replaced"},
			}
		}
		e := statusFixture(jobs, 50)
		return testing.AllocsPerRun(3, func() {
			report, err := e.Status(context.Background(), cleanup.Query{})
			if err != nil || len(report.Jobs) != 50 || report.Omitted != count-50 {
				t.Fatalf("Status(%d) = %d jobs, %d omitted, %v", count, len(report.Jobs), report.Omitted, err)
			}
		})
	}
	page := allocations(50)
	queue := allocations(6000)
	t.Logf("status allocations: 50 jobs %.0f; 6000 jobs %.0f; returned 50", page, queue)
	if queue > page+1 {
		t.Errorf("status allocations grow with omitted jobs: %.0f > %.0f + 1", queue, page)
	}
}

func TestStatusWindowPreservesFinishedOrderAndSnapshot(t *testing.T) {
	jobs := []cleanup.Job{
		{ID: "done-old", Seq: 1, Phase: cleanup.PhaseDone, Updated: time.Unix(3, 0)},
		{ID: "first", Seq: 2, Phase: cleanup.PhasePending, Errors: []cleanup.JobError{{Message: "identity: first"}}, Blocked: &cleanup.Blockage{Reason: "identity", Detail: "first"}},
		{ID: "done-tie-first", Seq: 3, Phase: cleanup.PhaseDone, Updated: time.Unix(5, 0)},
		{ID: "second", Seq: 4, Phase: cleanup.PhaseDeleting},
		{ID: "done-tie-last", Seq: 5, Phase: cleanup.PhaseDone, Updated: time.Unix(5, 0)},
	}
	e := statusFixture(jobs, 2)
	tests := []struct {
		name  string
		limit int
		want  []string
	}{
		{"default", 0, []string{"first", "second"}},
		{"negative uses default", -1, []string{"first", "second"}},
		{"one", 1, []string{"first"}},
		{"finished fills remainder", 3, []string{"first", "second", "done-tie-last"}},
		{"all", 8, []string{"first", "second", "done-tie-last", "done-tie-first", "done-old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := e.Status(context.Background(), cleanup.Query{Limit: tt.limit})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(report.Jobs))
			for i, job := range report.Jobs {
				got[i] = job.ID
			}
			if !slices.Equal(got, tt.want) || report.Omitted != len(jobs)-len(tt.want) {
				t.Fatalf("Status() = %v, omitted %d; want %v, omitted %d", got, report.Omitted, tt.want, len(jobs)-len(tt.want))
			}
			report.Jobs[0].Errors[0].Message = "changed"
			report.Jobs[0].Blocked.Detail = "changed"
			if job := e.jobs["first"]; job.Errors[0].Message != "identity: first" || job.Blocked.Detail != "first" {
				t.Fatalf("report mutated stored job: %+v", job)
			}
		})
	}
}
