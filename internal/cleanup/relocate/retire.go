package relocate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type retirementStage int

const (
	stageUnknown retirementStage = iota
	stageUntouched
	stageRepointed
	stageShelved
)

// Retire is Advance for a halted queue: it unregisters job's tree in place by
// moving only its admin directory out of git's registry, never moving or
// deleting the tree, and journals a refusal as Advance does. From
// PhaseRetiring it resumes as far as the filesystem agrees. It returns only a
// journal error or a cancelled ctx's error.
func (r *Relocator) Retire(ctx context.Context, job *cleanup.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !job.Phase.Retirable() {
		return fmt.Errorf("cleanup: job %s at %s cannot be retired in place", job.ID, job.Phase)
	}
	if !job.Phase.Untouched() {
		_, err := r.resume(ctx, job, nil)
		return err
	}
	if proceed, err := r.attest(ctx, job); err != nil || !proceed || job.Phase != cleanup.PhaseRetiring {
		return err
	}
	_, err := r.shed(ctx, job)
	return err
}

func (r *Relocator) attest(ctx context.Context, job *cleanup.Job) (bool, error) {
	current, err := identify(job.Original)
	var refused *cleanup.RefusedError
	switch {
	case errors.As(err, &refused):
		if tree, sightErr := sight(job.Original); sightErr == nil && !tree.exists {
			return r.vanished(ctx, job)
		}
		return r.block(ctx, job, refused.Reason, refused.Detail)
	case err != nil:
		return r.block(ctx, job, "identity", err.Error())
	case registrationOf(current) != registrationOf(*job):
		return r.block(ctx, job, "identity", replaced(job.Original, registrationOf(current), registrationOf(*job)))
	case current.Repo != job.Repo:
		return r.block(ctx, job, "identity", fmt.Sprintf("%s belongs to %s, the job captured %s", job.Original, current.Repo, job.Repo))
	case current.Links != job.Links:
		return r.block(ctx, job, "identity", drifted(job.Original, job.AdminDir, current.Links, job.Links))
	}
	current.Git = job.Git
	if proceed, err := r.steady(ctx, job, current); err != nil || !proceed {
		return proceed, err
	}
	if proceed, err := r.quiet(ctx, job, job.Deferred); err != nil || !proceed {
		return proceed, err
	}
	return r.enter(job, cleanup.PhaseRetiring)
}

func (r *Relocator) steady(ctx context.Context, job *cleanup.Job, current cleanup.Job) (bool, error) {
	if err := r.screen(ctx, current); err != nil {
		var refused *cleanup.RefusedError
		if errors.As(err, &refused) {
			return r.block(ctx, job, refused.Reason, refused.Detail)
		}
		return r.block(ctx, job, gitReason(err), err.Error())
	}
	head, err := r.head(ctx, job.Git, job.Original)
	if err != nil {
		return r.block(ctx, job, gitReason(err), err.Error())
	}
	if head != job.Head {
		return r.block(ctx, job, "identity", headChanged(job, job.Original, head))
	}
	branch, err := r.branch(ctx, job.Git, job.Original)
	if err != nil {
		return r.block(ctx, job, gitReason(err), err.Error())
	}
	if branch != job.Branch {
		return r.block(ctx, job, "identity", fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", job.Branch, job.Original, branch))
	}
	return true, nil
}

func (r *Relocator) quiet(ctx context.Context, job *cleanup.Job, rest bool) (bool, error) {
	watchers, err := r.retiring(ctx, job.Original)
	switch {
	case errors.Is(err, cleanup.ErrUnprobed):
		slog.Warn("cleanup: guarding a tree whose watchers could not be listed", "tree", job.Original, "error", err)
	case err != nil:
		return r.block(ctx, job, "activity", unclear(job.Original, err))
	}
	guarded, ignored := cleanup.WithIgnored(cleanup.WithRetiring(ctx, watchers))
	verdict := r.cfg.Guard(guarded, job.Original)
	if verdict == nil {
		verdict = r.cfg.Guard(guarded, job.AdminDir)
	}
	job.Ignored = *ignored
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var active *cleanup.ActiveError
	switch {
	case verdict == nil:
		return true, nil
	case !errors.As(verdict, &active):
		return r.block(ctx, job, "activity", unclear(job.Original, verdict))
	case !rest:
		return r.block(ctx, job, "activity", active.Error())
	case job.Phase == cleanup.PhaseWaiting:
		return false, nil
	}
	now := r.cfg.Now()
	job.Phase = cleanup.PhaseWaiting
	job.Note(now, active.Error())
	job.Updated = now
	return false, r.cfg.Journal.Save(*job)
}

func (r *Relocator) resume(ctx context.Context, job *cleanup.Job, _ *bool) (bool, error) {
	seen, err := retirementOf(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	switch seen.stage(job) {
	case stageRepointed:
		if err := restore(job); err != nil {
			return r.block(ctx, job, "reconcile", err.Error())
		}
		fallthrough
	case stageUntouched:
		if proceed, err := r.steady(ctx, job, *job); err != nil || !proceed {
			return proceed, err
		}
		if proceed, err := r.quiet(ctx, job, false); err != nil || !proceed {
			return proceed, err
		}
	case stageShelved:
	default:
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	return r.shed(ctx, job)
}

type retirement struct {
	tree, admin, retired sighting
	pointer              string
}

func retiredPointer(job *cleanup.Job) string {
	return "gitdir: " + cleanup.RetiredGitDirFor(job.Repo, job.ID) + "\n"
}

func retirementOf(job *cleanup.Job) (retirement, error) {
	var (
		seen retirement
		err  error
	)
	if seen.tree, err = sight(job.Original); err != nil {
		return seen, err
	}
	if seen.tree.is(job.Tree) {
		if seen.pointer, err = readLinkage(filepath.Join(job.Original, ".git")); err != nil && !gone(err) {
			return seen, err
		}
	}
	if seen.admin, err = sight(job.AdminDir); err != nil {
		return seen, err
	}
	seen.retired, err = sight(cleanup.RetiredGitDirFor(job.Repo, job.ID))
	return seen, err
}

func (s retirement) stage(job *cleanup.Job) retirementStage {
	switch {
	case !s.tree.is(job.Tree):
		return stageUnknown
	case s.admin.is(job.Admin) && !s.retired.exists && s.pointer == job.Links.DotGit:
		return stageUntouched
	case s.admin.is(job.Admin) && !s.retired.exists && s.pointer == retiredPointer(job):
		return stageRepointed
	case s.retired.is(job.Admin) && !s.admin.is(job.Admin) && s.pointer == retiredPointer(job):
		return stageShelved
	}
	return stageUnknown
}

func (s retirement) describe(job *cleanup.Job) string {
	return fmt.Sprintf(
		"original %s is %s reading %q; admin %s is %s; retired git dir %s is %s; the job captured tree %s and admin %s",
		job.Original, s.tree, s.pointer, job.AdminDir, s.admin,
		cleanup.RetiredGitDirFor(job.Repo, job.ID), s.retired,
		label(job.Tree), label(job.Admin),
	)
}

func (r *Relocator) shed(ctx context.Context, job *cleanup.Job) (bool, error) {
	seen, err := retirementOf(job)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	switch seen.stage(job) {
	case stageUntouched:
		if detail := repoint(job); detail != "" {
			return r.block(ctx, job, "reconcile", detail)
		}
		fallthrough
	case stageRepointed:
		if detail := shelve(job); detail != "" {
			return r.block(ctx, job, "reconcile", detail)
		}
	case stageShelved:
	default:
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	if reason, detail := r.verifyShed(ctx, job); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	return r.enter(job, cleanup.PhaseRetired)
}

func fenced(job *cleanup.Job, pointer string) string {
	tree, err := sight(job.Original)
	switch {
	case err != nil:
		return err.Error()
	case !tree.is(job.Tree):
		return fmt.Sprintf("%s is %s, the job captured tree %s", job.Original, tree, label(job.Tree))
	}
	dotGit := filepath.Join(job.Original, ".git")
	current, err := readLinkage(dotGit)
	switch {
	case err != nil:
		return err.Error()
	case current != pointer:
		return fmt.Sprintf("%s reads %q, want %q", dotGit, current, pointer)
	}
	return ""
}

func captured(job *cleanup.Job, adminDir string) string {
	admin, err := sight(adminDir)
	switch {
	case err != nil:
		return err.Error()
	case !admin.is(job.Admin):
		return fmt.Sprintf("%s is %s, the job captured admin %s", adminDir, admin, label(job.Admin))
	}
	back, err := readLinkage(filepath.Join(adminDir, "gitdir"))
	switch {
	case err != nil:
		return err.Error()
	case back != job.Links.AdminGitdir:
		return fmt.Sprintf("%s reads %q, want %q", filepath.Join(adminDir, "gitdir"), back, job.Links.AdminGitdir)
	}
	if common, err := commonOf(adminDir); err != nil || common != job.Repo {
		return fmt.Sprintf("%s reaches common dir %q, want %s (%v)", adminDir, common, job.Repo, err)
	}
	return ""
}

func vacant(path string) string {
	seen, err := sight(path)
	switch {
	case err != nil:
		return err.Error()
	case seen.exists:
		return fmt.Sprintf("%s is %s, want it absent", path, seen)
	}
	return ""
}

func repoint(job *cleanup.Job) string {
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	if err := durable.Mkdir(filepath.Dir(retired), 0o755); err != nil {
		return fmt.Sprintf("create %s: %v", filepath.Dir(retired), err)
	}
	if d := fenced(job, job.Links.DotGit); d != "" {
		return d
	}
	if d := captured(job, job.AdminDir); d != "" {
		return d
	}
	if d := vacant(retired); d != "" {
		return d
	}
	if err := swap(filepath.Join(job.Original, ".git"), job.Links.DotGit, retiredPointer(job)); err != nil {
		return err.Error()
	}
	return ""
}

func shelve(job *cleanup.Job) string {
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	d := fenced(job, retiredPointer(job))
	if d == "" {
		d = captured(job, job.AdminDir)
	}
	if d == "" {
		d = vacant(retired)
	}
	if d != "" {
		return d + "; " + undone(job)
	}
	renameErr := durable.Rename(job.AdminDir, retired)
	moved, err := sight(retired)
	switch {
	case err != nil:
		return fmt.Sprintf("rename %s to %s: %v; inspect %s: %v; reconcile by hand", job.AdminDir, retired, renameErr, retired, err)
	case moved.is(job.Admin) && renameErr != nil:
		return fmt.Sprintf("rename %s to %s: %v; the admin directory is at %s", job.AdminDir, retired, renameErr, retired)
	case moved.is(job.Admin):
		return ""
	case !moved.exists:
		return fmt.Sprintf("rename %s to %s: %v; %s", job.AdminDir, retired, renameErr, undone(job))
	}
	return evict(job, moved)
}

func evict(job *cleanup.Job, moved sighting) string {
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	detail := fmt.Sprintf("%s is %s after the rename from %s, the job captured admin %s", retired, moved, job.AdminDir, label(job.Admin))
	home := job.AdminDir
	if left, err := sight(job.AdminDir); err != nil || left.exists {
		aside, err := os.MkdirTemp(filepath.Dir(retired), "."+job.ID+".foreign-")
		if err != nil {
			return fmt.Sprintf("%s; setting it aside failed: %v, so it stays at %s, which %s names; reconcile by hand", detail, err, retired, filepath.Join(job.Original, ".git"))
		}
		home = filepath.Join(aside, filepath.Base(job.AdminDir))
	}
	if err := durable.Rename(retired, home); err != nil {
		return fmt.Sprintf("%s; moving it to %s failed: %v, so it stays at %s, which %s names; reconcile by hand", detail, home, err, retired, filepath.Join(job.Original, ".git"))
	}
	return fmt.Sprintf("%s; it is at %s, and %s names the empty %s; reconcile by hand", detail, home, filepath.Join(job.Original, ".git"), retired)
}

func restore(job *cleanup.Job) error {
	dotGit := filepath.Join(job.Original, ".git")
	if d := captured(job, job.AdminDir); d != "" {
		return fmt.Errorf("%s, so %s keeps naming %s", d, dotGit, cleanup.RetiredGitDirFor(job.Repo, job.ID))
	}
	if d := fenced(job, retiredPointer(job)); d != "" {
		return fmt.Errorf("%s; %s still holds the admin directory", d, job.AdminDir)
	}
	if err := swap(dotGit, retiredPointer(job), job.Links.DotGit); err != nil {
		return fmt.Errorf("restoring %s failed, so it still names %s: %w", dotGit, cleanup.RetiredGitDirFor(job.Repo, job.ID), err)
	}
	return nil
}

func undone(job *cleanup.Job) string {
	if err := restore(job); err != nil {
		return err.Error() + "; reconcile by hand"
	}
	return fmt.Sprintf("%s names %s again", filepath.Join(job.Original, ".git"), job.AdminDir)
}

func swap(path, from, to string) error {
	current, err := readLinkage(path)
	if err != nil {
		return err
	}
	if current != from {
		return fmt.Errorf("%s reads %q, want %q", path, current, from)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := durable.WriteFile(path, []byte(to), info.Mode().Perm()); err != nil {
		return fmt.Errorf("rewrite %s: %w", path, err)
	}
	return nil
}

func commonOf(gitDir string) (string, error) {
	content, err := readLinkage(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", err
	}
	named, ok := linkTarget(content, gitDir)
	if !ok {
		return "", fmt.Errorf("%s names no common directory", filepath.Join(gitDir, "commondir"))
	}
	return filepath.EvalSymlinks(named)
}

func (r *Relocator) through(ctx context.Context, job *cleanup.Job, gitDir string) (reason, detail string) {
	out, err := r.git(ctx, job.Git, "-C", job.Original, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return gitReason(err), err.Error()
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "reconcile", fmt.Sprintf("git rev-parse in %s printed %q, want a git dir and a common dir", job.Original, out)
	}
	reached, errDir := filepath.EvalSymlinks(lines[0])
	common, errCommon := filepath.EvalSymlinks(lines[1])
	want, errWant := filepath.EvalSymlinks(gitDir)
	if err := errors.Join(errDir, errCommon, errWant); err != nil || reached != want || common != job.Repo {
		return "reconcile", fmt.Sprintf("%s reaches git dir %s and common dir %s, want %s and %s (%v)", job.Original, lines[0], lines[1], gitDir, job.Repo, err)
	}
	return "", ""
}

func (r *Relocator) verifyShed(ctx context.Context, job *cleanup.Job) (reason, detail string) {
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	if d := captured(job, retired); d != "" {
		return "reconcile", d
	}
	if reason, detail := r.through(ctx, job, retired); reason != "" {
		return reason, detail
	}
	head, err := r.head(ctx, job.Git, job.Original)
	if err != nil {
		return gitReason(err), err.Error()
	}
	if head != job.Head {
		return "reconcile", headChanged(job, job.Original, head)
	}
	entries, err := registered(job.Repo, job.Original)
	if err != nil {
		return "reconcile", err.Error()
	}
	if len(entries) > 0 {
		return "reconcile", fmt.Sprintf("%s still registers %s", entries[0].admin, entries[0].names)
	}
	return "", ""
}

func (r *Relocator) reclaim(ctx context.Context, job *cleanup.Job, vetted *bool) (bool, error) {
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	tree, err := sight(job.Original)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !tree.exists {
		return r.landed(ctx, job)
	}
	if d := fenced(job, retiredPointer(job)); d != "" {
		return r.block(ctx, job, "identity", d)
	}
	if d := captured(job, retired); d != "" {
		return r.block(ctx, job, "identity", d)
	}
	entries, err := registered(job.Repo, job.Original)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if len(entries) > 0 {
		return r.block(ctx, job, "identity", fmt.Sprintf("%s registers %s, inside the retired tree %s", entries[0].admin, entries[0].names, job.Original))
	}
	current := *job
	current.AdminDir = retired
	if proceed, err := r.steady(ctx, job, current); err != nil || !proceed {
		return proceed, err
	}
	if reason, detail := r.unlisted(ctx, job, vetted, "watchers", r.retire(ctx, job.Original)); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	if reason, detail := r.unlisted(ctx, job, vetted, "quarantine", r.cfg.Watchers.CheckQuarantine(ctx, r.cfg.Journal.Layout().JobDir(job.ID))); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	for _, path := range []string{job.Original, retired} {
		if holders := r.held(ctx, job, path); holders != "" {
			return r.block(ctx, job, "activity", holders)
		}
	}
	if reason, detail := r.vet(ctx, job, job.Original, vetted); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	if job.Head != "" {
		if _, err := r.rewrite(ctx, job.Git, "--git-dir="+job.Repo, "update-ref", job.RecoveryRef, job.Head); err != nil {
			return r.block(ctx, job, gitReason(err), err.Error())
		}
	}
	seen, err := observe(job, false)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if !seen.original.is(job.Tree) || seen.registered.exists || seen.payload.exists {
		return r.block(ctx, job, "reconcile", seen.describe(job))
	}
	if err := durable.Rename(job.Original, job.Payload); err != nil {
		return r.block(ctx, job, "reconcile", fmt.Sprintf("rename %s to %s: %v", job.Original, job.Payload, err))
	}
	return r.landed(ctx, job)
}

func (r *Relocator) landed(ctx context.Context, job *cleanup.Job) (bool, error) {
	payload, err := sight(job.Payload)
	switch {
	case err != nil:
		return r.block(ctx, job, "reconcile", err.Error())
	case payload.is(job.Tree):
		retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
		seen, err := sight(retired)
		if err != nil {
			return r.block(ctx, job, "reconcile", err.Error())
		}
		if seen.exists && !seen.is(job.Admin) {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("%s is %s, the job captured admin %s", retired, seen, label(job.Admin)))
		}
		return r.enter(job, cleanup.PhaseUnregistered)
	case payload.exists:
		return r.block(ctx, job, "reconcile", fmt.Sprintf("%s is gone and payload %s is %s, the job captured tree %s", job.Original, job.Payload, payload, label(job.Tree)))
	}
	retired := cleanup.RetiredGitDirFor(job.Repo, job.ID)
	if d := vacant(retired); d != "" {
		return r.block(ctx, job, "identity", fmt.Sprintf("%s is gone, but %s; whatever moved the tree may still use it, so remove it by hand, then ccx vcs cleanup retry finishes the job", job.Original, d))
	}
	return r.enter(job, cleanup.PhaseDone)
}
