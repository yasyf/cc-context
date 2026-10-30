package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yasyf/daemonkit/durable"
	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func displacedTree(job *cleanup.Job) (string, error) {
	registered, err := sight(job.Registered)
	if err != nil {
		return "", err
	}
	if registered.exists {
		return "", errors.New("displaced registration has an occupied registered path")
	}
	payload, err := sight(job.Payload)
	if err != nil {
		return "", err
	}
	if payload.exists {
		if !payload.is(job.Tree) || job.Retirement == "" {
			return "", errors.New("displaced retirement has an unexpected payload")
		}
		return job.Payload, nil
	}
	original, err := sight(job.Original)
	if err != nil {
		return "", err
	}
	if !original.is(job.Tree) {
		return "", fmt.Errorf("%s is %s, the job captured tree %s", job.Original, original, label(job.Tree))
	}
	return job.Original, nil
}

func displacedPointer(tree string) (string, string, error) {
	dotGit, err := readLinkage(filepath.Join(tree, ".git"))
	if err != nil {
		return "", "", err
	}
	pointer, ok := strings.CutPrefix(dotGit, "gitdir: ")
	if !ok {
		return "", "", errors.New("retained tree has no admin pointer")
	}
	admin, ok := linkTarget(pointer, tree)
	if !ok {
		return "", "", errors.New("retained tree has an invalid admin pointer")
	}
	return admin, dotGit, nil
}

func displacedClaims(job *cleanup.Job, tree string) error {
	entries, err := enrolled(job.Repo)
	if err != nil {
		return err
	}
	claims, err := reaching(entries, tree, filepath.Dir(job.Payload))
	if err != nil {
		return err
	}
	if len(claims) != 0 {
		return errors.New(claims[0].claim(job))
	}
	carried, err := carried(job, entries, tree)
	if err != nil {
		return err
	}
	if carried != "" {
		return errors.New(carried)
	}
	return nil
}

func (r *Relocator) displacedFiles(ctx context.Context, job *cleanup.Job) (*retirementFiles, error) {
	if job.Adopted || !job.Phase.Logical() || job.Head == "" {
		return nil, errors.New("displaced retirement requires a captured linked checkout and committed HEAD")
	}
	tree, err := displacedTree(job)
	if err != nil {
		return nil, err
	}
	admin, dotGit, err := displacedPointer(tree)
	if err != nil {
		return nil, err
	}
	admin, err = filepath.EvalSymlinks(admin)
	if err != nil {
		return nil, err
	}
	seen, err := sight(admin)
	if err != nil {
		return nil, err
	}
	if !seen.is(job.Admin) {
		return nil, fmt.Errorf("%s is %s, the job captured admin %s", admin, seen, label(job.Admin))
	}
	if err := displacedClaims(job, tree); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(admin)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || cleanup.IDOf(info) != job.Admin {
		_ = root.Close()
		return nil, errors.New("displaced registration changed while opening")
	}
	back, content, err := rootLink(root, "gitdir", os.O_RDWR)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	files := &retirementFiles{root: root, back: back, tree: tree, admin: admin, displaced: true, want: cleanup.Links{DotGit: dotGit, AdminGitdir: job.Links.AdminGitdir}, treeID: job.Tree, adminID: job.Admin}
	if err := r.proveDisplaced(ctx, job, files, content); err != nil {
		files.close()
		return nil, err
	}
	return files, nil
}

func (r *Relocator) proveDisplaced(ctx context.Context, job *cleanup.Job, files *retirementFiles, backlink string) error {
	owned, err := ownMarker(files.root, job)
	if err != nil {
		return err
	}
	if backlink == "" {
		if !owned || job.Retirement == "" {
			return errors.New("empty displaced backlink has no owned retirement intent")
		}
	} else {
		back, ok := linkTarget(backlink, files.admin)
		if !ok || backlink != job.Links.AdminGitdir || filepath.Base(back) != ".git" {
			return errors.New("displaced registration no longer holds its captured backlink")
		}
		named, err := filepath.EvalSymlinks(filepath.Dir(back))
		if err != nil || named != files.tree {
			return errors.New("displaced registration does not link back to the captured tree")
		}
	}
	commonFile, common, err := rootLink(files.root, "commondir", os.O_RDONLY)
	if err != nil {
		return err
	}
	_ = commonFile.Close()
	named, ok := linkTarget(common, files.admin)
	if !ok {
		return errors.New("displaced registration names no common directory")
	}
	repo, err := filepath.EvalSymlinks(named)
	if err != nil {
		return err
	}
	if repo != job.Repo {
		return fmt.Errorf("displaced registration belongs to %s, not %s", repo, job.Repo)
	}
	head, err := r.resolveHead(ctx, job.Git, "--git-dir="+files.admin)
	if err != nil {
		return err
	}
	if head != job.Head {
		return errors.New(headChanged(job, files.tree, head))
	}
	branch, err := r.resolveBranch(ctx, job.Git, "--git-dir="+files.admin)
	if err != nil {
		return err
	}
	if branch != job.Branch {
		return fmt.Errorf("branch changed from %q to %q", job.Branch, branch)
	}
	return files.unchanged(job)
}

func (r *Relocator) verifyDisplaced(ctx context.Context, job *cleanup.Job, files *retirementFiles) error {
	back, content, err := rootLink(files.root, "gitdir", os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = back.Close() }()
	held, err := files.back.Stat()
	if err != nil {
		return err
	}
	named, err := back.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(held, named) {
		return errors.New("displaced backlink changed during its final proof")
	}
	return r.proveDisplaced(ctx, job, files, content)
}

func (r *Relocator) strictIdle(ctx context.Context, job *cleanup.Job, tree string) error {
	watchers, err := r.cfg.Watchers.Server(ctx)
	if err != nil {
		return fmt.Errorf("name the watchman server: %w", err)
	}
	guarded, ignored := cleanup.WithIgnored(cleanup.WithRetiring(ctx, watchers))
	err = r.cfg.Guard(guarded, tree)
	job.Ignored = append(job.Ignored, (*ignored)...)
	return err
}

func writePointer(files *retirementFiles, job *cleanup.Job) error {
	want := "gitdir: " + files.admin + "\n"
	staging, err := os.OpenRoot(filepath.Dir(job.Payload))
	if err != nil {
		return err
	}
	defer func() { _ = staging.Close() }()
	const name = ".ccx-pointer"
	staged, content, err := rootLink(staging, name, os.O_RDONLY)
	present := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if present {
		_ = staged.Close()
		if content != want {
			if !strings.HasPrefix(want, content) {
				return errors.New("job pointer staging file is not owned by this retirement")
			}
			if err := staging.Remove(name); err != nil {
				return err
			}
			if err := rootSync(staging); err != nil {
				return err
			}
			present = false
		}
	}
	if files.want.DotGit == want {
		if present {
			if err := staging.Remove(name); err != nil {
				return err
			}
			if err := rootSync(staging); err != nil {
				return err
			}
		}
		return files.unchanged(job)
	}
	root, err := os.OpenRoot(files.tree)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil || cleanup.IDOf(info) != job.Tree {
		return errors.New("retained tree changed while opening its pointer")
	}
	link, content, err := rootLink(root, ".git", os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close() }()
	if content != files.want.DotGit {
		return errors.New("retained tree pointer changed")
	}
	if !present {
		file, err := staging.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString(want)
		if err := errors.Join(writeErr, file.Sync(), file.Close(), rootSync(staging)); err != nil {
			return err
		}
	}
	held, err := link.Stat()
	if err != nil {
		return err
	}
	named, err := root.Lstat(".git")
	if err != nil {
		return err
	}
	if !os.SameFile(held, named) {
		return errors.New("retained tree pointer was replaced")
	}
	if err := files.unchanged(job); err != nil {
		return err
	}
	from, err := staging.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	to, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	if err := unix.Renameat(int(from.Fd()), name, int(to.Fd()), ".git"); err != nil {
		return err
	}
	files.want.DotGit = want
	if err := errors.Join(rootSync(root), rootSync(staging)); err != nil {
		return err
	}
	return files.unchanged(job)
}

func (r *Relocator) resumeDisplaced(ctx context.Context, job *cleanup.Job) (bool, error) {
	captured, err := sight(job.AdminDir)
	if err != nil {
		_, err = r.retirementBlocked(ctx, job, err)
		return true, err
	}
	if captured.is(job.Admin) {
		return false, nil
	}
	files, err := r.retirementFiles(ctx, job)
	if err != nil {
		_, err = r.retirementBlocked(ctx, job, err)
		return true, err
	}
	displaced := files.displaced
	files.close()
	if !displaced {
		return false, nil
	}
	if err := r.Retire(ctx, job); err != nil || job.Blocked != nil {
		return true, err
	}
	files, err = r.retirementFiles(ctx, job)
	if err != nil {
		_, err = r.retirementBlocked(ctx, job, err)
		return true, err
	}
	defer files.close()
	if !files.displaced {
		_, err = r.block(ctx, job, "identity", "displaced registration changed during resume")
		return true, err
	}
	if err := r.submodules(ctx, job.Git, files.admin, files.tree, "--git-dir="+files.admin, "--work-tree="+files.tree); err != nil {
		var refused *cleanup.RefusedError
		if errors.As(err, &refused) {
			_, err = r.block(ctx, job, refused.Reason, refused.Detail)
			return true, err
		}
		_, err = r.block(ctx, job, gitReason(err), err.Error())
		return true, err
	}
	if !job.Force {
		dirt, err := r.dirtAt(ctx, job.Git, "--git-dir="+files.admin, "--work-tree="+files.tree)
		if err != nil {
			_, err = r.block(ctx, job, gitReason(err), err.Error())
			return true, err
		}
		if dirt != "" {
			_, err = r.block(ctx, job, "dirty", dirt)
			return true, err
		}
	}
	if detail, err := r.unpinned(ctx, job.Git, job.Repo, job.RecoveryRef, job.Head); err != nil || detail != "" {
		if err != nil {
			detail = err.Error()
		}
		_, err = r.block(ctx, job, "identity", detail)
		return true, err
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, filepath.Dir(job.Payload)); err != nil {
		_, err = r.block(ctx, job, "quarantine", err.Error())
		return true, err
	}
	for _, tree := range []string{files.tree, files.admin} {
		if err := r.strictIdle(ctx, job, tree); err != nil {
			_, err = r.block(ctx, job, "activity", unclear(tree, err))
			return true, err
		}
	}
	if err := displacedClaims(job, files.tree); err != nil {
		_, err = r.block(ctx, job, "reconcile", err.Error())
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := r.verifyDisplaced(ctx, job, files); err != nil {
		_, err = r.block(ctx, job, "identity", err.Error())
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := writePointer(files, job); err != nil {
		_, err = r.block(ctx, job, "identity", err.Error())
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if files.tree != job.Payload {
		if err := durable.Rename(files.tree, job.Payload); err != nil {
			_, err = r.block(ctx, job, "reconcile", err.Error())
			return true, err
		}
		files.tree = job.Payload
	}
	if err := files.unchanged(job); err != nil {
		_, err = r.block(ctx, job, "identity", err.Error())
		return true, err
	}
	updated := *job
	updated.Phase, updated.Retirement, updated.Updated = cleanup.PhaseDeletingAdmin, "", r.cfg.Now()
	if err := r.cfg.Journal.Save(updated); err != nil {
		return true, err
	}
	*job = updated
	return true, nil
}

func (r *Relocator) rejectRoot(ctx context.Context, job *cleanup.Job, reason string, err error) (cleanup.DeletionRoot, error) {
	_, saveErr := r.block(ctx, job, reason, err.Error())
	return cleanup.DeletionRoot{}, saveErr
}

func adminLocator(job *cleanup.Job, admin, dotGit string) error {
	payload, err := sight(job.Payload)
	if err != nil {
		return err
	}
	if !payload.is(job.Tree) {
		return errors.New("retired admin deletion lost its captured payload")
	}
	current, pointer, err := displacedPointer(job.Payload)
	if err != nil {
		return err
	}
	if current != admin || pointer != dotGit || dotGit != "gitdir: "+admin+"\n" {
		return errors.New("retired admin deletion lost its absolute captured locator")
	}
	registered, err := sight(job.Registered)
	if err != nil {
		return err
	}
	if registered.exists {
		return errors.New("retired admin deletion has an occupied registered path")
	}
	return nil
}

func syncAbsentAdmin(ctx context.Context, job *cleanup.Job, admin, dotGit string) error {
	parent := filepath.Dir(admin)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	held, err := root.Stat(".")
	if err != nil {
		return err
	}
	check := func() error {
		named, err := sight(parent)
		if err != nil {
			return err
		}
		if !named.is(cleanup.IDOf(held)) {
			return errors.New("retired admin parent changed")
		}
		if _, err := root.Lstat(filepath.Base(admin)); !errors.Is(err, fs.ErrNotExist) {
			return errors.New("retired admin is not absent")
		}
		return adminLocator(job, admin, dotGit)
	}
	if err := check(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rootSync(root); err != nil {
		return err
	}
	return check()
}

func (r *Relocator) admitAdmin(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	payload, err := sight(job.Payload)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !payload.is(job.Tree) {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin deletion lost its captured payload"))
	}
	admin, dotGit, err := displacedPointer(job.Payload)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if err := adminLocator(job, admin, dotGit); err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if err := displacedClaims(job, job.Payload); err != nil {
		return r.rejectRoot(ctx, job, "reconcile", err)
	}
	seen, err := sight(admin)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if seen.exists && !seen.is(job.Admin) {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin is no longer the captured directory"))
	}
	directories := []string{filepath.Dir(job.Payload)}
	trees := []string{job.Payload}
	if seen.exists {
		directories = append(directories, admin)
		trees = append(trees, admin)
	}
	for _, dir := range directories {
		if err := r.cfg.Watchers.CheckQuarantine(ctx, dir); err != nil {
			return r.rejectRoot(ctx, job, "quarantine", err)
		}
	}
	job.Ignored = nil
	for _, tree := range trees {
		if err := r.strictIdle(ctx, job, tree); err != nil {
			return r.rejectRoot(ctx, job, "activity", errors.New(unclear(tree, err)))
		}
	}
	if !seen.exists {
		if err := syncAbsentAdmin(ctx, job, admin, dotGit); err != nil {
			return r.rejectRoot(ctx, job, "reconcile", err)
		}
		if err := ctx.Err(); err != nil {
			return cleanup.DeletionRoot{}, err
		}
		if _, err := r.enter(job, cleanup.PhaseDeletingRetiredPayload); err != nil {
			return cleanup.DeletionRoot{}, err
		}
		return r.Admit(ctx, job)
	}
	root, err := os.OpenRoot(admin)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	defer func() { _ = root.Close() }()
	held, err := root.Stat(".")
	if err != nil || cleanup.IDOf(held) != job.Admin {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin changed while opening"))
	}
	if _, err := ownMarker(root, job); err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	back, backlink, err := rootLink(root, "gitdir", os.O_RDONLY)
	if err == nil {
		_ = back.Close()
		if backlink != "" {
			return r.rejectRoot(ctx, job, "identity", errors.New("retired admin acquired a backlink"))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	seen, err = sight(admin)
	if err != nil || !seen.is(job.Admin) {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin changed during admission"))
	}
	if err := adminLocator(job, admin, dotGit); err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if err := ctx.Err(); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	return cleanup.DeletionRoot{Dir: filepath.Dir(admin), Name: filepath.Base(admin), ID: job.Admin}, nil
}
