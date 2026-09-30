// Package relocate is the logical half of a worktree removal: it preflights a
// linked worktree, journals the removal, has git move the tree into the job's
// private folder, and drops the registration. It also adopts the trees the
// retired janitor left unregistered in its quarantine. It never deletes a tree.
package relocate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Config wires a Relocator to its journal and to the seams it consults before
// it mutates anything. Journal, Guard, Watchers, and Now are all required.
type Config struct {
	Journal  *cleanup.Journal
	Guard    cleanup.Guard
	Watchers cleanup.Watchers
	// GitEnv is the environment of every git child; nil inherits the process's.
	GitEnv []string
	Now    func() time.Time
}

// Relocator implements cleanup.Relocator over the git binary each job names.
type Relocator struct {
	cfg Config
}

var _ cleanup.Relocator = (*Relocator)(nil)

// New returns a Relocator that journals through cfg.Journal and relocates into
// that journal's layout.
func New(cfg Config) *Relocator {
	return &Relocator{cfg: cfg}
}

// Preview runs Accept's whole preflight, the activity guard included, and
// returns the record Accept would journal without journaling or mutating
// anything. The record's id is freshly minted and its Seq is unset, so it
// names the paths a removal would use but is not itself a journalable job.
func (r *Relocator) Preview(ctx context.Context, req cleanup.Request) (cleanup.Job, error) {
	job, err := r.preflight(ctx, req)
	if err != nil {
		return cleanup.Job{}, err
	}
	r.stamp(&job, 0, cleanup.PhasePrepared)
	return job, nil
}

// Accept preflights the removal req asks for and journals it at PhasePrepared.
// The journal record is its only durable effect.
func (r *Relocator) Accept(ctx context.Context, seq uint64, req cleanup.Request) (cleanup.Job, error) {
	job, err := r.preflight(ctx, req)
	if err != nil {
		return cleanup.Job{}, err
	}
	r.stamp(&job, seq, cleanup.PhasePrepared)
	if err := r.cfg.Journal.Create(job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}

// Intend journals a deferred removal without consulting dirtiness and without
// refusing for activity: a tree the guard finds idle is journaled at
// PhaseQueued, a held one at PhaseWaiting with its holders noted, and one the
// guard could not inspect at PhaseWaiting with that failure noted. A path
// whose registration is not req.Expected is refused as replaced before git,
// the guard, or the journal is reached. A cancelled ctx returns ctx's error
// and journals nothing.
func (r *Relocator) Intend(ctx context.Context, seq uint64, req cleanup.DeferRequest) (cleanup.Job, error) {
	if err := req.Validate(); err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: defer request: %w", err)
	}
	job, err := identify(req.Worktree)
	if err != nil {
		return cleanup.Job{}, err
	}
	if held := registrationOf(job); held != req.Expected {
		return cleanup.Job{}, refuse(
			job.Original, "replaced",
			"%s no longer holds the worktree the request named: it holds tree %s registered under %s (%s), the request named tree %s registered under %s (%s)",
			job.Original, label(held.Tree), held.AdminDir, label(held.Admin),
			label(req.Expected.Tree), req.Expected.AdminDir, label(req.Expected.Admin),
		)
	}
	if common, err := filepath.EvalSymlinks(req.CommonDir); err != nil || common != job.Repo {
		return cleanup.Job{}, refuse(job.Original, "mismatch", "%s belongs to %s, not %s", job.Original, job.Repo, req.CommonDir)
	}
	job.Git, job.Force = req.Git, req.Force
	job.Deferred, job.Owner = true, req.Owner
	if err := r.screen(ctx, job); err != nil {
		return cleanup.Job{}, err
	}
	if err := r.capture(ctx, &job); err != nil {
		return cleanup.Job{}, err
	}

	verdict := r.unretired(ctx, job.Original)
	if err := ctx.Err(); err != nil {
		return cleanup.Job{}, err
	}
	var active *cleanup.ActiveError
	switch {
	case verdict == nil:
		r.stamp(&job, seq, cleanup.PhaseQueued)
	case errors.As(verdict, &active):
		r.stamp(&job, seq, cleanup.PhaseWaiting)
		job.Note(job.Created, active.Error())
	default:
		r.stamp(&job, seq, cleanup.PhaseWaiting)
		job.Note(job.Created, unclear(job.Original, verdict))
	}
	if err := r.cfg.Journal.Create(job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}

func (r *Relocator) stamp(job *cleanup.Job, seq uint64, phase cleanup.Phase) {
	now := r.cfg.Now()
	layout := r.cfg.Journal.Layout()
	job.Schema = cleanup.Schema
	job.ID = cleanup.MintID(now)
	job.Seq = seq
	job.Phase = phase
	job.Registered = layout.Registered(job.ID)
	job.Payload = layout.Payload(job.ID)
	if job.Head != "" && !job.Adopted {
		job.RecoveryRef = cleanup.RecoveryRefFor(job.ID)
	}
	job.Created, job.Updated = now, now
}
