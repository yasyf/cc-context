package relocate

import (
	"context"
	"fmt"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Admit proves afresh that job's payload may be deleted: it is the captured
// tree, nothing sits at the registered path, no worktree registration names
// the job folder or anything inside it, the job folder is unwatched, and no
// live process holds the payload. A payload that is already gone admits
// without further proof, since nothing is left to delete. A failed proof
// blocks the job; a cancelled ctx returns ctx's error, blocks nothing, and
// admits nothing, even when every proof had already passed.
func (r *Relocator) Admit(ctx context.Context, job *cleanup.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reason, detail := r.admission(ctx, job)
	if reason == "" {
		return ctx.Err()
	}
	_, err := r.block(ctx, job, reason, detail)
	return err
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
	if holders := r.held(ctx, job, job.Payload); holders != "" {
		return "activity", holders
	}
	return "", ""
}
