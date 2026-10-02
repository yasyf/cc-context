package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yasyf/daemonkit/durable"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Adopt journals the tree the retired janitor parked at req.Source at
// PhasePrepared, moving nothing. It takes only the inode the request names,
// and only while the legacy ref still pins the head the request carries; the
// ref itself is never written. The registry proof covers req.CommonDir alone:
// no admin entry of that repository names Source or anything inside it, and
// Source's own .git is neither a repository nor mutually linked to an admin
// directory. A registration another repository holds on a path inside Source
// is not looked for.
func (r *Relocator) Adopt(ctx context.Context, seq uint64, req cleanup.AdoptRequest) (cleanup.Job, error) {
	if err := req.Validate(); err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: adopt request: %w", err)
	}
	source := req.Source
	resolved, err := filepath.EvalSymlinks(source)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cleanup.Job{}, refuse(source, "identity", "%s does not exist", source)
	case err != nil:
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", source, err)
	case resolved != source:
		return cleanup.Job{}, refuse(source, "identity", "%s is not canonical: it resolves to %s", source, resolved)
	}
	tree, err := sight(source)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", source, err)
	}
	if !tree.is(req.Tree) {
		return cleanup.Job{}, refuse(source, "identity", "%s is %s, not the tree %s the request names", source, tree, label(req.Tree))
	}

	repo, err := gitDir(source, req.CommonDir)
	if err != nil {
		return cleanup.Job{}, err
	}
	reason, detail, err := registration(repo, source)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", source, err)
	}
	if reason != "" {
		return cleanup.Job{}, refuse(source, reason, "%s", detail)
	}

	detail, err = r.misplaced(source, repo)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", source, err)
	}
	if detail != "" {
		return cleanup.Job{}, refuse(source, "mismatch", "%s", detail)
	}

	detail, err = r.unpinned(ctx, req.Git, repo, req.RecoveryRef, req.Head)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", repo, err)
	}
	if detail != "" {
		return cleanup.Job{}, refuse(source, "recovery", "%s", detail)
	}
	if err := r.colocated(source, req.Tree); err != nil {
		return cleanup.Job{}, err
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, source); err != nil {
		return cleanup.Job{}, refuse(source, "watched", "%v", err)
	}
	if err := idle(source, r.cfg.Guard(ctx, source)); err != nil {
		return cleanup.Job{}, err
	}

	job := cleanup.Job{
		Adopted:     true,
		Source:      source,
		Owner:       req.Owner,
		Repo:        repo,
		Original:    req.Original,
		Tree:        req.Tree,
		Head:        req.Head,
		RecoveryRef: req.RecoveryRef,
		Git:         req.Git,
	}
	r.stamp(&job, seq, cleanup.PhasePrepared)
	if err := r.cfg.Journal.Create(job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}

func gitDir(source, commonDir string) (string, error) {
	repo, err := filepath.EvalSymlinks(commonDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", refuse(source, "mismatch", "%s is not a git directory: it does not exist", commonDir)
	}
	if err != nil {
		return "", fmt.Errorf("cleanup: inspect %s: %w", commonDir, err)
	}
	for _, name := range []string{"HEAD", "objects"} {
		_, err := os.Stat(filepath.Join(repo, name))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", refuse(source, "mismatch", "%s is not a git directory: it has no %s", repo, name)
		}
		if err != nil {
			return "", fmt.Errorf("cleanup: inspect %s: %w", repo, err)
		}
	}
	return repo, nil
}

func (r *Relocator) misplaced(source, repo string) (string, error) {
	root := r.cfg.Journal.Layout().Root
	overlap := fmt.Sprintf("%s overlaps the cleanup folder %s", source, root)
	for _, nesting := range []struct{ inner, outer, detail string }{
		{source, root, overlap},
		{root, source, overlap},
		{repo, source, fmt.Sprintf("%s holds the repository %s", source, repo)},
		{source, repo, fmt.Sprintf("%s lies inside the repository %s", source, repo)},
	} {
		outer, err := os.Stat(nesting.outer)
		if err != nil {
			return "", err
		}
		below, err := descends(nesting.inner, []cleanup.FileID{cleanup.IDOf(outer)})
		if err != nil {
			return "", err
		}
		if below {
			return nesting.detail, nil
		}
	}
	return "", nil
}

func holds(dir string, names ...string) (bool, error) {
	for _, name := range names {
		_, err := os.Lstat(filepath.Join(dir, name))
		if gone(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func repository(gitDir string) (bool, error) {
	linked, err := holds(gitDir, "commondir")
	if err != nil || linked {
		return false, err
	}
	return holds(gitDir, "HEAD")
}

func registration(repo, source string) (reason, detail string, err error) {
	dotGitPath := filepath.Join(source, ".git")
	info, err := os.Stat(dotGitPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		bare, err := holds(source, "HEAD", "objects")
		if err != nil {
			return "", "", err
		}
		if bare {
			return "main", fmt.Sprintf("%s is a git directory, not a parked worktree", source), nil
		}
	case err != nil:
		return "", "", err
	case info.IsDir():
		return "main", fmt.Sprintf("%s is a repository's main working copy, not a parked worktree", source), nil
	default:
		gitDir, err := pointed(source)
		if err != nil {
			return "", "", err
		}
		if gitDir != "" {
			own, err := repository(gitDir)
			if err != nil {
				return "", "", err
			}
			if own {
				return "main", fmt.Sprintf("%s is the working copy of the repository at %s, not a parked worktree", source, gitDir), nil
			}
		}
		adminDir, err := liveLink(source)
		if err != nil {
			return "", "", err
		}
		if adminDir != "" {
			return "registered", fmt.Sprintf("%s is still a registered worktree: %s links back to %s", source, adminDir, dotGitPath), nil
		}
	}
	entries, err := registered(repo, source)
	if err != nil {
		return "", "", err
	}
	if len(entries) > 0 {
		return "registered", fmt.Sprintf("%s still registers a worktree at %s", repo, filepath.Dir(entries[0].names)), nil
	}
	return "", "", nil
}

func pointed(tree string) (string, error) {
	dotGit, err := readLinkage(filepath.Join(tree, ".git"))
	if err != nil {
		return "", err
	}
	pointer, ok := strings.CutPrefix(dotGit, "gitdir: ")
	if !ok {
		return "", nil
	}
	gitDir, _ := linkTarget(pointer, tree)
	return gitDir, nil
}

func liveLink(source string) (string, error) {
	adminDir, err := pointed(source)
	if err != nil || adminDir == "" {
		return "", err
	}
	adminGitdir, err := readLinkage(filepath.Join(adminDir, "gitdir"))
	if gone(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	back, ok := linkTarget(adminGitdir, adminDir)
	if !ok || filepath.Base(back) != ".git" {
		return "", nil
	}
	named, err := os.Stat(filepath.Dir(back))
	if gone(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	tree, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if os.SameFile(named, tree) {
		return adminDir, nil
	}
	return "", nil
}

func (r *Relocator) unpinned(ctx context.Context, git, repo, ref, head string) (string, error) {
	out, err := r.git(ctx, git, "--git-dir="+repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if quietMiss(err, out) {
		return fmt.Sprintf("%s names no commit in %s", ref, repo), nil
	}
	if err != nil {
		return "", err
	}
	if got := strings.TrimSuffix(out, "\n"); got != head {
		return fmt.Sprintf("%s resolves to %s in %s, not the saved head %s", ref, got, repo, head), nil
	}
	return "", nil
}

func adoption(job *cleanup.Job, source, payload sighting) string {
	return fmt.Sprintf(
		"source %s is %s; payload %s is %s; the job captured tree %s",
		job.Source, source, job.Payload, payload, label(job.Tree),
	)
}

func (r *Relocator) take(ctx context.Context, job *cleanup.Job, _ *bool) (bool, error) {
	source, err := sight(job.Source)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	payload, err := sight(job.Payload)
	if err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if !source.is(job.Tree) && payload.is(job.Tree) {
		return r.publish(ctx, job)
	}
	if !source.is(job.Tree) || payload.exists {
		return r.block(ctx, job, "reconcile", adoption(job, source, payload))
	}
	if reason, detail := r.vouch(ctx, job); reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	if err := durable.Rename(job.Source, job.Payload); err != nil {
		return r.block(ctx, job, "reconcile", fmt.Sprintf("rename %s to %s: %v", job.Source, job.Payload, err))
	}
	if payload, err = sight(job.Payload); err != nil {
		return r.block(ctx, job, "reconcile", err.Error())
	}
	if !payload.is(job.Tree) {
		return r.block(ctx, job, "reconcile", fmt.Sprintf(
			"%s was replaced as it was renamed: payload %s is %s, the job captured tree %s",
			job.Source, job.Payload, payload, label(job.Tree),
		))
	}
	return r.publish(ctx, job)
}

func (r *Relocator) vouch(ctx context.Context, job *cleanup.Job) (reason, detail string) {
	_, detail, err := registration(job.Repo, job.Source)
	if err != nil {
		return "reconcile", err.Error()
	}
	if detail != "" {
		return "reconcile", detail
	}
	jobDir := r.cfg.Journal.Layout().JobDir(job.ID)
	entries, err := registered(job.Repo, jobDir)
	if err != nil {
		return "reconcile", err.Error()
	}
	if len(entries) > 0 {
		return "reconcile", entries[0].claim(job)
	}
	detail, err = r.unpinned(ctx, job.Git, job.Repo, job.RecoveryRef, job.Head)
	if err != nil {
		return gitReason(err), err.Error()
	}
	if detail != "" {
		return "identity", detail
	}
	for _, dir := range []string{job.Source, jobDir} {
		if err := r.cfg.Watchers.CheckQuarantine(ctx, dir); err != nil {
			return "quarantine", err.Error()
		}
	}
	if holders := r.held(ctx, job, job.Source); holders != "" {
		return "activity", holders
	}
	tree, err := sight(job.Source)
	if err != nil {
		return "identity", err.Error()
	}
	if !tree.is(job.Tree) {
		return "identity", fmt.Sprintf("%s is %s, the job captured tree %s", job.Source, tree, label(job.Tree))
	}
	return "", ""
}
