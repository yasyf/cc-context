package relocate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yasyf/cc-context/internal/cleanup"
)

type spot struct {
	path string
	id   cleanup.FileID
}

func (s spot) root() cleanup.DeletionRoot {
	return cleanup.DeletionRoot{Dir: filepath.Dir(s.path), Name: filepath.Base(s.path), ID: s.id}
}

func places(job *cleanup.Job) []string {
	origin := job.Original
	if job.Adopted {
		origin = job.Source
	}
	return []string{origin, job.Registered, job.Payload}
}

func locate(job *cleanup.Job) (spot, bool, error) {
	for _, path := range places(job) {
		id, ok, err := cleanup.Placed(path, job.Tree)
		if err != nil {
			return spot{}, false, err
		}
		if ok {
			return spot{path: path, id: id}, true, nil
		}
	}
	return spot{}, false, nil
}

type bond struct {
	native  bool
	owned   bool
	severed bool
	admin   cleanup.FileID
}

func openAdmin(job *cleanup.Job, admin cleanup.FileID) (*os.Root, error) {
	root, err := os.OpenRoot(job.AdminDir)
	if err != nil {
		return nil, err
	}
	if info, err := root.Stat("."); err != nil || cleanup.IDOf(info) != admin {
		_ = root.Close()
		return nil, fmt.Errorf("%s changed while it was opened", job.AdminDir)
	}
	return root, nil
}

func owns(job *cleanup.Job, admin cleanup.FileID) (bool, error) {
	root, err := openAdmin(job, admin)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	return ownMarker(root, job)
}

func rootRead(root *os.Root, name string) (string, error) {
	file, content, err := rootLink(root, name, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	return content, nil
}

func liveElsewhere(back, adminDir, tree string) string {
	target, ok := linkTarget(back, adminDir)
	if !ok {
		return fmt.Sprintf("%s names no worktree", filepath.Join(adminDir, "gitdir"))
	}
	named, err := filepath.EvalSymlinks(filepath.Dir(target))
	if gone(err) {
		return ""
	}
	here, hereErr := filepath.EvalSymlinks(tree)
	if err != nil || hereErr != nil {
		return fmt.Sprintf("resolve %s: %v", target, errors.Join(err, hereErr))
	}
	if named != here {
		return fmt.Sprintf("%s registers %s, not the tree at %s", adminDir, target, tree)
	}
	return ""
}

func lockedBy(root *os.Root, job *cleanup.Job, err error) string {
	locked, readErr := rootRead(root, "locked")
	if readErr != nil {
		return fmt.Sprintf("%s: %v", job.AdminDir, err)
	}
	if reason := strings.TrimSpace(locked); reason != "" {
		return "worktree is locked: " + reason
	}
	return "worktree is locked"
}

func bind(job *cleanup.Job, tree spot) (bond, string, string) {
	if job.Unbound() {
		return bond{}, "", ""
	}
	admin, ok, err := cleanup.Placed(job.AdminDir, job.Admin)
	if err != nil {
		return bond{}, "identity", err.Error()
	}
	if !ok {
		return bond{}, "", ""
	}
	root, err := openAdmin(job, admin)
	if err != nil {
		return bond{}, "identity", err.Error()
	}
	defer func() { _ = root.Close() }()
	owned, err := ownMarker(root, job)
	if err != nil {
		return bond{}, "locked", lockedBy(root, job, err)
	}
	common, err := rootRead(root, "commondir")
	if err != nil {
		return bond{}, "identity", err.Error()
	}
	named, ok := linkTarget(common, job.AdminDir)
	if !ok {
		return bond{}, "identity", fmt.Sprintf("%s names no common directory", job.AdminDir)
	}
	if repo, err := filepath.EvalSymlinks(named); err != nil || repo != job.Repo {
		return bond{}, "identity", fmt.Sprintf("%s no longer belongs to %s", job.AdminDir, job.Repo)
	}
	back, err := rootRead(root, "gitdir")
	switch {
	case err != nil && !gone(err):
		return bond{}, "identity", err.Error()
	case strings.TrimSpace(back) == "" && !owned:
		return bond{}, "identity", fmt.Sprintf("%s has no backlink and no lock of job %s", job.AdminDir, job.ID)
	case strings.TrimSpace(back) != "":
		if detail := liveElsewhere(back, job.AdminDir, tree.path); detail != "" {
			return bond{}, "identity", detail
		}
	}
	dotGitPath := filepath.Join(tree.path, ".git")
	dotGit, err := readLinkage(dotGitPath)
	severed := gone(err)
	switch {
	case severed:
	case err != nil:
		return bond{}, "identity", err.Error()
	default:
		pointer, isLink := strings.CutPrefix(dotGit, "gitdir: ")
		target, valid := linkTarget(pointer, tree.path)
		resolved, resolveErr := filepath.EvalSymlinks(target)
		if !isLink || !valid || resolveErr != nil || resolved != job.AdminDir {
			return bond{}, "identity", fmt.Sprintf("%s no longer names %s", dotGitPath, job.AdminDir)
		}
	}
	return bond{native: true, owned: owned, severed: severed, admin: admin}, "", ""
}

func claims(job *cleanup.Job, tree string, native bool) (string, error) {
	entries, err := enrolled(job.Repo)
	if err != nil {
		return "", err
	}
	hits, err := reaching(entries, tree)
	if err != nil {
		return "", err
	}
	for _, e := range hits {
		if native && e.admin == job.AdminDir {
			continue
		}
		return e.claim(job), nil
	}
	return carried(job, entries, tree)
}

func (r *Relocator) followHead(ctx context.Context, job *cleanup.Job, admin cleanup.FileID, tree string) error {
	location := "--git-dir=" + job.AdminDir
	head, err := r.resolveHead(ctx, job.Git, location)
	if err != nil {
		return err
	}
	branch, err := r.resolveBranch(ctx, job.Git, location)
	if err != nil {
		return err
	}
	if head == "" || head == job.Head && branch == job.Branch {
		return nil
	}
	if r.cfg.Quiet > 0 {
		root, err := openAdmin(job, admin)
		if err != nil {
			return err
		}
		moved, err := lastMove(root)
		_ = root.Close()
		if err != nil {
			return err
		}
		if requested := job.Created.Add(r.cfg.Quiet); moved.After(requested) {
			return refuse(tree, "identity", "%s moved to %s at %s, more than %s after its removal was requested at %s: something works in it again; ccx vcs worktree rm --force removes it once that is done", tree, head, moved.Local().Format(time.RFC3339), r.cfg.Quiet, job.Created.Local().Format(time.RFC3339))
		}
	}
	holder, err := r.git(ctx, job.Git, "--git-dir="+job.Repo, "for-each-ref", "--count=1", "--contains="+head, "--format=%(refname)", "refs/heads", "refs/remotes")
	if err != nil || strings.TrimSpace(holder) == "" {
		return err
	}
	now := r.cfg.Now()
	job.Note(now, fmt.Sprintf("the tree moved from %s to %s after the removal was requested; %s holds it", job.Head, head, strings.TrimSpace(holder)))
	job.Head, job.Branch, job.Updated = head, branch, now
	return r.cfg.Journal.Save(*job)
}

func (r *Relocator) native(ctx context.Context, job *cleanup.Job, tree string) (bool, string, string) {
	location := []string{"--git-dir=" + job.AdminDir, "--work-tree=" + tree}
	head, err := r.resolveHead(ctx, job.Git, location[0])
	if err != nil {
		return false, gitReason(err), err.Error()
	}
	branchGone := head == "" && job.Branch != ""
	if head != job.Head && !branchGone {
		return false, "identity", headChanged(job, tree, head)
	}
	branch, err := r.resolveBranch(ctx, job.Git, location[0])
	if err != nil {
		return false, gitReason(err), err.Error()
	}
	if branch != job.Branch {
		return false, "identity", fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", job.Branch, tree, branch)
	}
	if err := r.submodules(ctx, job.Git, job.AdminDir, tree, location...); err != nil {
		var refused *cleanup.RefusedError
		if errors.As(err, &refused) {
			return false, refused.Reason, refused.Detail
		}
		return false, gitReason(err), err.Error()
	}
	return branchGone, "", ""
}

func (r *Relocator) clean(ctx context.Context, job *cleanup.Job, tree string, skip func(status, name []byte) bool) (string, string) {
	if job.Force {
		return "", ""
	}
	dirt, err := r.dirtSkipping(ctx, job.Git, skip, "--git-dir="+job.AdminDir, "--work-tree="+tree)
	if err != nil {
		return gitReason(err), err.Error()
	}
	if dirt != "" {
		return "dirty", dirt
	}
	return "", ""
}

func (r *Relocator) indexed(index string) *Relocator {
	env := r.cfg.GitEnv
	if env == nil {
		env = os.Environ()
	}
	cfg := r.cfg
	cfg.GitEnv = append(slices.Clip(env), "GIT_INDEX_FILE="+index)
	return &Relocator{cfg: cfg}
}

func discardIndex(index string) error {
	for _, path := range []string{index, index + ".lock", index + seedSuffix, index + seedSuffix + ".lock"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("discard the scratch index: %w", err)
		}
	}
	return nil
}

func restorable(status, name []byte) bool {
	return dropping(status, name) || len(status) == 2 && (status[1] == ' ' || status[1] == 'D')
}

const seedSuffix = ".seed"

var strictStat = []string{
	"core.splitIndex=false",
	"core.untrackedCache=false",
	"core.trustctime=true",
	"core.checkStat=default",
	"core.ignoreStat=false",
	"core.sparseCheckout=false",
}

func (r *Relocator) readHead(ctx context.Context, job *cleanup.Job, index string, location []string) error {
	scratch := r.indexed(index)
	_, err := os.Stat(index)
	switch {
	case err == nil:
		if _, err := scratch.git(ctx, job.Git, append(slices.Clip(location), "read-tree", "-m", job.Head)...); err == nil {
			return scratch.reviewLostIgnores(ctx, job.Git, location)
		}
		if err := discardIndex(index); err != nil {
			return err
		}
	case !gone(err):
		return fmt.Errorf("read the scratch index: %w", err)
	default:
		seeded, err := r.seedIndex(ctx, job, index, location)
		if err != nil || seeded {
			return err
		}
	}
	_, err = scratch.git(ctx, job.Git, append(slices.Clip(location), "read-tree", job.Head)...)
	return err
}

func (r *Relocator) seedIndex(ctx context.Context, job *cleanup.Job, index string, location []string) (bool, error) {
	seed := index + seedSuffix
	if err := discardIndex(seed); err != nil {
		return false, err
	}
	copied, err := copyLastIndex(job, seed)
	if err != nil || !copied {
		return false, err
	}
	seeding := r.indexed(seed)
	shown := false
	if _, err := seeding.git(ctx, job.Git, append(slices.Clip(location), "read-tree", "-m", job.Head)...); err == nil {
		shown = true
		err := seeding.stream(ctx, job.Git, 0, func(head []byte) {
			shown = shown && bytes.HasPrefix(head, []byte("H "))
		}, append(slices.Clip(location), "ls-files", "-v", "-z")...)
		if err != nil {
			return false, errors.Join(err, discardIndex(seed))
		}
	}
	if !shown {
		return false, discardIndex(seed)
	}
	if err := os.Rename(seed, index); err != nil {
		return false, fmt.Errorf("publish the scratch index: %w", err)
	}
	return true, nil
}

func copyLastIndex(job *cleanup.Job, seed string) (bool, error) {
	for _, admin := range []string{job.AdminDir, filepath.Join(job.Repo, cleanup.RetiredWorktrees, filepath.Base(job.AdminDir))} {
		last := filepath.Join(admin, "index")
		info, err := os.Stat(last)
		if gone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read the tree's last index: %w", err)
		}
		body, err := os.ReadFile(last) //nolint:gosec // the job owns this admin directory
		if err != nil {
			return false, fmt.Errorf("read the tree's last index: %w", err)
		}
		if err := os.WriteFile(seed, body, 0o600); err != nil { //nolint:gosec // the journal layout names this path
			return false, fmt.Errorf("seed the scratch index: %w", err)
		}
		if err := os.Chtimes(seed, info.ModTime(), info.ModTime()); err != nil {
			return false, fmt.Errorf("seed the scratch index: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func (r *Relocator) reviewLostIgnores(ctx context.Context, git string, location []string) error {
	var kept []string
	err := r.stream(ctx, git, 0, func(head []byte) {
		if name, ok := bytes.CutPrefix(head, []byte("S ")); ok {
			kept = append(kept, string(name))
		}
	}, append(slices.Clip(location), "ls-files", "-v", "-z", "--", ":(glob)**/.gitignore")...)
	if err != nil || len(kept) == 0 {
		return err
	}
	_, err = r.git(ctx, git, append(append(slices.Clip(location), "update-index", "--no-skip-worktree", "--"), kept...)...)
	return err
}

func (r *Relocator) keepLostIgnores(ctx context.Context, git string, location []string) error {
	lost, err := r.git(ctx, git, append(slices.Clip(location), "ls-files", "--deleted", "-z", "--", ":(glob)**/.gitignore")...)
	if err != nil || lost == "" {
		return err
	}
	names := strings.Split(strings.TrimSuffix(lost, "\x00"), "\x00")
	_, err = r.git(ctx, git, append(append(slices.Clip(location), "update-index", "--skip-worktree", "--"), names...)...)
	return err
}

func (r *Relocator) matchesHead(ctx context.Context, job *cleanup.Job, tree string) (string, string) {
	index := r.cfg.Journal.Layout().ScratchIndex(job.ID)
	if err := os.Remove(index + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "reconcile", fmt.Sprintf("discard the scratch index's lock: %v", err)
	}
	scratch := r.indexed(index)
	location := []string{"--git-dir=" + job.Repo, "--work-tree=" + tree}
	for _, setting := range strictStat {
		location = append(location, "-c", setting)
	}
	if job.Head != "" {
		if err := r.readHead(ctx, job, index, location); err != nil {
			return gitReason(err), err.Error()
		}
		if err := scratch.keepLostIgnores(ctx, job.Git, location); err != nil {
			return gitReason(err), err.Error()
		}
	}
	switch _, err := os.Lstat(filepath.Join(tree, ".gitmodules")); {
	case err == nil:
		submodule, err := scratch.checkedOutSubmoduleAt(ctx, job.Git, tree, location...)
		if err != nil {
			return gitReason(err), err.Error()
		}
		if submodule != "" {
			return "submodules", fmt.Sprintf("worktree has a submodule checked out at %s", submodule)
		}
	case !gone(err):
		return "identity", err.Error()
	}
	if job.Force {
		return "", ""
	}
	if _, err := scratch.git(ctx, job.Git, append(slices.Clip(location), "update-index", "-q", "--refresh")...); err != nil {
		return gitReason(err), err.Error()
	}
	dirt, err := scratch.changes(ctx, job.Git, restorable, append(location, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=none", "--no-renames")...)
	if err != nil {
		return gitReason(err), err.Error()
	}
	if dirt != "" {
		return "dirty", dirt
	}
	return "", ""
}

func (r *Relocator) quiet(ctx context.Context, job *cleanup.Job, tree string, clean bool) string {
	err := r.unserved(ctx, tree)
	var active *cleanup.ActiveError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &active):
		return active.Error()
	case clean && errors.Is(err, cleanup.ErrUnprobed):
		if pushed, pushErr := r.headPushed(ctx, job); pushErr == nil && pushed {
			slog.Warn("cleanup: treating a clean, pushed tree as idle past a failed probe", "job", job.ID, "tree", tree, "error", err)
			return ""
		}
	}
	return unclear(tree, err)
}

func (r *Relocator) prove(ctx context.Context, job *cleanup.Job, tree spot, claiming bool) (bond, string, string) {
	held, reason, detail := bind(job, tree)
	if reason != "" {
		return held, reason, detail
	}
	claim, err := claims(job, tree.path, held.native)
	if err != nil {
		return held, "reconcile", err.Error()
	}
	if claim != "" {
		return held, "reconcile", claim
	}
	if job.Head != "" && !claiming {
		unpinned, err := r.unpinned(ctx, job.Git, job.Repo, job.RecoveryRef, job.Head)
		if err != nil {
			return held, gitReason(err), err.Error()
		}
		if unpinned != "" {
			return held, "identity", unpinned
		}
	}
	againstHead := !held.native || held.severed
	if held.native {
		if claiming {
			if err := r.followHead(ctx, job, held.admin, tree.path); err != nil {
				var refused *cleanup.RefusedError
				if errors.As(err, &refused) {
					return held, refused.Reason, refused.Detail
				}
				return held, gitReason(err), err.Error()
			}
		}
		branchGone, reason, detail := r.native(ctx, job, tree.path)
		if reason != "" {
			return held, reason, detail
		}
		againstHead = againstHead || branchGone
	}
	dirt := func() (string, string) {
		if againstHead {
			return r.matchesHead(ctx, job, tree.path)
		}
		return r.clean(ctx, job, tree.path, dropping)
	}
	if claiming {
		if reason, detail := dirt(); reason != "" {
			return held, reason, detail
		}
		if reason, detail := r.unlisted(ctx, job, "watchers", r.retire(ctx, tree.path)); reason != "" {
			return held, reason, detail
		}
	}
	if reason, detail := r.unlisted(ctx, job, "quarantine", r.cfg.Watchers.CheckQuarantine(ctx, tree.path)); reason != "" {
		return held, reason, detail
	}
	if holders := r.quiet(ctx, job, tree.path, !job.Force); holders != "" {
		return held, "activity", holders
	}
	if reason, detail := dirt(); reason != "" {
		return held, reason, detail
	}
	if job.Head != "" && claiming {
		if _, err := r.rewrite(ctx, job.Git, "--git-dir="+job.Repo, "update-ref", job.RecoveryRef, job.Head); err != nil {
			return held, gitReason(err), err.Error()
		}
	}
	return held, "", ""
}

func lock(job *cleanup.Job, admin cleanup.FileID) error {
	root, err := openAdmin(job, admin)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := publishMarker(root, job); err != nil {
		return fmt.Errorf("lock %s: %w", job.AdminDir, err)
	}
	return nil
}

func (r *Relocator) claim(ctx context.Context, job *cleanup.Job) (bool, error) {
	tree, found, err := locate(job)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !found {
		return r.settle(ctx, job)
	}
	held, reason, detail := r.prove(ctx, job, tree, true)
	if reason != "" {
		return r.block(ctx, job, reason, detail)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if held.native && !held.owned {
		if job.Retirement == "" {
			if err := r.retirementState(job, cleanup.RetirementIntent); err != nil {
				return false, err
			}
		}
		if err := lock(job, held.admin); err != nil {
			return r.block(ctx, job, "identity", err.Error())
		}
	}
	if held.native {
		if err := unregister(job, held.admin); err != nil {
			return r.block(ctx, job, "reconcile", fmt.Sprintf("unregister %s: %v", job.AdminDir, err))
		}
	}
	return r.enter(job, cleanup.PhaseClaimed)
}

func registeredAway(job *cleanup.Job) string {
	back, err := readLinkage(filepath.Join(job.AdminDir, "gitdir"))
	if err != nil {
		return ""
	}
	target, ok := linkTarget(back, job.AdminDir)
	if !ok || filepath.Dir(target) == job.Original {
		return ""
	}
	if info, err := os.Stat(filepath.Dir(target)); err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Dir(target)
}

func abandonedAdmin(job *cleanup.Job) (spot, bool, error) {
	path := filepath.Join(job.Repo, cleanup.RetiredWorktrees, filepath.Base(job.AdminDir))
	id, ok, err := cleanup.Placed(path, job.Admin)
	if err != nil || !ok {
		return spot{}, false, err
	}
	back, err := readLinkage(filepath.Join(path, "gitdir"))
	if err != nil && !gone(err) {
		return spot{}, false, err
	}
	if target, named := linkTarget(back, path); err == nil && named {
		if _, err := os.Lstat(target); !gone(err) {
			return spot{}, false, nil
		}
	}
	return spot{path: path, id: id}, true, nil
}

func (r *Relocator) settle(ctx context.Context, job *cleanup.Job) (bool, error) {
	if job.Unbound() {
		return r.enter(job, cleanup.PhaseDone)
	}
	admin, ok, err := cleanup.Placed(job.AdminDir, job.Admin)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !ok {
		_, abandoned, err := abandonedAdmin(job)
		switch {
		case err != nil:
			return r.block(ctx, job, "identity", err.Error())
		case abandoned:
			return r.enter(job, cleanup.PhaseDeletingRegistration)
		}
		return r.enter(job, cleanup.PhaseDone)
	}
	owned, err := owns(job, admin)
	if err != nil {
		return r.block(ctx, job, "locked", fmt.Sprintf("%s: %v", job.AdminDir, err))
	}
	if !owned {
		if moved := registeredAway(job); moved != "" {
			return r.block(ctx, job, "identity", fmt.Sprintf("%s moved to %s, where %s still registers it; the job deletes nothing there", job.Original, moved, job.AdminDir))
		}
		return r.block(ctx, job, "activity", fmt.Sprintf(
			"%s is gone from every path job %s names, but %s still registers it without the job's lock; the tree may only have moved away, so the job waits for it to return or for git worktree prune to drop the registration",
			job.Original, job.ID, job.AdminDir,
		))
	}
	return r.enter(job, cleanup.PhaseDeletingRegistration)
}

func (r *Relocator) admitClaimed(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	tree, found, err := locate(job)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !found {
		if proceed, err := r.settle(ctx, job); err != nil || !proceed {
			return cleanup.DeletionRoot{}, err
		}
		return r.Admit(ctx, job)
	}
	var (
		reason, detail string
		held           bond
	)
	if job.Unbound() {
		reason, detail = r.vouch(ctx, job, tree)
	} else {
		held, reason, detail = r.prove(ctx, job, tree, false)
		if reason == "" && held.native && !held.owned {
			reason, detail = "identity", fmt.Sprintf("%s no longer carries job %s's lock", job.AdminDir, job.ID)
		}
	}
	if reason == "" {
		tree, reason, detail = r.moveOut(job, tree, held)
	}
	if reason != "" {
		_, err := r.block(ctx, job, reason, detail)
		return cleanup.DeletionRoot{}, err
	}
	if _, err := r.enter(job, cleanup.PhaseDeletingTree); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	return tree.root(), ctx.Err()
}

func (r *Relocator) moveOut(job *cleanup.Job, tree spot, held bond) (spot, string, string) {
	if tree.path == job.Payload {
		job.Moved = true
		return tree, "", ""
	}
	if !job.Movable() || !cleanup.Within(tree.path, r.cfg.Move) {
		return tree, "", ""
	}
	if detail := relativeLink(tree.path); detail != "" {
		return tree, "identity", detail
	}
	if _, err := os.Lstat(job.Payload); !gone(err) {
		return tree, "identity", fmt.Sprintf("%s is taken, want it absent: %v", job.Payload, err)
	}
	if id, ok, err := cleanup.Placed(tree.path, job.Tree); err != nil || !ok || id != tree.id {
		return tree, "identity", fmt.Sprintf("%s changed before it moved: %v", tree.path, err)
	}
	if held.native {
		if err := unregister(job, held.admin); err != nil {
			return tree, "reconcile", fmt.Sprintf("unregister %s: %v", job.AdminDir, err)
		}
	}
	if err := os.Rename(tree.path, job.Payload); err != nil {
		return tree, "reconcile", fmt.Sprintf("move %s out: %v", tree.path, err)
	}
	job.Moved = true
	return spot{path: job.Payload, id: tree.id}, "", ""
}

func relativeLink(tree string) string {
	dotGit, err := readLinkage(filepath.Join(tree, ".git"))
	if err != nil {
		return ""
	}
	pointer, isLink := strings.CutPrefix(dotGit, "gitdir: ")
	if isLink && !filepath.IsAbs(strings.TrimSpace(pointer)) {
		return fmt.Sprintf("%s links its registration by a relative path, which a move would break", tree)
	}
	return ""
}

func unregister(job *cleanup.Job, admin cleanup.FileID) error {
	root, err := openAdmin(job, admin)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	back, err := root.OpenFile("gitdir", os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if gone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(back.Truncate(0), back.Sync(), back.Close())
}

func (r *Relocator) admitTree(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	tree, found, err := locate(job)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !found {
		if proceed, err := r.settle(ctx, job); err != nil || !proceed {
			return cleanup.DeletionRoot{}, err
		}
		return r.Admit(ctx, job)
	}
	held, reason, detail := r.recheck(ctx, job, tree)
	if reason == "" {
		reason, detail = r.residue(ctx, job, tree, held)
	}
	moved := job.Moved
	if reason == "" {
		tree, reason, detail = r.moveOut(job, tree, held)
	}
	if reason != "" {
		_, err := r.block(ctx, job, reason, detail)
		return cleanup.DeletionRoot{}, err
	}
	if job.Moved != moved {
		job.Updated = r.cfg.Now()
		if err := r.cfg.Journal.Save(*job); err != nil {
			return cleanup.DeletionRoot{}, err
		}
	}
	return tree.root(), ctx.Err()
}

func (r *Relocator) recheck(ctx context.Context, job *cleanup.Job, tree spot) (bond, string, string) {
	var held bond
	if job.Unbound() {
		_, detail, err := registration(job.Repo, tree.path)
		if err != nil {
			return held, "reconcile", err.Error()
		}
		if detail != "" {
			return held, "reconcile", detail
		}
		if job.Orphaned {
			if detail := parented(tree.path, job.Parent); detail != "" {
				return held, "identity", detail
			}
		}
	} else {
		var reason, detail string
		if held, reason, detail = bind(job, tree); reason != "" {
			return held, reason, detail
		}
		if held.native && !held.owned {
			return held, "identity", fmt.Sprintf("%s no longer carries job %s's lock", job.AdminDir, job.ID)
		}
		claim, err := claims(job, tree.path, held.native)
		if err != nil {
			return held, "reconcile", err.Error()
		}
		if claim != "" {
			return held, "reconcile", claim
		}
	}
	if err := r.cfg.Watchers.CheckQuarantine(ctx, tree.path); err != nil {
		return held, probeReason("quarantine", err), err.Error()
	}
	if holders := r.quiet(ctx, job, tree.path, false); holders != "" {
		return held, "activity", holders
	}
	return held, "", ""
}

func (r *Relocator) residue(ctx context.Context, job *cleanup.Job, tree spot, held bond) (string, string) {
	if job.Head != "" {
		unpinned, err := r.unpinned(ctx, job.Git, job.Repo, job.RecoveryRef, job.Head)
		if err != nil {
			return gitReason(err), err.Error()
		}
		if unpinned != "" {
			return "identity", unpinned
		}
	}
	if job.Unbound() {
		return "", ""
	}
	if held.native {
		if _, reason, detail := r.native(ctx, job, tree.path); reason != "" {
			return reason, detail
		}
		if job.Head == "" {
			return r.clean(ctx, job, tree.path, deleted)
		}
	}
	return r.matchesHead(ctx, job, tree.path)
}

func (r *Relocator) admitRegistration(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	if job.Unbound() {
		_, err := r.enter(job, cleanup.PhaseDone)
		return cleanup.DeletionRoot{}, err
	}
	if _, found, err := locate(job); err != nil || found {
		if err != nil {
			return r.rejectRoot(ctx, job, "identity", err)
		}
		if _, err := r.enter(job, cleanup.PhaseClaimed); err != nil {
			return cleanup.DeletionRoot{}, err
		}
		return r.Admit(ctx, job)
	}
	admin, ok, err := cleanup.Placed(job.AdminDir, job.Admin)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if ok {
		return r.adminRoot(ctx, job, admin)
	}
	abandoned, found, err := abandonedAdmin(job)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if !found {
		_, err := r.enter(job, cleanup.PhaseDone)
		return cleanup.DeletionRoot{}, err
	}
	if holders := r.quiet(ctx, job, abandoned.path, false); holders != "" {
		_, err := r.block(ctx, job, "activity", holders)
		return cleanup.DeletionRoot{}, err
	}
	return abandoned.root(), ctx.Err()
}

func (r *Relocator) adminRoot(ctx context.Context, job *cleanup.Job, admin cleanup.FileID) (cleanup.DeletionRoot, error) {
	root, err := openAdmin(job, admin)
	if err != nil {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	defer func() { _ = root.Close() }()
	if _, err := ownMarker(root, job); err != nil {
		return r.rejectRoot(ctx, job, "identity", fmt.Errorf("%s: %w", job.AdminDir, err))
	}
	back, err := rootRead(root, "gitdir")
	if err != nil && !gone(err) {
		return r.rejectRoot(ctx, job, "identity", err)
	}
	if strings.TrimSpace(back) != "" {
		if target, ok := linkTarget(back, job.AdminDir); ok {
			if _, err := os.Lstat(target); !gone(err) {
				return r.rejectRoot(ctx, job, "identity", fmt.Errorf("%s registers %s, which exists again", job.AdminDir, target))
			}
		}
	}
	if holders := r.quiet(ctx, job, job.AdminDir, false); holders != "" {
		_, err := r.block(ctx, job, "activity", holders)
		return cleanup.DeletionRoot{}, err
	}
	return spot{path: job.AdminDir, id: admin}.root(), ctx.Err()
}

// Readmit re-proves, while a deletion Admit handed out is under way, what can
// change beneath it without running git: the root still sits where the job
// captured it, the registration still carries the job's lock and no other
// registration claims the tree, and nothing watches or holds the root, a
// probe that cannot answer blocking. It returns the root Admit handed out, the
// zero root once that root is gone or the tree has come back, or blocks the
// job; it never moves the job to another phase. A legacy job is admitted again
// in full.
func (r *Relocator) Readmit(ctx context.Context, job *cleanup.Job) (cleanup.DeletionRoot, error) {
	if err := ctx.Err(); err != nil {
		return cleanup.DeletionRoot{}, err
	}
	switch job.Phase {
	case cleanup.PhaseDeletingTree:
		tree, found, err := locate(job)
		if err != nil {
			return r.rejectRoot(ctx, job, "identity", err)
		}
		if !found {
			return cleanup.DeletionRoot{}, nil
		}
		if _, reason, detail := r.recheck(ctx, job, tree); reason != "" {
			_, err := r.block(ctx, job, reason, detail)
			return cleanup.DeletionRoot{}, err
		}
		return tree.root(), ctx.Err()
	case cleanup.PhaseDeletingRegistration:
		if _, found, err := locate(job); err != nil || found {
			if err != nil {
				return r.rejectRoot(ctx, job, "identity", err)
			}
			return cleanup.DeletionRoot{}, nil
		}
		admin, ok, err := cleanup.Placed(job.AdminDir, job.Admin)
		if err != nil {
			return r.rejectRoot(ctx, job, "identity", err)
		}
		if !ok {
			return cleanup.DeletionRoot{}, nil
		}
		return r.adminRoot(ctx, job, admin)
	}
	return r.Admit(ctx, job)
}
