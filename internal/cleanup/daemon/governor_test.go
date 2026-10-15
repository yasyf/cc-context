package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestGovernor(t *testing.T) {
	errSampler := errors.New("fseventsd not found")
	type step struct {
		slices []time.Duration
		cpu    time.Duration
		err    error
		quiet  bool
		reset  bool
		want   cleanup.Governor
		rate   int
	}
	const (
		full, floor = 1000, 100
		onTime      = time.Millisecond
		budget      = 50 * time.Millisecond
		stall       = 2 * budget
	)
	sampling := cleanup.Governor{State: "sampling"}
	unavailable := cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"}
	cleared := func(percent float64) cleanup.Governor { return cleanup.Governor{State: "clear", CPUPercent: percent} }
	throttled := func(percent float64) cleanup.Governor {
		return cleanup.Governor{State: "throttled", CPUPercent: percent}
	}
	stalled := func(percent float64) cleanup.Governor {
		return cleanup.Governor{State: "throttled", CPUPercent: percent, Detail: "slices stalled past twice their time budget"}
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"the first slice waits for the first delta", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"the sampling window sets the ambient load, so a busy fseventsd stays clear", []step{
			{want: sampling},
			{cpu: 4 * time.Second, quiet: true, want: cleared(100), rate: full},
			{cpu: 6 * time.Second, want: cleared(150), rate: full},
		}},
		{"pauses past the margin over the ambient load", []step{
			{want: sampling},
			{cpu: 4 * time.Second, quiet: true, want: cleared(100), rate: full},
			{cpu: 7 * time.Second, want: throttled(175), rate: floor},
		}},
		{"a loud first delta measures against no ambient load", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75), rate: floor},
		}},
		{"resumes after three samples near the ambient load, skipping the one that settles", []step{
			{want: sampling},
			{cpu: 4 * time.Second, quiet: true, want: cleared(100), rate: full},
			{cpu: 7 * time.Second, want: throttled(175), rate: floor},
			{cpu: 4500 * time.Millisecond, quiet: true, want: throttled(112.5), rate: floor},
			{cpu: 4500 * time.Millisecond, quiet: true, want: throttled(112.5), rate: floor},
			{cpu: 4500 * time.Millisecond, quiet: true, want: cleared(112.5), rate: full},
		}},
		{"the ambient load follows a rise while throttled, so the floor never starves", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{cpu: 4 * time.Second, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: throttled(100), rate: floor},
			{cpu: 4 * time.Second, quiet: true, want: cleared(100), rate: full},
		}},
		{"a loud window never moves the ambient load", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{cpu: 2500 * time.Millisecond, want: cleared(62.5), rate: full},
			{cpu: 2500 * time.Millisecond, want: cleared(62.5), rate: full},
			{cpu: 3250 * time.Millisecond, want: throttled(81.25), rate: floor},
		}},
		{"a sample at the resume margin resets the streak", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{cpu: 4 * time.Second, want: throttled(100), rate: floor},
			{cpu: time.Second, quiet: true, want: throttled(25), rate: floor},
			{cpu: 2500 * time.Millisecond, quiet: true, want: throttled(62.5), rate: floor},
			{cpu: time.Second, quiet: true, want: throttled(25), rate: floor},
			{cpu: time.Second, quiet: true, want: throttled(25), rate: floor},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a sampler error holds the floor rate and discards the counter", []step{
			{err: errSampler, want: unavailable, rate: floor},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: 3 * time.Second, quiet: true, want: cleared(75), rate: full},
			{err: errSampler, want: unavailable, rate: floor},
		}},
		{"a slice that takes twice its budget holds the throttle through the next sample", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{onTime, stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
		}},
		{"a slice that ends at its budget is on time", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{budget, budget}, cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a slice a nanosecond short of twice its budget is on time", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{stall - time.Nanosecond}, cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a stall throttle lifts after three samples of on-time slices, and a stalled slice restarts the count", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime, stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a sample with no slice neither counts toward lifting a stall throttle nor restarts the count", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a stall throttle outlasts the fseventsd throttle it began beside", []step{
			{want: sampling},
			{cpu: 4 * time.Second, quiet: true, want: cleared(100), rate: full},
			{slices: []time.Duration{stall}, cpu: 7 * time.Second, want: stalled(175), rate: floor},
			{slices: []time.Duration{stall}, cpu: 4500 * time.Millisecond, quiet: true, want: stalled(112.5), rate: floor},
			{slices: []time.Duration{stall}, cpu: 4500 * time.Millisecond, quiet: true, want: stalled(112.5), rate: floor},
			{slices: []time.Duration{stall}, cpu: 4500 * time.Millisecond, quiet: true, want: stalled(112.5), rate: floor},
			{slices: []time.Duration{onTime}, cpu: 4500 * time.Millisecond, quiet: true, want: stalled(112.5), rate: floor},
			{slices: []time.Duration{onTime}, cpu: 4500 * time.Millisecond, quiet: true, want: stalled(112.5), rate: floor},
			{slices: []time.Duration{onTime}, cpu: 4500 * time.Millisecond, quiet: true, want: cleared(112.5), rate: full},
		}},
		{"a stall throttle keeps counting through a sampler error and its fresh baseline", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, err: errSampler, want: unavailable, rate: floor},
			{slices: []time.Duration{onTime}, cpu: 3 * time.Second, want: sampling},
			{cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{onTime}, cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"an idle queue forgets a stall throttle and the slices behind it", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{slices: []time.Duration{stall}, cpu: time.Second, quiet: true, want: stalled(25), rate: floor},
			{slices: []time.Duration{stall}, reset: true, want: cleanup.Governor{State: "idle"}},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"a counter that falls is a fresh baseline, not a low reading", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75), rate: floor},
			{cpu: 500 * time.Millisecond, quiet: true, want: throttled(12.5), rate: floor},
			{cpu: -3 * time.Second, quiet: true, want: throttled(0), rate: floor},
			{cpu: 500 * time.Millisecond, quiet: true, want: throttled(12.5), rate: floor},
			{cpu: 500 * time.Millisecond, quiet: true, want: cleared(12.5), rate: full},
		}},
		{"a counter that falls leaves a clear governor clear", []step{
			{want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
			{cpu: -time.Second, want: cleared(0), rate: full},
			{cpu: 4 * time.Second, want: throttled(100), rate: floor},
		}},
		{"a counter that falls before the first delta keeps deletion waiting", []step{
			{cpu: 3 * time.Second, want: sampling},
			{cpu: -time.Second, want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
		{"an idle queue resets the governor", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75), rate: floor},
			{reset: true, want: cleanup.Governor{State: "idle"}},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: time.Second, quiet: true, want: cleared(25), rate: full},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGovernor(DefaultTuning())
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			var cpu time.Duration
			for i, s := range tt.steps {
				for _, took := range s.slices {
					g.sliced(took)
				}
				if s.reset {
					g.reset()
				} else {
					now = now.Add(4 * time.Second)
					cpu += s.cpu
					g.observe(now, cpu, s.err, s.quiet)
				}
				if got := g.report(); got != s.want {
					t.Fatalf("step %d: report() = %+v, want %+v", i, got, s.want)
				}
				if got := g.rate(); got != s.rate {
					t.Fatalf("step %d: rate() = %d, want %d", i, got, s.rate)
				}
			}
		})
	}
}

func TestGovernorThrottlesOnTheSliceThatStalls(t *testing.T) {
	type sample struct {
		cpu   time.Duration
		err   error
		quiet bool
	}
	const (
		full, floor = 1000, 100
		stall       = 100 * time.Millisecond
	)
	errSampler := errors.New("fseventsd not found")
	detail := "slices stalled past twice their time budget"
	tests := []struct {
		name    string
		samples []sample
		took    time.Duration
		want    cleanup.Governor
		rate    int
	}{
		{"a stalled slice drops a clear governor to the floor", []sample{{quiet: true}, {cpu: time.Second, quiet: true}}, stall, cleanup.Governor{State: "throttled", CPUPercent: 25, Detail: detail}, floor},
		{"a stalled slice names itself beside the fseventsd throttle", []sample{{quiet: true}, {cpu: 3 * time.Second}}, stall, cleanup.Governor{State: "throttled", CPUPercent: 75, Detail: detail}, floor},
		{"a stalled slice leaves an unreadable sampler's report", []sample{{err: errSampler}}, stall, cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"}, floor},
		{"a slice a nanosecond short of a stall leaves a clear governor clear", []sample{{quiet: true}, {cpu: time.Second, quiet: true}}, stall - time.Nanosecond, cleanup.Governor{State: "clear", CPUPercent: 25}, full},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGovernor(DefaultTuning())
			now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
			var cpu time.Duration
			for _, s := range tt.samples {
				now = now.Add(4 * time.Second)
				cpu += s.cpu
				g.observe(now, cpu, s.err, s.quiet)
			}
			g.sliced(tt.took)
			if got := g.report(); got != tt.want {
				t.Errorf("report() = %+v, want %+v", got, tt.want)
			}
			if got := g.rate(); got != tt.rate {
				t.Errorf("rate() = %d, want %d", got, tt.rate)
			}
		})
	}
}

func TestGovernorSampleSchedule(t *testing.T) {
	g := newGovernor(DefaultTuning())
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if !g.due(now) {
		t.Fatal("an idle governor is not due for its first sample")
	}
	g.observe(now, 0, nil, true)
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"at the sample", now, false},
		{"just before the interval", now.Add(5*time.Second - time.Nanosecond), false},
		{"at the interval", now.Add(5 * time.Second), true},
		{"past the interval", now.Add(time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := g.due(tt.at); got != tt.want {
				t.Errorf("due(%s) = %t, want %t", tt.at.Sub(now), got, tt.want)
			}
		})
	}
	if got, want := g.next(), now.Add(5*time.Second); !got.Equal(want) {
		t.Errorf("next() = %s, want %s", got, want)
	}
	g.reset()
	if !g.due(now) {
		t.Error("a reset governor is not due for a fresh baseline")
	}
}
