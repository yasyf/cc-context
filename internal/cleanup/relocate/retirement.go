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

type retirementFiles struct {
	root            *os.Root
	back            *os.File
	tree            string
	want            cleanup.Links
	treeID, adminID cleanup.FileID
}

const retirementPrefix = "ccx cleanup "

func retirementMarker(job *cleanup.Job) string { return retirementPrefix + job.ID + "\n" }

// RetiringJob names the cleanup job whose marker locks the worktree
// registration at adminDir, the state a removal leaves while the deletion
// queue is paused: git no longer lists the worktree, and its tree waits at
// its path for the queue.
func RetiringJob(adminDir string) (string, bool) {
	root, err := os.OpenRoot(adminDir)
	if err != nil {
		return "", false
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("locked")
	id, ok := strings.CutPrefix(strings.TrimSpace(string(data)), retirementPrefix)
	return id, err == nil && ok && id != ""
}

func (f *retirementFiles) close() {
	_ = f.back.Close()
	_ = f.root.Close()
}

func rootLink(root *os.Root, name string, flags int) (*os.File, string, error) {
	file, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() > maxLinkage) {
		err = errNotLinkage
	}
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLinkage))
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, string(data), nil
}

func rootSync(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func ownMarker(root *os.Root, job *cleanup.Job) (bool, error) {
	file, marker, err := rootLink(root, "locked", os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = file.Close()
	if marker != retirementMarker(job) {
		return false, fmt.Errorf("registration has a lock not owned by job %s", job.ID)
	}
	return true, nil
}

func publishMarker(root *os.Root, job *cleanup.Job) error {
	name := "ccx-retiring-" + cleanup.MintID(job.Updated)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }()
	_, writeErr := file.WriteString(retirementMarker(job))
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := root.Link(name, "locked"); err != nil {
		return err
	}
	return rootSync(root)
}

func (r *Relocator) retirementFiles(ctx context.Context, job *cleanup.Job) (*retirementFiles, error) {
	seen, err := observe(job, job.Phase == cleanup.PhaseMoved || job.Phase == cleanup.PhaseDetached)
	if err != nil {
		return nil, err
	}
	tree, held, moved := mountLocation(job, seen)
	if tree == "" || !held.exists || !held.dir || !seen.admin.exists || !seen.admin.dir || held.id.Ino != job.Tree.Ino || seen.admin.id.Ino != job.Admin.Ino {
		return nil, fmt.Errorf("%s", seen.describe(job))
	}
	refresh := held.id != job.Tree || seen.admin.id != job.Admin
	if refresh && job.Head == "" {
		return nil, errors.New("cannot refresh device numbers without a captured HEAD")
	}
	want := job.Links
	if moved {
		want, err = expectedLinks(job)
		if err != nil {
			return nil, err
		}
	}
	dotGit, err := readLinkage(filepath.Join(tree, ".git"))
	if err != nil {
		return nil, err
	}
	if job.Phase == cleanup.PhasePrepared && moved && dotGit == job.Links.DotGit {
		want.DotGit = job.Links.DotGit
	}
	if dotGit != want.DotGit {
		return nil, fmt.Errorf("%s no longer holds its captured .git link", tree)
	}
	root, err := os.OpenRoot(job.AdminDir)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("registration changed while opening %s: %w", job.AdminDir, err)
	}
	if cleanup.IDOf(info) != seen.admin.id {
		_ = root.Close()
		return nil, fmt.Errorf("registration changed while opening %s", job.AdminDir)
	}
	back, content, err := rootLink(root, "gitdir", os.O_RDWR)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	files := &retirementFiles{root: root, back: back, tree: tree, want: want, treeID: held.id, adminID: seen.admin.id}
	valid := content == want.AdminGitdir || job.Phase == cleanup.PhasePrepared && moved && content == job.Links.AdminGitdir
	if job.Retirement != "" && content == "" {
		valid = true
	}
	if !valid {
		files.close()
		return nil, fmt.Errorf("%s no longer holds its captured backlink", job.AdminDir)
	}
	owned, err := ownMarker(root, job)
	if err != nil || content == "" && !owned {
		files.close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("empty backlink has no owned retirement marker")
	}
	common, err := readLinkage(filepath.Join(job.AdminDir, "commondir"))
	if err == nil {
		named, valid := linkTarget(common, job.AdminDir)
		if !valid {
			err = errors.New("registration names no common directory")
		} else if repo, resolveErr := filepath.EvalSymlinks(named); resolveErr != nil {
			err = resolveErr
		} else if repo != job.Repo {
			err = fmt.Errorf("registration belongs to %s, not %s", repo, job.Repo)
		}
	}
	if err == nil {
		var head, branch string
		head, err = r.resolveHead(ctx, job.Git, "--git-dir="+job.AdminDir)
		if err == nil && head != job.Head {
			err = errors.New(headChanged(job, tree, head))
		}
		if err == nil {
			branch, err = r.resolveBranch(ctx, job.Git, "--git-dir="+job.AdminDir)
		}
		if err == nil && branch != job.Branch {
			err = fmt.Errorf("branch changed from %q to %q", job.Branch, branch)
		}
	}
	if err != nil {
		files.close()
		return nil, err
	}
	if err := files.unchanged(job); err != nil {
		files.close()
		return nil, err
	}
	return files, nil
}

func (r *Relocator) retirementDevices(job *cleanup.Job, files *retirementFiles) error {
	if job.Tree == files.treeID && job.Admin == files.adminID {
		return nil
	}
	updated := *job
	updated.Tree, updated.Admin = files.treeID, files.adminID
	updated.Updated = r.cfg.Now()
	if err := r.cfg.Journal.Save(updated); err != nil {
		return err
	}
	*job = updated
	return nil
}

func (f *retirementFiles) unchanged(job *cleanup.Job) error {
	tree, err := sight(f.tree)
	if err != nil {
		return fmt.Errorf("retiring tree changed: %w", err)
	}
	if !tree.is(f.treeID) {
		return errors.New("retiring tree changed")
	}
	admin, err := sight(job.AdminDir)
	if err != nil {
		return fmt.Errorf("retiring registration changed: %w", err)
	}
	if !admin.is(f.adminID) {
		return errors.New("retiring registration changed")
	}
	held, err := f.back.Stat()
	if err != nil {
		return err
	}
	named, err := f.root.Lstat("gitdir")
	if err != nil {
		return fmt.Errorf("retiring backlink changed: %w", err)
	}
	if !os.SameFile(held, named) {
		return errors.New("retiring backlink changed")
	}
	return nil
}

func (r *Relocator) retirementIdle(ctx context.Context, job *cleanup.Job, tree string) error {
	watchers, err := r.retiring(ctx, tree)
	if err != nil {
		return err
	}
	guarded, ignored := cleanup.WithIgnored(cleanup.WithRetiring(ctx, watchers))
	err = r.cfg.Guard(guarded, tree)
	job.Ignored = *ignored
	return err
}

func (r *Relocator) retirementState(job *cleanup.Job, state cleanup.Retirement) error {
	job.Retirement = state
	job.Updated = r.cfg.Now()
	return r.cfg.Journal.Save(*job)
}

func (r *Relocator) retirementBlocked(ctx context.Context, job *cleanup.Job, err error) (bool, error) {
	reason := "identity"
	if errors.Is(err, cleanup.ErrUnprobed) {
		reason = timeoutReason
	}
	return r.block(ctx, job, reason, err.Error())
}

// Retire unregisters an idle queued tree in place while physical deletion is paused.
// Worktree repair and move must be excluded until the operation completes.
func (r *Relocator) Retire(ctx context.Context, job *cleanup.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	files, err := r.retirementFiles(ctx, job)
	if err != nil {
		_, err = r.retirementBlocked(ctx, job, err)
		return err
	}
	defer files.close()
	if err := r.retirementDevices(job, files); err != nil {
		return err
	}
	owned, err := ownMarker(files.root, job)
	if err != nil || job.Retirement == "" && owned {
		if err == nil {
			err = errors.New("registration lock has no retirement intent")
		}
		_, err = r.block(ctx, job, "identity", err.Error())
		return err
	}
	if err := r.retirementIdle(ctx, job, files.tree); err != nil {
		_, err = r.block(ctx, job, "activity", unclear(files.tree, err))
		return err
	}
	if job.Retirement == "" {
		if err := r.retirementState(job, cleanup.RetirementIntent); err != nil {
			return err
		}
	}
	if !owned {
		if info, err := files.back.Stat(); err != nil || info.Size() == 0 {
			_, err = r.block(ctx, job, "identity", "retired backlink has no owned lock marker")
			return err
		}
		if err := publishMarker(files.root, job); err != nil {
			_, err = r.block(ctx, job, "identity", err.Error())
			return err
		}
	}
	if job.Head != "" {
		if _, err := r.rewrite(ctx, job.Git, "--git-dir="+job.Repo, "update-ref", job.RecoveryRef, job.Head); err != nil {
			_, err = r.block(ctx, job, gitReason(err), err.Error())
			return err
		}
	}
	if err := r.retire(ctx, files.tree); err != nil {
		_, err = r.block(ctx, job, "watchers", err.Error())
		return err
	}
	if err := r.unserved(ctx, files.tree); err != nil {
		_, err = r.block(ctx, job, "activity", unclear(files.tree, err))
		return err
	}
	if err := files.unchanged(job); err != nil {
		_, err = r.block(ctx, job, "identity", err.Error())
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := files.back.Truncate(0); err != nil {
		_, err = r.block(ctx, job, "reconcile", fmt.Sprintf("truncate retiring backlink: %v", err))
		return err
	}
	if err := files.back.Sync(); err != nil {
		_, err = r.block(ctx, job, "reconcile", fmt.Sprintf("sync retiring backlink: %v", err))
		return err
	}
	if err := files.unchanged(job); err != nil {
		_, err = r.block(ctx, job, "identity", err.Error())
		return err
	}
	return r.retirementState(job, cleanup.RetirementComplete)
}

func (r *Relocator) restoreRegistration(ctx context.Context, job *cleanup.Job) (bool, error) {
	files, err := r.retirementFiles(ctx, job)
	if err != nil {
		return r.retirementBlocked(ctx, job, err)
	}
	defer files.close()
	if err := r.retirementDevices(job, files); err != nil {
		return false, err
	}
	owned, err := ownMarker(files.root, job)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !owned {
		info, err := files.back.Stat()
		if err != nil || info.Size() == 0 {
			return r.block(ctx, job, "identity", "retired backlink has no owned lock marker")
		}
	}
	if err := files.unchanged(job); err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := r.retirementState(job, cleanup.RetirementIntent); err != nil {
		return false, err
	}
	if owned {
		if _, err := files.back.WriteAt([]byte(files.want.AdminGitdir), 0); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("restore retiring backlink: %v", err))
		}
		if err := files.back.Truncate(int64(len(files.want.AdminGitdir))); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("truncate restored backlink: %v", err))
		}
		if err := files.back.Sync(); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("sync restored backlink: %v", err))
		}
		if err := files.root.Remove("locked"); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("remove retirement marker: %v", err))
		}
		if err := rootSync(files.root); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("sync restored registration: %v", err))
		}
	}
	return true, r.retirementState(job, "")
}
