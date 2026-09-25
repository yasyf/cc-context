package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const dropPrefix = "stack drop"

type dropPR struct {
	Number      int
	URL         string
	State       string
	BaseRefName string
}

func dropPRFrom(p ghPull) dropPR {
	return dropPR{Number: p.Number, URL: p.HTMLURL, State: p.graphQLState(), BaseRefName: p.Base.Ref}
}

type dropOpts struct {
	repair bool
	dryRun bool
}

func newStackDropCmd() *cobra.Command {
	var o dropOpts
	cmd := &cobra.Command{
		Use:   "drop [branch]",
		Short: "Take a branch out of the middle of the stack without closing the pull request above it",
		Long: `Take a branch out of the middle of the stack without closing the pull request above it.

GitHub closes every pull request whose base branch is deleted, and nothing
warns: gt delete restacks the children locally and reports success, and the git
push --delete that follows is what closes them. The local stack looks perfect
either way, which is why the obvious three steps in the obvious order lose a
pull request every time.

So the order is fixed here and it is the only order: every child's base moves
onto this branch's parent on GitHub first, that move is read back before
anything is deleted, and only then does the branch go — gt's rows and the local
ref, the children replayed onto the parent, and the remote ref last. Each child
is read back a second time afterwards, and a child that came back closed or
still pointing at the deleted ref fails the call rather than earning a footnote.

The branch's own pull request is left alone: whether the work is abandoned is a
judgement about the work, not about the stack. GitHub closes it anyway when its
head ref goes, and the report names the state it came back in rather than
claiming one.

The children are restacked but not pushed, so their pull requests keep showing
the dropped commits until ccx vcs stack submit puts the new heads up.

--repair is the way back from a stack this already happened to. GitHub refuses
both obvious remedies, each in the other's name: a closed pull request's base
cannot be changed, and a reopen needs a base that exists. The way through is to
push the deleted ref back — it only has to exist while GitHub validates the
reopen — reopen, move the base, and delete the ref again; the second delete
closes nothing, because no pull request points at it any more. It takes no
branch: it finds every pull request in this stack whose base ref is gone.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackDrop(cmd, args, o)
		},
	}
	cmd.Flags().BoolVar(&o.repair, "repair", false, "reopen and retarget every pull request in this stack whose base ref is gone, instead of dropping a branch")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would be done and change nothing")
	return cmd
}

func runStackDrop(cmd *cobra.Command, args []string, o dropOpts) error {
	ctx := cmd.Context()
	switch {
	case o.repair && len(args) > 0:
		return fmt.Errorf("%s: --repair takes no branch — it finds every pull request in this stack whose base ref is gone", dropPrefix)
	case !o.repair && len(args) == 0:
		return fmt.Errorf("%s: name the branch to drop", dropPrefix)
	}
	l, err := resolveLane(ctx, dropPrefix, workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return fmt.Errorf("%s: this repository is not on the graphite lane, and a stack is Graphite's — delete the branch with git branch -d", dropPrefix)
	}
	if o.repair {
		return runStackRepair(cmd, l, o.dryRun)
	}
	return runStackDropBranch(cmd, l, args[0], o.dryRun)
}

// dropPlan's own is the dropped branch's own pull request, which the drop only reads.
type dropPlan struct {
	branch   string
	parent   string
	nwo      string
	remote   string
	onRemote bool
	children []string
	chain    []string
	prs      map[string]dropPR
	own      dropPR
}

func (p dropPlan) orphans() []string {
	var orphans []string
	for _, child := range p.children {
		if _, ok := p.prs[child]; !ok {
			orphans = append(orphans, child)
		}
	}
	return orphans
}

func runStackDropBranch(cmd *cobra.Command, l lane, branch string, dryRun bool) error {
	ctx := cmd.Context()
	commonDir, err := gtCommonDir(ctx, l.dir(), dropPrefix)
	if err != nil {
		return err
	}
	state, err := gtStateAt(ctx, commonDir, dropPrefix)
	if err != nil {
		return err
	}
	plan, err := dropPlanFor(ctx, l, state, branch)
	if err != nil {
		return err
	}
	if dryRun {
		cmd.Println(dropReport(plan, gtRestackResult{}, dropPR{}, true))
		return nil
	}
	if err := dropRetargetChildren(ctx, plan); err != nil {
		return err
	}
	// A retarget that did not take is recoverable here and a closed pull
	// request one step later.
	if err := dropVerifyChildren(ctx, plan, "before the delete"); err != nil {
		return err
	}
	result, err := dropLocal(ctx, l, commonDir, plan)
	if err != nil {
		return err
	}
	if err := dropRemoteDelete(ctx, l, plan); err != nil {
		return err
	}
	if err := dropVerifyChildren(ctx, plan, "after the delete"); err != nil {
		return err
	}
	own, err := dropOwnPR(ctx, plan)
	if err != nil {
		return err
	}
	cmd.Println(dropReport(plan, result, own, false))
	return nil
}

func dropPlanFor(ctx context.Context, l lane, state gtState, branch string) (dropPlan, error) {
	trunk, err := gtTrunkBranch(dropPrefix, state)
	if err != nil {
		return dropPlan{}, err
	}
	if branch == trunk {
		return dropPlan{}, fmt.Errorf("%s: %s is trunk, and a stack cannot be dropped out from under itself", dropPrefix, branch)
	}
	s, tracked := state[branch]
	if !tracked || len(s.Parents) == 0 {
		return dropPlan{}, fmt.Errorf("%s: %w — there is no stack position to drop it from; delete it with git branch -d", dropPrefix, &errGTUntracked{Branch: branch})
	}
	plan := dropPlan{branch: branch, parent: s.Parents[0].Ref}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return dropPlan{}, fmt.Errorf("%s: %w", dropPrefix, err)
	}
	if holder := holders[branch]; holder != "" {
		return dropPlan{}, fmt.Errorf("%s: %s is checked out in %s, and git deletes no branch a working copy holds — switch that one to %s first, or take the lane down with ccx vcs worktree rm",
			dropPrefix, branch, holder, plan.parent)
	}
	if plan.chain, err = gtUpstack(dropPrefix, state, branch); err != nil {
		return dropPlan{}, err
	}
	plan.children = dropChildren(state, branch)

	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		return dropPlan{}, fmt.Errorf("%s: retargeting a pull request needs GitHub metadata: %w", dropPrefix, err)
	}
	plan.nwo = repo.NameWithOwner
	plan.prs = make(map[string]dropPR, len(plan.children))
	for _, child := range plan.children {
		pr, found, err := dropPRForBranch(ctx, plan.nwo, child, "open")
		if err != nil {
			return dropPlan{}, err
		}
		if found {
			plan.prs[child] = pr
		}
	}
	if plan.own, _, err = dropPRForBranch(ctx, plan.nwo, branch, "open"); err != nil {
		return dropPlan{}, err
	}
	if plan.remote, err = vcs.GitRemoteFor(ctx, l.dir(), branch); err != nil {
		return dropPlan{}, fmt.Errorf("%s: %w", dropPrefix, err)
	}
	heads, err := dropRemoteHeads(ctx, l.dir(), plan.remote)
	if err != nil {
		return dropPlan{}, err
	}
	_, plan.onRemote = heads[branch]
	return plan, nil
}

// dropChildren names the branches gt records as sitting directly on branch —
// the pull requests a delete would close, and the only ones a retarget has to
// reach: everything above them keeps the parent it already has.
func dropChildren(state gtState, branch string) []string {
	var children []string
	for name, s := range state {
		if len(s.Parents) > 0 && s.Parents[0].Ref == branch {
			children = append(children, name)
		}
	}
	slices.Sort(children)
	return children
}

// dropRetargetChildren moves every child's base onto the parent, before any
// deletion, which is the whole verb. A child already pointing there is left
// alone.
func dropRetargetChildren(ctx context.Context, plan dropPlan) error {
	for _, child := range plan.children {
		pr, ok := plan.prs[child]
		if !ok || pr.BaseRefName == plan.parent {
			continue
		}
		if err := dropRetarget(ctx, plan.nwo, pr.Number, plan.parent); err != nil {
			return err
		}
	}
	return nil
}

// dropVerifyChildren reads every child's pull request back and fails on either
// state this verb exists to prevent: no longer open, or still pointing at the
// branch being deleted. A silent success here is the failure being fixed.
func dropVerifyChildren(ctx context.Context, plan dropPlan, when string) error {
	var failures []error
	for _, child := range plan.children {
		pr, ok := plan.prs[child]
		if !ok {
			continue
		}
		got, err := dropPRAt(ctx, plan.nwo, pr.Number)
		if err != nil {
			return err
		}
		switch {
		case got.State != "OPEN":
			failures = append(failures, fmt.Errorf("%s's PR #%d reads %s %s — reopen and retarget it with ccx vcs stack drop --repair", child, got.Number, strings.ToLower(got.State), when))
		case got.BaseRefName != plan.parent:
			failures = append(failures, fmt.Errorf("%s's PR #%d still targets %s rather than %s %s", child, got.Number, got.BaseRefName, plan.parent, when))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", dropPrefix, errors.Join(failures...))
}

// dropLocal takes the branch out of the local stack. The state the replay reads
// is the one Reparent just wrote: it leaves each moved row's parent revision
// where it was, so the range replayed is the child's own commits and nothing of
// the branch being dropped. The branch's row and ref go last, once nothing
// records it as a parent.
func dropLocal(ctx context.Context, l lane, commonDir string, plan dropPlan) (gtRestackResult, error) {
	moves := make(map[string]string, len(plan.children))
	for _, child := range plan.children {
		moves[child] = plan.parent
	}
	if err := gtmeta.Reparent(ctx, commonDir, moves); err != nil {
		return gtRestackResult{}, fmt.Errorf("%s: %w", dropPrefix, err)
	}
	state, err := gtStateAt(ctx, commonDir, dropPrefix)
	if err != nil {
		return gtRestackResult{}, err
	}
	result, err := gtRestackChain(ctx, dropPrefix, l.checkout, l.dir(), commonDir, state, plan.chain)
	if err != nil {
		return result, fmt.Errorf("%s: %w", dropPrefix, err)
	}
	if err := dropStrandCheck(ctx, l.dir(), state, plan.branch); err != nil {
		return result, err
	}
	if err := gtmeta.Forget(ctx, commonDir, []string{plan.branch}); err != nil {
		return result, fmt.Errorf("%s: %w", dropPrefix, err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"branch", "-D", plan.branch}); err != nil {
		return result, fmt.Errorf("%s: git branch -D %s: %w", dropPrefix, plan.branch, err)
	}
	return result, nil
}

// dropStrandCheck refuses to delete a branch whose commits are still in the
// stack. It is the local half of the read-back: a replay that conflicted
// partway leaves the rows moved and the refs not, and the retry then finds no
// children under the branch at all and would delete it over work that never
// moved.
//
// A branch level with its parent carries nothing, so nothing can strand.
func dropStrandCheck(ctx context.Context, dir render.Dir, state gtState, branch string) error {
	head := gtRestackRef(branch)
	own, err := gitIsAncestor(ctx, dir, dropPrefix, head, gtRestackRef(state[branch].Parents[0].Ref))
	if err != nil || own {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(state)) {
		if name == branch {
			continue
		}
		carried, err := gitIsAncestor(ctx, dir, dropPrefix, head, gtRestackRef(name))
		if err != nil {
			return err
		}
		if carried {
			return fmt.Errorf("%s: %s still carries %s's commits, so deleting it would strand them — rebase it with %s, then run the drop again",
				dropPrefix, name, branch, gtRebaseStep)
		}
	}
	return nil
}

func dropRemoteDelete(ctx context.Context, l lane, plan dropPlan) error {
	if !plan.onRemote {
		return nil
	}
	return dropDeleteRef(ctx, l, plan.remote, plan.branch, "")
}

// dropOwnPR reads the dropped branch's own pull request back: deleting a head
// ref closes the pull request on it, and the drop makes no call of its own
// there, so the report names what GitHub came back with.
func dropOwnPR(ctx context.Context, plan dropPlan) (dropPR, error) {
	if plan.own.Number == 0 {
		return dropPR{}, nil
	}
	return dropPRAt(ctx, plan.nwo, plan.own.Number)
}

func dropReport(plan dropPlan, result gtRestackResult, own dropPR, dryRun bool) string {
	dropped, retargeted := "dropped", "retargeted"
	if dryRun {
		dropped, retargeted = "would drop", "would retarget"
	}
	segs := []string{dropped + " " + plan.branch}
	if len(plan.prs) > 0 {
		segs = append(segs, fmt.Sprintf("%s %d onto %s: %s", retargeted, len(plan.prs), plan.parent, strings.Join(dropPRNames(plan), ", ")))
	}
	if orphans := plan.orphans(); len(orphans) > 0 {
		segs = append(segs, "no open PR: "+strings.Join(orphans, ", "))
	}
	if !dryRun {
		segs = append(segs, gtRestackSegment(result))
	}
	if !plan.onRemote {
		segs = append(segs, "not on "+plan.remote)
	}
	if own.Number != 0 {
		segs = append(segs, fmt.Sprintf("its own PR #%d reads %s", own.Number, strings.ToLower(own.State)))
	}
	if len(result.held) > 0 {
		segs = append(segs, fmt.Sprintf("%d branches gt is holding, left where they are", len(result.held)))
	}
	if len(plan.prs) > 0 {
		segs = append(segs, "push the restacked branches with ccx vcs stack submit")
	}
	return strings.Join(segs, shipSep)
}

// dropPRNames is sorted in child order, so one drop's report reads the same twice.
func dropPRNames(plan dropPlan) []string {
	names := make([]string, 0, len(plan.prs))
	for _, child := range plan.children {
		if pr, ok := plan.prs[child]; ok {
			names = append(names, fmt.Sprintf("%s → #%d", child, pr.Number))
		}
	}
	return names
}

type repairJob struct {
	branch string
	pr     dropPR
	base   string
}

// runStackRepair recovers a stack a drop already went wrong on. It takes no
// branch because the branch is gone: what is left on disk is a pull request
// whose baseRefName names no ref the remote carries, and that is what this
// walks the stack for.
func runStackRepair(cmd *cobra.Command, l lane, dryRun bool) error {
	ctx := cmd.Context()
	stack, state, err := gtStackAll(ctx, l.dir(), dropPrefix)
	if err != nil {
		return err
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		return fmt.Errorf("%s: repairing a pull request needs GitHub metadata: %w", dropPrefix, err)
	}
	nwo := repo.NameWithOwner
	here, err := gitCurrentBranch(ctx, l.dir(), dropPrefix)
	if err != nil {
		return err
	}
	remote, err := vcs.GitRemoteFor(ctx, l.dir(), here)
	if err != nil {
		return fmt.Errorf("%s: %w", dropPrefix, err)
	}
	heads, err := dropRemoteHeads(ctx, l.dir(), remote)
	if err != nil {
		return err
	}
	jobs, err := dropRepairJobs(ctx, nwo, state, heads, stack)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return fmt.Errorf("%s: every pull request in this stack points at a ref the remote carries — nothing to repair", dropPrefix)
	}
	if dryRun {
		cmd.Println(dropRepairReport(jobs, true))
		return nil
	}
	if err := dropRepairApply(ctx, l, nwo, remote, jobs); err != nil {
		return err
	}
	cmd.Println(dropRepairReport(jobs, false))
	return nil
}

// dropRepairJobs names every pull request in the stack pointing at a base ref
// the remote no longer carries, and the branch gt records as its parent, which
// is where the deletion should have pointed it. A merged pull request is never
// touched.
func dropRepairJobs(ctx context.Context, nwo string, state gtState, heads map[string]string, stack []string) ([]repairJob, error) {
	var jobs []repairJob
	for _, branch := range stack {
		pr, found, err := dropPRForBranch(ctx, nwo, branch, "all")
		if err != nil {
			return nil, err
		}
		if !found || pr.State == "MERGED" {
			continue
		}
		if _, live := heads[pr.BaseRefName]; live {
			continue
		}
		s, tracked := state[branch]
		if !tracked || len(s.Parents) == 0 {
			return nil, fmt.Errorf("%s: %s's PR #%d targets %s, which the remote does not carry, and gt records no parent to point it at instead — run gt track --parent <branch> %s",
				dropPrefix, branch, pr.Number, pr.BaseRefName, branch)
		}
		parent := s.Parents[0].Ref
		if _, live := heads[parent]; !live {
			return nil, fmt.Errorf("%s: %s's PR #%d must move onto %s, which the remote does not carry either — submit that branch first with ccx vcs stack submit",
				dropPrefix, branch, pr.Number, parent)
		}
		jobs = append(jobs, repairJob{branch: branch, pr: pr, base: parent})
	}
	return jobs, nil
}

// dropRepairApply walks the one order GitHub allows: the dead ref goes back
// first, because that is all a reopen is validated against; the reopen next,
// because a closed pull request's base cannot be changed; the base moves while
// the pull request is open; and the ref goes again last, closing nothing,
// because by then no pull request points at it.
func dropRepairApply(ctx context.Context, l lane, nwo, remote string, jobs []repairJob) error {
	for _, dead := range dropDeadRefs(jobs) {
		group := dropGroup(jobs, dead.ref)
		sha, err := dropPushRef(ctx, l, remote, dead.ref, dead.base)
		if err != nil {
			return err
		}
		if err := dropRepairGroup(ctx, l, nwo, remote, dead, sha, group); err != nil {
			return errors.Join(err, dropRepairStranded(nwo, remote, dead, group))
		}
	}
	return dropVerifyRepair(ctx, nwo, jobs)
}

// dropRepairGroup moves every pull request sharing one resurrected ref, so one
// resurrection serves them all, and reads them back before deleting it again:
// a retarget that did not take turns that delete into a second close.
func dropRepairGroup(ctx context.Context, l lane, nwo, remote string, dead deadRef, sha string, group []repairJob) error {
	for _, job := range group {
		if job.pr.State != "OPEN" {
			if err := dropReopen(ctx, nwo, job.pr.Number); err != nil {
				return err
			}
		}
		if err := dropRetarget(ctx, nwo, job.pr.Number, job.base); err != nil {
			return err
		}
	}
	if err := dropVerifyRepair(ctx, nwo, group); err != nil {
		return err
	}
	return dropDeleteRef(ctx, l, remote, dead.ref, sha)
}

// dropRepairStranded names the state a failed repair leaves and the commands
// that finish it: the resurrected ref is still on the remote, and deleting it
// before every pull request has moved off it closes them again.
func dropRepairStranded(nwo, remote string, dead deadRef, group []repairJob) error {
	steps := make([]string, 0, len(group)+1)
	for _, job := range group {
		steps = append(steps, ghCommand(dropRetargetArgv(nwo, job.pr.Number, job.base)))
	}
	steps = append(steps, "git push "+remote+" --delete "+dead.ref)
	return fmt.Errorf("%s: %s is back on %s and every pull request above must move off it before it goes again — finish with: %s",
		dropPrefix, dead.ref, remote, strings.Join(steps, " && "))
}

// deadRef is one base ref to put back, and the branch whose local head it goes
// back at. Any resolvable commit would do, and a local one always resolves: the
// sha the remote reports for a branch need not be an object this checkout has.
type deadRef struct {
	ref  string
	base string
}

func dropDeadRefs(jobs []repairJob) []deadRef {
	var refs []deadRef
	for _, job := range jobs {
		if !slices.ContainsFunc(refs, func(r deadRef) bool { return r.ref == job.pr.BaseRefName }) {
			refs = append(refs, deadRef{ref: job.pr.BaseRefName, base: job.base})
		}
	}
	return refs
}

func dropGroup(jobs []repairJob, ref string) []repairJob {
	var group []repairJob
	for _, job := range jobs {
		if job.pr.BaseRefName == ref {
			group = append(group, job)
		}
	}
	return group
}

func dropVerifyRepair(ctx context.Context, nwo string, jobs []repairJob) error {
	var failures []error
	for _, job := range jobs {
		got, err := dropPRAt(ctx, nwo, job.pr.Number)
		if err != nil {
			return err
		}
		switch {
		case got.State != "OPEN":
			failures = append(failures, fmt.Errorf("%s's PR #%d is still %s", job.branch, got.Number, strings.ToLower(got.State)))
		case got.BaseRefName != job.base:
			failures = append(failures, fmt.Errorf("%s's PR #%d targets %s rather than %s", job.branch, got.Number, got.BaseRefName, job.base))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", dropPrefix, errors.Join(failures...))
}

func dropRepairReport(jobs []repairJob, dryRun bool) string {
	verb := "repaired"
	if dryRun {
		verb = "would repair"
	}
	names := make([]string, 0, len(jobs))
	for _, job := range jobs {
		names = append(names, fmt.Sprintf("#%d %s → %s", job.pr.Number, job.pr.BaseRefName, job.base))
	}
	return strings.Join([]string{fmt.Sprintf("%s %d pull requests", verb, len(jobs)), strings.Join(names, ", ")}, shipSep)
}

// dropPushRef puts a deleted branch back at base's local head and returns the
// commit it landed on, which is the lease the delete that follows is held
// under.
func dropPushRef(ctx context.Context, l lane, remote, ref, base string) (string, error) {
	argv := []string{"push", remote, gtRestackRef(base) + ":" + gtRestackRef(ref)}
	if _, err := render.RunCLI(ctx, l.dir(), "git", argv); err != nil {
		return "", fmt.Errorf("%s: git push %s %s:%s: %w", dropPrefix, remote, gtRestackRef(base), gtRestackRef(ref), err)
	}
	return gtRestackHead(ctx, dropPrefix, l.dir(), base)
}

// dropDeleteRef deletes ref on the remote. A lease is passed when the ref is
// one this run resurrected, so a branch somebody else recreated in the window
// refuses instead of being deleted out from under them.
func dropDeleteRef(ctx context.Context, l lane, remote, ref, lease string) error {
	argv := []string{"push", remote}
	if lease != "" {
		argv = append(argv, "--force-with-lease="+gtRestackRef(ref)+":"+lease)
	}
	argv = append(argv, "--delete", ref)
	if _, err := render.RunCLI(ctx, l.dir(), "git", argv); err != nil {
		return fmt.Errorf("%s: git push %s --delete %s: %w", dropPrefix, remote, ref, err)
	}
	return nil
}

// dropRemoteHeads reads every branch the remote carries in one call: which refs
// exist there decides both whether a delete has anything to do and which pull
// requests a repair reaches, and asking per ref costs a round trip each.
func dropRemoteHeads(ctx context.Context, dir render.Dir, remote string) (map[string]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"ls-remote", "--heads", remote})
	if err != nil {
		return nil, fmt.Errorf("%s: git ls-remote --heads %s: %w", dropPrefix, remote, err)
	}
	heads := make(map[string]string)
	if strings.TrimSpace(out) == "" {
		return heads, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		name, headed := strings.CutPrefix(ref, "refs/heads/")
		if !ok || !headed {
			return nil, fmt.Errorf("%s: git ls-remote --heads %s printed %q, which is no branch record", dropPrefix, remote, line)
		}
		heads[name] = sha
	}
	return heads, nil
}

// dropPRForBranch resolves branch's newest pull request, and finds none in an
// empty list. state scopes it: a drop retargets the open ones, a repair has to
// see the closed one a deletion left.
func dropPRForBranch(ctx context.Context, nwo, branch, state string) (dropPR, bool, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", ghPullsByHeadArgv(nwo, branch, state))
	if err != nil {
		return dropPR{}, false, fmt.Errorf("%s: list pull requests of %s: %w", dropPrefix, branch, err)
	}
	var prs []ghPull
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return dropPR{}, false, fmt.Errorf("%s: parse pull requests of %s: %w", dropPrefix, branch, err)
	}
	if len(prs) == 0 {
		return dropPR{}, false, nil
	}
	return dropPRFrom(prs[0]), true, nil
}

// dropPRAt reads one pull request by number, the address every verification
// uses: a branch-keyed lookup cannot answer for a pull request whose head
// branch was just deleted, and that is the read that has to work.
func dropPRAt(ctx context.Context, nwo string, number int) (dropPR, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", []string{"api", ghPullPath(nwo, number)})
	if err != nil {
		return dropPR{}, fmt.Errorf("%s: read PR #%d: %w", dropPrefix, number, err)
	}
	var pr ghPull
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return dropPR{}, fmt.Errorf("%s: parse PR #%d: %w", dropPrefix, number, err)
	}
	return dropPRFrom(pr), nil
}

func dropRetargetArgv(nwo string, number int, base string) []string {
	return ghPatchPullArgv(nwo, number, "-f", "base="+base)
}

func dropRetarget(ctx context.Context, nwo string, number int, base string) error {
	if _, err := render.RunCLI(ctx, render.Ambient, "gh", dropRetargetArgv(nwo, number, base)); err != nil {
		return fmt.Errorf("%s: retarget PR #%d onto %s: %w", dropPrefix, number, base, err)
	}
	return nil
}

func dropReopen(ctx context.Context, nwo string, number int) error {
	if _, err := render.RunCLI(ctx, render.Ambient, "gh", ghPatchPullArgv(nwo, number, "-f", "state=open")); err != nil {
		return fmt.Errorf("%s: reopen PR #%d: %w", dropPrefix, number, err)
	}
	return nil
}
