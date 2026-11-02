package relocate

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Admit returns the captured root whose physical deletion may proceed, where
// it sits. A claimed job is proven again in full, then hands over its tree;
// once the tree is gone, the admin directory the job locked; once that is gone
// too, it is done and Admit returns no root. A legacy job hands over the
// payload or retired admin its relocation left, matched by inode. Every stage
// requires activity proof.
// A failed proof blocks the job; cancellation admits nothing and blocks nothing.
func (r *Relocator) Admit(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	if err := ctx.Err(); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	switch job.Phase {
	case cleanup.PhaseDone:
		return cleanup.DeletionRoot{}, nil
	case cleanup.PhaseClaimed:
		return r.admitClaimed(ctx, job)
	case cleanup.PhaseDeletingTree:
		return r.admitTree(ctx, job)
	case cleanup.PhaseDeletingRegistration:
		return r.admitRegistration(ctx, job)
	case cleanup.PhaseDeletingAdmin:
		return r.admitAdmin(ctx, job)
	}
	payload, reason, detail := r.admission(ctx, job)
	if reason == "" {
		return cleanup.DeletionRoot{Dir: filepath.Dir(job.Payload), Name: filepath.Base(job.Payload), ID: payload}, ctx.Err()
	}
	_, err := r.block(ctx, job, reason, detail)
	return cleanup.DeletionRoot{}, err
}

func (r *Relocator) admission(ctx context.Context, job *cleanup.Job) (payload cleanup.FileID, reason, detail string) {
	payload, ok, err := cleanup.Placed(job.Payload, job.Tree)
	switch {
	case err != nil:
		return payload, "identity", err.Error()
	case payload.Zero():
		return payload, "", ""
	case !ok:
		seen, err := sight(job.Payload)
		if err != nil {
			return payload, "identity", err.Error()
		}
		return payload, "identity", fmt.Sprintf("payload %s is %s, the job captured tree %s", job.Payload, seen, label(job.Tree))
	}
	squatter, err := sight(job.Registered)
	if err != nil {
		return payload, "reconcile", err.Error()
	}
	if squatter.exists {
		return payload, "reconcile", fmt.Sprintf("registered %s is %s, want it absent", job.Registered, squatter)
	}
	claim, err := claimed(job)
	if err != nil {
		return payload, "reconcile", err.Error()
	}
	if claim != "" {
		return payload, "reconcile", claim
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, filepath.Dir(job.Payload)); err != nil {
		return payload, probeReason("quarantine", err), err.Error()
	}
	if job.Phase == cleanup.PhaseDeletingRetiredPayload {
		job.Ignored = nil
		if failed, err := r.strictIdle(ctx, job, job.Payload); err != nil {
			return payload, "activity", unclear(failed, err)
		}
		return payload, "", ""
	}
	if holders := r.held(ctx, job, job.Payload); holders != "" {
		return payload, "activity", holders
	}
	return payload, "", ""
}
