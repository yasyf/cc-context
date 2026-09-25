package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// dryRunPaths caps how many paths one line names, the bound the status report
// puts on its own dirty line.
const dryRunPaths = 8

// shipDryRun is what a ship would do, read out of the state ship already
// resolves before its first mutation and printed instead of doing any of it.
type shipDryRun struct {
	lane    string
	root    string
	branch  string
	trunk   string
	place   string
	parent  string
	because string

	named  []string
	staged []string
	sweeps bool

	plan     branchPlan
	refusals []string

	moves    []dryRunMove
	prs      []dryRunPR
	creates  []string
	rewrites []string
	notes    []string
}

// dryRunMove is one branch a restack would rewrite: where the ref stands now,
// what it would be replayed onto, and the working copy holding it.
type dryRunMove struct {
	branch string
	head   string
	onto   string
	holder string
	why    string
}

// dryRunPR is one open pull request the submit would push, and whether that
// push moves its head or lands on the sha it already carries.
type dryRunPR struct {
	number int
	branch string
	head   string
	moves  bool
	holder string
	author string
}

// runShipDryRun prints the ship and makes none of it. Every read below is one
// ship makes before its first mutation: gt state off disk, commit ancestry, the
// worktree list, and the Graphite lookup naming the open pull requests. It
// fetches nothing, runs no gt verb, and writes nothing.
func runShipDryRun(ctx context.Context, cmd *cobra.Command, l lane, o shipOpts, c *gtCache) error {
	r := shipDryRun{lane: kindLabel(l.kind), root: l.root}
	if l.gt {
		r.lane = "gt"
	}
	if err := dryRunPosition(ctx, l, o, c, &r); err != nil {
		return err
	}
	if err := dryRunScope(ctx, l, o, &r); err != nil {
		return err
	}
	if err := dryRunRefusals(ctx, l, o, &r); err != nil {
		return err
	}
	if l.gt {
		if err := dryRunStack(ctx, l, o, c, &r); err != nil {
			return err
		}
	} else {
		r.notes = append(r.notes, "the restack, pr head and rewrite lines are graphite-lane facts, and this lane has none")
	}
	if o.expectRemote != "" {
		r.notes = append(r.notes, "publish the existing Git commit with exact remote lease "+o.expectRemote+"; no fetch, rebase, or retry")
	}
	cmd.Print(render.Cap(renderShipDryRun(r), o.budget))
	return nil
}

// dryRunPosition resolves where the commit would go, and on the graphite lane
// the parent a track would record — the decision nobody sees until a submit has
// published the branch it landed on.
func dryRunPosition(ctx context.Context, l lane, o shipOpts, c *gtCache, r *shipDryRun) error {
	if l.gt {
		return dryRunPositionGT(ctx, l, o, c, r)
	}
	if l.kind == vcs.JJ {
		names, err := jjBookmarkNames(ctx, l.dir(), "ship", jjNearestBookmarkRevset)
		if err != nil {
			return err
		}
		r.branch = strings.Join(names, " ")
		return nil
	}
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return err
	}
	trunk, err := gitTrunkBranch(ctx, l.dir())
	if err != nil {
		return err
	}
	r.branch, r.trunk = branch, trunk
	return dryRunPlace(ctx, l, o, r)
}

func dryRunPositionGT(ctx context.Context, l lane, o shipOpts, c *gtCache, r *shipDryRun) error {
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return err
	}
	state, err := c.at(ctx)
	if err != nil {
		return err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return err
	}
	r.branch, r.trunk = branch, trunk
	if err := dryRunPlace(ctx, l, o, r); err != nil {
		return err
	}
	if branch == "" || branch == trunk {
		return nil
	}
	if s, tracked := state[branch]; tracked {
		if len(s.Parents) > 0 {
			r.parent, r.because = s.Parents[0].Ref, "graphite already tracks "+branch+" on it"
		}
		return nil
	}
	return dryRunTrack(ctx, l, o, state, r)
}

// dryRunTrack resolves the parent gt track would record without running it. gt
// track -f takes the most recent tracked ancestor and outranks --parent, so
// ship drops -f when --parent is given: the two spellings resolve to different
// branches, and only one of them was asked for.
func dryRunTrack(ctx context.Context, l lane, o shipOpts, state gtState, r *shipDryRun) error {
	if o.parent != "" {
		r.parent = o.parent
		r.because = "untracked, so gt track --parent records it (ship drops -f, which would outrank it)"
	} else {
		parent, err := dryRunNearestTracked(ctx, l, state, r.trunk, r.branch)
		if err != nil {
			return err
		}
		r.parent = parent
		r.because = "untracked, so gt track -f takes the nearest tracked ancestor"
	}
	err := gtRefuseLandedParent(ctx, l.dir(), state, r.branch, r.parent)
	var landed *errLandedParent
	if errors.As(err, &landed) {
		r.notes = append(r.notes, landed.Error())
		return nil
	}
	return err
}

// dryRunNearestTracked is the branch gt track -f would adopt onto: of the
// tracked branches this one already contains, the one every other candidate is
// an ancestor of. Trunk is the floor, so a branch cut straight off it lands
// there. One for-each-ref names every contained branch, so only those are
// ordered, walked in name order, which keeps the answer the same across runs
// when two of them are siblings rather than a chain.
func dryRunNearestTracked(ctx context.Context, l lane, state gtState, trunk, branch string) (string, error) {
	out, err := render.RunCLI(ctx, l.dir(), "git", []string{
		"for-each-ref", "--merged=" + gtRestackRef(branch), "--format=%(refname)", "refs/heads/",
	})
	if err != nil {
		return "", fmt.Errorf("ship: git for-each-ref --merged %s: %w", branch, err)
	}
	var names []string
	for _, ref := range strings.Fields(out) {
		name := strings.TrimPrefix(ref, "refs/heads/")
		if _, tracked := state[name]; tracked && name != branch && name != trunk {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	nearest := trunk
	for _, name := range names {
		ahead, err := gitIsAncestor(ctx, l.dir(), "ship", gtRestackRef(nearest), gtRestackRef(name))
		if err != nil {
			return "", err
		}
		if ahead {
			nearest = name
		}
	}
	return nearest, nil
}

// dryRunPlace names the branch the commit lands on, through the same pure
// resolver the real run uses.
func dryRunPlace(ctx context.Context, l lane, o shipOpts, r *shipDryRun) error {
	repo, err := shipTrunkRepo(ctx, l, o, r.branch, r.trunk)
	if err != nil {
		return err
	}
	plan, err := resolveBranchPlan(l, repo, o, r.branch, r.trunk)
	if err != nil {
		return err
	}
	r.plan = plan
	if plan.action == branchCreate {
		off := plan.parent
		if off == "" {
			off = r.branch
		}
		r.place = "create " + plan.name + " off " + off
		return nil
	}
	r.place = "append to " + plan.name
	return nil
}

// dryRunRefusals collects the refusals the run would make after the point
// --dry-run stops at. Without them the report answers what a ship would do and
// stays silent on whether it would run at all, which reads as permission: the
// gap that let a --no-commit dry run print a clean plan for a ship that then
// refused on an untracked file. Each one is the real check or the real
// predicate, never a restatement of its wording.
func dryRunRefusals(ctx context.Context, l lane, o shipOpts, r *shipDryRun) error {
	checks := []func() error{}
	if o.noCommit {
		checks = append(checks, func() error { _, err := shipRefuseDirty(ctx, l.dir(), l.kind, o); return err })
	}
	if l.gt {
		checks = append(checks, func() error { return gtTrunkFlagRefusal(o, r.branch, r.trunk) })
	}
	if gitBacked(l) {
		checks = append(checks, func() error { return refuseExistingBranch(ctx, l.dir(), o, r.plan) })
	}
	for _, check := range checks {
		err := check()
		var refusal *shipRefusal
		switch {
		case err == nil:
		case errors.As(err, &refusal):
			r.refusals = append(r.refusals, refusal.Error())
		default:
			return err
		}
	}
	if o.noCommit || o.amend || len(r.named) > 0 {
		return nil
	}
	return dryRunEmptyCommit(ctx, l, o, r)
}

// dryRunEmptyCommit answers what a ship with nothing to commit would do, which
// is not one thing. A branch already carrying commits trunk does not has
// nothing to cut and everything to submit, so the run ships it as --no-commit
// rather than refusing; only where there is nothing to submit either does it
// refuse. Reporting the refusal unconditionally would be the same false
// certainty this report exists to remove, in the other direction.
func dryRunEmptyCommit(ctx context.Context, l lane, o shipOpts, r *shipDryRun) error {
	landed, standing, err := shipAlreadyCommitted(ctx, l.dir(), l.kind, r.plan)
	if err != nil {
		return err
	}
	if landed {
		r.notes = append(r.notes, "nothing to commit"+shipScope(o)+", and the branch carries commits trunk does not — the run ships it as --no-commit")
		return nil
	}
	if standing != "" {
		r.refusals = append(r.refusals, "ship: nothing to commit"+shipScope(o)+", and "+standing+" — nothing to submit")
		return nil
	}
	short, subject, err := shipDescribe(ctx, l.dir(), l.kind)
	if err != nil {
		return err
	}
	r.refusals = append(r.refusals, fmt.Sprintf("ship: nothing to commit%s — did a prior ship already land %s %q?", shipScope(o), short, subject))
	return nil
}

// dryRunScope separates what the caller named from what the index already
// carries. The separation is the point: the graphite commit carries no
// pathspec, so it takes the whole index whatever paths scoped the add, and the
// staged work of a concurrent session rides out with it.
func dryRunScope(ctx context.Context, l lane, o shipOpts, r *shipDryRun) error {
	if o.noCommit {
		r.notes = append(r.notes, "--no-commit cuts no commit, so nothing in the working copy is committed")
		return nil
	}
	if !gitBacked(l) {
		r.notes = append(r.notes, "jj has no index, so the commit carries exactly the named paths")
		return nil
	}
	entries, err := vcs.GitStatus(ctx, vcs.GitArgs{Dir: l.dir(), Sub: []string{"status"}})
	if err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	for _, e := range entries {
		if pathWithinShip(l.root, e.Path, o.paths) {
			r.named = append(r.named, e.Path)
			continue
		}
		if e.X != ' ' && e.X != '?' {
			r.staged = append(r.staged, e.Path)
		}
	}
	r.sweeps = l.gt
	return nil
}

// dryRunStack reads the stack once and answers the three questions a restack
// and a submit raise: which refs move, which pull request heads move with them,
// and which paths the replay resolves anew.
func dryRunStack(ctx context.Context, l lane, o shipOpts, c *gtCache, r *shipDryRun) error {
	if r.branch == "" || r.branch == r.trunk {
		return nil
	}
	anchor, err := dryRunAnchor(ctx, c, r)
	if err != nil || anchor == "" {
		return err
	}
	state, chain, err := gtStackChain(ctx, c, anchor)
	if err != nil {
		return err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	dryRunMoves(state, chain, holders, o, r)
	if err := dryRunRewrites(ctx, l, state, r); err != nil {
		return err
	}
	return dryRunHeads(ctx, l, o, state, chain, holders, r)
}

// dryRunAnchor is the branch whose stack this ship publishes. For a tracked
// branch that is the branch itself. An untracked one has no stack yet, so the
// answer is the parent a track would adopt it onto — whose whole downstack the
// submit then publishes, which is the surprise the report exists to end. Empty
// means there is no stack to read.
func dryRunAnchor(ctx context.Context, c *gtCache, r *shipDryRun) (string, error) {
	state, err := c.at(ctx)
	if err != nil {
		return "", err
	}
	if _, tracked := state[r.branch]; tracked {
		return r.branch, nil
	}
	if r.parent == "" || r.parent == r.trunk {
		r.notes = append(r.notes, r.branch+" is untracked and would be adopted onto trunk, so the submit publishes it alone")
		return "", nil
	}
	r.notes = append(r.notes, r.branch+" is untracked, so the submit publishes the downstack of "+r.parent+" along with it")
	return r.parent, nil
}

// dryRunMoves names every branch this ship rewrites, in both directions. The
// downstack half is the restack gt asks for, which moves parents whose pull
// requests are already open; the upstack half is what a commit onto this branch
// does to the branches above it, which the run never announces.
func dryRunMoves(state gtState, chain []string, holders map[string]string, o shipOpts, r *shipDryRun) {
	movers, held := gtRestackPlan(state, gtBottomUp(chain))
	for _, branch := range movers {
		why := "its parent moves under it"
		if state[branch].NeedsRestack {
			why = "graphite reads it as off its parent"
		}
		r.moves = append(r.moves, dryRunMove{
			branch: branch,
			head:   shortSHA(state[branch].Head),
			onto:   state[branch].Parents[0].Ref,
			holder: statusHolder(holders[branch], r.root),
			why:    why,
		})
	}
	frozen := make([]string, 0, len(held))
	for branch := range held {
		frozen = append(frozen, branch)
	}
	slices.Sort(frozen)
	for _, branch := range frozen {
		r.notes = append(r.notes, branch+" is "+held[branch]+", so the restack would leave it off its parent")
	}
	if o.noCommit || !strings.HasPrefix(r.place, "append") {
		return
	}
	up, err := gtUpstack("ship", state, r.branch)
	if err != nil {
		r.notes = append(r.notes, err.Error())
		return
	}
	for _, branch := range up {
		r.moves = append(r.moves, dryRunMove{
			branch: branch,
			head:   shortSHA(state[branch].Head),
			onto:   state[branch].Parents[0].Ref,
			holder: statusHolder(holders[branch], r.root),
			why:    "the commit moves " + r.branch + " under it",
		})
	}
}

// dryRunRewrites names the paths a replay resolves anew: the ones this branch
// changed above its recorded base that the base has changed since. They are
// where a restack returns a different tree from the one measured — a generated
// file re-taken from the base, or a rename detector pairing a deletion below
// with an edit above.
func dryRunRewrites(ctx context.Context, l lane, state gtState, r *shipDryRun) error {
	seen := map[string]bool{}
	for _, m := range r.moves {
		s := state[m.branch]
		if len(s.Parents) == 0 {
			continue
		}
		base := s.Parents[0].SHA
		above, err := dryRunDiffNames(ctx, l, base, gtRestackRef(m.branch))
		if err != nil {
			return err
		}
		below, err := dryRunDiffNames(ctx, l, base, gtRestackRef(s.Parents[0].Ref))
		if err != nil {
			return err
		}
		for _, path := range above {
			if slices.Contains(below, path) && !seen[path] {
				seen[path] = true
				r.rewrites = append(r.rewrites, path)
			}
		}
	}
	slices.Sort(r.rewrites)
	return nil
}

func dryRunDiffNames(ctx context.Context, l lane, from, to string) ([]string, error) {
	out, err := render.RunCLI(ctx, l.dir(), "git", []string{"diff", "--name-only", from, to})
	if err != nil {
		return nil, fmt.Errorf("ship: diff --name-only %s %s: %w", from, to, err)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// dryRunHeads names every open pull request the submit force-pushes over, the
// working copy its branch lives in, and whether the head moves or is re-pushed
// where it stands — a submit pushes the whole stack, not only what a restack
// rewrote. Ownership is the holder rather than the GitHub author: one shared
// account authors every lane, so the path is the only field telling them apart.
func dryRunHeads(ctx context.Context, l lane, o shipOpts, state gtState, chain []string, holders map[string]string, r *shipDryRun) error {
	if o.noPush {
		r.notes = append(r.notes, "--no-push moves no remote ref, so no pull request head moves")
		return nil
	}
	moving := map[string]bool{r.branch: true}
	for _, m := range r.moves {
		moving[m.branch] = true
	}
	owner, name, err := gtRepoOwnerName(ctx, l, "ship")
	if err != nil {
		r.notes = append(r.notes, "graphite: "+err.Error())
		return nil
	}
	infos, err := gtAPIClient().PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:        owner,
		RepoName:         name,
		PRNumbers:        []int{},
		PRHeadRefNames:   gtBottomUp(chain),
		TrunkBranchNames: []string{r.trunk},
		Callsite:         "ccx",
	})
	if err != nil {
		r.notes = append(r.notes, "graphite: "+err.Error()+" — the pull request lines are unanswered, not empty")
		return nil
	}
	open := map[string]bool{}
	for _, pr := range infos {
		if pr.State != gtapi.PROpen {
			continue
		}
		open[pr.HeadRefName] = true
		r.prs = append(r.prs, dryRunPR{
			number: pr.PRNumber,
			branch: pr.HeadRefName,
			head:   shortSHA(state[pr.HeadRefName].Head),
			moves:  moving[pr.HeadRefName],
			holder: statusHolder(holders[pr.HeadRefName], r.root),
			author: pr.AuthorGithubHandle,
		})
	}
	slices.SortFunc(r.prs, func(a, b dryRunPR) int { return a.number - b.number })
	return dryRunCreates(ctx, l, state, chain, open, r)
}

// dryRunCreates names the pull requests the submit would open, each with the
// title it derives from the first commit above its base. A title taken from
// the wrong range is the one submit surprise no open pull request can warn
// about, and it is read here from the range the submit itself reads.
func dryRunCreates(ctx context.Context, l lane, state gtState, chain []string, open map[string]bool, r *shipDryRun) error {
	tr, err := gtTrunkRefOffline(ctx, l.dir(), "ship", r.trunk)
	if errors.Is(err, vcs.ErrNoTrunk) {
		r.notes = append(r.notes, "no remote trunk ref is fetched yet, so the titles a submit would derive cannot be read without one")
		return nil
	}
	if err != nil {
		return err
	}
	stacked := map[string]bool{}
	for _, branch := range chain {
		stacked[branch] = true
	}
	for _, branch := range gtBottomUp(chain) {
		if open[branch] || len(state[branch].Parents) == 0 {
			continue
		}
		base := state[branch].Parents[0].Ref
		from := base
		if !stacked[base] {
			base, from = tr.Name(), string(tr.Ref())
		}
		title, _, err := gtCreateMeta(ctx, l.dir(), "ship", branch, from, base)
		if err != nil {
			r.notes = append(r.notes, err.Error())
			continue
		}
		r.creates = append(r.creates, branch+shipSep+"opens a pull request titled "+strconv.Quote(title))
	}
	return nil
}

func renderShipDryRun(r shipDryRun) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%-*s%s\n", infoLabelWidth, label, value)
	}
	line("dry run", "nothing below was done")
	line("lane", r.lane)
	line("root", r.root)
	if r.branch != "" {
		line("branch", r.branch)
	}
	if r.trunk != "" {
		line("trunk", r.trunk)
	}
	if r.place != "" {
		line("lands", r.place)
	}
	if r.parent != "" {
		line("parent", r.parent+shipSep+r.because)
	}
	line("commit", dryRunScopeValue(r))
	for _, m := range r.moves {
		line("restack", dryRunMoveValue(m))
	}
	for _, p := range r.prs {
		line("pr head", dryRunPRValue(p))
	}
	for _, c := range r.creates {
		line("pr new", c)
	}
	if len(r.rewrites) > 0 {
		line("rewrites", dryRunPathValue(r.rewrites))
	}
	for _, f := range r.refusals {
		line("refuses", f)
	}
	for _, n := range r.notes {
		line("note", n)
	}
	return b.String()
}

// dryRunScopeValue reports the named paths and the staged ones as two counts,
// never one, since the second set is the one nobody asked for.
func dryRunScopeValue(r shipDryRun) string {
	if len(r.named) == 0 && len(r.staged) == 0 {
		return "nothing to commit"
	}
	segs := []string{fmt.Sprintf("%d %s named", len(r.named), plural(len(r.named), "file", "files"))}
	if len(r.named) > 0 {
		segs = append(segs, dryRunPathValue(r.named))
	}
	if len(r.staged) > 0 {
		verb := "left out: the commit names its own paths"
		if r.sweeps {
			verb = "swept in: the graphite commit takes the whole index"
		}
		segs = append(segs, fmt.Sprintf("%d staged elsewhere (%s)", len(r.staged), verb), dryRunPathValue(r.staged))
	}
	return strings.Join(segs, shipSep)
}

func dryRunMoveValue(m dryRunMove) string {
	return strings.Join([]string{m.branch, m.head + " onto " + m.onto, m.holder, m.why}, shipSep)
}

func dryRunPRValue(p dryRunPR) string {
	fate := " force-pushed unchanged"
	if p.moves {
		fate = " moves"
	}
	segs := []string{fmt.Sprintf("#%d", p.number), p.branch, p.head + fate, p.holder}
	if p.author != "" {
		segs = append(segs, "author "+p.author)
	}
	return strings.Join(segs, shipSep)
}

func dryRunPathValue(paths []string) string {
	if len(paths) <= dryRunPaths {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(paths[:dryRunPaths], ", "), len(paths)-dryRunPaths)
}
