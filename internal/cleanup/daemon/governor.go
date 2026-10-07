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
)

type governor struct {
	sampleEvery   time.Duration
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
	streak  int
	ambient float64
	learned bool
	settled bool
}

func newGovernor(t Tuning) *governor {
	return &governor{
		sampleEvery:   t.SampleEvery,
		pauseAbove:    t.PauseAbove,
		resumeBelow:   t.ResumeBelow,
		resumeSamples: t.ResumeSamples,
		rates: map[string]int{
			governorClear:       t.Rate,
			governorUnavailable: t.Rate,
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

func (g *governor) observe(now time.Time, cpu time.Duration, err error, quiet bool) {
	previous, since, had, was, settled := g.cpu, now.Sub(g.at), g.counted, g.state, g.settled
	g.at, g.settled = now, quiet
	if err != nil {
		g.state, g.detail, g.percent, g.counted, g.streak = governorUnavailable, err.Error(), 0, false, 0
		return
	}
	g.cpu, g.counted, g.detail = cpu, true, ""
	if !had {
		g.state, g.percent, g.streak = governorSampling, 0, 0
		return
	}
	if cpu < previous {
		g.percent = 0
		return
	}
	g.percent = float64(cpu-previous) / float64(since) * 100
	switch {
	case quiet && (was == governorSampling || !g.learned):
		g.ambient, g.learned = g.percent, true
	case quiet && settled:
		g.ambient += ambientWeight * (g.percent - g.ambient)
	}
	over := g.percent - g.ambient
	if was != governorThrottled {
		g.state = governorClear
		if over > g.pauseAbove {
			g.state = governorThrottled
		}
		g.streak = 0
		return
	}
	if over >= g.resumeBelow {
		g.streak = 0
		return
	}
	g.streak++
	if g.streak >= g.resumeSamples {
		g.state, g.streak = governorClear, 0
	}
}

func (g *governor) reset() {
	g.state, g.percent, g.detail, g.counted, g.streak = governorIdle, 0, "", false, 0
}

func (g *governor) report() cleanup.Governor {
	return cleanup.Governor{State: g.state, CPUPercent: g.percent, Detail: g.detail}
}
