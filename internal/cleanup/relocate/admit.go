package relocate

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Admit returns the captured root whose physical deletion may proceed.
// External admin deletion requires its intact payload locator; retired payload
// deletion requires the durable admin-complete phase. Both stages require
// strict activity proof. Native payload admission retains its existing rules.
// A failed proof blocks the job; cancellation admits nothing and blocks nothing.
func (r *Relocator) Admit(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	if err := ctx.Err(); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	if job.Phase == cleanup.PhaseDeletingAdmin {
		return r.admitAdmin(ctx, job)
	}
	reason, detail := r.admission(ctx, job)
	if reason == "" {
		return cleanup.DeletionRoot{Dir: filepath.Dir(job.Payload), Name: filepath.Base(job.Payload), ID: job.Tree}, ctx.Err()
	}
	_, err := r.block(ctx, job, reason, detail)
	return cleanup.DeletionRoot{}, err
}

func (r *Relocator) admission(ctx context.Context, job *cleanup.Job) (reason, detail string) {
	payload, err := sight(job.Payload)
	if err != nil {
		return "identity", err.Error()
	}
	if !payload.exists {
		return "", ""
	}
	if !payload.is(job.Tree) {
		return "identity", fmt.Sprintf("payload %s is %s, the job captured tree %s", job.Payload, payload, label(job.Tree))
	}
	squatter, err := sight(job.Registered)
	if err != nil {
		return "reconcile", err.Error()
	}
	if squatter.exists {
		return "reconcile", fmt.Sprintf("registered %s is %s, want it absent", job.Registered, squatter)
	}
	claim, err := claimed(job)
	if err != nil {
		return "reconcile", err.Error()
	}
	if claim != "" {
		return "reconcile", claim
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, r.cfg.Journal.Layout().JobDir(job.ID)); err != nil {
		return "quarantine", err.Error()
	}
	if job.Phase == cleanup.PhaseDeletingRetiredPayload {
		job.Ignored = nil
		if err := r.strictIdle(ctx, job, job.Payload); err != nil {
			return "activity", unclear(job.Payload, err)
		}
		return "", ""
	}
	if holders := r.held(ctx, job, job.Payload); holders != "" {
		return "activity", holders
	}
	return "", ""
}
