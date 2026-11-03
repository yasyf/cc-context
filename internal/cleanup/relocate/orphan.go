package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/cleanup"
)

// Orphan verifies an orphaned pool worktree an operator named by path and
// journals it at PhasePrepared under seq, moving nothing. The tree's .git must
// point into req.CommonDir's worktrees at an admin directory that no longer
// exists, and no registration of that repository may name the tree. No head is
// left to prove the tree clean against, so the request itself authorizes the
// deletion; the tree must still be idle and unwatched.
//
// A request naming Admin is for a tree whose admin directory an older ccx
// retired: that directory still holds the commit, so the tree is journaled at
// PhasePending as an ordinary removal, with every proof a removal gets.
func (r *Relocator) Orphan(ctx context.Context, seq uint64, req cleanup.OrphanRequest) (cleanup.Job, error) {
	if err := req.Validate(); err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: orphan request: %w", err)
	}
	path := req.Path
	resolved, err := filepath.EvalSymlinks(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cleanup.Job{}, refuse(path, "identity", "%s does not exist", path)
	case err != nil:
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	case resolved != path:
		return cleanup.Job{}, refuse(path, "identity", "%s is not canonical: it resolves to %s", path, resolved)
	}
	tree, ok, err := cleanup.Placed(path, req.Tree)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	}
	if !ok {
		return cleanup.Job{}, refuse(path, "identity", "%s is not the tree %s the request names", path, label(req.Tree))
	}
	if detail := parented(path, req.Parent); detail != "" {
		return cleanup.Job{}, refuse(path, "identity", "%s", detail)
	}
	repo, err := filepath.EvalSymlinks(req.CommonDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		repo = req.CommonDir
	case err != nil:
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", req.CommonDir, err)
	}
	if req.Admin != "" {
		return r.retiredTree(ctx, seq, req, repo, tree)
	}
	reason, detail, err := registration(repo, path)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	}
	if reason != "" {
		return cleanup.Job{}, refuse(path, reason, "%s", detail)
	}
	reason, detail, err = abandoned(path, req.CommonDir)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	}
	if reason != "" {
		return cleanup.Job{}, refuse(path, reason, "%s", detail)
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, path); err != nil {
		return cleanup.Job{}, refuse(path, "watched", "%v", err)
	}
	if err := idle(path, r.unserved(ctx, path)); err != nil {
		return cleanup.Job{}, err
	}

	job := cleanup.Job{
		Orphaned: true,
		Repo:     repo,
		Original: path,
		Tree:     tree,
		Parent:   req.Parent,
		Git:      req.Git,
	}
	r.stamp(&job, seq, cleanup.PhasePrepared)
	if err := r.cfg.Journal.Create(job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}

func (r *Relocator) retiredTree(ctx context.Context, seq uint64, req cleanup.OrphanRequest, repo string, tree cleanup.FileID) (cleanup.Job, error) {
	path := req.Path
	retired := filepath.Join(repo, cleanup.RetiredWorktrees, filepath.Base(req.Admin))
	admin, ok, err := cleanup.Placed(retired, req.AdminID)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", retired, err)
	}
	if !ok {
		return cleanup.Job{}, refuse(path, "identity", "%s is not the admin directory %s the request names", retired, label(req.AdminID))
	}
	back, err := readLinkage(filepath.Join(retired, "gitdir"))
	if err != nil {
		return cleanup.Job{}, refuse(path, "mismatch", "%s has no backlink: %v", retired, err)
	}
	if detail := liveElsewhere(back, retired, path); detail != "" {
		return cleanup.Job{}, refuse(path, "mismatch", "%s", detail)
	}
	dotGit, err := readLinkage(filepath.Join(path, ".git"))
	switch {
	case gone(err):
		dotGit = "gitdir: " + retired + "\n"
	case err != nil:
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	default:
		pointer, isLink := strings.CutPrefix(dotGit, "gitdir: ")
		target, valid := linkTarget(pointer, path)
		resolved, resolveErr := filepath.EvalSymlinks(target)
		if !isLink || !valid || resolveErr != nil || resolved != retired {
			return cleanup.Job{}, refuse(path, "mismatch", "%s/.git does not name %s", path, retired)
		}
	}
	location := "--git-dir=" + retired
	head, err := r.resolveHead(ctx, req.Git, location)
	if err != nil {
		return cleanup.Job{}, refuse(path, gitReason(err), "%v", err)
	}
	if head == "" {
		return cleanup.Job{}, refuse(path, "identity", "%s holds no commit to prove the tree against", retired)
	}
	branch, err := r.resolveBranch(ctx, req.Git, location)
	if err != nil {
		return cleanup.Job{}, refuse(path, gitReason(err), "%v", err)
	}
	job := cleanup.Job{
		Repo:     repo,
		AdminDir: filepath.Join(repo, "worktrees", filepath.Base(retired)),
		Admin:    admin,
		Original: path,
		Tree:     tree,
		Head:     head,
		Branch:   branch,
		Git:      req.Git,
		Links:    cleanup.Links{DotGit: dotGit, AdminGitdir: back},
	}
	claim, err := claims(&job, path, false)
	if err != nil {
		return cleanup.Job{}, fmt.Errorf("cleanup: inspect %s: %w", path, err)
	}
	if claim != "" {
		return cleanup.Job{}, refuse(path, "reconcile", "%s", claim)
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, path); err != nil {
		return cleanup.Job{}, refuse(path, "watched", "%v", err)
	}
	if err := idle(path, r.unserved(ctx, path)); err != nil {
		return cleanup.Job{}, err
	}
	r.stamp(&job, seq, cleanup.PhasePending)
	if err := r.cfg.Journal.Create(job); err != nil {
		return cleanup.Job{}, err
	}
	return job, nil
}

func parented(tree string, parent cleanup.FileID) string {
	dir := filepath.Dir(tree)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Sprintf("inspect %s: %v", dir, err)
	}
	if cleanup.IDOf(info).Ino != parent.Ino {
		return fmt.Sprintf("%s is no longer the directory %s the job captured", dir, label(parent))
	}
	return ""
}

func abandoned(tree, common string) (reason, detail string, err error) {
	dotGitPath := filepath.Join(tree, ".git")
	dotGit, err := readLinkage(dotGitPath)
	if gone(err) {
		return "mismatch", fmt.Sprintf("%s has no .git naming an admin directory", tree), nil
	}
	if err != nil {
		return "", "", err
	}
	pointer, isLink := strings.CutPrefix(dotGit, "gitdir: ")
	admin, valid := linkTarget(pointer, tree)
	if !isLink || !valid {
		return "mismatch", fmt.Sprintf("%s names no admin directory", dotGitPath), nil
	}
	if worktrees := filepath.Join(common, "worktrees"); filepath.Dir(admin) != worktrees {
		return "mismatch", fmt.Sprintf("%s names %s, outside %s", dotGitPath, admin, worktrees), nil
	}
	switch _, err := os.Lstat(admin); {
	case err == nil:
		return "registered", fmt.Sprintf("%s names %s, which still exists", dotGitPath, admin), nil
	case !gone(err):
		return "", "", err
	}
	return "", "", nil
}
