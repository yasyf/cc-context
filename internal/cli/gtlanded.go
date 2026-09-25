package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// gtStackPRs is Graphite's answer about one stack's pull requests, and the
// repository it was asked about.
type gtStackPRs struct {
	owner string
	name  string
	infos []gtapi.PullRequestInfo
}

// gtStackInfo asks Graphite about every branch of a stack, and whether it
// mirrors the repository at all, in one round trip each. It is the one lookup a
// restack and a submit share: the landed parents it names are dropped before
// the restack, and the open pull requests it names are what the submit updates.
func gtStackInfo(ctx context.Context, l lane, s gtSubmit, tr vcs.Trunk, branches []string) (gtStackPRs, error) {
	owner, name, err := gtRepoOwnerName(ctx, l, s.prefix)
	if err != nil {
		return gtStackPRs{}, err
	}
	client := gtAPIClient()
	var synced gtapi.RepoSync
	var infos []gtapi.PullRequestInfo
	var infoErr error
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		synced, err = gtRepoSynced(gctx, client, l.root, owner, name)
		return err
	})
	// pull-request-info's failure is held out of the group so an unsynced repo —
	// the diagnosis worth reporting — is read first whichever call returns first.
	g.Go(func() error {
		infos, infoErr = client.PullRequestInfo(gctx, gtapi.PullRequestInfoRequest{
			RepoOwner:        owner,
			RepoName:         name,
			PRNumbers:        []int{},
			PRHeadRefNames:   branches,
			TrunkBranchNames: []string{tr.Name()},
			Callsite:         "ccx",
		})
		return nil
	})
	if err := g.Wait(); err != nil {
		return gtStackPRs{}, gtSubmitFailure(err, s)
	}
	if synced.Status != gtapi.RepoSynced {
		problem := fmt.Sprintf("graphite does not sync %s/%s (%s) — add the repo at app.graphite.dev, or pass --no-gt", owner, name, synced.Status)
		return gtStackPRs{}, &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: fmt.Errorf("gtapi: is-repo-synced: %s %s", synced.Status, synced.Message)}
	}
	if infoErr != nil {
		return gtStackPRs{}, gtSubmitFailure(infoErr, s)
	}
	return gtStackPRs{owner: owner, name: name, infos: infos}, nil
}

// open maps each branch with an open pull request to Graphite's record of it.
func (p gtStackPRs) open() map[string]gtapi.PullRequestInfo {
	open := map[string]gtapi.PullRequestInfo{}
	for _, pr := range p.infos {
		if pr.State == gtapi.PROpen {
			open[pr.HeadRefName] = pr
		}
	}
	return open
}

// gtLanded is a branch whose pull request trunk already took, and the number it
// landed as.
type gtLanded struct {
	branch string
	pr     int
}

// errLandedMoved is a landed branch whose squash on trunk does not carry the
// change the branch now does: the queue landed an older version, or the branch
// moved on since, and dropping it would drop the difference.
type errLandedMoved struct {
	Branch string
	PR     int
	Squash string
	Head   string
}

func (e *errLandedMoved) Error() string {
	return fmt.Sprintf("%s landed as #%d in %.12s, but that squash does not carry the change %s holds at %.12s — land or move the rest of its work first, or drop the branch with ccx vcs stack drop %s",
		e.Branch, e.PR, e.Squash, e.Branch, e.Head, e.Branch)
}

// gtFindLanded names the branches whose pull request already landed. A queue
// squash leaves the head no ancestor of trunk, so containment never sees one:
// a pull request Graphite reports merged or closed counts as landed when the
// pinned trunk carries the squash naming it, and only when that squash is the
// branch's own change, so an older version landing never drops newer work. A
// branch with any open pull request has not landed, whatever an older one did.
func gtFindLanded(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk, state gtState, branches []string, infos []gtapi.PullRequestInfo) ([]gtLanded, error) {
	byHead := map[string][]gtapi.PullRequestInfo{}
	for _, pr := range infos {
		byHead[pr.HeadRefName] = append(byHead[pr.HeadRefName], pr)
	}
	var landed []gtLanded
	for _, branch := range branches {
		prs := byHead[branch]
		if slices.ContainsFunc(prs, func(pr gtapi.PullRequestInfo) bool { return pr.State == gtapi.PROpen }) {
			continue
		}
		for _, pr := range prs {
			if pr.State != gtapi.PRMerged && pr.State != gtapi.PRClosed {
				continue
			}
			squash := prSquashOn(ctx, dir, string(tr.Ref()), pr.PRNumber)
			if squash == "" {
				continue
			}
			s := state[branch]
			same, err := gtSamePatch(ctx, dir, prefix, squash, s.Parents[0].SHA, s.Head)
			if err != nil {
				return nil, err
			}
			if !same {
				return nil, fmt.Errorf("%s: %w", prefix, &errLandedMoved{Branch: branch, PR: pr.PRNumber, Squash: squash, Head: s.Head})
			}
			landed = append(landed, gtLanded{branch: branch, pr: pr.PRNumber})
			break
		}
	}
	return landed, nil
}

// gtSamePatch reports whether squash carries exactly the change base..head
// does, by patch identity.
func gtSamePatch(ctx context.Context, dir render.Dir, prefix, squash, base, head string) (bool, error) {
	landed, err := render.RunCLI(ctx, dir, "git", []string{"show", squash})
	if err != nil {
		return false, fmt.Errorf("%s: git show %s: %w", prefix, squash, err)
	}
	own, err := render.RunCLI(ctx, dir, "git", []string{"diff", base, head})
	if err != nil {
		return false, fmt.Errorf("%s: git diff %s %s: %w", prefix, base, head, err)
	}
	landedID, err := gtPatchID(ctx, dir, prefix, landed)
	if err != nil {
		return false, err
	}
	ownID, err := gtPatchID(ctx, dir, prefix, own)
	if err != nil {
		return false, err
	}
	return landedID != "" && landedID == ownID, nil
}

func gtPatchID(ctx context.Context, dir render.Dir, prefix, patch string) (string, error) {
	out, err := render.RunCLIStdin(ctx, dir, "git", []string{"patch-id", "--stable"}, []byte(patch))
	if err != nil {
		return "", fmt.Errorf("%s: git patch-id: %w", prefix, err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	return id, nil
}

// gtDropLanded takes the landed branches out of gt's record: each child moves
// onto the first ancestor that did not land, trunk at the floor, and its
// recorded base becomes the landed head it forked from, so the restack that
// follows replays the child's own commits alone onto the new parent. The landed
// branches themselves keep their refs, for ccx vcs prune to clear.
func gtDropLanded(ctx context.Context, dir render.Dir, prefix, commonDir string, state gtState, landed []gtLanded) error {
	if len(landed) == 0 {
		return nil
	}
	dropped := make(map[string]bool, len(landed))
	names := make([]string, 0, len(landed))
	for _, l := range landed {
		dropped[l.branch] = true
		names = append(names, l.branch)
	}
	moves := map[string]string{}
	forks := map[string]string{}
	for branch, s := range state {
		if len(s.Parents) == 0 || dropped[branch] || !dropped[s.Parents[0].Ref] {
			continue
		}
		fork := state[s.Parents[0].Ref].Head
		parent := s.Parents[0].Ref
		for dropped[parent] {
			parent = state[parent].Parents[0].Ref
		}
		moves[branch] = parent
		on, err := gitIsAncestor(ctx, dir, prefix, fork, gtRestackRef(branch))
		if err != nil {
			return err
		}
		if on {
			forks[branch] = fork
		}
	}
	if err := gtmeta.Reparent(ctx, commonDir, moves); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := gtmeta.RecordRestacked(ctx, commonDir, forks); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := gtmeta.Forget(ctx, commonDir, names); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

// gtLandedSegment names the branches a pass dropped for having landed.
func gtLandedSegment(landed []gtLanded) string {
	if len(landed) == 0 {
		return ""
	}
	named := make([]string, 0, len(landed))
	for _, l := range landed {
		named = append(named, fmt.Sprintf("%s (#%d)", l.branch, l.pr))
	}
	return "dropped landed " + strings.Join(named, ", ")
}

// gtWithout is chain minus the landed branches, in chain's order.
func gtWithout(chain []string, landed []gtLanded) []string {
	return slices.DeleteFunc(slices.Clone(chain), func(branch string) bool {
		return slices.ContainsFunc(landed, func(l gtLanded) bool { return l.branch == branch })
	})
}

// gtStackPass is the stack a submit works from: the stack brought
// onto the one trunk commit the submit anchors on, its landed branches dropped.
type gtStackPass struct {
	commonDir string
	state     gtState
	tr        vcs.Trunk
	pin       gtTrunkPinned
	chain     []string
	prs       gtStackPRs
	landed    []gtLanded
	result    gtRestackResult
	refused   []gtRefused
}

// gtRefused is a branch whose replay conflicted, and the branches it takes out
// of the submit with it: itself and everything stacked on it.
type gtRefused struct {
	conflict *errRestackConflict
	left     []string
}

// gtRefusedErr names what a submit left behind for a conflict, with the step
// that resolves each.
func gtRefusedErr(prefix string, refused []gtRefused) error {
	problems := make([]string, 0, len(refused))
	for _, r := range refused {
		problems = append(problems, fmt.Sprintf("left %s unsubmitted: %s", strings.Join(r.left, ", "), r.conflict.Error()))
	}
	return errors.New(gtStuck(prefix, strings.Join(problems, "; "), "; then run this again"))
}

// gtStackRestack fetches trunk once, pins the local trunk branch onto it, drops
// the branches whose pull requests landed, and replays the rest of chain onto
// what is left, bottom-up. chain must list parents before their children.
func gtStackRestack(ctx context.Context, errW io.Writer, l lane, s gtSubmit, chain []string) (gtStackPass, error) {
	commonDir, err := gtCommonDir(ctx, l.dir(), s.prefix)
	if err != nil {
		return gtStackPass{}, err
	}
	state, err := gtStateAt(ctx, commonDir, s.prefix)
	if err != nil {
		return gtStackPass{}, err
	}
	trunk, err := gtTrunkBranch(s.prefix, state)
	if err != nil {
		return gtStackPass{}, err
	}
	tr, err := gtTrunkRef(ctx, l.dir(), s.prefix, trunk)
	if err != nil {
		return gtStackPass{}, err
	}
	pin, err := gtTrunkPin(ctx, s.prefix, l.checkout, l.dir(), tr, state[trunk].Head)
	if err != nil {
		return gtStackPass{}, fmt.Errorf("%s: %w", s.prefix, err)
	}
	pass := gtStackPass{commonDir: commonDir, tr: tr, pin: pin, chain: chain}
	state, err = gtStateAt(ctx, commonDir, s.prefix)
	if err != nil {
		return gtStackPass{}, err
	}
	if len(chain) > 0 {
		if pass.prs, err = gtStackInfo(ctx, l, s, tr, chain); err != nil {
			return gtStackPass{}, err
		}
		if pass.landed, err = gtFindLanded(ctx, l.dir(), s.prefix, tr, state, chain, pass.prs.infos); err != nil {
			return gtStackPass{}, err
		}
	}
	if len(pass.landed) > 0 {
		if err := gtDropLanded(ctx, l.dir(), s.prefix, commonDir, state, pass.landed); err != nil {
			return gtStackPass{}, err
		}
		if state, err = gtStateAt(ctx, commonDir, s.prefix); err != nil {
			return gtStackPass{}, err
		}
		pass.chain = gtWithout(chain, pass.landed)
	}
	strays, err := gtFindStrays(ctx, l.dir(), s.prefix, tr, state, chain, pass.chain, pass.prs.infos)
	if err != nil {
		return gtStackPass{}, err
	}
	for _, stray := range strays {
		if _, err := fmt.Fprintf(errW, "%s: left %s alone: %s\n", s.prefix, stray.branch, stray.reason); err != nil {
			return gtStackPass{}, err
		}
		pass.chain = gtWithoutBranch(pass.chain, stray.branch)
	}
	if err := gtTrunkDrift(errW, s.prefix, state, pass.chain, pin, string(tr.Ref())); err != nil {
		return gtStackPass{}, err
	}
	_, held := gtRestackPlan(state, pass.chain)
	for _, branch := range pass.chain {
		if reason := held[branch]; reason != "" {
			return gtStackPass{}, errors.New(gtStuck(s.prefix, gtOffParent(branch, reason), ""))
		}
	}
	for {
		pass.result, err = gtRestackChain(ctx, s.prefix, l.checkout, l.dir(), commonDir, state, pass.chain)
		var conflict *errRestackConflict
		if !errors.As(err, &conflict) {
			break
		}
		left := gtAbove(state, pass.chain, []string{conflict.Branch})
		pass.refused = append(pass.refused, gtRefused{conflict: conflict, left: left})
		pass.chain = slices.DeleteFunc(pass.chain, func(branch string) bool { return slices.Contains(left, branch) })
	}
	if err != nil {
		return gtStackPass{}, fmt.Errorf("%s: %w", s.prefix, err)
	}
	if len(pass.chain) == 0 && len(pass.refused) > 0 {
		return gtStackPass{}, gtRefusedErr(s.prefix, pass.refused)
	}
	if pass.state, err = gtStateAt(ctx, commonDir, s.prefix); err != nil {
		return gtStackPass{}, err
	}
	if pass.result.empty, err = gtEmptyBranches(ctx, l.dir(), s.prefix, tr, pass.state, pass.chain, pass.result.empty); err != nil {
		return gtStackPass{}, err
	}
	for _, branch := range gtAbove(pass.state, pass.chain, pass.result.empty) {
		if !slices.Contains(pass.result.empty, branch) {
			if _, err := fmt.Fprintf(errW, "%s: skipped %s: it sits on %s, which is empty\n", s.prefix, branch, pass.state[branch].Parents[0].Ref); err != nil {
				return gtStackPass{}, err
			}
		}
		pass.chain = gtWithoutBranch(pass.chain, branch)
	}
	for _, branch := range pass.chain {
		if pass.state[branch].NeedsRestack {
			return gtStackPass{}, errors.New(gtStuck(s.prefix, gtOffParent(branch, pass.result.held[branch]), ""))
		}
	}
	return pass, nil
}

// gtEmptyBranches adds to empty, in chain order, every branch stacked on
// another branch without a commit above it: there is nothing to open a pull
// request for. One on trunk is left to gtDropContained.
func gtEmptyBranches(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk, state gtState, chain, empty []string) ([]string, error) {
	var found []string
	for _, branch := range chain {
		parent := state[branch].Parents[0].Ref
		if slices.Contains(empty, branch) {
			found = append(found, branch)
			continue
		}
		if parent == tr.Name() {
			continue
		}
		own, err := gtRevCount(ctx, prefix, dir, gtRestackRef(parent)+".."+gtRestackRef(branch))
		if err != nil {
			return nil, err
		}
		if own == 0 {
			found = append(found, branch)
		}
	}
	return found, nil
}

func gtWithoutBranch(chain []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(chain), func(branch string) bool { return branch == drop })
}

// gtAbove is roots and every branch of chain stacked on one of them, in chain
// order: an empty branch has no pull request to submit, and a branch above it
// would be submitted onto a base the submit left out.
func gtAbove(state gtState, chain, roots []string) []string {
	var above []string
	for _, branch := range chain {
		if slices.Contains(roots, branch) || slices.Contains(above, state[branch].Parents[0].Ref) {
			above = append(above, branch)
		}
	}
	return above
}

// gtStray is a branch gt parents on this stack that belongs to another lane,
// and the evidence that says so.
type gtStray struct {
	branch string
	reason string
}

// gtFindStrays names the branches above the one checked out here whose gt
// parent the rest of the record contradicts: an open pull request based on
// another branch, or a history carrying none of the parent's own commits under
// any sha. gt track adopts a branch cut at the same commit as another onto it,
// and a submit that trusted that record replayed another lane's work onto this
// stack and pushed it. Everything above a stray goes with it.
func gtFindStrays(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk, state gtState, chain, live []string, infos []gtapi.PullRequestInfo) ([]gtStray, error) {
	here, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	at := slices.Index(chain, here)
	if at < 0 {
		return nil, nil
	}
	var strays []gtStray
	stray := map[string]bool{}
	for _, branch := range chain[at+1:] {
		if !slices.Contains(live, branch) {
			continue
		}
		parent := state[branch].Parents[0].Ref
		if stray[parent] {
			stray[branch] = true
			strays = append(strays, gtStray{branch: branch, reason: "it sits on " + parent + ", which was left alone"})
			continue
		}
		reason, err := gtStrayReason(ctx, dir, prefix, tr, state, branch, parent, infos)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			stray[branch] = true
			strays = append(strays, gtStray{branch: branch, reason: reason})
		}
	}
	return strays, nil
}

func gtStrayReason(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk, state gtState, branch, parent string, infos []gtapi.PullRequestInfo) (string, error) {
	for _, pr := range infos {
		if pr.HeadRefName != branch || pr.State != gtapi.PROpen || pr.BaseRefName == "" || pr.BaseRefName == parent || pr.IsBaseRefGraphiteBase {
			continue
		}
		return fmt.Sprintf("gt records its parent as %s, but its pull request #%d is based on %s — re-record it with gt track --force --parent %s %s", parent, pr.PRNumber, pr.BaseRefName, pr.BaseRefName, branch), nil
	}
	if parent == tr.Name() {
		return "", nil
	}
	below := state[parent].Parents[0].Ref
	outside := []string{"^" + string(tr.Ref())}
	if below != tr.Name() {
		outside = append(outside, "^"+gtRestackRef(below))
	}
	parentOwn, err := gtRevCount(ctx, prefix, dir, gtRestackRef(parent), outside...)
	if err != nil || parentOwn == 0 {
		return "", err
	}
	own, err := gtRevCount(ctx, prefix, dir, gtRestackRef(branch), outside...)
	if err != nil || own == 0 {
		return "", err
	}
	missing, err := gtRevCount(ctx, prefix, dir, gtRestackRef(parent)+"..."+gtRestackRef(branch), append([]string{"--left-only", "--cherry-pick"}, outside...)...)
	if err != nil || missing < parentOwn {
		return "", err
	}
	return fmt.Sprintf("gt records its parent as %s, but it carries none of %s's commits — re-record it with gt track --force --parent %s %s, or run ccx vcs stack submit from %s's working copy to put it on %s", parent, parent, below, branch, branch, parent), nil
}
