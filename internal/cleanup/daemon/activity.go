package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	governorHeld = "held"

	detailUnsampled = "activity not sampled yet"
)

type gate struct {
	every   time.Duration
	settle  time.Duration
	percent float64
	busy    int
	create  time.Duration
	resume  int

	held     bool
	strained bool
	quiet    int
	sampled  bool
	at       time.Time
	detail   string
}

func newGate(t Tuning) *gate {
	return &gate{
		every:   t.ActivityEvery,
		settle:  t.ProbeSettle,
		percent: t.BusyPercent,
		busy:    t.MaxBusy,
		create:  t.MaxCreate,
		resume:  t.QuietSamples,
		held:    true,
		detail:  detailUnsampled,
	}
}

func (g *gate) next(slicedAt time.Time) time.Time {
	settled := slicedAt.Add(g.settle)
	if !g.sampled {
		return settled
	}
	return later(g.at.Add(g.every), settled)
}

func (g *gate) observe(now time.Time, activity cleanup.Activity, err error) {
	g.at, g.sampled = now, true
	if err != nil {
		g.held, g.strained, g.quiet, g.detail = true, true, 0, err.Error()
		return
	}
	busy := 0
	for _, percent := range activity.Sessions {
		if percent > g.percent {
			busy++
		}
	}
	g.strained = activity.Create > g.create
	switch {
	case busy > g.busy || g.strained:
		g.held, g.quiet = true, 0
	case g.held:
		g.quiet++
		if g.quiet >= g.resume {
			g.held, g.quiet = false, 0
		}
	}
	g.detail = fmt.Sprintf("%d of %d sessions busy (at most %d), creates take %s (at most %s)",
		busy, len(activity.Sessions), g.busy, activity.Create.Round(10*time.Microsecond), g.create)
	if g.quiet > 0 {
		g.detail += fmt.Sprintf(", quiet %d of %d samples", g.quiet, g.resume)
	}
}

func (g *gate) report() cleanup.Governor {
	return cleanup.Governor{State: governorHeld, Detail: g.detail}
}

func (g *gate) label() string {
	if g.held {
		return g.detail
	}
	return "open"
}

func (e *Engine) yields(job cleanup.Job) bool {
	if e.gate == nil {
		return false
	}
	e.wanted = true
	rests := e.rests(job)
	e.deferred = e.deferred || rests
	return rests
}

func (e *Engine) holds() bool {
	return e.gate != nil && e.gate.held
}

func (e *Engine) rests(job cleanup.Job) bool {
	return e.holds() && (e.gate.strained || !e.busy.Allows(deletesAt(job)))
}

func deletesAt(job cleanup.Job) string {
	switch job.Phase {
	case cleanup.PhaseUnregistered, cleanup.PhaseDeleting, cleanup.PhaseDeletingAdmin, cleanup.PhaseDeletingRetiredPayload:
		return job.Payload
	case cleanup.PhaseDeletingRegistration:
		return job.AdminDir
	default:
		return job.Original
	}
}

func (e *Engine) sampleDue(now time.Time) bool {
	if !e.wanted || (e.gate.sampled && now.Before(e.gate.at.Add(e.gate.every))) {
		return false
	}
	if settled := e.slicedAt.Add(e.gate.settle); now.Before(settled) {
		e.paceUntil = later(e.paceUntil, settled)
		return false
	}
	return true
}

func (e *Engine) sample(ctx context.Context, now time.Time) bool {
	activity, err := e.activity.Sample(ctx)
	if e.stopping(ctx) {
		return true
	}
	e.gate.observe(now, activity, err)
	e.publishGovernor()
	return false
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
