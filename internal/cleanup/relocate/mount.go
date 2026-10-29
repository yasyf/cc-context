package relocate

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func mountLocation(job *cleanup.Job, seen observation) (string, sighting, bool) {
	switch job.Phase {
	case cleanup.PhaseQueued, cleanup.PhaseWaiting, cleanup.PhasePending:
		if !seen.registered.exists && !seen.payload.exists {
			return job.Original, seen.original, false
		}
	case cleanup.PhasePrepared:
		if !seen.registered.exists && !seen.payload.exists {
			return job.Original, seen.original, false
		}
		if seen.registered.exists && !seen.payload.exists {
			return job.Registered, seen.registered, true
		}
	case cleanup.PhaseMoved:
		if seen.registered.exists && !seen.payload.exists {
			return job.Registered, seen.registered, true
		}
		if !seen.registered.exists && seen.payload.exists {
			return job.Payload, seen.payload, true
		}
	case cleanup.PhaseDetached:
		if !seen.registered.exists && seen.payload.exists {
			return job.Payload, seen.payload, true
		}
	}
	return "", sighting{}, false
}

func (r *Relocator) devices(ctx context.Context, job *cleanup.Job, seen observation) (bool, error) {
	if job.Adopted || job.Head == "" || job.Tree.Ino == 0 || job.Admin.Ino == 0 {
		return true, nil
	}
	tree, held, moved := mountLocation(job, seen)
	if tree == "" || !held.exists || !held.dir || !seen.admin.exists || !seen.admin.dir || held.id.Ino != job.Tree.Ino || seen.admin.id.Ino != job.Admin.Ino {
		return true, nil
	}
	if held.id.Dev == job.Tree.Dev && seen.admin.id.Dev == job.Admin.Dev {
		return true, nil
	}
	want := job.Links
	if moved {
		var err error
		want, err = expectedLinks(job)
		if err != nil {
			return r.block(ctx, job, "identity", err.Error())
		}
	}
	if job.Links.DotGit == "" || job.Links.AdminGitdir == "" {
		return true, nil
	}
	links, err := readLinks(tree, job.AdminDir)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if job.Phase == cleanup.PhasePrepared && moved {
		if links.DotGit == job.Links.DotGit {
			want.DotGit = job.Links.DotGit
		}
		if links.AdminGitdir == job.Links.AdminGitdir {
			want.AdminGitdir = job.Links.AdminGitdir
		}
	}
	if detail := drifted(tree, job.AdminDir, links, want); detail != "" {
		return r.block(ctx, job, "identity", detail)
	}
	common, err := readLinkage(filepath.Join(job.AdminDir, "commondir"))
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	named, valid := linkTarget(common, job.AdminDir)
	if !valid {
		return r.block(ctx, job, "identity", "the current registration names no common directory")
	}
	repo, err := filepath.EvalSymlinks(named)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if repo != job.Repo {
		return r.block(ctx, job, "identity", fmt.Sprintf("the registration belongs to %s, not the captured %s", repo, job.Repo))
	}
	head, err := r.resolveHead(ctx, job.Git, "--git-dir="+job.AdminDir)
	if err != nil {
		return r.block(ctx, job, gitReason(err), err.Error())
	}
	if head != job.Head {
		return r.block(ctx, job, "identity", headChanged(job, tree, head))
	}
	branch, err := r.resolveBranch(ctx, job.Git, "--git-dir="+job.AdminDir)
	if err != nil {
		return r.block(ctx, job, gitReason(err), err.Error())
	}
	if branch != job.Branch {
		return r.block(ctx, job, "identity", fmt.Sprintf("branch changed: the job captured %q, %s now holds %q", job.Branch, tree, branch))
	}
	current, err := sight(tree)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	admin, err := sight(job.AdminDir)
	if err != nil {
		return r.block(ctx, job, "identity", err.Error())
	}
	if !current.is(held.id) || !admin.is(seen.admin.id) {
		return r.block(ctx, job, "identity", "the tree or registration changed while revalidating its device numbers")
	}
	updated := *job
	updated.Tree, updated.Admin = held.id, seen.admin.id
	updated.Updated = r.cfg.Now()
	if err := r.cfg.Journal.Save(updated); err != nil {
		return false, err
	}
	*job = updated
	return true, nil
}
