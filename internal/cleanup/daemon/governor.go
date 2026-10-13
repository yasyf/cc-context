package daemon

import (
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const (
	governorIdle        = "idle"
	governorSampling    = "sampling"
	governorClear       = "clear"
	governorThrottled   = "throttled"
	governorUnavailable = "unavailable"

	ambientWeight = 0.25
	detailStalled = "slices stalled past twice their time budget"
)

type hold struct {
	held   bool
	streak int
}

func (h *hold) observe(tripped, calm bool, resumeSamples int) {
	switch {
	case !h.held:
		h.held = tripped
	case !calm:
		h.streak = 0
	default:
		h.streak++
		if h.streak >= resumeSamples {
			*h = hold{}
		}
	}
}

type tally struct {
	slices int
	stalls int
}

type governor struct {
	sampleEvery   time.Duration
	sliceBudget   time.Duration
	pauseAbove    float64
	resumeBelow   float64
	resumeSamples int
	rates         map[string]int

	state   string
	percent float64
	detail  string
	counted bool
	cpu     time.Duration
	at      time.Time
	hot     hold
	stalled hold
	window  tally
	ambient float64
	learned bool
	settled bool
}

func newGovernor(t Tuning) *governor {
	return &governor{
		sampleEvery:   t.SampleEvery,
		sliceBudget:   t.SliceBudget,
		pauseAbove:    t.PauseAbove,
		resumeBelow:   t.ResumeBelow,
		resumeSamples: t.ResumeSamples,
		rates: map[string]int{
			governorClear:       t.Rate,
			governorUnavailable: t.FloorRate,
			governorThrottled:   t.FloorRate,
		},
		state: governorIdle,
	}
}

func (g *governor) due(now time.Time) bool {
	return g.state == governorIdle || !now.Before(g.next())
}

func (g *governor) next() time.Time { return g.at.Add(g.sampleEvery) }

func (g *governor) rate() int { return g.rates[g.state] }

func (g *governor) sliced(took time.Duration) {
	g.window.slices++
	if took >= 2*g.sliceBudget {
		g.window.stalls++
	}
}

func (g *governor) observe(now time.Time, cpu time.Duration, err error, quiet bool) {
	previous, since, had, was, settled := g.cpu, now.Sub(g.at), g.counted, g.state, g.settled
	g.at, g.settled = now, quiet
	if g.window.slices > 0 {
		g.stalled.observe(g.window.stalls > 0, g.window.stalls == 0, g.resumeSamples)
	}
	g.window = tally{}
	if err != nil {
		g.state, g.detail, g.percent, g.counted, g.hot = governorUnavailable, err.Error(), 0, false, hold{}
		return
	}
	g.cpu, g.counted, g.detail = cpu, true, ""
	switch {
	case !had:
		g.state, g.percent, g.hot = governorSampling, 0, hold{}
		return
	case cpu < previous:
		g.percent = 0
		if was == governorSampling {
			return
		}
	default:
		g.percent = float64(cpu-previous) / float64(since) * 100
		switch {
		case quiet && (was == governorSampling || !g.learned):
			g.ambient, g.learned = g.percent, true
		case quiet && settled:
			g.ambient += ambientWeight * (g.percent - g.ambient)
		}
		over := g.percent - g.ambient
		g.hot.observe(over > g.pauseAbove, over < g.resumeBelow, g.resumeSamples)
	}
	switch {
	case g.stalled.held:
		g.state, g.detail = governorThrottled, detailStalled
	case g.hot.held:
		g.state = governorThrottled
	default:
		g.state = governorClear
	}
}

func (g *governor) reset() {
	g.state, g.percent, g.detail, g.counted = governorIdle, 0, "", false
	g.hot, g.stalled, g.window = hold{}, hold{}, tally{}
}

func (g *governor) report() cleanup.Governor {
	return cleanup.Governor{State: g.state, CPUPercent: g.percent, Detail: g.detail}
}
