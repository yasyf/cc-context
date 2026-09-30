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
)

type governor struct {
	sampleEvery   time.Duration
	pauseAbove    float64
	resumeBelow   float64
	resumeSamples int

	state    string
	percent  float64
	detail   string
	baseline bool
	cpu      time.Duration
	at       time.Time
	streak   int
}

func newGovernor(t Tuning) *governor {
	return &governor{
		sampleEvery:   t.SampleEvery,
		pauseAbove:    t.PauseAbove,
		resumeBelow:   t.ResumeBelow,
		resumeSamples: t.ResumeSamples,
		state:         governorIdle,
	}
}

func (g *governor) due(now time.Time) bool {
	return g.state == governorIdle || !now.Before(g.next())
}

func (g *governor) next() time.Time { return g.at.Add(g.sampleEvery) }

func (g *governor) allows() bool {
	return g.state == governorClear || g.state == governorUnavailable
}

func (g *governor) observe(now time.Time, cpu time.Duration, err error) {
	previous, since, had := g.cpu, now.Sub(g.at), g.baseline
	g.at = now
	if err != nil {
		g.state, g.detail, g.percent, g.baseline, g.streak = governorUnavailable, err.Error(), 0, false, 0
		return
	}
	g.cpu, g.baseline, g.detail = cpu, true, ""
	if !had {
		g.state, g.percent, g.streak = governorSampling, 0, 0
		return
	}
	if cpu < previous {
		g.percent = 0
		return
	}
	g.percent = float64(cpu-previous) / float64(since) * 100
	if g.state != governorThrottled {
		g.state = governorClear
		if g.percent > g.pauseAbove {
			g.state = governorThrottled
		}
		g.streak = 0
		return
	}
	if g.percent >= g.resumeBelow {
		g.streak = 0
		return
	}
	g.streak++
	if g.streak >= g.resumeSamples {
		g.state, g.streak = governorClear, 0
	}
}

func (g *governor) reset() {
	g.state, g.percent, g.detail, g.baseline, g.streak = governorIdle, 0, "", false, 0
}

func (g *governor) report() cleanup.Governor {
	return cleanup.Governor{State: g.state, CPUPercent: g.percent, Detail: g.detail}
}
