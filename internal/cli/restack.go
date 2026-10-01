package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	jjRestackAncestorRevset = "trunk() & ::@"
	jjRestackStackRevset    = "trunk()..@"
	jjRestackConflictRevset = "conflicts() & @::"
)

var errRestackDetached = errors.New("restack: detached HEAD — check out a branch before restacking")

func gtBranchCount(n int) string {
	if n == 1 {
		return "1 branch"
	}
	return fmt.Sprintf("%d branches", n)
}

type restackOpts struct {
	noGT   bool
	to     string
	parent string
}

func newRestackCmd() *cobra.Command {
	var o restackOpts
	cmd := &cobra.Command{
		Use:   "restack",
		Short: "Fetch and restack the working-copy stack onto trunk",
		Long: `Fetch and restack the working-copy stack onto trunk.

On the gt lane this is ccx vcs stack rebase --no-push, and --to stops it at a
branch the same way. jj rebases the
working-copy stack onto trunk() and rolls a conflict back. On plain git, trunk
itself is fast-forwarded, and any other branch is replayed without a checkout
onto its parent: the fetched base of its open pull request, or the fetched trunk
when it has none. The working copy holding it, which must be clean, is then
moved onto the new head. A conflict moves nothing: the rebase stops in
a conflict-<branch> workspace with rerere off, and ccx vcs stack continue
finishes it once the files are resolved and added, or ccx vcs stack abort drops
it.

--parent <branch> moves a plain-git branch onto another branch: it is replayed
from the nearer of its fork points on <branch> and on its pull request's base
onto the fetched <branch>, so the old base's newer commits stay out. With an
open pull request it is then pushed as ccx vcs push pushes, under a lease on the
head the push observed, and the pull request is retargeted onto <branch>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestack(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	cmd.Flags().StringVar(&o.to, "to", "", stackToUsage+" (gt lane only)")
	cmd.Flags().StringVar(&o.parent, "parent", "", "move the branch onto this branch, then push it and retarget its open pull request there (git lane only)")
	return cmd
}

func runRestack(cmd *cobra.Command, o restackOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "restack", workingDir(ctx), o.noGT)
	if err != nil {
		return err
	}
	if l.gt && o.parent != "" {
		return errors.New("restack: --parent on the graphite lane is ccx vcs stack rebase --parent <branch>=<parent>")
	}
	if l.gt {
		return runStackRebase(cmd, stackRebaseOpts{noPush: true, to: o.to})
	}
	if o.to != "" {
		return fmt.Errorf("restack: --to stops a Graphite stack, and this repository is on the %s lane", kindLabel(l.kind))
	}
	if o.parent != "" && l.kind != vcs.Git {
		return fmt.Errorf("restack: --parent moves a git branch, and this repository is on the %s lane", kindLabel(l.kind))
	}

	var summary string
	switch l.kind {
	case vcs.JJ:
		summary, err = restackJJ(ctx, l.dir())
	case vcs.Git:
		summary, err = restackGit(ctx, cmd, l, o.parent)
	default:
		panic(fmt.Sprintf("restack: unsupported vcs kind %d", l.kind))
	}
	if err != nil || summary == "" {
		return err
	}
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
}

func restackJJ(ctx context.Context, dir render.Dir) (string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if len(trunkNames) != 1 {
		return "", fmt.Errorf("restack: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", trunkNames)
	}
	trunk := trunkNames[0]

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"git", "fetch"}); err != nil {
		return "", fmt.Errorf("restack: jj git fetch: %w", err)
	}
	ancestors, err := jjLogLines(ctx, dir, "restack", jjRestackAncestorRevset)
	if err != nil {
		return "", err
	}
	if len(ancestors) > 0 {
		return "fetched · already up to date", nil
	}

	rebased, err := jjRestackOntoTrunk(ctx, dir, trunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched · rebased %d commit(s) onto %s", rebased, trunk), nil
}

func jjRestackOntoTrunk(ctx context.Context, dir render.Dir, trunk string) (int, error) {
	stack, err := jjLogLines(ctx, dir, "restack", jjRestackStackRevset)
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("restack: trunk %q is not an ancestor of @ but trunk()..@ is empty", trunk)
	}

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"rebase", "-b", "@", "--destination", "trunk()"}); err != nil {
		return 0, fmt.Errorf("restack: jj rebase onto trunk %q: %w — retry manually: jj rebase -b @ --destination 'trunk()'", trunk, err)
	}
	rebaseOp, err := jjOpID(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("restack: read jj rebase operation: %w", err)
	}

	conflicts, err := jjLogLines(ctx, dir, "restack", jjRestackConflictRevset)
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp})
		if revertErr == nil {
			return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed (rebase rolled back): %w", trunk, err)
		}
		return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", trunk, err, revertErr, rebaseOp)
	}
	if len(conflicts) > 0 {
		if _, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp}); revertErr != nil {
			return 0, fmt.Errorf("restack: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", trunk, revertErr, rebaseOp)
		}
		return 0, fmt.Errorf("restack: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @ --destination 'trunk()', then fix the conflicts (jj status)", trunk, len(conflicts), strings.Join(conflicts, "\n  "))
	}
	return len(stack), nil
}

// restackGit fast-forwards trunk in place, and rebases any other branch as a
// one-branch stack rebase: a conflict stops in a workspace of its own, with
// rerere off, for ccx vcs stack continue or ccx vcs stack abort. The working
// copy holding the branch must be clean, as a stack rebase requires. A rebase
// finished here prints its own summary and returns an empty one.
func restackGit(ctx context.Context, cmd *cobra.Command, l lane, parent string) (string, error) {
	dir := l.dir()
	branch, err := gitCurrentBranch(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errRestackDetached
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, branch)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if parent == branch {
		return "", fmt.Errorf("restack: --parent moves a branch onto another, and %s cannot sit on %s", branch, parent)
	}
	trunkName, recorded, err := gitRemoteHead(ctx, dir, "restack", remote)
	if err != nil {
		return "", err
	}
	var retarget *restackRetarget
	if trunkName != "" && branch != trunkName {
		if retarget, err = restackGitPR(ctx, dir, remote, branch); err != nil {
			return "", err
		}
	}
	fetch := []string{trunkName, parent}
	if retarget != nil {
		fetch = append(fetch, retarget.Base)
	}
	fetch = slices.DeleteFunc(fetch, func(name string) bool { return name == "" })
	heads := map[string]string{}
	if len(fetch) > 0 {
		if heads, err = stackRemoteHeads(ctx, dir, "restack", remote, fetch, ""); err != nil {
			return "", err
		}
	}

	var trunk vcs.Trunk
	if !recorded && heads[trunkName] != "" {
		trunk, err = vcs.TrunkFromName(ctx, dir, remote, trunkName)
	} else {
		trunk, err = vcs.ResolveTrunk(ctx, dir, remote)
	}
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if parent != "" && branch == trunk.Name() {
		return "", fmt.Errorf("restack: --parent moves a branch onto another, and %s cannot sit on %s", branch, parent)
	}
	was := trunk
	if retarget != nil && retarget.Base != trunk.Name() {
		if was, err = vcs.TrunkFromName(ctx, dir, remote, retarget.Base); err != nil {
			return "", fmt.Errorf("restack: PR #%d is based on %s: %w", retarget.PR, retarget.Base, err)
		}
	}
	onto := was
	if parent != "" {
		if onto, err = vcs.TrunkFromName(ctx, dir, remote, parent); err != nil {
			return "", fmt.Errorf("restack: --parent %s: %w", parent, err)
		}
	} else {
		retarget = nil
	}
	upToDate, err := gitIsAncestor(ctx, dir, "restack", string(onto.Ref()), "HEAD")
	if err != nil {
		return "", fmt.Errorf("restack: compare HEAD with %s: %w", onto.Ref(), err)
	}
	if upToDate && parent == "" {
		return "fetched · already up to date", nil
	}

	if branch == trunk.Name() {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"merge", "--ff-only", string(trunk.Ref())}); err != nil {
			return "", fmt.Errorf("restack: fast-forward %s to %s: %w — resolve manually: git fetch %s && git merge --ff-only %s", branch, trunk.Ref(), err, remote, trunk.Ref())
		}
		return "fetched · fast-forwarded " + trunk.Name(), nil
	}

	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if err := stackCheckHolders(ctx, l.checkout.Root, []string{branch}, holders); err != nil {
		return "", err
	}
	if err := stackRequireGit(ctx, dir, "restack"); err != nil {
		return "", err
	}
	run, err := restackGitRun(ctx, dir, l.checkout.Root, branch, onto, was)
	if err != nil {
		return "", err
	}
	run.Retarget = retarget
	commonDir, err := gtCommonDir(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	runs, err := stackRuns(commonDir)
	if err != nil {
		return "", err
	}
	if err := stackAdmit(ctx, cmd, l, commonDir, runs, run, false); err != nil {
		return "", err
	}
	if err := stackClaim(commonDir, run); err != nil {
		return "", err
	}
	if err := stackSaveRun(run); err != nil {
		return "", err
	}
	return "", stackDrive(ctx, cmd, l, commonDir, run)
}

// restackRetarget is the open pull request a --parent restack publishes and
// moves, read before anything moved.
type restackRetarget struct {
	Remote string `json:"remote"`
	Repo   string `json:"repo"`
	PR     int    `json:"pr"`
	Base   string `json:"base"`
	Pushed bool   `json:"pushed,omitempty"`
}

// restackGitPR is the branch's open pull request, nil when it has none; its
// base is the branch's parent.
func restackGitPR(ctx context.Context, dir render.Dir, remote, branch string) (*restackRetarget, error) {
	repo, err := vcs.LookupRepo(ctx, dir, false)
	if err != nil {
		return nil, fmt.Errorf("restack: %w", err)
	}
	out, err := render.RunCLI(ctx, render.Ambient, "gh", ghPullsByHeadArgv(repo.NameWithOwner, branch, "open"))
	if err != nil {
		return nil, fmt.Errorf("restack: list the open pull requests of %s: %w", branch, err)
	}
	var prs []ghPull
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("restack: parse the open pull requests of %s: %w", branch, err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &restackRetarget{Remote: remote, Repo: repo.NameWithOwner, PR: prs[0].Number, Base: prs[0].Base.Ref}, nil
}

// restackGitPublish pushes a branch a --parent restack moved and then
// retargets its pull request onto the new parent, so the pull request never
// shows the old commits against the new base. save records the push, so a run
// killed before the retarget resumes past it under ccx vcs stack continue.
func restackGitPublish(ctx context.Context, dir render.Dir, branch, head, parent string, r *restackRetarget, save func() error) (string, error) {
	retarget := ghPatchPullArgv(r.Repo, r.PR, "-f", "base="+parent)
	var pushed string
	if !r.Pushed {
		local, err := stackRevParse(ctx, dir, gtRestackRef(branch))
		if err != nil {
			return "", err
		}
		if local != head {
			return "", fmt.Errorf("restack: %s moved to %.12s after this run rewrote it to %.12s, so nothing was pushed or retargeted", branch, local, head)
		}
		if pushed, err = vcsPushGit(ctx, dir, r.Remote, branch, head, false); err != nil {
			return "", fmt.Errorf("restack: %s sits on %s locally, but the push failed — finish with ccx vcs push, then %s: %w", branch, parent, ghCommand(retarget), err)
		}
		r.Pushed = true
		if err := save(); err != nil {
			return "", err
		}
	}
	if r.Base == parent {
		return pushed, nil
	}
	if _, err := render.RunCLI(ctx, render.Ambient, "gh", retarget); err != nil {
		return "", fmt.Errorf("restack: %s is on %s and pushed; only retargeting PR #%d failed — finish it with: %s: %w", branch, parent, r.PR, ghCommand(retarget), err)
	}
	return strings.TrimPrefix(fmt.Sprintf("%s%sretargeted PR #%d onto %s (was %s)", pushed, shipSep, r.PR, parent, r.Base), shipSep), nil
}

func restackGitRun(ctx context.Context, dir render.Dir, origin, branch string, onto, was vcs.Trunk) (*stackRebaseRun, error) {
	pin, err := stackRevParse(ctx, dir, string(onto.Ref()))
	if err != nil {
		return nil, err
	}
	head, err := stackRevParse(ctx, dir, gtRestackRef(branch))
	if err != nil {
		return nil, err
	}
	base, err := restackGitForkPoint(ctx, dir, onto, pin, head)
	if err != nil {
		return nil, err
	}
	if was.Name() != onto.Name() {
		if base, err = restackGitNearerBase(ctx, dir, branch, onto, was, head, base); err != nil {
			return nil, err
		}
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("restack: %w", err)
	}
	return &stackRebaseRun{
		Trunk:   onto.Name(),
		Pin:     pin,
		NoPush:  true,
		Git:     true,
		Origin:  origin,
		Roots:   []string{branch},
		Pid:     os.Getpid(),
		Started: stackProcStart(os.Getpid()),
		Host:    host,
		Branches: []stackRebaseBranch{{
			Name:      branch,
			Parent:    onto.Name(),
			WasParent: was.Name(),
			Local:     head,
			Head:      head,
			OldBase:   base,
		}},
	}, nil
}

func restackGitForkPoint(ctx context.Context, dir render.Dir, onto vcs.Trunk, pin, head string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"merge-base", "--fork-point", string(onto.Ref()), head})
	if err != nil {
		return "", fmt.Errorf("restack: git merge-base --fork-point %s: %w", onto.Ref(), err)
	}
	switch code {
	case 0:
		return strings.TrimSpace(out), nil
	case 1:
		return stackMergeBase(ctx, dir, pin, head)
	default:
		return "", fmt.Errorf("restack: git merge-base --fork-point %s: exit %d: %s", onto.Ref(), code, strings.TrimSpace(stderr))
	}
}

// restackGitNearerBase is where the branch's own commits start when it moves
// off was: the later of its fork points on was and on the new parent, so
// neither base's commits are replayed as the branch's. Fork points on separate
// lines of history leave no single start that excludes both, and refuse.
func restackGitNearerBase(ctx context.Context, dir render.Dir, branch string, onto, was vcs.Trunk, head, base string) (string, error) {
	wasPin, err := stackRevParse(ctx, dir, string(was.Ref()))
	if err != nil {
		return "", err
	}
	wasBase, err := restackGitForkPoint(ctx, dir, was, wasPin, head)
	if err != nil {
		return "", err
	}
	later, err := gitIsAncestor(ctx, dir, "restack", base, wasBase)
	if err != nil {
		return "", err
	}
	if later {
		return wasBase, nil
	}
	earlier, err := gitIsAncestor(ctx, dir, "restack", wasBase, base)
	if err != nil {
		return "", err
	}
	if earlier {
		return base, nil
	}
	return "", fmt.Errorf("restack: %s forked from %s at %.12s and from %s at %.12s, on separate lines of history, so no single replay leaves out both bases' commits — move it by hand with git rebase --onto %s <the parent of its first own commit> %s",
		branch, onto.Name(), base, was.Name(), wasBase, onto.Ref(), branch)
}
