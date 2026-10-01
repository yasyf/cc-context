package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yasyf/daemonkit/durable"

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

func (o observation) untouched(job *cleanup.Job) bool {
	return o.original.is(job.Tree) && !o.registered.exists && !o.payload.exists && o.admin.is(job.Admin)
}

func (o observation) relocated(job *cleanup.Job) bool {
	return !o.original.is(job.Tree) && o.registered.is(job.Tree) && !o.payload.exists && o.admin.is(job.Admin)
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

func readLinks(tree, adminDir string) (cleanup.Links, error) {
	dotGit, err := readLinkage(filepath.Join(tree, ".git"))
	if err != nil {
		return cleanup.Links{}, err
	}
	adminGitdir, err := readLinkage(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return cleanup.Links{}, err
	}
	return cleanup.Links{DotGit: dotGit, AdminGitdir: adminGitdir}, nil
}

func linkDrift(tree, adminDir string, want cleanup.Links) string {
	links, err := readLinks(tree, adminDir)
	switch {
	case err != nil:
		return err.Error()
	case links.DotGit != want.DotGit:
		return fmt.Sprintf("%s reads %q, want %q", filepath.Join(tree, ".git"), links.DotGit, want.DotGit)
	case links.AdminGitdir != want.AdminGitdir:
		return fmt.Sprintf("%s reads %q, want %q", filepath.Join(adminDir, "gitdir"), links.AdminGitdir, want.AdminGitdir)
	}
	return ""
}

func unclear(tree string, err error) string {
	return fmt.Sprintf("could not verify that %s is idle: %v", tree, err)
}

func parked(job *cleanup.Job, detail string) string {
	return fmt.Sprintf(
		"%s; the tree is parked at %s, still registered; `git worktree move %s %s` restores it",
		detail, job.Registered, job.Registered, job.Original,
	)
}

func (r *Relocator) held(ctx context.Context, job *cleanup.Job, tree string) string {
	err := r.cfg.Guard(ctx, tree)
	if err == nil || r.excused(ctx, job, tree, err) {
		return ""
	}
	var active *cleanup.ActiveError
	if errors.As(err, &active) {
		return active.Error()
	}
	return unclear(tree, err)
}

func (r *Relocator) vet(ctx context.Context, job *cleanup.Job, tree string, vetted *bool) (reason, detail string) {
	if job.Force || *vetted {
		return "", ""
	}
	dirt, err := r.dirt(ctx, job.Git, tree)
	if err != nil {
		return "git", err.Error()
	}
	if dirt != "" {
		return "dirty", dirt
	}
	*vetted = true
	return "", ""
}

func (r *Relocator) block(ctx context.Context, job *cleanup.Job, reason, detail string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	job.Block(r.cfg.Now(), reason, detail)
	return false, r.cfg.Journal.Save(*job)
}

func (r *Relocator) enter(job *cleanup.Job, phase cleanup.Phase) (bool, error) {
	job.Phase = phase
	job.Updated = r.cfg.Now()
	if err := r.cfg.Journal.Save(*job); err != nil {
		return false, err
	}
	return true, nil
}

// Advance resumes job from the phase its record holds, trusting that phase only
// as far as the filesystem agrees with it. It never deletes a tree, never
// touches the original path once the tree has left it, and never runs git's
// repair or prune. A tree it did not find clean earlier in the same call is
// checked for uncommitted changes again before the move and before the
// detaching rename, unless the job is forced. The detaching rename waits on
// the registry naming nothing in the job folder but the parked tree itself,
// and PhaseUnregistered is published only once the registry names nothing at
// or inside the job folder at all, and nothing the move carried along with the
// tree. A cancelled ctx returns ctx's error, whatever phase the call reached,
// and journals no blockage.
func (r *Relocator) Advance(ctx context.Context, job *cleanup.Job) error {
	vetted := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var step func(context.Context, *cleanup.Job, *bool) (bool, error)
		switch job.Phase {
		case cleanup.PhaseQueued, cleanup.PhaseWaiting:
			step = r.release
		case cleanup.PhasePrepared:
			step = r.relocate
			if job.Adopted {
				step = r.take
			}
		case cleanup.PhaseMoved:
			step = r.detach
		case cleanup.PhaseDetached:
			step = r.unregister
		default:
			return nil
		}
		proceed, err := step(ctx, job, &vetted)
		if err != nil || !proceed {
			return err
		}
	}
}

func (r *Relocator) release(ctx context.Context, job *cleanup.Job, vetted *bool) (bool, error) {
	if err := r.unretired(ctx, job); err != nil {
		var active *cleanup.ActiveError
		if !errors.As(err, &active) {
			if tree, sightErr := sight(job.Original); sightErr == nil && !tree.exists {
				return r.vanished(ctx, job)
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

	seen, err := observe(job, false)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !seen.original.exists {
		return r.vanished(ctx, job)
	}
	if !seen.original.is(job.Tree) || !seen.admin.is(job.Admin) {
		return r.block(ctx, job, "identity", seen.describe(job))
	}
	if drift := linkDrift(job.Original, job.AdminDir, job.Links); drift != "" {
		return r.block(ctx, job, "identity", drift)
	}
	head, err := r.head(ctx, job.Git, job.Original)
	if err != nil {
		return r.block(ctx, job, "git", err.Error())
	}
	if head != job.Head {
		return r.block(ctx, job, "identity", headChanged(job, job.Original, head))
	}
	if reason, detail := r.vet(ctx, job, job.Original, vetted); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	return r.enter(job, cleanup.PhasePrepared)
}

// vanished settles a deferred removal whose tree left its path by other hands:
// with its registration gone too there is nothing left to remove, and with the
// registration still there only git worktree prune can drop it.
func (r *Relocator) vanished(ctx context.Context, job *cleanup.Job) (bool, error) {
	admin, err := sight(job.AdminDir)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if admin.exists {
		return r.block(ctx, job, "identity", fmt.Sprintf("%s is gone, but git still registers it at %s; git worktree prune drops the registration, then ccx vcs cleanup retry finishes the job", job.Original, job.AdminDir))
	}
	return r.enter(job, cleanup.PhaseDone)
}

func headChanged(job *cleanup.Job, tree, head string) string {
	return fmt.Sprintf("HEAD changed: the job captured %q, %s now reads %q", job.Head, tree, head)
}

func (r *Relocator) relocate(ctx context.Context, job *cleanup.Job, vetted *bool) (bool, error) {
	expected, err := expectedLinks(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	seen, err := observe(job, false)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if seen.untouched(job) {
		if drift := linkDrift(job.Original, job.AdminDir, job.Links); drift != "" {
			return r.block(ctx, job, "reconcile", drift)
		}
		if reason, detail := r.move(ctx, job, vetted); reason != "" {
			return r.block(ctx, job, reason, detail)
		}
		if seen, err = observe(job, false); err != nil {
			return r.block(ctx, job, "reconcile", err.Error())
		}
	}
	if !seen.relocated(job) {
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	if err := settleLinks(job, expected); err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	return r.enter(job, cleanup.PhaseMoved)
}

func (r *Relocator) move(ctx context.Context, job *cleanup.Job, vetted *bool) (reason, detail string) {
	nested, err := nestedWorktree(job.Repo, job.AdminDir, job.Original)
	if err != nil {
		return "reconcile", err.Error()
	}
	if nested != "" {
		return "reconcile", fmt.Sprintf("worktree %s is registered inside %s", nested, job.Original)
	}
	if reason, detail := r.unlisted(ctx, job, vetted, "watchers", r.retire(ctx, job.Original)); reason != "" {
		return reason, detail
	}
	if reason, detail := r.unlisted(ctx, job, vetted, "quarantine", r.cfg.Watchers.CheckQuarantine(ctx, r.cfg.Journal.Layout().JobDir(job.ID))); reason != "" {
		return reason, detail
	}
	if holders := r.held(ctx, job, job.Original); holders != "" {
		return "activity", holders
	}
	if reason, detail := r.vet(ctx, job, job.Original, vetted); reason != "" {
		return reason, detail
	}

	rewriting := context.WithoutCancel(ctx)
	if job.Head != "" {
		if _, err := r.git(rewriting, job.Git, "--git-dir="+job.Repo, "update-ref", job.RecoveryRef, job.Head); err != nil {
			return "git", err.Error()
		}
	}
	tree, err := sight(job.Original)
	if err != nil {
		return "identity", err.Error()
	}
	if !tree.is(job.Tree) {
		return "identity", fmt.Sprintf("%s is %s, the job captured tree %s", job.Original, tree, label(job.Tree))
	}
	_, err = r.git(rewriting, job.Git, "--git-dir="+job.Repo, "-c", "worktree.useRelativePaths=false", "worktree", "move", job.Original, job.Registered)
	if err == nil {
		return "", ""
	}
	seen, seeErr := observe(job, false)
	if seeErr == nil && seen.untouched(job) && linkDrift(job.Original, job.AdminDir, job.Links) == "" {
		return "git", err.Error()
	}
	slog.Warn("cleanup: git worktree move failed after the tree left its original state", "job", job.ID, "error", err)
	return "", ""
}

func (r *Relocator) retire(ctx context.Context, tree string) error {
	watchers, err := r.retiring(ctx, tree)
	if err != nil {
		return err
	}
	return r.cfg.Watchers.Retire(cleanup.WithRetiring(ctx, watchers), tree)
}

func (r *Relocator) unlisted(ctx context.Context, job *cleanup.Job, vetted *bool, reason string, err error) (string, string) {
	switch {
	case err == nil:
		return "", ""
	case !errors.Is(err, cleanup.ErrUnprobed):
		return reason, err.Error()
	}
	pushed, pushErr := r.pushed(ctx, job.Git, job.Repo, job.Branch, job.Head)
	switch {
	case pushErr != nil:
		return "git", pushErr.Error()
	case !pushed:
		return reason, fmt.Sprintf("%v; no remote-tracking ref holds %s's head %s", err, job.Original, job.Head)
	}
	*vetted = false
	slog.Warn("cleanup: moving a pushed tree whose watchers could not be listed", "job", job.ID, "tree", job.Original, "check", reason, "error", err)
	return "", ""
}

func (r *Relocator) excused(ctx context.Context, job *cleanup.Job, tree string, err error) bool {
	var active *cleanup.ActiveError
	if !errors.Is(err, cleanup.ErrUnprobed) || errors.As(err, &active) {
		return false
	}
	if dirt, dirtErr := r.dirt(ctx, job.Git, tree); dirtErr != nil || dirt != "" {
		return false
	}
	if pushed, pushErr := r.pushed(ctx, job.Git, job.Repo, job.Branch, job.Head); pushErr != nil || !pushed {
		return false
	}
	slog.Warn("cleanup: treating a clean, pushed tree as idle past a failed probe", "job", job.ID, "tree", tree, "error", err)
	return true
}

func settleLinks(job *cleanup.Job, expected cleanup.Links) error {
	dotGitPath, adminGitdirPath := filepath.Join(job.Registered, ".git"), filepath.Join(job.AdminDir, "gitdir")
	links, err := readLinks(job.Registered, job.AdminDir)
	if err != nil {
		return err
	}
	if links.DotGit != expected.DotGit && links.DotGit != job.Links.DotGit {
		return fmt.Errorf("%s reads %q, neither the captured %q nor the expected %q", dotGitPath, links.DotGit, job.Links.DotGit, expected.DotGit)
	}
	if links.AdminGitdir != expected.AdminGitdir && links.AdminGitdir != job.Links.AdminGitdir {
		return fmt.Errorf("%s reads %q, neither the captured %q nor the expected %q", adminGitdirPath, links.AdminGitdir, job.Links.AdminGitdir, expected.AdminGitdir)
	}
	if links.AdminGitdir != expected.AdminGitdir {
		if err := durable.WriteFile(adminGitdirPath, []byte(expected.AdminGitdir), 0o644); err != nil {
			return fmt.Errorf("rewrite %s: %w", adminGitdirPath, err)
		}
	}
	if links.DotGit != expected.DotGit {
		if err := durable.WriteFile(dotGitPath, []byte(expected.DotGit), 0o644); err != nil {
			return fmt.Errorf("rewrite %s: %w", dotGitPath, err)
		}
	}
	return nil
}

func (r *Relocator) detach(ctx context.Context, job *cleanup.Job, vetted *bool) (bool, error) {
	expected, err := expectedLinks(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	seen, err := observe(job, true)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if !seen.registered.exists && seen.payload.is(job.Tree) {
		return r.enter(job, cleanup.PhaseDetached)
	}
	if !seen.registered.is(job.Tree) || seen.payload.exists {
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}

	if !seen.admin.is(job.Admin) {
		return r.block(ctx, job, "identity", seen.describe(job))
	}
	if drift := linkDrift(job.Registered, job.AdminDir, expected); drift != "" {
		return r.block(ctx, job, "identity", drift)
	}
	head, err := r.head(ctx, job.Git, job.Registered)
	if err != nil {
		return r.block(ctx, job, "git", err.Error())
	}
	if head != job.Head {
		return r.block(ctx, job, "identity", headChanged(job, job.Registered, head))
	}
	if holders := r.held(ctx, job, job.Registered); holders != "" {
		return r.block(ctx, job, "activity", parked(job, holders))
	}
	if reason, detail := r.vet(ctx, job, job.Registered, vetted); reason != "" {
		return r.block(ctx, job, reason, parked(job, detail))
	}
	claim, err := intruder(job, expected)
	if err != nil {
		return r.block(ctx, job, "reconcile", parked(job, err.Error()))
	}
	if claim != "" {
		return r.block(ctx, job, "reconcile", parked(job, claim))
	}
	if seen, err = observe(job, true); err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if !seen.registered.is(job.Tree) || seen.payload.exists {
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	if err := durable.Rename(job.Registered, job.Payload); err != nil {
		return r.block(ctx, job, "reconcile", fmt.Sprintf("rename %s to %s: %v", job.Registered, job.Payload, err))
	}
	return r.enter(job, cleanup.PhaseDetached)
}

func parkedEntry(job *cleanup.Job, expected cleanup.Links) entry {
	return entry{admin: job.AdminDir, names: strings.TrimSuffix(expected.AdminGitdir, "\n")}
}

func (r *Relocator) unregister(ctx context.Context, job *cleanup.Job, _ *bool) (bool, error) {
	expected, err := expectedLinks(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	seen, err := observe(job, true)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if seen.registered.exists || !seen.payload.is(job.Tree) {
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	registry, err := enrolled(job.Repo)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	entries, err := reaching(registry, filepath.Dir(job.Payload))
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	for _, e := range entries {
		if !seen.admin.is(job.Admin) || e != parkedEntry(job, expected) {
			return r.block(ctx, job, "reconcile", e.claim(job))
		}
	}
	stowaway, err := carried(job, registry, job.Payload)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if stowaway != "" {
		return r.block(ctx, job, "reconcile", stowaway)
	}
	if seen.admin.is(job.Admin) {
		adminGitdirPath := filepath.Join(job.AdminDir, "gitdir")
		if _, err := readLinkage(adminGitdirPath); errors.Is(err, fs.ErrNotExist) {
			return r.block(ctx, job, "reconcile", fmt.Sprintf(
				"admin %s is still the directory %s the job captured but %s is gone: an interrupted removal left the registration half-deleted; remove %s to finish dropping it",
				job.AdminDir, label(job.Admin), adminGitdirPath, job.AdminDir,
			))
		}
	}
	if len(entries) > 0 {
		if _, err := r.git(context.WithoutCancel(ctx), job.Git, "--git-dir="+job.Repo, "worktree", "remove", job.Registered); err != nil {
			return r.block(ctx, job, "git", err.Error())
		}
	}
	return r.publish(ctx, job)
}

func (r *Relocator) publish(ctx context.Context, job *cleanup.Job) (bool, error) {
	claim, err := claimed(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if claim != "" {
		return r.block(ctx, job, "reconcile", claim)
	}
	return r.enter(job, cleanup.PhaseUnregistered)
}
