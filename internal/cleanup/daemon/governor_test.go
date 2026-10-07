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
		cpu   time.Duration
		err   error
		quiet bool
		reset bool
		want  cleanup.Governor
		rate  int
	}
	const full, floor = 1000, 100
	sampling := cleanup.Governor{State: "sampling"}
	unavailable := cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"}
	cleared := func(percent float64) cleanup.Governor { return cleanup.Governor{State: "clear", CPUPercent: percent} }
	throttled := func(percent float64) cleanup.Governor {
		return cleanup.Governor{State: "throttled", CPUPercent: percent}
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
		{"a sampler error proceeds at the full rate and discards the counter", []step{
			{err: errSampler, want: unavailable, rate: full},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: 3 * time.Second, quiet: true, want: cleared(75), rate: full},
			{err: errSampler, want: unavailable, rate: full},
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
