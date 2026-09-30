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
		reset bool
		want  cleanup.Governor
		allow bool
	}
	sampling := cleanup.Governor{State: "sampling"}
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
			{cpu: time.Second, want: cleared(25), allow: true},
		}},
		{"exactly the pause threshold stays clear", []step{
			{want: sampling},
			{cpu: 2 * time.Second, want: cleared(50), allow: true},
		}},
		{"pauses above the threshold", []step{
			{want: sampling},
			{cpu: 2500 * time.Millisecond, want: throttled(62.5)},
		}},
		{"the first delta can throttle", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{cpu: 3 * time.Second, want: throttled(75)},
		}},
		{"resumes only after three consecutive samples below the floor", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: cleared(12.5), allow: true},
		}},
		{"a sample at the floor resets the streak", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: time.Second, want: throttled(25)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: cleared(12.5), allow: true},
		}},
		{"between the floor and the ceiling a clear governor stays clear", []step{
			{want: sampling},
			{cpu: 1500 * time.Millisecond, want: cleared(37.5), allow: true},
			{cpu: 1500 * time.Millisecond, want: cleared(37.5), allow: true},
		}},
		{"a sampler error proceeds unthrottled and discards the baseline", []step{
			{err: errSampler, want: cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"}, allow: true},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{err: errSampler, want: cleanup.Governor{State: "unavailable", Detail: "fseventsd not found"}, allow: true},
		}},
		{"a counter that falls is a fresh baseline, not a low reading", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: -3 * time.Second, want: throttled(0)},
			{cpu: 500 * time.Millisecond, want: throttled(12.5)},
			{cpu: 500 * time.Millisecond, want: cleared(12.5), allow: true},
		}},
		{"a counter that falls leaves a clear governor clear", []step{
			{want: sampling},
			{cpu: time.Second, want: cleared(25), allow: true},
			{cpu: -time.Second, want: cleared(0), allow: true},
			{cpu: 3 * time.Second, want: throttled(75)},
		}},
		{"a counter that falls before the first delta keeps deletion waiting", []step{
			{cpu: 3 * time.Second, want: sampling},
			{cpu: -time.Second, want: sampling},
			{cpu: time.Second, want: cleared(25), allow: true},
		}},
		{"an idle queue resets the governor", []step{
			{want: sampling},
			{cpu: 3 * time.Second, want: throttled(75)},
			{reset: true, want: cleanup.Governor{State: "idle"}},
			{cpu: 3 * time.Second, want: sampling},
			{cpu: time.Second, want: cleared(25), allow: true},
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
					g.observe(now, cpu, s.err)
				}
				if got := g.report(); got != s.want {
					t.Fatalf("step %d: report() = %+v, want %+v", i, got, s.want)
				}
				if got := g.allows(); got != s.allow {
					t.Fatalf("step %d: allows() = %t, want %t", i, got, s.allow)
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
	g.observe(now, 0, nil)
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
