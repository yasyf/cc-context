package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type sighting struct {
	id     cleanup.FileID
	dir    bool
	exists bool
}

func sight(path string) (sighting, error) {
	id, info, err := cleanup.LstatID(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return sighting{}, nil
	}
	if err != nil {
		return sighting{}, err
	}
	return sighting{id: id, dir: info.IsDir(), exists: true}, nil
}

func (s sighting) is(want cleanup.FileID) bool {
	return s.exists && s.dir && s.id == want
}

func (s sighting) String() string {
	switch {
	case !s.exists:
		return "absent"
	case s.dir:
		return "directory " + label(s.id)
	}
	return "non-directory " + label(s.id)
}

func label(id cleanup.FileID) string {
	return fmt.Sprintf("%d:%d", id.Dev, id.Ino)
}

type observation struct {
	original, registered, payload, admin sighting
	released                             bool
}

func observe(job *cleanup.Job, released bool) (observation, error) {
	seen := observation{released: released}
	var err error
	if !released {
		if seen.original, err = sight(job.Original); err != nil {
			return seen, err
		}
	}
	if seen.registered, err = sight(job.Registered); err != nil {
		return seen, err
	}
	if seen.payload, err = sight(job.Payload); err != nil {
		return seen, err
	}
	seen.admin, err = sight(job.AdminDir)
	return seen, err
}

func (o observation) describe(job *cleanup.Job) string {
	original := ""
	if !o.released {
		original = fmt.Sprintf("original %s is %s; ", job.Original, o.original)
	}
	return fmt.Sprintf(
		"%sregistered %s is %s; payload %s is %s; admin %s is %s; the job captured tree %s and admin %s",
		original, job.Registered, o.registered, job.Payload, o.payload, job.AdminDir, o.admin,
		label(job.Tree), label(job.Admin),
	)
}

func expectedLinks(job *cleanup.Job) (cleanup.Links, error) {
	jobDir, err := filepath.EvalSymlinks(filepath.Dir(job.Registered))
	if err != nil {
		return cleanup.Links{}, fmt.Errorf("resolve the job folder: %w", err)
	}
	return cleanup.Links{
		DotGit:      "gitdir: " + job.AdminDir + "\n",
		AdminGitdir: filepath.Join(jobDir, filepath.Base(job.Registered), ".git") + "\n",
	}, nil
}

func unclear(tree string, err error) string {
	return fmt.Sprintf("could not verify that %s is idle: %v", tree, err)
}

func (r *Relocator) held(ctx context.Context, job *cleanup.Job, tree string) string {
	err := r.unserved(ctx, tree)
	if err == nil || r.excused(ctx, job, tree, err) {
		return ""
	}
	var active *cleanup.ActiveError
	if errors.As(err, &active) {
		return active.Error()
	}
	return unclear(tree, err)
}

func (r *Relocator) block(ctx context.Context, job *cleanup.Job, reason, detail string) (bool, error) {
	if err := ctx.Err(); err != nil && reason != timeoutReason {
		return false, err
	}
	job.Block(r.cfg.Now(), reason, detail)
	return false, r.cfg.Journal.Save(*job)
}

func (r *Relocator) enter(job *cleanup.Job, phase cleanup.Phase) (bool, error) {
	job.Phase = phase
	job.Updated = r.cfg.Now()
	if !phase.Logical() {
		job.Retirement, job.RetireAfter = "", time.Time{}
	}
	if phase == cleanup.PhaseDone {
		if err := discardIndex(r.cfg.Journal.Layout().ScratchIndex(job.ID)); err != nil {
			slog.Warn("cleanup: leave a scratch index behind", "job", job.ID, "error", err)
		}
	}
	if err := r.cfg.Journal.Save(*job); err != nil {
		return false, err
	}
	return true, nil
}

// Advance proves job against its tree where it sits and claims the tree's
// registration in place, trusting the record only as far as the filesystem
// agrees. It never deletes or moves a tree and never runs git's repair or
// prune.
//
// A cancelled ctx returns its error and journals nothing, unless a git step
// expired its own budget, as a mutation can past the cancellation; then it
// journals a timeout blockage and returns nil.
func (r *Relocator) Advance(ctx context.Context, job *cleanup.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !job.Phase.Logical() {
		return nil
	}
	if job.Phase == cleanup.PhaseQueued || job.Phase == cleanup.PhaseWaiting {
		if proceed, err := r.release(ctx, job); err != nil || !proceed {
			return err
		}
	}
	claim := r.claim
	if job.Unbound() {
		claim = r.take
	}
	_, err := claim(ctx, job)
	return err
}

func (r *Relocator) release(ctx context.Context, job *cleanup.Job) (bool, error) {
	err := r.unretired(ctx, job)
	if err == nil {
		return true, nil
	}
	var active *cleanup.ActiveError
	if !errors.As(err, &active) {
		if _, found, locateErr := locate(job); locateErr == nil && !found {
			return true, nil
		}
		return r.block(ctx, job, "activity", unclear(job.Original, err))
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if job.Phase == cleanup.PhaseWaiting {
		return false, nil
	}
	now := r.cfg.Now()
	job.Phase = cleanup.PhaseWaiting
	job.Note(now, active.Error())
	job.Updated = now
	return false, r.cfg.Journal.Save(*job)
}

func headChanged(job *cleanup.Job, tree, head string) string {
	return fmt.Sprintf("HEAD changed: the job captured %q, %s now reads %q", job.Head, tree, head)
}

func (r *Relocator) retire(ctx context.Context, tree string) error {
	watchers, err := r.retiring(ctx, tree)
	if err != nil {
		return err
	}
	return r.cfg.Watchers.Retire(cleanup.WithRetiring(ctx, watchers.Retiring), tree)
}

func (r *Relocator) unlisted(ctx context.Context, job *cleanup.Job, reason string, err error) (string, string) {
	switch {
	case err == nil:
		return "", ""
	case !errors.Is(err, cleanup.ErrUnprobed):
		return reason, err.Error()
	}
	pushed, pushErr := r.headPushed(ctx, job)
	switch {
	case pushErr != nil:
		return gitReason(pushErr), pushErr.Error()
	case !pushed && job.Orphaned:
		return timeoutReason, err.Error()
	case !pushed:
		return timeoutReason, fmt.Sprintf("%v; no remote-tracking ref holds %s's head %s", err, job.Original, job.Head)
	}
	slog.Warn("cleanup: deleting a pushed tree whose watchers could not be listed", "job", job.ID, "tree", job.Original, "check", reason, "error", err)
	return "", ""
}

func (r *Relocator) excused(ctx context.Context, job *cleanup.Job, tree string, err error) bool {
	var active *cleanup.ActiveError
	if !errors.Is(err, cleanup.ErrUnprobed) || errors.As(err, &active) {
		return false
	}
	if tree == job.Original || tree == job.Registered {
		if _, err := readLinkage(filepath.Join(tree, ".git")); err != nil {
			return false
		}
		if dirt, dirtErr := r.dirt(ctx, job.Git, tree); dirtErr != nil || dirt != "" {
			return false
		}
	}
	if pushed, pushErr := r.headPushed(ctx, job); pushErr != nil || !pushed {
		return false
	}
	slog.Warn("cleanup: treating a clean, pushed tree as idle past a failed probe", "job", job.ID, "tree", tree, "error", err)
	return true
}
