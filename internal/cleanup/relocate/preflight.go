package relocate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yasyf/cc-context/internal/cleanup"
)

const maxLinkage = 1 << 16

var errNotLinkage = errors.New("not a regular file a worktree link fits in")

func refuse(worktree, reason, format string, args ...any) error {
	return &cleanup.RefusedError{Worktree: worktree, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

func unlinked(worktree string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotLinkage) {
		return refuse(worktree, "unregistered", "not a registered linked worktree: %v", err)
	}
	return fmt.Errorf("cleanup: inspect %s: %w", worktree, err)
}

func readLinkage(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // path is a link file of the worktree under inspection; opened no-follow and read size-capped
	if errors.Is(err, syscall.ELOOP) {
		return "", fmt.Errorf("%s: %w", path, errNotLinkage)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLinkage {
		return "", fmt.Errorf("%s: %w", path, errNotLinkage)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLinkage))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

func linkTarget(content, base string) (string, bool) {
	target := strings.TrimRight(content, "\r\n")
	if target == "" || strings.ContainsAny(target, "\r\n") {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target), true
}

func identify(requested string) (cleanup.Job, error) {
	worktree, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return cleanup.Job{}, unlinked(requested, err)
	}
	tree, info, err := cleanup.LstatID(worktree)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	if !info.IsDir() {
		return cleanup.Job{}, refuse(worktree, "unregistered", "not a directory")
	}

	dotGitPath := filepath.Join(worktree, ".git")
	dotGitInfo, err := os.Stat(dotGitPath)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	if dotGitInfo.IsDir() {
		return cleanup.Job{}, refuse(worktree, "main", "%s is a repository's main working copy, not a linked worktree", worktree)
	}
	dotGit, err := readLinkage(dotGitPath)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	pointer, ok := strings.CutPrefix(dotGit, "gitdir: ")
	if !ok {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s is not a gitdir link", dotGitPath)
	}
	named, ok := linkTarget(pointer, worktree)
	if !ok {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s is not a gitdir link", dotGitPath)
	}
	adminDir, err := filepath.EvalSymlinks(named)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}

	commondir, err := readLinkage(filepath.Join(adminDir, "commondir"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, headErr := os.Lstat(filepath.Join(adminDir, "HEAD")); headErr == nil {
			return cleanup.Job{}, refuse(worktree, "main", "%s is the working copy of the repository at %s, not a linked worktree", worktree, adminDir)
		}
	}
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	common, ok := linkTarget(commondir, adminDir)
	if !ok {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s names no common directory", filepath.Join(adminDir, "commondir"))
	}
	repo, err := filepath.EvalSymlinks(common)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	if filepath.Dir(adminDir) != filepath.Join(repo, "worktrees") {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s is not a worktree admin directory of %s", adminDir, repo)
	}
	admin, adminInfo, err := cleanup.LstatID(adminDir)
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	if !adminInfo.IsDir() {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s is not a directory", adminDir)
	}

	adminGitdir, err := readLinkage(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return cleanup.Job{}, unlinked(worktree, err)
	}
	back, ok := linkTarget(adminGitdir, adminDir)
	if !ok || filepath.Base(back) != ".git" {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s does not name a worktree's .git file", filepath.Join(adminDir, "gitdir"))
	}
	if backDir, err := filepath.EvalSymlinks(filepath.Dir(back)); err != nil || backDir != worktree {
		return cleanup.Job{}, refuse(worktree, "unregistered", "%s registers %s, not %s", adminDir, back, dotGitPath)
	}

	return cleanup.Job{
		Repo:     repo,
		AdminDir: adminDir,
		Admin:    admin,
		Original: worktree,
		Tree:     tree,
		Links:    cleanup.Links{DotGit: dotGit, AdminGitdir: adminGitdir},
	}, nil
}

func sameVolume(tree, jobs cleanup.FileID) bool {
	return tree.Dev == jobs.Dev
}

func nestedWorktree(repo, adminDir, worktree string) (string, error) {
	entries, err := registered(repo, worktree)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.admin != adminDir && e.names != filepath.Join(worktree, ".git") {
			return filepath.Dir(e.names), nil
		}
	}
	return "", nil
}

func spellings(path string) []string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		return []string{path, resolved}
	}
	return []string{path}
}

func inside(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

func within(path, dir string) bool {
	return path == dir || inside(path, dir)
}

func (r *Relocator) screen(ctx context.Context, job cleanup.Job) error {
	locked, err := readLinkage(filepath.Join(job.AdminDir, "locked"))
	switch {
	case err == nil:
		if reason := strings.TrimSpace(locked); reason != "" {
			return refuse(job.Original, "locked", "worktree is locked: %s", reason)
		}
		return refuse(job.Original, "locked", "worktree is locked")
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}

	modules, err := os.ReadDir(filepath.Join(job.AdminDir, "modules"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}
	if len(modules) > 0 {
		return refuse(job.Original, "submodules", "worktree has initialized submodules under %s", filepath.Join(job.AdminDir, "modules"))
	}
	_, err = os.Lstat(filepath.Join(job.Original, ".gitmodules"))
	switch {
	case err == nil:
		gitlink, err := r.hasGitlink(ctx, job.Git, job.Original)
		if err != nil {
			return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
		}
		if gitlink {
			return refuse(job.Original, "submodules", "worktree has submodules in its index; git cannot move it")
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}

	nested, err := nestedWorktree(job.Repo, job.AdminDir, job.Original)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}
	if nested != "" {
		return refuse(job.Original, "nested", "worktree %s is registered inside %s", nested, job.Original)
	}

	return r.colocated(job.Original, job.Tree)
}

func nearest(path string) (fs.FileInfo, error) {
	for {
		info, err := os.Stat(path)
		if !errors.Is(err, fs.ErrNotExist) {
			return info, err
		}
		path = filepath.Dir(path)
	}
}

func (r *Relocator) colocated(tree string, id cleanup.FileID) error {
	jobsDir := r.cfg.Journal.Layout().JobsDir()
	jobsInfo, err := nearest(jobsDir)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", jobsDir, err)
	}
	if !sameVolume(id, cleanup.IDOf(jobsInfo)) {
		return refuse(tree, "volume", "%s and the cleanup folder %s are on different volumes", tree, jobsDir)
	}
	return nil
}

func (r *Relocator) retiring(ctx context.Context, tree string) ([]cleanup.ProcessID, error) {
	watchers, err := r.cfg.Watchers.Retiring(ctx, tree)
	if err != nil {
		return nil, fmt.Errorf("name the watchers of %s: %w", tree, err)
	}
	return watchers, nil
}

func (r *Relocator) unretired(ctx context.Context, tree string) error {
	watchers, err := r.retiring(ctx, tree)
	if err != nil {
		return err
	}
	return r.cfg.Guard(cleanup.WithRetiring(ctx, watchers), tree)
}

func idle(tree string, err error) error {
	if err == nil {
		return nil
	}
	var active *cleanup.ActiveError
	if errors.As(err, &active) {
		return active
	}
	return fmt.Errorf("cleanup: could not verify that %s is idle: %w", tree, err)
}

func (r *Relocator) capture(ctx context.Context, job *cleanup.Job) error {
	head, err := r.head(ctx, job.Git, job.Original)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}
	branch, err := r.branch(ctx, job.Git, job.Original)
	if err != nil {
		return fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
	}
	job.Head, job.Branch = head, branch
	return nil
}

func (r *Relocator) preflight(ctx context.Context, req cleanup.Request) (cleanup.Job, error) {
	if err := req.Validate(); err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: remove request: %w", err)
	}
	job, err := identify(req.Worktree)
	if err != nil {
		return cleanup.Job{}, err
	}
	job.Git, job.Force = req.Git, req.Force
	if err := r.screen(ctx, job); err != nil {
		return cleanup.Job{}, err
	}
	if !job.Force {
		dirt, err := r.dirt(ctx, job.Git, job.Original)
		if err != nil {
			return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", job.Original, err)
		}
		if dirt != "" {
			return cleanup.Job{}, refuse(job.Original, "dirty", "%s", dirt)
		}
	}
	if err := idle(job.Original, r.unretired(ctx, job.Original)); err != nil {
		return cleanup.Job{}, err
	}
	if err := r.capture(ctx, &job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}
