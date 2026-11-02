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
		if !payload.dir || payload.id.Ino != job.Tree.Ino || job.Retirement == "" {
			return "", errors.New("displaced retirement has an unexpected payload")
		}
		return job.Payload, nil
	}
	original, err := sight(job.Original)
	if err != nil {
		return "", err
	}
	if !original.dir || original.id.Ino != job.Tree.Ino {
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
	if !seen.dir || seen.id.Ino != job.Admin.Ino {
		return nil, fmt.Errorf("%s is %s, the job captured admin %s", admin, seen, label(job.Admin))
	}
	treeSeen, err := sight(tree)
	if err != nil {
		return nil, err
	}
	if !treeSeen.dir || treeSeen.id.Ino != job.Tree.Ino {
		return nil, errors.New("displaced retirement lost the captured tree inode")
	}
	if err := displacedClaims(job, tree); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(admin)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil || cleanup.IDOf(info) != seen.id {
		_ = root.Close()
		return nil, errors.New("displaced registration changed while opening")
	}
	back, content, err := rootLink(root, "gitdir", os.O_RDWR)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	files := &retirementFiles{root: root, back: back, tree: tree, admin: admin, displaced: true, want: cleanup.Links{DotGit: dotGit, AdminGitdir: job.Links.AdminGitdir}, treeID: treeSeen.id, adminID: seen.id}
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

func (r *Relocator) strictIdle(ctx context.Context, job *cleanup.Job, first string, additional ...string) (string, error) {
	server, err := r.cfg.Watchers.Server(ctx)
	if err != nil {
		return first, fmt.Errorf("name the watchman server: %w", err)
	}
	guarded, ignored := cleanup.WithIgnored(ctx)
	roots := make([]cleanup.GuardRoot, len(additional))
	for i, tree := range additional {
		roots[i] = cleanup.GuardRoot{Path: tree, Retiring: server}
	}
	failed, err := r.cfg.Guard(guarded, cleanup.GuardRoot{Path: first, Retiring: server}, roots...)
	job.Ignored = append(job.Ignored, (*ignored)...)
	return failed, err
}

func (r *Relocator) rejectRoot(ctx context.Context, job *cleanup.Job, reason string, err error) (cleanup.DeletionRoot, error) {
	_, saveErr := r.block(ctx, job, reason, err.Error())
	return cleanup.DeletionRoot{}, saveErr
}

func adminLocator(job *cleanup.Job, admin, dotGit string) error {
	_, ok, err := cleanup.Placed(job.Payload, job.Tree)
	if err != nil {
		return err
	}
	if !ok {
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
	_, ok, err := cleanup.Placed(job.Payload, job.Tree)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !ok {
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
	current, present, err := cleanup.Placed(admin, job.Admin)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !current.Zero() && !present {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin is no longer the captured directory"))
	}
	directories := []string{filepath.Dir(job.Payload)}
	trees := []string{job.Payload}
	if present {
		directories = append(directories, admin)
		trees = append(trees, admin)
	}
	for _, dir := range directories {
		if err := r.cfg.Watchers.CheckQuarantine(ctx, dir); err != nil {
			return r.rejectRoot(ctx, job, probeReason("quarantine", err), err)
		}
	}
	job.Ignored = nil
	if failed, err := r.strictIdle(ctx, job, trees[0], trees[1:]...); err != nil {
		return r.rejectRoot(ctx, job, "activity", errors.New(unclear(failed, err)))
	}
	if !present {
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
	if err != nil || cleanup.IDOf(held) != current {
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
	if again, still, err := cleanup.Placed(admin, job.Admin); err != nil || !still || again != current {
		return r.rejectRoot(ctx, job, "identity", errors.New("retired admin changed during admission"))
	}
	if err := adminLocator(job, admin, dotGit); err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if err := ctx.Err(); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	return cleanup.DeletionRoot{Dir: filepath.Dir(admin), Name: filepath.Base(admin), ID: current}, nil
}
