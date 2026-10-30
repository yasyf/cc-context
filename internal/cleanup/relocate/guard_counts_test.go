package relocate

import (
	"context"
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestDisplacedActivityProbeCallCountsCI(t *testing.T) {
	f := newFixture(t)
	job := f.intend()
	admin := moveCapturedAdmin(t, f, job)
	retiring := []cleanup.ProcessID{{PID: 101, Start: 10}, {PID: 102, Start: 11}}
	server := []cleanup.ProcessID{{PID: 101, Start: 10}}
	f.watchers.retiring = func(context.Context, string) ([]cleanup.ProcessID, error) { return retiring, nil }
	f.watchers.server = func(context.Context) ([]cleanup.ProcessID, error) { return server, nil }
	f.guarded, f.discounts, f.guardRequests = nil, nil, nil
	retireFixture(t, f, &job)
	if job.Blocked != nil || job.Retirement != cleanup.RetirementComplete {
		t.Fatalf("logical retirement = %+v", job)
	}
	logicalRoots := []string{job.Original, admin, job.Original, admin}
	logicalDiscounts := [][]cleanup.ProcessID{retiring, server, server, server}
	if !reflect.DeepEqual(f.guarded, logicalRoots) || !reflect.DeepEqual(f.discounts, logicalDiscounts) {
		t.Fatalf("logical guards = %v/%v, want %v/%v", f.guarded, f.discounts, logicalRoots, logicalDiscounts)
	}
	if len(f.guardRequests) != 2 {
		t.Fatalf("logical requests = %v, want two complete root pairs", f.guardRequests)
	}
	for _, request := range f.guardRequests {
		if len(request) != 2 {
			t.Fatalf("logical request omitted a root: %v", request)
		}
	}
	t.Logf("logical_guard_requests=%d roots=%v", len(f.guardRequests), f.guarded)
	f.guarded, f.discounts, f.guardRequests = nil, nil, nil
	f.advance(&job)
	adminStage(t, f, &job)
	resumeRoots := append(logicalRoots, job.Original, admin)
	resumeDiscounts := append(logicalDiscounts, server, server)
	if !reflect.DeepEqual(f.guarded, resumeRoots) || !reflect.DeepEqual(f.discounts, resumeDiscounts) {
		t.Fatalf("resume guards = %v/%v, want %v/%v", f.guarded, f.discounts, resumeRoots, resumeDiscounts)
	}
	if len(f.guardRequests) != 3 {
		t.Fatalf("resume requests = %v, want three complete root pairs", f.guardRequests)
	}
	for _, request := range f.guardRequests {
		if len(request) != 2 {
			t.Fatalf("resume request omitted a root: %v", request)
		}
	}
	t.Logf("resume_guard_requests=%d roots=%v", len(f.guardRequests), f.guarded)
}
