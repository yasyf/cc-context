package relocate

import "github.com/yasyf/cc-context/internal/cleanup"

func mountLocation(job *cleanup.Job, seen observation) (string, sighting, bool) {
	switch job.Phase {
	case cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePending:
		if !seen.registered.exists && !seen.payload.exists {
			return job.Original, seen.original, false
		}
	case cleanup.PhasePrepared:
		if !seen.registered.exists && !seen.payload.exists {
			return job.Original, seen.original, false
		}
		if seen.registered.exists && !seen.payload.exists {
			return job.Registered, seen.registered, true
		}
	case cleanup.PhaseMoved:
		if seen.registered.exists && !seen.payload.exists {
			return job.Registered, seen.registered, true
		}
		if !seen.registered.exists && seen.payload.exists {
			return job.Payload, seen.payload, true
		}
	case cleanup.PhaseDetached:
		if !seen.registered.exists && seen.payload.exists {
			return job.Payload, seen.payload, true
		}
	}
	return "", sighting{}, false
}
