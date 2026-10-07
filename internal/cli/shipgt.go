package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	// gtDiverged and its detail forms are gt 1.8.6's standing repo-wide
	// reminder, repeated whole on every invocation, that no flag, env var or
	// config suppresses. gtNoise matches these to drop it and nothing else.
	gtDiverged        = "The following branches have diverged from Graphite's tracking:"
	gtDivergedBullet  = "▸ "
	gtDivergedCause   = "This can happen when a Git command run outside of Graphite changes the commit history of a branch."
	gtDivergedTrack   = "You can use gt track <branch> to remediate a diverged branch."
	gtDivergedUntrack = "To silence reminders about a diverged branch, untrack it with gt untrack <branch>."
)

// gtRef is one parent entry in a gt state branch record.
type gtRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// gtBranchState is one branch's gt state entry. gt omits false/empty fields
// (a trunk entry is just {"trunk":true}), so every field tolerates zero.
type gtBranchState struct {
	Trunk        bool
	NeedsRestack bool `json:"needs_restack"`
	Head         string
	State        string
	Parents      []gtRef
}

// gtState is gt state's parsed output: branch name to its tracked state.
type gtState map[string]gtBranchState

type errGTUntracked struct{ Branch string }

func (e *errGTUntracked) Error() string {
	return "gt state has no parent for " + e.Branch
}

// gtReport re-emits the diagnostics gt printed to errW. It runs on the failure
// path too, and that is the point: every gt failure ship reports replaces gt's
// own sentence with a recovery step, so lines nobody re-emits are lines the
// person who ran ship never sees. A streamed run reports none — they already
// reached the terminal as gt wrote them.
func gtReport(ctx context.Context, errW io.Writer, r gtResult) error {
	blocks := gtUnseen(ctx, gtBlocks(r.Diagnostics()))
	if len(blocks) == 0 {
		return nil
	}
	if _, err := io.WriteString(errW, strings.Join(blocks, "\n")+"\n"); err != nil {
		return fmt.Errorf("ship: report gt diagnostics: %w", err)
	}
	return nil
}

type gtSeenKey struct{}

// gtDedupe scopes a set of already-reported diagnostics to ctx. One ship makes
// four or five gt calls, and gt repeats its repo-wide complaints on every one.
func gtDedupe(ctx context.Context) context.Context {
	return context.WithValue(ctx, gtSeenKey{}, map[string]bool{})
}

func gtUnseen(ctx context.Context, blocks []string) []string {
	seen, ok := ctx.Value(gtSeenKey{}).(map[string]bool)
	if !ok {
		return blocks
	}
	unseen := blocks[:0]
	for _, block := range blocks {
		if seen[block] {
			continue
		}
		seen[block] = true
		unseen = append(unseen, block)
	}
	return unseen
}

func gtStateQuery(ctx context.Context, dir render.Dir, prefix string) (gtState, error) {
	return gtStateQueryFocused(ctx, dir, prefix, "")
}

func gtStateQueryFocused(ctx context.Context, dir render.Dir, prefix, branch string) (gtState, error) {
	commonDir, err := gtCommonDir(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	return gtStateAtFocused(ctx, commonDir, prefix, branch, "")
}

func gtStateAt(ctx context.Context, commonDir, prefix string) (gtState, error) {
	return gtStateAtFocused(ctx, commonDir, prefix, "", "")
}

func gtStateAtFocused(ctx context.Context, commonDir, prefix, branch, parent string) (gtState, error) {
	tracked, orphans, err := gtmeta.ReadOriginOrphans(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	if branch != "" {
		orphans, err = gtRelatedOrphans(ctx, commonDir, prefix, tracked, orphans, branch, parent)
		if err != nil {
			return nil, err
		}
	}
	adopted, err := gtAdoptOrphans(ctx, commonDir, prefix, tracked, orphans)
	if err != nil {
		return nil, err
	}
	if adopted {
		if tracked, err = gtmeta.ReadOrigin(ctx, commonDir); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
	}
	state := make(gtState, len(tracked))
	for branch, s := range tracked {
		entry := gtBranchState{Trunk: s.Trunk, NeedsRestack: s.NeedsRestack, Head: s.Head, State: s.State}
		for _, parent := range s.Parents {
			entry.Parents = append(entry.Parents, gtRef{Ref: parent.Ref, SHA: parent.SHA})
		}
		state[branch] = entry
	}
	return state, nil
}

func gtRelatedOrphans(ctx context.Context, commonDir, prefix string, tracked gtmeta.State, orphans []gtmeta.Orphan, branch, parent string) ([]gtmeta.Orphan, error) {
	byBranch := make(map[string]gtmeta.Orphan, len(orphans))
	for _, orphan := range orphans {
		byBranch[orphan.Branch] = orphan
	}
	seeds := []string{branch, parent}
	if _, known := tracked[branch]; !known && byBranch[branch].Branch == "" && parent == "" && len(orphans) > 0 {
		var refs strings.Builder
		for _, orphan := range orphans {
			refs.WriteString(gtRestackRef(orphan.Branch) + "\n")
		}
		out, err := render.RunCLIStdin(ctx, render.Dir(commonDir), "git", []string{"for-each-ref", "--merged=" + gtRestackRef(branch), "--format=%(refname:lstrip=2)", "--stdin"}, []byte(refs.String()))
		if err != nil {
			return nil, fmt.Errorf("%s: list orphan ancestors of %s: %w", prefix, branch, err)
		}
		seeds = append(seeds, strings.Fields(out)...)
	}
	selected := make(map[string]bool)
	for _, seed := range seeds {
		for name := seed; name != "" && !selected[name]; {
			selected[name] = true
			if orphan, ok := byBranch[name]; ok {
				name = orphan.Parent
			} else if state, ok := tracked[name]; ok && len(state.Parents) > 0 {
				name = state.Parents[0].Ref
			} else {
				break
			}
		}
	}
	relevant := make([]gtmeta.Orphan, 0, len(selected))
	for _, orphan := range orphans {
		if selected[orphan.Branch] {
			relevant = append(relevant, orphan)
		}
	}
	return relevant, nil
}

// gtAdoptOrphans re-records onto trunk each branch whose parent's ref was
// deleted after the parent landed, the equivalent of gt track -p <trunk>. A
// parent revision the remote trunk holds, as an ancestor or as the content a
// squash carried, is one that landed; any other orphan is left as it is.
func gtAdoptOrphans(ctx context.Context, commonDir, prefix string, tracked gtmeta.State, orphans []gtmeta.Orphan) (bool, error) {
	if len(orphans) == 0 {
		return false, nil
	}
	trunk := ""
	for name, s := range tracked {
		if s.Trunk {
			trunk = name
		}
	}
	dir := render.Dir(commonDir)
	remote := "refs/remotes/origin/" + trunk
	present, err := gitRefExists(ctx, dir, prefix, remote)
	if err != nil || !present {
		return false, err
	}
	moves := map[string]string{}
	for _, o := range orphans {
		landed, err := gtLandedIn(ctx, dir, prefix, o.ParentRevision, remote)
		if err != nil {
			return false, err
		}
		if landed {
			moves[o.Branch] = trunk
		}
	}
	if err := gtmeta.Reparent(ctx, commonDir, moves); err != nil {
		return false, fmt.Errorf("%s: %w", prefix, err)
	}
	return len(moves) > 0, nil
}

// gtLandedIn reports whether trunk already holds rev: as an ancestor, or as a
// squash whose merge into trunk changes nothing.
func gtLandedIn(ctx context.Context, dir render.Dir, prefix, rev, trunk string) (bool, error) {
	if rev == "" {
		return false, nil
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"cat-file", "-e", rev + "^{commit}"}); err != nil {
		return false, nil
	}
	held, err := gitIsAncestor(ctx, dir, prefix, rev, trunk)
	if err != nil || held {
		return held, err
	}
	merged, code, _, err := render.RunCLIExitCode(ctx, dir, "git", []string{"merge-tree", "--write-tree", trunk, rev})
	if err != nil || code != 0 {
		return false, err
	}
	tree, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", trunk + "^{tree}"})
	if err != nil {
		return false, fmt.Errorf("%s: git rev-parse %s^{tree}: %w", prefix, trunk, err)
	}
	return strings.TrimSpace(strings.SplitN(merged, "\n", 2)[0]) == strings.TrimSpace(tree), nil
}

func gtCommonDir(ctx context.Context, dir render.Dir, prefix string) (string, error) {
	argv := []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return "", fmt.Errorf("%s: git rev-parse --git-common-dir: %w", prefix, err)
	}
	return strings.TrimSpace(out), nil
}

// gtCache memoizes one run's reads of gt's metadata: the git common dir, which
// never moves, and the tracked state at it, which does. A ship otherwise re-reads
// that state once per caller that asks — six times over for an untracked branch
// that needs a restack. It never persists beyond the run.
type gtCache struct {
	dir       render.Dir
	prefix    string
	commonDir string
	state     gtState
	restack   *stackRebaseRun
	focus     string
	parent    string
}

func newGTCache(dir render.Dir, prefix string) *gtCache {
	return &gtCache{dir: dir, prefix: prefix}
}

// common resolves the git common dir on the first read that needs one, rather
// than when the cache is built: a run that never reads gt's metadata should not
// pay the lookup, and resolving it eagerly would put it ahead of the branch
// lookup whose failure names the more useful command.
func (c *gtCache) common(ctx context.Context) (string, error) {
	if c.commonDir == "" {
		commonDir, err := gtCommonDir(ctx, c.dir, c.prefix)
		if err != nil {
			return "", err
		}
		c.commonDir = commonDir
	}
	return c.commonDir, nil
}

// at answers from the memo, reading gt's metadata on a miss.
func (c *gtCache) at(ctx context.Context) (gtState, error) {
	if c.state != nil {
		return c.state, nil
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return nil, err
	}
	state, err := gtStateAtFocused(ctx, commonDir, c.prefix, c.focus, c.parent)
	if err != nil {
		return nil, err
	}
	c.state = state
	return state, nil
}

// forget drops the memoized state, which every step that moves a head or
// rewrites gt's rows must do: a state read before one names refs that have since
// moved, and every reader downstream takes it for the truth.
func (c *gtCache) forget() { c.state = nil }

// gtTrunkBranch returns the one branch state marks Trunk.
func gtTrunkBranch(prefix string, state gtState) (string, error) {
	for name, s := range state {
		if s.Trunk {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s: gt state named no trunk branch", prefix)
}

// gtDownstack walks branch to trunk via each entry's first parent, returning
// every branch visited (branch first), excluding trunk.
func gtDownstack(prefix string, state gtState, branch, trunk string) ([]string, error) {
	var chain []string
	seen := make(map[string]bool)
	cur := branch
	for cur != trunk {
		if seen[cur] {
			return nil, fmt.Errorf("%s: gt state parent chain cycles at %s", prefix, cur)
		}
		seen[cur] = true
		chain = append(chain, cur)
		s, ok := state[cur]
		switch {
		case ok && len(s.Parents) > 0:
			cur = s.Parents[0].Ref
		case cur == branch:
			return nil, fmt.Errorf("%s: %w", prefix, &errGTUntracked{Branch: cur})
		default:
			return nil, fmt.Errorf("%s: gt state has no parent for %s, an ancestor of %s — the stack is unresolvable; repair its parent with gt track %s, then run ccx vcs stack submit", prefix, cur, branch, cur)
		}
	}
	return chain, nil
}

func gtStackChain(ctx context.Context, c *gtCache, branch string) (gtState, []string, error) {
	state, err := c.at(ctx)
	if err != nil {
		return nil, nil, err
	}
	trunk, err := gtTrunkBranch(c.prefix, state)
	if err != nil {
		return nil, nil, err
	}
	if branch == trunk {
		return state, nil, nil
	}
	chain, err := gtDownstack(c.prefix, state, branch, trunk)
	if err != nil {
		return nil, nil, err
	}
	return state, chain, nil
}

// stackBranches lists the current downstack chain — current branch first, up
// to (excluding) trunk — or nil when the current branch is trunk.
func stackBranches(ctx context.Context, c *gtCache) ([]string, error) {
	branch, err := gitCurrentBranch(ctx, c.dir, c.prefix)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", c.prefix)
	}
	if c.focus != branch {
		c.focus, c.parent = branch, ""
		c.forget()
	}
	_, chain, err := gtStackChain(ctx, c, branch)
	return chain, err
}

func shipPreflightGT(ctx context.Context, errW io.Writer, l lane, o shipOpts, c *gtCache) (branchPlan, string, error) {
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return branchPlan{}, "", err
	}
	c.focus, c.parent = branch, o.parent
	state, err := c.at(ctx)
	if err != nil {
		return branchPlan{}, "", err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return branchPlan{}, "", err
	}

	if err := gtTrunkFlagRefusal(o, branch, trunk); err != nil {
		return branchPlan{}, "", err
	}

	repo, err := shipTrunkRepo(ctx, l, o, branch, trunk)
	if err != nil {
		return branchPlan{}, "", err
	}
	plan, err := resolveBranchPlan(l, repo, o, branch, trunk)
	if err != nil {
		return branchPlan{}, "", err
	}
	if branch == "" || branch == trunk {
		return plan, "", nil
	}
	switch cut, err := gtCutFromTrunk(ctx, l.dir(), o, plan, state, branch, trunk); {
	case err != nil:
		return branchPlan{}, "", err
	case cut:
		plan.moveOntoParent, plan.commitBeforeMove = true, true
		return plan, "", nil
	}

	var seg string
	moves := false
	switch s, tracked := state[branch]; {
	case !tracked && o.parent != "":
		err = gtAdoptRefusal(ctx, l, o, state, branch, o.parent, c)
		moves = true
	case !tracked:
		var adopted gtState
		if adopted, seg, err = gtTrack(ctx, errW, l, o, branch, c); err == nil {
			state = adopted
		}
	case plan.action != branchCreate && o.parent != "" && s.Parents[0].Ref != o.parent:
		_, err = gtReparentRefusal(ctx, l, o, state, branch, o.parent, c)
		moves = true
	}
	if plan.commitBeforeMove, err = gtCommitBeforeMove(ctx, l, o, plan, err); err != nil {
		return branchPlan{}, "", err
	}
	if moves || plan.commitBeforeMove {
		plan.moveOntoParent = true
		return plan, seg, nil
	}
	plan.needsRestack, err = gtNeedsRestack(state, branch, trunk)
	return plan, seg, err
}

func gtCutFromTrunk(ctx context.Context, dir render.Dir, o shipOpts, plan branchPlan, state gtState, branch, trunk string) (bool, error) {
	if _, tracked := state[branch]; tracked || o.parent != "" || !gtCreates(o, plan) {
		return false, nil
	}
	head, base, root, err := gtRootBase(ctx, dir, branch, trunk)
	return root && base == head, err
}

// gtNeedsRestack reports whether any branch of branch's downstack is off its
// parent.
func gtNeedsRestack(state gtState, branch, trunk string) (bool, error) {
	chain, err := gtDownstack("ship", state, branch, trunk)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(chain, func(b string) bool { return state[b].NeedsRestack }), nil
}

// gtMoveOntoParent applies the move the preflight cleared: an untracked branch
// is adopted onto --parent, a tracked one re-parented onto it. It runs after
// every other refusal, so a ship that stops short of its commit never leaves
// the branch moved.
func gtMoveOntoParent(ctx context.Context, errW io.Writer, l lane, o shipOpts, c *gtCache) (string, bool, error) {
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return "", false, err
	}
	state, err := c.at(ctx)
	if err != nil {
		return "", false, err
	}
	var seg string
	if _, tracked := state[branch]; tracked {
		state, seg, err = gtReparent(ctx, l, o, branch, o.parent, c)
	} else {
		state, seg, err = gtTrack(ctx, errW, l, o, branch, c)
	}
	if err != nil {
		return "", false, err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", false, err
	}
	needsRestack, err := gtNeedsRestack(state, branch, trunk)
	return seg, needsRestack, err
}

// gtTrunkFlagRefusal is the refusal a flag earns on trunk in the graphite lane,
// and nil where it earns none. It is a pure predicate over state both callers
// already hold, so --dry-run reports the same refusal the run would make
// without reaching the preflight that makes it.
func gtTrunkFlagRefusal(o shipOpts, branch, trunk string) error {
	if branch != trunk {
		return nil
	}
	switch {
	case o.noCommit:
		return refuse("ship: --no-commit on trunk is refused in the graphite lane — there is no stacked branch to submit")
	case o.amend:
		return refuse("ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)")
	}
	return nil
}

func gtResumeCmd(o shipOpts) string {
	if o.noPush {
		return ""
	}
	argv := []string{"ccx vcs ship --no-commit"}
	if o.tipOnly {
		argv = append(argv, "--tip-only")
	}
	if o.draft {
		argv = append(argv, "--draft")
	}
	for _, value := range o.prTitle {
		argv = append(argv, "--pr-title "+strconv.Quote(value))
	}
	for _, value := range o.prBodyFile {
		argv = append(argv, "--pr-body-file "+strconv.Quote(value))
	}
	return strings.Join(argv, " ")
}

// gtStuckSuffix states what the run left behind and how to pick it back up. A
// --no-commit run cut nothing, so the same invocation is the way back in and
// naming a resume line would only restate the command the caller just ran.
func gtStuckSuffix(o shipOpts) string {
	if o.noCommit {
		return ". Nothing was committed and the working copy is untouched, so re-run this same command once it is fixed."
	}
	resume := gtResumeCmd(o)
	if resume == "" {
		return ". The commit already landed and nothing was pushed, so there is nothing left to re-run."
	}
	return ". The commit already landed, so a plain re-run refuses as an empty commit — submit it with: " + resume
}

// gtSubmit is one submit's parameters, shared by ship and stack submit: which
// command is speaking, what its refusals append about the work already done,
// and the two switches gt's own --draft and --no-verify flags map to.
type gtSubmit struct {
	prefix      string
	suffix      string
	draft       bool
	noVerify    bool
	leases      map[string]string
	trunkHead   string
	publication *stackRebaseRun
	otherLanes  []string
}

func gtStuck(prefix, problem, suffix string) string {
	return prefix + ": " + problem + suffix
}

func gtRestack(ctx context.Context, l lane, suffix, branch string, c *gtCache) (string, error) {
	state, chain, err := gtStackChain(ctx, c, branch)
	if err != nil {
		return "", err
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return "", err
	}
	result, err := gtRestackChain(ctx, "ship", l.checkout, l.dir(), commonDir, state, gtBottomUp(chain), false)
	c.forget()
	if err != nil {
		var conflict *errRestackConflict
		if errors.As(err, &conflict) {
			return "", errors.New(gtStuck("ship", gtRestackStopped(err, conflict), suffix))
		}
		return "", err
	}
	state, chain, err = gtStackChain(ctx, c, branch)
	if err != nil {
		return "", err
	}
	var left []string
	for _, b := range chain {
		if !state[b].NeedsRestack {
			continue
		}
		if held := result.held[b]; strings.HasPrefix(held, gtHeldElsewhere) {
			left = append(left, b+" ("+held+")")
			continue
		}
		return "", errors.New(gtStuck("ship", gtOffParent(b, result.held[b]), suffix))
	}
	seg := gtRestackSegment(result)
	if len(left) > 0 {
		seg += " · left " + strings.Join(left, ", ") + " off its parent for its own lane to restack"
	}
	return seg, nil
}

// gtRestackStopped leads with the failure that explains the rest and still
// carries the rest. A conflict stops the replay, but the alignment and metadata
// passes run anyway, and a working copy left stale or a database left disagreeing
// with the refs is not something to hide behind the conflict that caused it.
func gtRestackStopped(err error, lead error) string {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return lead.Error()
	}
	problem := lead.Error()
	for _, also := range joined.Unwrap() {
		if also != nil && !errors.Is(also, lead) {
			problem += "; also " + also.Error()
		}
	}
	return problem
}

// gtOffParent explains a branch the restack left where it was. A branch gt is
// holding — gt freeze, a merge in progress — is one the restack was never
// allowed to move, and saying so is the difference between a state the user
// chose and a failure. Anything else is a branch gt still reads as unrestacked
// after a pass that should have moved it, which nothing here can explain.
func gtOffParent(branch, held string) string {
	if held != "" {
		return branch + " is " + held + ", so the restack left it off its parent — release it, or run ccx vcs stack submit"
	}
	return "restack left " + branch + " off its parent — run ccx vcs stack submit"
}

// gtTrack adopts an untracked branch, reporting the parent it landed on. gt
// track -f "sets the parent to the most recent tracked ancestor of the branch
// being tracked to skip prompts" and takes precedence over --parent, so a branch
// cut off another feature branch is adopted onto it and gt submit then publishes
// that unrelated branch too; --parent therefore drops -f, and either way the
// resolved parent is read back out of gt state and named in the report.
//
// A track that fails is reported as the one step that fixes it, so gt's own
// sentence would otherwise vanish twice over: the advice replaces it, and a
// canned message hides it from errors.Is. Both are kept — the diagnostics reach
// errW, and gt's failure stays the advice's cause.
//
// A --parent gt track would refuse, because the parent was rewritten after the
// branch was cut from it, first has the branch's own commits replayed onto it.
func gtTrack(ctx context.Context, errW io.Writer, l lane, o shipOpts, branch string, c *gtCache) (gtState, string, error) {
	picked := ""
	if o.parent == "" {
		parent, stacked, err := gtTrunkParent(ctx, l, c, branch)
		if err != nil {
			return nil, "", err
		}
		if stacked {
			if parent, picked, err = gtInferParent(ctx, c, branch); err != nil {
				return nil, "", err
			}
		}
		if parent != "" {
			state, err := c.at(ctx)
			if err != nil {
				return nil, "", err
			}
			if err := gtRefuseUntrackedBelow(ctx, c.dir, state, branch, parent); err != nil {
				return nil, "", err
			}
		}
		o.parent = parent
	}
	argv := []string{"track", branch, "-f", "--no-interactive"}
	replayed, adopted := "", ""
	untracked := fmt.Errorf("ship: gt track could not adopt %s — name the branch it was cut from with --parent <branch>, or pass --no-gt", branch)
	if o.parent != "" {
		argv = []string{"track", branch, "--parent", o.parent, "--no-interactive"}
		untracked = fmt.Errorf("ship: gt track could not adopt %s onto %s — pass --no-gt to ship it without graphite", branch, o.parent)
		state, err := c.at(ctx)
		if err != nil {
			return nil, "", err
		}
		if adopted, err = gtAdoptRecordedParent(ctx, errW, l, c, state, o.parent); err != nil {
			return nil, "", err
		}
		if adopted != "" {
			if state, err = c.at(ctx); err != nil {
				return nil, "", err
			}
		}
		if err := gtAdoptRefusal(ctx, l, o, state, branch, o.parent, c); err != nil {
			return nil, "", err
		}
		replaying, err := gtReplayable(ctx, c.dir, state, o.parent)
		if err != nil {
			return nil, "", err
		}
		if replaying {
			if replayed, _, _, err = gtOnto(ctx, l, o, branch, o.parent); err != nil {
				return nil, "", err
			}
		}
		if !o.amend {
			trunk, err := gtTrunkBranch("ship", state)
			if err != nil {
				return nil, "", err
			}
			if o.parent == trunk {
				head, base, root, err := gtRootBase(ctx, l.dir(), branch, trunk)
				if err != nil {
					return nil, "", err
				}
				if root && (base != head || !o.noCommit) {
					commonDir, err := gtCommonDir(ctx, l.dir(), "ship")
					if err != nil {
						return nil, "", err
					}
					if err := gtmeta.AdoptRoot(ctx, commonDir, branch, trunk, base, head); err != nil {
						return nil, "", err
					}
					c.forget()
					state, err := c.at(ctx)
					return state, "tracked " + branch + " onto " + trunk + replayed, err
				}
			}
		}
	}
	r, runErr := gtRun(ctx, c.dir, argv, errW)
	c.forget()
	if err := gtReport(ctx, errW, r); err != nil {
		return nil, "", err
	}
	if runErr != nil {
		return nil, "", &gtAdvice{advice: untracked.Error(), cause: runErr}
	}
	state, err := c.at(ctx)
	if err != nil {
		return nil, "", err
	}
	s, tracked := state[branch]
	if !tracked {
		return nil, "", untracked
	}
	seg := adopted + "tracked " + branch
	if len(s.Parents) > 0 {
		seg += " onto " + s.Parents[0].Ref
	}
	return state, seg + picked + replayed, nil
}

type gtRecordedParent struct {
	name, base string
	pr         int
}

func gtAdoptRecordedParent(ctx context.Context, errW io.Writer, l lane, c *gtCache, state gtState, parent string) (string, error) {
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", err
	}
	var chain []gtRecordedParent
	for name := parent; name != trunk; {
		if _, tracked := state[name]; tracked {
			break
		}
		local, err := gitRefExists(ctx, c.dir, "ship", gtRestackRef(name))
		if err != nil {
			return "", err
		}
		if !local && name == parent {
			return "", nil
		}
		if !local {
			return "", refuse("ship: %s, the parent Graphite records for %s, is no local branch — fetch it, or pass --no-gt", name, chain[len(chain)-1].name)
		}
		recorded, err := gtRecordedBase(ctx, l, name)
		if err != nil {
			return "", err
		}
		if recorded.base == "" {
			return "", refuse("ship: %s is a branch graphite does not track here, and Graphite records no open pull request for it — track it with gt track %s --parent <its parent>, or pass --no-gt", name, name)
		}
		chain = append(chain, recorded)
		name = recorded.base
	}
	var segs []string
	for _, b := range slices.Backward(chain) {
		r, runErr := gtRun(ctx, c.dir, []string{"track", b.name, "--parent", b.base, "--no-interactive"}, errW)
		c.forget()
		if err := gtReport(ctx, errW, r); err != nil {
			return "", err
		}
		if runErr != nil {
			return "", &gtAdvice{advice: fmt.Sprintf("ship: gt track could not adopt %s onto %s, its parent in Graphite's record of #%d — pass --no-gt to ship without graphite", b.name, b.base, b.pr), cause: runErr}
		}
		segs = append(segs, fmt.Sprintf("tracked %s onto %s from Graphite's record of #%d", b.name, b.base, b.pr)+shipSep)
	}
	return strings.Join(segs, ""), nil
}

func gtRecordedBase(ctx context.Context, l lane, branch string) (gtRecordedParent, error) {
	owner, repo, err := gtRepoOwnerName(ctx, l, "ship")
	if err != nil {
		return gtRecordedParent{}, err
	}
	infos, err := gtAPI(ctx).PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: repo, PRHeadRefNames: []string{branch}, Callsite: "ccx"})
	if err != nil {
		return gtRecordedParent{}, fmt.Errorf("ship: read Graphite's record of %s: %w", branch, err)
	}
	for _, info := range infos {
		if info.State == gtapi.PROpen && info.HeadRefName == branch {
			return gtRecordedParent{name: branch, base: info.Newest().BaseName, pr: info.PRNumber}, nil
		}
	}
	return gtRecordedParent{name: branch}, nil
}

func gtRootBase(ctx context.Context, dir render.Dir, branch, trunk string) (head, base string, root bool, err error) {
	remoteTrunk, err := gtTrunkRefOffline(ctx, dir, "ship", trunk)
	if errors.Is(err, vcs.ErrNoTrunk) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	head, err = gtRestackHead(ctx, "ship", dir, branch)
	if err != nil {
		return "", "", false, err
	}
	contained, err := gitIsAncestor(ctx, dir, "ship", head, string(remoteTrunk.Ref()))
	if err != nil {
		return "", "", false, err
	}
	if contained {
		return head, head, true, nil
	}
	parents, err := render.RunCLI(ctx, dir, "git", []string{"show", "-s", "--format=%P", gtRestackRef(branch)})
	if err != nil {
		return "", "", false, fmt.Errorf("ship: git show parents of %s: %w", branch, err)
	}
	fields := strings.Fields(parents)
	if len(fields) != 1 {
		return head, "", false, nil
	}
	base = fields[0]
	contained, err = gitIsAncestor(ctx, dir, "ship", base, string(remoteTrunk.Ref()))
	if err != nil {
		return "", "", false, err
	}
	return head, base, contained, nil
}

// gtTrunkParent names trunk as the parent of a branch with no tracked branch
// among its commits above the remote trunk, none at all included: a branch cut
// straight from trunk belongs there, whatever else gt tracks at that commit.
// stacked reports a tracked branch among them. gt track -f walks every
// tracked branch for the nearest ancestor, which took seven minutes in a
// repository tracking hundreds and then failed on a branch cut from trunk.
func gtTrunkParent(ctx context.Context, l lane, c *gtCache, branch string) (parent string, stacked bool, err error) {
	state, err := c.at(ctx)
	if err != nil {
		return "", false, err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", false, err
	}
	tr, err := gtTrunkRefOffline(ctx, l.dir(), "ship", trunk)
	if errors.Is(err, vcs.ErrNoTrunk) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	out, err := render.RunCLI(ctx, c.dir, "git", []string{"rev-list", string(tr.Ref()) + ".." + gtRestackRef(branch)})
	if err != nil {
		return "", false, fmt.Errorf("ship: git rev-list %s..%s: %w", tr.Ref(), branch, err)
	}
	above := map[string]bool{}
	for line := range strings.Lines(out) {
		above[strings.TrimSpace(line)] = true
	}
	if len(above) == 0 {
		return trunk, false, nil
	}
	for name, s := range state {
		if name != trunk && name != branch && above[s.Head] {
			return "", true, nil
		}
	}
	return trunk, false, nil
}

// gtInferParent names the parent gt track -f would pick for a branch stacked
// on a tracked one: its nearest tracked ancestor, read from branches whose refs
// exist, rather than gt's own walk over every tracked branch, which dies on one
// another lane just deleted. One the remote trunk already contains gives way to
// trunk, and picked then names it for the report.
func gtInferParent(ctx context.Context, c *gtCache, branch string) (parent, picked string, err error) {
	state, err := c.at(ctx)
	if err != nil {
		return "", "", err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", "", err
	}
	nearest, err := gtNearestTracked(ctx, c.dir, state, trunk, branch)
	if err != nil {
		return "", "", err
	}
	err = gtRefuseLandedParent(ctx, c.dir, state, branch, nearest)
	var landed *errLandedParent
	if errors.As(err, &landed) {
		return trunk, fmt.Sprintf(" (its nearest tracked ancestor %s is already in %s/%s)", nearest, landed.Remote, landed.Trunk), nil
	}
	return nearest, "", err
}

func gtNearestTracked(ctx context.Context, dir render.Dir, state gtState, trunk, branch string) (string, error) {
	var candidates []string
	for name := range state {
		if name != branch && name != trunk && !gtRecordedAbove(state, name, branch) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return trunk, nil
	}
	slices.Sort(candidates)
	var refs strings.Builder
	for _, name := range candidates {
		refs.WriteString(gtRestackRef(name) + "\n")
	}
	out, err := render.RunCLIStdin(ctx, dir, "git", []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, []byte(refs.String()))
	if err != nil {
		return "", fmt.Errorf("ship: resolve tracked parent refs: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(candidates) {
		return "", fmt.Errorf("ship: resolve tracked parent refs: got %d results for %d refs", len(lines), len(candidates))
	}
	heads := make(map[string]string, len(candidates))
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[1] != "commit" && (fields[0] != gtRestackRef(candidates[i]) || fields[1] != "missing")) {
			return "", fmt.Errorf("ship: resolve tracked parent ref %s: %q", candidates[i], line)
		}
		if fields[1] == "commit" {
			heads[candidates[i]] = fields[0]
		}
	}
	out, err = render.RunCLI(ctx, dir, "git", []string{"rev-list", gtRestackRef(trunk) + ".." + gtRestackRef(branch)})
	if err != nil {
		return "", fmt.Errorf("ship: list %s's commits above %s: %w", branch, trunk, err)
	}
	contained := make(map[string]bool)
	for _, head := range strings.Fields(out) {
		contained[head] = true
	}
	nearest := ""
	for _, name := range candidates {
		if !contained[heads[name]] {
			continue
		}
		if nearest == "" {
			nearest = name
			continue
		}
		ahead, err := gitIsAncestor(ctx, dir, "ship", gtRestackRef(nearest), gtRestackRef(name))
		if err != nil {
			if present, refErr := gitRefExists(ctx, dir, "ship", gtRestackRef(name)); refErr != nil || present {
				return "", err
			}
			continue
		}
		if ahead {
			nearest = name
		}
	}
	return cmp.Or(nearest, trunk), nil
}

// gtRefuseUntrackedBelow refuses to adopt branch onto an inferred parent when
// an untracked branch sits between them: adopted there, branch would carry
// that branch's commits into its own pull request.
func gtRefuseUntrackedBelow(ctx context.Context, dir render.Dir, state gtState, branch, parent string) error {
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return err
	}
	floor := gtRestackRef(parent)
	if parent == trunk {
		tr, err := gtTrunkRefOffline(ctx, dir, "ship", trunk)
		if errors.Is(err, vcs.ErrNoTrunk) {
			return nil
		}
		if err != nil {
			return err
		}
		floor = string(tr.Ref())
	}
	head, err := gtRestackHead(ctx, "ship", dir, branch)
	if err != nil {
		return err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{
		"for-each-ref", "--merged=" + gtRestackRef(branch), "--no-merged=" + floor, "--format=%(objectname) %(refname:lstrip=2)", "refs/heads/",
	})
	if err != nil {
		return fmt.Errorf("ship: git for-each-ref --merged %s --no-merged %s: %w", branch, floor, err)
	}
	var between []string
	for line := range strings.Lines(out) {
		sha, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if _, tracked := state[name]; !tracked && sha != head {
			between = append(between, name)
		}
	}
	if len(between) == 0 {
		return nil
	}
	return fmt.Errorf("ship: %s sits on untracked %s, above %s, and would carry its commits into its own pull request — ship %s first, then ship %s again; or pass --parent %s to adopt it onto %s with them", branch, strings.Join(between, ", "), parent, between[0], branch, parent, parent)
}

// gtRecordedAbove reports whether gt records name as stacked, at any depth, on
// branch, which makes it no candidate for branch's parent.
func gtRecordedAbove(state gtState, name, branch string) bool {
	for seen := map[string]bool{}; !seen[name]; {
		seen[name] = true
		s, tracked := state[name]
		if !tracked || len(s.Parents) == 0 {
			return false
		}
		if name = s.Parents[0].Ref; name == branch {
			return true
		}
	}
	return false
}

// gtAdoptRefusal is every refusal adopting an untracked branch onto parent
// can make before gt track runs: a parent the remote trunk already contains,
// and a parent rewritten after the branch was cut from it whose replay would
// fail. A parent gt does not know is left for gt track to refuse in its own
// words.
func gtAdoptRefusal(ctx context.Context, l lane, o shipOpts, state gtState, branch, parent string, c *gtCache) error {
	replaying, err := gtReplayable(ctx, c.dir, state, parent)
	if err != nil || !replaying {
		return err
	}
	if err := gtRefuseLandedParent(ctx, c.dir, state, branch, parent); err != nil {
		return err
	}
	_, err = gtOntoPlan(ctx, l, o, branch, parent)
	return err
}

// gtReplayable reports whether parent is a branch gt tracks and git holds, the
// only kind a branch can be replayed onto before gt records it there.
func gtReplayable(ctx context.Context, dir render.Dir, state gtState, parent string) (bool, error) {
	if _, tracked := state[parent]; !tracked {
		return false, nil
	}
	return gitRefExists(ctx, dir, "ship", gtRestackRef(parent))
}

// gtReparentRefusal is every refusal moving a tracked branch onto parent can
// make, checked before anything moves; it returns the move's replay.
func gtReparentRefusal(ctx context.Context, l lane, o shipOpts, state gtState, branch, parent string, c *gtCache) (gtOntoMove, error) {
	if _, tracked := state[parent]; !tracked {
		return gtOntoMove{}, refuse("ship: --parent %s names a branch graphite does not track, so %s cannot be recorded on it — track %s first", parent, branch, parent)
	}
	if held := state[branch].State; held != "" {
		return gtOntoMove{}, refuse("ship: %s is %s, so ship leaves it on %s — release it, then ship again", branch, held, state[branch].Parents[0].Ref)
	}
	up, err := gtUpstack("ship", state, branch)
	if err != nil {
		return gtOntoMove{}, err
	}
	if parent == branch || slices.Contains(up, parent) {
		return gtOntoMove{}, refuse("ship: --parent %s names %s or a branch stacked above it, so %s cannot move onto it", parent, branch, branch)
	}
	if err := gtRefuseLandedParent(ctx, c.dir, state, branch, parent); err != nil {
		return gtOntoMove{}, err
	}
	return gtOntoPlan(ctx, l, o, branch, parent)
}

// gtReparent moves a tracked branch onto the parent --parent names, which gt
// has no verb for short of an untrack and a re-track: its own commits are
// replayed onto the parent when the parent is no longer in its history, and the
// record then names the parent at the head the branch now sits on. The branches
// above it are left for their own lanes to restack.
func gtReparent(ctx context.Context, l lane, o shipOpts, branch, parent string, c *gtCache) (gtState, string, error) {
	state, err := c.at(ctx)
	if err != nil {
		return nil, "", err
	}
	if _, err := gtReparentRefusal(ctx, l, o, state, branch, parent, c); err != nil {
		return nil, "", err
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return nil, "", err
	}
	was := state[branch].Parents[0].Ref
	replayed, onto, moved, ontoErr := gtOnto(ctx, l, o, branch, parent)
	if ontoErr != nil && !moved {
		return nil, "", ontoErr
	}
	if err := gtmeta.Reparent(ctx, commonDir, map[string]string{branch: parent}); err != nil {
		return nil, "", fmt.Errorf("ship: %w", err)
	}
	if err := gtmeta.RecordRestacked(ctx, commonDir, map[string]string{branch: onto}); err != nil {
		return nil, "", fmt.Errorf("ship: %w", err)
	}
	c.forget()
	if ontoErr != nil {
		return nil, "", ontoErr
	}
	if state, err = c.at(ctx); err != nil {
		return nil, "", err
	}
	return state, "reparented " + branch + " from " + was + " onto " + parent + replayed, nil
}

// gtOntoMove is the replay that puts a branch on its parent's head, computed
// without moving anything: git replay writes objects, never refs. A zero head
// means the branch already carries the parent's head.
type gtOntoMove struct {
	onto    string
	fork    string
	was     string
	head    string
	own     int
	holders map[string]string
}

// gtOntoPlan computes the replay gtOnto makes and refuses the ones it cannot:
// a branch whose own commits sit among copies of the parent's, a replay that
// conflicts, and one that would overwrite uncommitted work in the working copy
// holding the branch. The commits parent carries no patch of are the branch's
// own, and they alone are replayed.
func gtOntoPlan(ctx context.Context, l lane, o shipOpts, branch, parent string) (gtOntoMove, error) {
	onto, err := gtRestackHead(ctx, "ship", l.dir(), parent)
	if err != nil {
		return gtOntoMove{}, err
	}
	on, err := gitIsAncestor(ctx, l.dir(), "ship", onto, gtRestackRef(branch))
	if err != nil || on {
		return gtOntoMove{onto: onto}, err
	}
	fork, own, err := gtOwnFork(ctx, l.dir(), o, branch, parent)
	if err != nil {
		return gtOntoMove{}, err
	}
	was, err := gtRestackHead(ctx, "ship", l.dir(), branch)
	if err != nil {
		return gtOntoMove{}, err
	}
	head, err := gtReplay(ctx, "ship", l.dir(), onto, fork, branch, gtBranchState{Head: was})
	if errors.Is(err, errReplayConflict) {
		return gtOntoMove{}, refuse("ship: %s's own commits do not replay onto %s cleanly — rebase them onto %s by hand, then ship again", branch, parent, parent)
	}
	if err != nil {
		return gtOntoMove{}, err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return gtOntoMove{}, fmt.Errorf("ship: %w", err)
	}
	if holder := holders[branch]; holder != "" {
		if _, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"read-tree", "-m", "-u", "-n", was, head}); err != nil {
			return gtOntoMove{}, &errReplayDirty{
				shipRefusal: &shipRefusal{msg: fmt.Sprintf("ship: replaying %s onto %s would overwrite uncommitted changes in %s (%v) — commit or set them aside, then ship again", branch, parent, holder, err)},
				was:         was,
				head:        head,
			}
		}
	}
	return gtOntoMove{onto: onto, fork: fork, was: was, head: head, own: own, holders: holders}, nil
}

type errReplayDirty struct {
	*shipRefusal
	was  string
	head string
}

func (e *errReplayDirty) Unwrap() error { return e.shipRefusal }

func gtCommitBeforeMove(ctx context.Context, l lane, o shipOpts, plan branchPlan, moveErr error) (bool, error) {
	var dirty *errReplayDirty
	if !errors.As(moveErr, &dirty) || plan.action != branchAppend || len(o.skipHunks) > 0 || len(o.onlyHunks) > 0 {
		return false, moveErr
	}
	pending, err := gtCommitsPending(ctx, l.dir(), o)
	if err != nil {
		return false, err
	}
	if !pending {
		return false, moveErr
	}
	out, err := render.RunCLI(ctx, l.dir(), "git", []string{"diff", "--name-only", "-z", "--no-renames", dirty.was, dirty.head})
	if err != nil {
		return false, fmt.Errorf("ship: git diff %s %s: %w", shortSHA(dirty.was), shortSHA(dirty.head), err)
	}
	replayed := map[string]bool{}
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			replayed[path] = true
		}
	}
	if len(replayed) == 0 {
		return true, nil
	}
	paths := slices.Sorted(maps.Keys(replayed))
	entries, err := vcs.GitStatus(ctx, vcs.GitArgs{Dir: l.dir(), Sub: []string{"status", "--untracked-files=all"}, Paths: paths})
	if err != nil {
		return false, fmt.Errorf("ship: %w", err)
	}
	for _, e := range entries {
		if e.Y != ' ' && !pathWithinShip(ctx, l.root, e.Path, o.paths) && (replayed[e.Path] || replayed[e.Orig]) {
			return false, moveErr
		}
	}
	return true, nil
}

// gtOnto puts branch on parent's head before gt records parent under it,
// moving the ref and the working copy holding it by the replay gtOntoPlan
// computes. It returns the report segment, the parent head the branch now sits
// on, and whether the branch ref moved.
func gtOnto(ctx context.Context, l lane, o shipOpts, branch, parent string) (string, string, bool, error) {
	m, err := gtOntoPlan(ctx, l, o, branch, parent)
	if err != nil || m.head == "" {
		return "", m.onto, false, err
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"update-ref", gtRestackRef(branch), m.head, m.was}); err != nil {
		return "", "", false, fmt.Errorf("ship: move %s onto %s: %w", branch, parent, err)
	}
	if _, err := gtRestackAlign(ctx, "ship", m.holders, []restackMove{{branch: branch, head: m.head, parent: m.onto, previous: m.was}}); err != nil {
		return "", m.onto, true, err
	}
	if m.own == 0 {
		return fmt.Sprintf(" (moved up to %s, holding no commit of its own until this one)", parent), m.onto, true, nil
	}
	return fmt.Sprintf(" (replayed its %d own commit(s))", m.own), m.onto, true, nil
}

// gtOwnFork finds where branch's own work starts above parent: the commit below
// the first one parent carries no patch of. A branch with no work of its own
// forks at its head when the ship is about to commit some, and is refused
// otherwise, as is one whose own commits have a copy of parent's among them,
// which no single replay range can leave out.
func gtOwnFork(ctx context.Context, dir render.Dir, o shipOpts, branch, parent string) (string, int, error) {
	marks, err := gtCherryMarks(ctx, "ship", dir, gtRestackRef(parent), gtRestackRef(branch))
	if err != nil {
		return "", 0, err
	}
	first := slices.IndexFunc(marks, func(m gtCherryMark) bool { return m.own })
	if first < 0 {
		pending, err := gtCommitsPending(ctx, dir, o)
		if err != nil {
			return "", 0, err
		}
		if pending {
			head, err := gtRestackHead(ctx, "ship", dir, branch)
			return head, 0, err
		}
		return "", 0, refuse("ship: %s holds no commit of its own above %s — every commit on it is already on %s under another sha", branch, parent, parent)
	}
	var copies []string
	for _, m := range marks[first:] {
		if !m.own {
			copies = append(copies, shortSHA(m.sha))
		}
	}
	if len(copies) > 0 {
		receipt, err := stackReadPublication(ctx, dir, parent)
		if err != nil {
			return "", 0, err
		}
		if receipt != nil {
			head, err := stackRevParse(ctx, dir, gtRestackRef(branch))
			if err != nil {
				return "", 0, err
			}
			if head == receipt.Head {
				return "", 0, refuse("ship: %s starts at %s's published head rather than its local source; run ccx vcs stack repair-published-child --parent %s from the child worktree, then ccx vcs stack submit", branch, parent, parent)
			}
		}
		return "", 0, refuse("ship: %s is not in the history of %s, and %s interleaves copies of %s's commits (%s) with its own, so no replay onto %s leaves them out — rebase it onto %s by hand with git rebase -i %s, then ship again",
			parent, branch, branch, parent, strings.Join(copies, ", "), parent, parent, parent)
	}
	return marks[first].sha + "^", len(marks) - first, nil
}

// gtCherryMark is one commit on the branch side of a comparison against its
// parent, and whether it is the branch's own — no commit on the parent side
// carries the same patch.
type gtCherryMark struct {
	sha string
	own bool
}

// gtCherryMarks walks the commits head holds and parent does not, oldest first,
// marking each by patch identity against the commits parent holds and head does
// not: the one comparison that recognizes a parent's commit after a rewrite gave
// it a new sha.
// gtCommitsPending reports whether the ship commits work the working copy holds
// before it submits: changes under its paths, or anywhere when unscoped.
func gtCommitsPending(ctx context.Context, dir render.Dir, o shipOpts) (bool, error) {
	if o.noCommit || o.amend {
		return false, nil
	}
	argv := []string{"status", "--porcelain"}
	if len(o.rootPaths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.rootPaths...)
	}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return false, fmt.Errorf("ship: git status: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

func gtCherryMarks(ctx context.Context, prefix string, dir render.Dir, parent, head string) ([]gtCherryMark, error) {
	span := parent + "..." + head
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--cherry-mark", "--right-only", "--no-merges", "--reverse", "--topo-order", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-list --cherry-mark %s: %w", prefix, span, err)
	}
	var marks []gtCherryMark
	for _, line := range strings.Fields(out) {
		marks = append(marks, gtCherryMark{sha: line[1:], own: line[0] == '+'})
	}
	return marks, nil
}

// gtRefuseLandedParent stops an adopt that landed on a branch the remote trunk
// already contains. gt track -f takes the most recent tracked ancestor, which in
// a repository carrying stale worktree branches is routinely one with no commit
// of its own left; every submit built on it then proposes a pull request holding
// no commits, which graphite refuses.
func gtRefuseLandedParent(ctx context.Context, dir render.Dir, state gtState, branch, parent string) error {
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return err
	}
	if parent == trunk {
		return nil
	}
	tr, err := gtTrunkRefOffline(ctx, dir, "ship", trunk)
	// A repository that has never fetched its trunk carries no ref to measure
	// containment against, and the submit fetches one before it needs the answer.
	if errors.Is(err, vcs.ErrNoTrunk) {
		return nil
	}
	if err != nil {
		return err
	}
	contained, err := gitIsAncestor(ctx, dir, "ship", gtRestackRef(parent), string(tr.Ref()))
	if err != nil {
		return err
	}
	if !contained {
		return nil
	}
	return fmt.Errorf("ship: %w", &errLandedParent{Branch: branch, Parent: parent, Remote: tr.Remote(), Trunk: tr.Name()})
}

// errLandedParent is an adopt that landed on a parent the remote trunk already
// contains. It is typed because --dry-run reports the same finding as a note
// rather than a refusal, and matching the message would be the only other way
// to tell it apart from a failure reading the trunk.
type errLandedParent struct {
	Branch string
	Parent string
	Remote string
	Trunk  string
}

func (e *errLandedParent) Error() string {
	return fmt.Sprintf("gt track adopted %s onto %s, which %s/%s already contains — that parent holds no commit of its own, so a stack built on it submits a pull request graphite refuses; name a real parent with --parent <branch>, or clear the stale branch out with ccx vcs prune", e.Branch, e.Parent, e.Remote, e.Trunk)
}

// gtCreates reports whether this commit starts a branch, the one commit gt
// itself still runs: a new branch needs a branch_metadata row, and gtmeta has
// no insert. An amend forms no new commit and so never creates.
func gtCreates(o shipOpts, plan branchPlan) bool {
	return !o.amend && plan.action == branchCreate
}

// gtCommitArgv is gt create's argv. It always names the branch explicitly — gt
// would otherwise "generate a branch name from the commit message", which by
// then carries the Claude-Session-Id trailer — and gets --no-ai.
func gtCommitArgv(o shipOpts, plan branchPlan) []string {
	argv := []string{"create", plan.name}
	if plan.parent != "" {
		argv = append(argv, "--onto", plan.parent)
	}
	argv = append(argv, "-m", o.message, "--no-ai", "--no-interactive")
	if o.noVerify || o.hooksRan {
		argv = append(argv, "--no-verify")
	}
	return argv
}

// gtModifyArgv is git's spelling of the commit gt modify makes. It carries no
// pathspec, exactly as gt carries none: shipGitAdd staged the ship's paths
// already, and the commit is of the index.
func gtModifyArgv(o shipOpts) []string {
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"commit", "--amend", "-m", o.message}
	case o.amend:
		argv = []string{"commit", "--amend", "--no-edit"}
	default:
		argv = []string{"commit", "-m", o.message}
	}
	if o.noVerify || o.hooksRan {
		argv = append(argv, "--no-verify")
	}
	return argv
}

func gtCommit(ctx context.Context, l lane, errW io.Writer, o shipOpts, plan branchPlan, env []string) error {
	if gtCreates(o, plan) && plan.commitBeforeMove {
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"switch", "-c", plan.name}); err != nil {
			return fmt.Errorf("ship: git switch -c %s: %w", plan.name, err)
		}
		if _, err := render.RunCLIEnv(ctx, l.dir(), "git", gtModifyArgv(o), env); err != nil {
			return errors.Join(fmt.Errorf("ship: git commit: %w", err), shipRestoreBranch(ctx, l.dir(), plan.from, plan.name))
		}
		return nil
	}
	if gtCreates(o, plan) {
		r, runErr := gtRun(ctx, l.dir(), gtCommitArgv(o, plan), errW, env...)
		if err := gtReport(ctx, errW, r); err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("ship: %w", runErr)
		}
		return nil
	}
	if _, err := render.RunCLIEnv(ctx, l.dir(), "git", gtModifyArgv(o), env); err != nil {
		return fmt.Errorf("ship: git commit: %w", err)
	}
	if o.tipOnly {
		return nil
	}
	return gtModifyRestack(ctx, l, o, plan.from)
}

func gtPublishedUpstack(ctx context.Context, dir render.Dir, state gtState, branch string) ([]string, error) {
	up, err := gtUpstack("ship", state, branch)
	if err != nil {
		return nil, err
	}
	carried := map[string]bool{branch: true}
	var published []string
	for _, name := range up {
		if !carried[state[name].Parents[0].Ref] {
			continue
		}
		present, err := stackHasPublication(ctx, dir, []string{name})
		if err != nil {
			return nil, err
		}
		if present {
			carried[name] = true
			published = append(published, name)
		}
	}
	return published, nil
}

// gtModifyRestack replays the branches above the one just committed onto its new
// head, the other half of what gt modify does. State is re-read after the commit:
// each child's recorded parent revision now names a head its parent has left,
// which is the "needs restack" gtRestackPlan reads.
func gtModifyRestack(ctx context.Context, l lane, o shipOpts, branch string) error {
	commonDir, err := gtCommonDir(ctx, l.dir(), "ship")
	if err != nil {
		return err
	}
	state, err := gtStateAtFocused(ctx, commonDir, "ship", branch, "")
	if err != nil {
		return err
	}
	up, err := gtUpstack("ship", state, branch)
	if err != nil {
		return err
	}
	if _, err := gtRestackChain(ctx, "ship", l.checkout, l.dir(), commonDir, state, up, true); err != nil {
		var conflict *errRestackConflict
		if errors.As(err, &conflict) {
			return errors.New(gtStuck("ship", gtRestackStopped(err, conflict), gtStuckSuffix(o)))
		}
		return err
	}
	return nil
}

// shipCommitGT stages, refuses an empty commit, runs pre-commit hooks (or
// reports "hooks hunk-skip" for a hunk selection), then places the commit.
// Staging is shipGitAdd's job on both lanes, since gt add is a git-add
// passthrough that costs a whole gt startup.
func shipCommitGT(ctx context.Context, l lane, errW io.Writer, o shipOpts, sel *shipSelection, plan branchPlan) (string, error) {
	o.message = withSessionTrailer(ctx, o.message)
	if sel != nil {
		seg := ""
		if !o.noVerify && shipHasHookConfig(l.root) {
			seg = "hooks hunk-skip"
		}
		return seg, shipCommitGTSelect(ctx, l, errW, o, sel, plan)
	}
	sweptSeg, err := shipGitAdd(ctx, l.dir(), o)
	if err != nil {
		return "", err
	}
	if !o.amend {
		if err := shipRefuseEmptyGit(ctx, l.dir(), o, plan); err != nil {
			return "", err
		}
	}
	hookSeg, hooksRan, err := shipRunHooks(ctx, errW, l.dir(), vcs.Git, o)
	if err != nil {
		return "", err
	}
	o.hooksRan = hooksRan
	if err := gtCommit(ctx, l, errW, o, plan, nil); err != nil {
		return "", err
	}
	segs := make([]string, 0, 2)
	for _, seg := range []string{sweptSeg, hookSeg} {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return strings.Join(segs, shipSep), nil
}

// shipCommitGTSelect commits a hunk selection through the same throwaway-index
// technique as shipCommitGitSelect — gt shells out to git, which honors
// GIT_INDEX_FILE, so running the commit under the same env commits only the temp
// index. gt's only hunk surface is interactive -p, so staging stays on git.
func shipCommitGTSelect(ctx context.Context, l lane, errW io.Writer, o shipOpts, sel *shipSelection, plan branchPlan) error {
	idxFile, err := os.CreateTemp("", "ccx-ship-index-*")
	if err != nil {
		return fmt.Errorf("ship: create temp index: %w", err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	defer func() { _ = os.Remove(idxPath) }()
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := render.RunCLIEnv(ctx, l.dir(), "git", []string{"read-tree", "HEAD"}, env); err != nil {
		return fmt.Errorf("ship: git read-tree: %w", err)
	}
	if addArgv, ok := gitSelectAddArgv(o.rootPaths, sel); ok {
		if _, err := render.RunCLIEnv(ctx, l.dir(), "git", addArgv, env); err != nil {
			return fmt.Errorf("ship: git add: %w", err)
		}
	}
	for _, path := range sortedSelectionFiles(sel) {
		if err := gitStageSelected(ctx, l.dir(), path, sel, env); err != nil {
			return err
		}
	}
	if err := gtCommit(ctx, l, errW, o, plan, env); err != nil {
		return err
	}

	restoreArgv := append([]string{"restore", "--staged", "--"}, gitRestorePaths(o.rootPaths)...)
	if _, err := render.RunCLI(ctx, l.dir(), "git", restoreArgv); err != nil {
		return fmt.Errorf("ship: git restore --staged: %w", err)
	}
	return nil
}

type gtAPIKey struct{}

// withGTAPI returns ctx carrying client in place of api.graphite.com, for a
// test pairing one httptest server with one test rather than with the process.
func withGTAPI(ctx context.Context, client *gtapi.Client) context.Context {
	return context.WithValue(ctx, gtAPIKey{}, client)
}

// gtAPIDefault is the client a context carrying none falls back to. Only
// TestMain replaces it, before any test runs, so that a test reaching the real
// api.graphite.com panics instead of submitting with the developer's token.
var gtAPIDefault = gtapi.Default

// gtAPI returns the Graphite API client ctx carries, falling back to the
// process-wide one against api.graphite.com.
func gtAPI(ctx context.Context) *gtapi.Client {
	if client, ok := ctx.Value(gtAPIKey{}).(*gtapi.Client); ok {
		return client
	}
	return gtAPIDefault()
}

// shipPushGT submits the downstack of the branch the commit landed on, over
// Graphite's HTTP API plus ccx's own git push in place of a gt submit process.
// The downstack is re-read here, after the commit, because a gt create adds a
// branch to it. It joins the trunk-ref fetch and reaches the network for nothing
// else, and never rebases or retries — gt owns restacking.
// The resolved downstack it returns is the one the pull request step then
// backfills into, so the stack is walked and its pull requests fetched once.
func shipPushGT(ctx context.Context, errW io.Writer, l lane, o shipOpts, meta map[string]prMeta, fetch *gtTrunkFetch, branch, suffix string, c *gtCache) (submitted string, bodyless []string, stack []stackEntry, err error) {
	state, err := c.at(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", nil, nil, err
	}
	tr, err := fetch.join()
	if err != nil {
		return "", nil, nil, err
	}
	if c.restack != nil {
		state = stackPublicationState(state, c.restack)
	}
	chain, err := gtDownstack("ship", state, branch, trunk)
	if err != nil {
		return "", nil, nil, err
	}
	landed, err := gitIsAncestor(ctx, l.dir(), "ship", state[branch].Head, string(tr.Ref()))
	if err != nil {
		return "", nil, nil, err
	}
	if landed {
		problem := fmt.Sprintf("%s/%s already contains %s, so it has no commit left to submit — clear it out with ccx vcs prune, or ship a branch carrying commits of its own", tr.Remote(), tr.Name(), branch)
		return "", nil, nil, errors.New(gtStuck("ship", problem, suffix))
	}
	sub := gtSubmit{prefix: "ship", suffix: suffix, draft: o.draft, noVerify: o.noVerify}
	if c.restack != nil {
		sub.publication = c.restack
		sub.trunkHead = c.restack.Pin
		sub.leases = stackPublicationLeases(c.restack)
	} else if !o.allLanes {
		sub.otherLanes = slices.DeleteFunc(slices.Clone(chain), func(name string) bool { return branchLane(name) == branchLane(branch) })
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	members := gtBottomUp(chain)
	if c.restack != nil {
		for _, b := range c.restack.Branches {
			if b.Landed == "" && !slices.Contains(members, b.Name) {
				members = append(members, b.Name)
			}
		}
	}
	_, entries, err := gtSubmitStack(ctx, l, errW, sub, commonDir, state, tr, members, branch)
	if err != nil {
		return "", nil, nil, err
	}
	submitted, bodyless, stack = gtPRSegment(branch, chain, meta, entries)
	var above []string
	for _, name := range members[len(chain):] {
		if _, ok := entries[name]; ok {
			above = append(above, name)
		}
	}
	if len(above) > 0 {
		submitted += shipSep + "resubmitted " + strings.Join(above, ", ") + " above " + branch
	}
	return submitted, bodyless, stack, nil
}

// gtSubmitBranch is one branch of an API submit, fully resolved before any
// network or push side effect.
type gtSubmitBranch struct {
	name    string
	head    string
	base    string
	baseSha string
	pr      int
	title   string
	body    string
	lease   string
	// leaseSet pins lease even when empty, where an empty lease means the
	// branch must not exist on the remote yet.
	leaseSet bool
	// parkedOn is the graphite-base branch Graphite's pre-submit moved the pull
	// request onto. Graphite retargets it back to base only when that branch
	// already sits at baseSha, so the push moves it there under parkedLease.
	parkedOn    string
	parkedLease string
}

// gtSubmitStack drives one submit over Graphite's API: drop the branches the
// remote trunk already holds, confirm the repo is synced, learn each branch's
// open PR, force-push the rest in one atomic push, then post one entry per
// branch bottom-up, as real gt does, reporting the branches that got one. The
// push comes first so the headSha Graphite records is the one on the remote. A
// branch other than tip whose open pull request already carries exactly this
// head and base is left out of both, so another lane's pull request is never
// resubmitted unchanged.
//
// Every branch's pull request comes back as Graphite answered for it, so the
// report step never asks GitHub the same question again.
func gtSubmitStack(ctx context.Context, l lane, errW io.Writer, s gtSubmit, commonDir string, state gtState, tr vcs.Trunk, branches []string, tip string) ([]string, map[string]stackEntry, error) {
	if s.trunkHead == "" {
		pin, err := gtTrunkHead(ctx, l.dir(), s.prefix, tr)
		if err != nil {
			return nil, nil, err
		}
		s.trunkHead = pin
	}
	state = maps.Clone(state)
	trunk := state[tr.Name()]
	trunk.Head = s.trunkHead
	state[tr.Name()] = trunk
	branches, contained, err := gtDropContained(ctx, l.dir(), s.prefix, tr, state, branches)
	if err != nil {
		return nil, nil, err
	}
	if err := gtAnnounceContained(errW, s.prefix, tr, contained); err != nil {
		return nil, nil, err
	}
	branches, held := gtDropHeld(state, branches)
	if others := slices.DeleteFunc(slices.Clone(branches), func(name string) bool { return !slices.Contains(s.otherLanes, name) }); len(others) > 0 {
		branches = slices.DeleteFunc(branches, func(name string) bool { return slices.Contains(others, name) })
		held = append(held, others...)
		if _, err := fmt.Fprintf(errW, "%s: not pushing %s — another lane's; --all-lanes pushes them\n", s.prefix, strings.Join(others, ", ")); err != nil {
			return nil, nil, fmt.Errorf("%s: name the other lanes' branches: %w", s.prefix, err)
		}
	}
	if len(branches) == 0 {
		if s.publication != nil {
			if err := stackRecordPublication(ctx, l.dir(), s.publication, nil); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil
	}
	owner, name, err := gtRepoOwnerName(ctx, l, s.prefix)
	if err != nil {
		return nil, nil, err
	}
	var reopened map[string]dropPR
	if s.publication != nil {
		if reopened, err = stackReopenBaseGone(ctx, l, owner+"/"+name, tr.Remote(), s.publication); err != nil {
			return nil, nil, err
		}
	}
	client := gtAPI(ctx)
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
			PRHeadRefNames:   branches,
			TrunkBranchNames: []string{tr.Name()},
			Callsite:         "ccx",
		})
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, nil, gtSubmitFailure(err, s)
	}
	if synced.Status != gtapi.RepoSynced {
		problem := fmt.Sprintf("graphite does not sync %s/%s (%s) — add the repo at app.graphite.dev, or pass --no-gt", owner, name, synced.Status)
		return nil, nil, &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: fmt.Errorf("gtapi: is-repo-synced: %s %s", synced.Status, synced.Message)}
	}
	if infoErr != nil {
		return nil, nil, gtSubmitFailure(infoErr, s)
	}
	known := make(map[string]gtapi.PullRequestInfo, len(infos))
	open := map[string]int{}
	entries := map[string]stackEntry{}
	for _, pr := range infos {
		if pr.State == gtapi.PROpen {
			known[pr.HeadRefName] = pr
			open[pr.HeadRefName] = pr.PRNumber
			entries[pr.HeadRefName] = stackEntry{Branch: pr.HeadRefName, PR: pr.PRNumber, URL: pr.URL, HasBody: strings.TrimSpace(pr.Body) != "", State: string(pr.State)}
		}
	}
	for branch, pr := range reopened {
		if _, listed := open[branch]; !listed {
			open[branch] = pr.Number
			entries[branch] = stackEntry{Branch: branch, PR: pr.Number, URL: pr.URL, HasBody: true, State: string(gtapi.PROpen)}
		}
	}

	last, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", s.prefix, err)
	}
	plan, err := gtSubmitPlan(ctx, l.dir(), s.prefix, state, tr, branches, held, open, last)
	if err != nil {
		return nil, nil, err
	}
	if s.publication != nil && s.publication.TipOnly {
		plan, err = gtTipOnlyPlan(plan, s.publication)
		if err != nil {
			return nil, nil, err
		}
	}
	if s.publication != nil {
		plan = slices.DeleteFunc(plan, func(b gtSubmitBranch) bool { return s.publication.branch(b.name).Pinned })
	}
	for i, b := range plan {
		if lease, ok := s.leases[b.name]; ok {
			plan[i].lease, plan[i].leaseSet = lease, true
		}
	}
	if err := gtRefuseQueueRestack(ctx, l.dir(), client, owner, name, s.prefix, tr, plan, known); err != nil {
		return nil, nil, err
	}
	if err := gtParkedBases(ctx, l.dir(), plan, known, s.trunkHead); err != nil {
		return nil, nil, err
	}
	submit, unchanged := gtDropUnchanged(plan, last, known, tip, s.draft)
	if err := gtAnnounceUnchanged(errW, s.prefix, unchanged); err != nil {
		return nil, nil, err
	}
	if err := gtAnnounceStack(errW, s.prefix, gtPlanNames(submit)); err != nil {
		return nil, nil, err
	}
	if err := gtRefuseInherited(ctx, s.prefix, l.dir(), tr, s.trunkHead, plan); err != nil {
		return nil, nil, err
	}
	if err := gtRefuseMergingPush(ctx, l.dir(), s, tr, plan, known); err != nil {
		return nil, nil, err
	}
	for _, branch := range branches {
		if s.publication != nil && s.publication.branch(branch) != nil && (s.publication.branch(branch).Kept || s.publication.branch(branch).Stays) {
			continue
		}
		parent := state[branch].Parents[0].Ref
		parentHead := state[parent].Head
		if parent == tr.Name() && s.trunkHead != "" {
			parentHead = s.trunkHead
		}
		contains, err := gitIsAncestor(ctx, l.dir(), s.prefix, parentHead, state[branch].Head)
		if err != nil {
			return nil, nil, err
		}
		if !contains {
			return nil, nil, errors.New(gtStuck(s.prefix, gtOffParent(branch, state[branch].State), s.suffix))
		}
	}
	if len(submit) == 0 {
		if s.publication != nil {
			if err := stackRecordPublication(ctx, l.dir(), s.publication, plan); err != nil {
				return nil, nil, err
			}
		}
		return nil, entries, nil
	}

	pre := make([]gtapi.PreSubmitBranch, 0, len(submit))
	for _, b := range submit {
		pre = append(pre, gtapi.PreSubmitBranch{HeadRefName: b.name, PRNumber: b.pr})
	}
	if _, err := client.PreSubmitPullRequests(ctx, owner, name, pre); err != nil {
		return nil, nil, gtSubmitFailure(err, s)
	}

	if s.publication != nil {
		if err := stackPushPublication(ctx, l.dir(), s, plan); err != nil {
			return nil, nil, err
		}
	} else if err := gtPushStack(ctx, l.dir(), s, plan); err != nil {
		return nil, nil, err
	}
	versions := make(map[string]gtmeta.Version, len(submit))
	for _, b := range submit {
		versions[b.name] = gtmeta.Version{HeadSha: b.head, BaseSha: b.baseSha, BaseName: b.base}
	}
	if err := gtmeta.RecordSubmitted(ctx, commonDir, versions); err != nil {
		return nil, nil, gtSubmitFailure(err, s)
	}
	if s.publication != nil {
		if err := stackRecordPublication(ctx, l.dir(), s.publication, plan); err != nil {
			return nil, nil, err
		}
		if err := stackCheckSources(ctx, l.dir(), s.publication); err != nil {
			return nil, nil, err
		}
	} else if err := gtRecordPushedPublication(ctx, l.dir(), plan); err != nil {
		return nil, nil, err
	}

	var landed []gtapi.SubmittedPR
	for i, pr := range gtSubmitPRs(submit, s.draft) {
		out, err := client.SubmitPullRequests(ctx, gtapi.SubmitRequest{
			RepoOwner:       owner,
			RepoName:        name,
			TrunkBranchName: tr.Name(),
			PRs:             []gtapi.SubmitPR{pr},
		})
		if err != nil {
			return nil, nil, gtSubmitFailure(gtSubmitPartial(err, pr.Head, landed), s)
		}
		landed = append(landed, out...)
		if submit[i].pr != 0 {
			continue
		}
		for _, created := range out {
			entries[created.Head] = stackEntry{Branch: created.Head, PR: created.PRNumber, URL: created.PRURL, HasBody: strings.TrimSpace(submit[i].body) != "", State: string(gtapi.PROpen), metaApplied: s.publication != nil && s.publication.TipOnly}
		}
	}
	if err := gtConfirmVersions(ctx, client, owner, name, tr.Name(), submit, s.draft); err != nil {
		return nil, nil, gtSubmitFailure(err, s)
	}
	gtRecordPushed(ctx, errW, owner+"/"+name, submit, entries)
	return gtPlanNames(submit), entries, nil
}

const (
	gtVersionAttempts = 3
	gtVersionSettle   = time.Second
)

// gtConfirmVersions reads back each updated pull request and re-posts the ones
// whose newest Graphite version still names another head. A submit can land
// before Graphite records the push it describes, leaving the merge queue to
// read the pull request's head as moved until something submits it again.
func gtConfirmVersions(ctx context.Context, client *gtapi.Client, owner, name, trunk string, submit []gtSubmitBranch, draft bool) error {
	stale := slices.DeleteFunc(slices.Clone(submit), func(b gtSubmitBranch) bool { return b.pr == 0 })
	for attempt := 1; len(stale) > 0; attempt++ {
		infos, err := client.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
			RepoOwner:        owner,
			RepoName:         name,
			PRHeadRefNames:   gtPlanNames(stale),
			TrunkBranchNames: []string{trunk},
			Callsite:         "ccx",
		})
		if err != nil {
			return err
		}
		recorded := map[string]string{}
		for _, pr := range infos {
			if pr.State == gtapi.PROpen {
				recorded[pr.HeadRefName] = pr.Newest().HeadSha
			}
		}
		stale = slices.DeleteFunc(stale, func(b gtSubmitBranch) bool {
			head, open := recorded[b.name]
			return !open || head == b.head
		})
		if len(stale) == 0 {
			return nil
		}
		if attempt == gtVersionAttempts {
			behind := make([]string, len(stale))
			for i, b := range stale {
				at := "no version"
				if head := recorded[b.name]; head != "" {
					at = shortOID(head)
				}
				behind[i] = fmt.Sprintf("#%d at %s, pushed %s", b.pr, at, shortOID(b.head))
			}
			return fmt.Errorf("graphite still records %s after %d submits — rerun ccx vcs stack submit so the merge queue reads the pushed head", strings.Join(behind, "; "), attempt)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * gtVersionSettle):
		}
		for _, pr := range gtSubmitPRs(stale, draft) {
			if _, err := client.SubmitPullRequests(ctx, gtapi.SubmitRequest{RepoOwner: owner, RepoName: name, TrunkBranchName: trunk, PRs: []gtapi.SubmitPR{pr}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// gtRecordPushed writes each pushed head into the shared pull request cache so
// a status read before GitHub shows the push answers with the head that was
// pushed; the cache is advisory, so a write that fails only warns.
func gtRecordPushed(ctx context.Context, errW io.Writer, repo string, submit []gtSubmitBranch, entries map[string]stackEntry) {
	heads := map[int]string{}
	for _, b := range submit {
		if e := entries[b.name]; e.PR != 0 {
			heads[e.PR] = b.head
		}
	}
	if len(heads) == 0 {
		return
	}
	store, err := openPRState(ctx, repo, errW)
	if err == nil {
		err = store.Pushed(ctx, heads)
	}
	if err != nil {
		_, _ = fmt.Fprintf(errW, "ship: record the pushed heads in the pull request cache: %v\n", err)
	}
}

func gtTipOnlyPlan(plan []gtSubmitBranch, run *stackRebaseRun) ([]gtSubmitBranch, error) {
	if run.Ship == nil || run.Tip == "" {
		return nil, errors.New("ship: --tip-only has no child publication")
	}
	for _, branch := range plan {
		if branch.name != run.Tip {
			continue
		}
		if branch.pr == 0 {
			meta := run.Ship.Meta[run.Tip]
			if meta.Title != "" {
				branch.title = meta.Title
			}
			if meta.Body != nil {
				branch.body = *meta.Body
			}
		}
		return []gtSubmitBranch{branch}, nil
	}
	return nil, fmt.Errorf("ship: --tip-only child %s is absent from the publication plan", run.Tip)
}

// gtDropUnchanged leaves out a branch whose open pull request already carries
// this head and base, and whose newest Graphite version records the same
// parent, unless its parent is resubmitted: Graphite's pre-submit
// moves every open child of a submitted branch that the submit leaves out onto
// a graphite-base branch.
func gtDropUnchanged(plan []gtSubmitBranch, last map[string]gtmeta.Version, known map[string]gtapi.PullRequestInfo, tip string, draft bool) (submit []gtSubmitBranch, unchanged []string) {
	resubmitted := map[string]bool{}
	for _, b := range plan {
		now := gtmeta.Version{HeadSha: b.head, BaseSha: b.baseSha, BaseName: b.base}
		pr, open := known[b.name]
		newest := pr.Newest()
		if b.name != tip && !resubmitted[b.base] && b.pr != 0 && open && pr.IsDraft == draft && last[b.name] == now && pr.BaseRefName == b.base && newest.BaseName == b.base && newest.HeadSha == b.head && newest.BaseSha == b.baseSha {
			unchanged = append(unchanged, b.name)
			continue
		}
		resubmitted[b.name] = true
		submit = append(submit, b)
	}
	return submit, unchanged
}

// gtRefuseQueueRestack refuses a submit over a pull request the merge queue
// parked on graphite-base/N after its parent landed: the queue replays
// graphite-base/N..head onto trunk, so moving that branch to a new parent's
// head drops the new parent's commits from the pull request, and a push onto
// trunk races the queue's own force-push, which overwrote #31266 in
// Forge-AI/monorepo with a replay of its landed parent.
func gtRefuseQueueRestack(ctx context.Context, dir render.Dir, client *gtapi.Client, owner, name, prefix string, tr vcs.Trunk, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) error {
	from := map[string]string{}
	var parents []string
	for _, b := range plan {
		pr := known[b.name]
		parent := pr.Newest().BaseName
		if !pr.IsBaseRefGraphiteBase || parent == "" || parent == tr.Name() {
			continue
		}
		from[b.name] = parent
		if !slices.Contains(parents, parent) {
			parents = append(parents, parent)
		}
	}
	if len(parents) == 0 {
		return nil
	}
	infos, err := client.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:        owner,
		RepoName:         name,
		PRHeadRefNames:   parents,
		TrunkBranchNames: []string{tr.Name()},
		Callsite:         "ccx",
	})
	if err != nil {
		return fmt.Errorf("%s: read the parents Graphite parked pull requests off: %w", prefix, err)
	}
	landed := map[string]int{}
	for _, info := range infos {
		if pruneLanded(ctx, dir, tr.Name(), info) {
			landed[info.HeadRefName] = info.PRNumber
		}
	}
	for _, b := range plan {
		parent := from[b.name]
		number, ok := landed[parent]
		if parent == "" || !ok {
			continue
		}
		pr := known[b.name]
		race := fmt.Sprintf("drops commits from #%d", pr.PRNumber)
		if b.base == tr.Name() {
			race = "graphite-app force-pushes its own restack over this push"
		}
		return fmt.Errorf("%s: #%d sits on %s while Graphite's merge queue restacks it onto %s after its parent %s landed as #%d; submitting %s onto %s now races that restack and %s — wait until #%d's base leaves %s, then re-run",
			prefix, pr.PRNumber, pr.BaseRefName, tr.Name(), parent, number, b.name, b.base, race, pr.PRNumber, pr.BaseRefName)
	}
	return nil
}

// gtParkedBases marks each branch whose pull request Graphite parked on a
// graphite-base branch, leased on where that branch stands on the remote now.
func gtParkedBases(ctx context.Context, dir render.Dir, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo, trunkHead string) error {
	var parked []string
	for i, b := range plan {
		if pr := known[b.name]; pr.IsBaseRefGraphiteBase {
			plan[i].parkedOn = pr.BaseRefName
			parked = append(parked, pr.BaseRefName)
		}
	}
	if len(parked) == 0 {
		return nil
	}
	heads, err := stackRemoteHeads(ctx, dir, stackRebasePrefix, "origin", parked, trunkHead)
	if err != nil {
		return err
	}
	for i, b := range plan {
		if b.parkedOn == "" {
			continue
		}
		if heads[b.parkedOn] == "" {
			plan[i].parkedOn = ""
			continue
		}
		plan[i].parkedLease = heads[b.parkedOn]
	}
	return nil
}

// gtAnnounceUnchanged names the branches a submit left alone because their pull
// requests already carry what it would have pushed.
func gtAnnounceUnchanged(errW io.Writer, prefix string, unchanged []string) error {
	if len(unchanged) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(errW, "%s: not resubmitting %s, unchanged since its last submit: %s\n", prefix, gtBranchCount(len(unchanged)), strings.Join(unchanged, ", ")); err != nil {
		return fmt.Errorf("%s: name the unchanged branches: %w", prefix, err)
	}
	return nil
}

// gtTrunkRef resolves the remote-tracking trunk a submit anchors on, fetching
// that one ref first: against a local trunk left behind, a branch whose commits
// are already upstream reads as a stack member, and graphite refuses the empty
// pull request that follows. The fetch moves no local branch and no working
// copy — gt still owns restacking.
func gtTrunkRef(ctx context.Context, dir render.Dir, prefix, trunk string) (vcs.Trunk, error) {
	remote, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: %w", prefix, err)
	}
	ref := "refs/remotes/" + remote + "/" + trunk
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"show-ref", "--verify", "--quiet", ref})
	if err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: git show-ref %s: %w", prefix, ref, err)
	}
	if code != 0 && (code != 1 || stderr != "") {
		return vcs.Trunk{}, fmt.Errorf("%s: git show-ref %s: exit %d: %s", prefix, ref, code, stderr)
	}
	args := []string{"--no-tags", "--no-write-fetch-head"}
	if code == 0 {
		args = append(args, "--negotiation-tip="+ref)
	}
	if err := gitFetch(ctx, dir, append(args, remote, trunk)...); err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: git fetch %s %s: %w", prefix, remote, trunk, err)
	}
	return gtTrunkRefAt(ctx, dir, prefix, remote, trunk)
}

const gitFetchAttempts = 6

// gitFetch runs git fetch, retrying while a fetch in another worktree of the
// shared clone holds the lock on a ref this one updates: that fetch moves the
// same ref, so the retry finds it free.
func gitFetch(ctx context.Context, dir render.Dir, args ...string) error {
	delay := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		_, err := render.RunCLI(ctx, dir, "git", append([]string{"fetch"}, args...))
		if err == nil || attempt == gitFetchAttempts || !strings.Contains(err.Error(), "cannot lock ref") {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// gtTrunkFetch is one gtTrunkRef in flight, so the round trip runs under the
// staging and the commit rather than after them: it reads nothing they produce,
// and past the preflight nothing before the submit reads the ref it moves.
type gtTrunkFetch struct {
	done   chan struct{}
	cancel context.CancelFunc
	tr     vcs.Trunk
	err    error
}

// gtStartTrunkFetch takes the trunk the preflight resolved, which no commit renames.
func gtStartTrunkFetch(ctx context.Context, dir render.Dir, prefix, trunk string) *gtTrunkFetch {
	ctx, cancel := context.WithCancel(ctx)
	f := &gtTrunkFetch{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(f.done)
		f.tr, f.err = gtTrunkRef(ctx, dir, prefix, trunk)
	}()
	return f
}

func (f *gtTrunkFetch) join() (vcs.Trunk, error) {
	<-f.done
	return f.tr, f.err
}

// stop is join for a run that ended before the submit, waiting so no fetch
// outlives the command.
func (f *gtTrunkFetch) stop() {
	f.cancel()
	<-f.done
}

// gtTrunkRefOffline reads the remote-tracking trunk without fetching, for the
// checks that run on every ship rather than only on a submit. Containment only
// grows, so a ref left behind still answers "already in trunk" correctly where a
// fetch would only widen the answer.
func gtTrunkRefOffline(ctx context.Context, dir render.Dir, prefix, trunk string) (vcs.Trunk, error) {
	remote, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: %w", prefix, err)
	}
	return gtTrunkRefAt(ctx, dir, prefix, remote, trunk)
}

func gtTrunkRefAt(ctx context.Context, dir render.Dir, prefix, remote, trunk string) (vcs.Trunk, error) {
	tr, err := vcs.TrunkFromName(ctx, dir, remote, trunk)
	if err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: %w", prefix, err)
	}
	return tr, nil
}

// gtTrunkHead resolves the commit the remote trunk stands at — the base sha a
// submit sends for a branch stacked on trunk, in place of the local trunk's own.
func gtTrunkHead(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", string(tr.Ref())})
	if err != nil {
		return "", fmt.Errorf("%s: git rev-parse %s: %w", prefix, tr.Ref(), err)
	}
	return strings.TrimSpace(out), nil
}

// gtDropContained splits branches into the ones a submit has work for and the
// ones the remote trunk already holds, which have nothing to push and no pull
// request to open. Both halves keep the bottom-up order.
func gtDropContained(ctx context.Context, dir render.Dir, prefix string, tr vcs.Trunk, state gtState, branches []string) (submit, contained []string, err error) {
	for _, name := range branches {
		in, err := gitIsAncestor(ctx, dir, prefix, state[name].Head, state[tr.Name()].Head)
		if err != nil {
			return nil, nil, err
		}
		if in {
			contained = append(contained, name)
			continue
		}
		submit = append(submit, name)
	}
	return submit, contained, nil
}

// gtDropHeld splits off the downstack branches gt is holding, which a submit
// leaves where they are and never pushes. The branch being submitted is kept
// whatever its state.
func gtDropHeld(state gtState, branches []string) (submit, held []string) {
	for i, name := range branches {
		if state[name].State != "" && i < len(branches)-1 {
			held = append(held, name)
			continue
		}
		submit = append(submit, name)
	}
	return submit, held
}

// gtSyncSchema is the on-disk format version of the cached repo-sync verdict; a
// mismatch reads as a miss.
const gtSyncSchema = 1

// gtSyncTTL bounds how long a cached sync verdict is served.
const gtSyncTTL = 24 * time.Hour

// gtSyncRecord is one repo's cached SYNCED verdict, a sibling of its GitHub
// metadata record. Only that verdict is stored: every other one aborts the
// submit, so no run is left to serve a cached refusal to.
type gtSyncRecord struct {
	Schema    int       `json:"schema"`
	FetchedAt time.Time `json:"fetched_at"`
}

// gtRepoSynced asks Graphite whether it mirrors owner/name, serving a cached
// verdict when one is on disk so a synced repository pays for the round trip at
// most once a day.
func gtRepoSynced(ctx context.Context, client *gtapi.Client, root, owner, name string) (gtapi.RepoSync, error) {
	path, err := gtSyncCachePath(ctx, root)
	if err != nil {
		return gtapi.RepoSync{}, err
	}
	if readGTSyncRecord(path) {
		return gtapi.RepoSync{Status: gtapi.RepoSynced}, nil
	}

	var synced gtapi.RepoSync
	err = cache.WithLock(ctx, filepath.Dir(path), "gtsync", func() error {
		// Re-read under the lock: a concurrent submit on the same repo has likely
		// already paid for the answer this one was about to ask for.
		if readGTSyncRecord(path) {
			synced = gtapi.RepoSync{Status: gtapi.RepoSynced}
			return nil
		}
		got, err := client.IsRepoSynced(ctx, owner, name)
		if err != nil {
			return err
		}
		synced = got
		if got.Status != gtapi.RepoSynced {
			return nil
		}
		data, err := json.Marshal(gtSyncRecord{Schema: gtSyncSchema, FetchedAt: time.Now()})
		if err != nil {
			return fmt.Errorf("marshal graphite sync verdict for %q: %w", root, err)
		}
		return cache.Store(path, data, 0o600)
	})
	if err != nil {
		return gtapi.RepoSync{}, err
	}
	return synced, nil
}

// gtSyncCachePath resolves the cached sync verdict for the repository root
// belongs to, a sibling of its GitHub metadata record and its gt-reachability
// verdict. The key is the repository, so its linked worktrees share one answer.
func gtSyncCachePath(ctx context.Context, root string) (string, error) {
	repoPath, err := vcs.RepoCachePath(ctx, root)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(repoPath), "gtsync.json"), nil
}

func readGTSyncRecord(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path is rooted at the cache dir and keyed by sha256 hex
	if err != nil {
		return false
	}
	var rec gtSyncRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Schema != gtSyncSchema {
		return false
	}
	return time.Since(rec.FetchedAt) < gtSyncTTL
}

// gtRepoOwnerName resolves the GitHub repository the API submit names, from
// the lane's cached record when the gate already read it.
func gtRepoOwnerName(ctx context.Context, l lane, prefix string) (string, string, error) {
	repo := l.repo
	if repo == nil {
		looked, err := vcs.LookupRepo(ctx, l.dir(), false)
		if err != nil {
			return "", "", fmt.Errorf("%s: a graphite submit needs GitHub metadata: %w", prefix, err)
		}
		repo = &looked
	}
	owner, name, ok := strings.Cut(repo.NameWithOwner, "/")
	if !ok {
		return "", "", fmt.Errorf("%s: malformed repository name %q", prefix, repo.NameWithOwner)
	}
	return owner, name, nil
}

// gtSubmitPlan resolves each branch of the submit from gt's own state: its
// head, its open PR, and the lease of its last submitted version. A branch with
// no PR gets the title and body a create requires. One stacked on a branch gt
// holds stays on it; one stacked on neither that nor another branch of this
// submit is anchored on the remote trunk, not on gt's local sha.
func gtSubmitPlan(ctx context.Context, dir render.Dir, prefix string, state gtState, tr vcs.Trunk, branches, held []string, open map[string]int, last map[string]gtmeta.Version) ([]gtSubmitBranch, error) {
	trunkHead := state[tr.Name()].Head
	stacked := make(map[string]bool, len(branches))
	for _, name := range branches {
		stacked[name] = true
	}
	plan := make([]gtSubmitBranch, 0, len(branches))
	for _, name := range branches {
		s := state[name]
		b := gtSubmitBranch{
			name:    name,
			head:    s.Head,
			base:    s.Parents[0].Ref,
			baseSha: s.Parents[0].SHA,
			pr:      open[name],
			lease:   last[name].HeadSha,
		}
		switch {
		case stacked[b.base]:
		case slices.Contains(held, b.base) || state[b.base].State != "":
			b.baseSha = state[b.base].Head
		default:
			b.base, b.baseSha = tr.Name(), trunkHead
		}
		if b.pr == 0 {
			title, body, err := gtCreateMeta(ctx, dir, prefix, b.name, b.head, b.baseSha, b.base)
			if err != nil {
				return nil, err
			}
			b.title, b.body = title, body
		}
		plan = append(plan, b)
	}
	return plan, nil
}

// gtCreateMeta derives a created PR's title and body from the branch's first
// commit above rev — a deliberate divergence from gt submit --no-edit, which
// creates PRs with empty bodies. Refusals name base, never rev. The
// Claude-Session-Id trailer is dropped from the body, the same line the
// non-graphite lane keeps out of descriptions by never passing --fill.
func gtCreateMeta(ctx context.Context, dir render.Dir, prefix, branch, head, rev, base string) (string, string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--reverse", "--format=%s%x00%b%x00", rev + ".." + head})
	if err != nil {
		return "", "", fmt.Errorf("%s: git log %s..%s: %w", prefix, rev, head, err)
	}
	fields := strings.Split(out, "\x00")
	if len(fields) < 3 {
		return "", "", fmt.Errorf("%s: %s holds no commit above %s to derive a PR title from", prefix, branch, base)
	}
	title := strings.TrimPrefix(fields[0], "\n")
	if title == "" {
		return "", "", fmt.Errorf("%s: the first commit on %s above %s has an empty subject, and graphite needs a title to create a pull request — give that commit a subject line", prefix, branch, base)
	}
	return title, gtStripSessionTrailer(fields[1]), nil
}

func gtStripSessionTrailer(body string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "Claude-Session-Id: ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// gtPushArgv is the one atomic multi-ref push gt submit makes for a whole
// stack: every branch's lease, then every branch's refspec. The hook decision
// rides along: without --no-verify a repository's pre-push hook runs the suite
// the commit was told to skip.
func gtPushArgv(s gtSubmit, plan []gtSubmitBranch) []string {
	argv := pushArgv("origin")
	for _, b := range plan {
		lease := "--force-with-lease"
		if b.lease != "" || b.leaseSet {
			lease += "=refs/heads/" + b.name + ":" + b.lease
		}
		argv = append(argv, lease)
		if b.parkedOn != "" {
			argv = append(argv, "--force-with-lease=refs/heads/"+b.parkedOn+":"+b.parkedLease)
		}
	}
	argv = append(argv, "--progress")
	for _, b := range plan {
		argv = append(argv, b.head+":refs/heads/"+b.name)
		if b.parkedOn != "" {
			argv = append(argv, b.baseSha+":refs/heads/"+b.parkedOn)
		}
	}
	if s.noVerify {
		argv = append(argv, "--no-verify")
	}
	return append(argv, "--atomic")
}

// gtPushStack force-pushes the whole stack under each branch's last submitted
// lease, so a remote someone else advanced is refused rather than overwritten
// and no ref moves unless all of them do. A lease is behind the remote when this
// repository pushed the branch outside a submit, which records no lease; the
// push is retried once on the remote's head where this repository vouches for
// it, and refused for any branch whose remote moved by other hands.
func gtPushStack(ctx context.Context, dir render.Dir, s gtSubmit, plan []gtSubmitBranch) error {
	if err := thinRefuseAdoptedPush(ctx, dir, s.prefix, slices.Collect(maps.Keys(gtPushedHeads(plan)))); err != nil {
		return err
	}
	err := gtRunPush(ctx, dir, s, plan)
	if err == nil {
		return thinRecordPush(ctx, dir, "origin", gtPushedHeads(plan))
	}
	if !gitPushStaleLease(err) {
		return gtPushFailure(s, plan, err)
	}
	var moved []string
	for _, name := range gtStaleRefs(err) {
		i := slices.IndexFunc(plan, func(b gtSubmitBranch) bool { return b.name == name })
		if i < 0 {
			continue
		}
		if plan[i].leaseSet {
			moved = append(moved, name)
			continue
		}
		remote, held, err := gtHeldRemote(ctx, dir, s.prefix, plan[i])
		if err != nil {
			return err
		}
		if !held {
			moved = append(moved, name)
			continue
		}
		plan[i].lease, plan[i].leaseSet = remote, true
	}
	if len(moved) == 0 {
		err = gtRunPush(ctx, dir, s, plan)
		if err == nil {
			return thinRecordPush(ctx, dir, "origin", gtPushedHeads(plan))
		}
		if !gitPushStaleLease(err) {
			return gtPushFailure(s, plan, err)
		}
		moved = gtStaleRefs(err)
	}
	problem := "remote " + strings.Join(moved, ", ") + " changed since last submit, by a push this repository did not make — fetch it and fold in what it added, then submit again"
	return &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: err}
}

func gtRunPush(ctx context.Context, dir render.Dir, s gtSubmit, plan []gtSubmitBranch) error {
	argv := gtPushArgv(s, plan)
	_, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil && gitPushRemoteFailed(err) {
		_, err = render.RunCLI(ctx, dir, "git", argv)
	}
	return err
}

// gtRecordPushedPublication writes the publication receipt of each branch a
// push without a replay run published: its pushed head on the parent head it
// was submitted onto, which stack new --published-parent reads.
func gtRecordPushedPublication(ctx context.Context, dir render.Dir, plan []gtSubmitBranch) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range plan {
		prior, err := stackReadPublication(ctx, dir, b.name)
		if err != nil {
			return err
		}
		expected := ""
		if prior != nil {
			expected = prior.OID
		}
		receipt := stackPublication{Branch: b.name, Source: b.head, SourceBase: b.baseSha, Head: b.head, Base: b.baseSha, Parent: b.base}
		if err := stackReceiptTx(ctx, dir, &tx, receipt, expected); err != nil {
			return err
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack publication: record pushed receipts: %w", err)
	}
	return nil
}

func gtPushedHeads(plan []gtSubmitBranch) map[string]string {
	pushed := make(map[string]string, len(plan))
	for _, b := range plan {
		pushed[b.name] = b.head
		if b.parkedOn != "" {
			pushed[b.parkedOn] = b.baseSha
		}
	}
	return pushed
}

func gtPushFailure(s gtSubmit, plan []gtSubmitBranch, err error) error {
	if err == nil {
		return nil
	}
	problem := "the atomic push of " + strings.Join(gtPlanNames(plan), ", ") + " moved nothing: " + gitPushVerdict(err)
	return &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: err}
}

var gtStaleRefPattern = regexp.MustCompile(`-> (\S+) \(stale info\)`)

func gtStaleRefs(err error) []string {
	matches := gtStaleRefPattern.FindAllStringSubmatch(err.Error(), -1)
	refs := make([]string, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, m[1])
	}
	return refs
}

func gtHeldRemote(ctx context.Context, dir render.Dir, prefix string, b gtSubmitBranch) (string, bool, error) {
	pushed, err := gtPushedHere(ctx, dir, prefix, b.name)
	if err != nil {
		return "", false, err
	}
	heads, err := stackRemoteHeads(ctx, dir, prefix, "origin", []string{b.name}, b.head)
	if err != nil {
		return "", false, err
	}
	remote := heads[b.name]
	if remote == "" || remote == pushed {
		return remote, remote != "", nil
	}
	ancestor, err := gitIsAncestor(ctx, dir, prefix, remote, b.head)
	if err != nil || ancestor {
		return remote, ancestor, err
	}
	held, err := gitReflogHolds(ctx, dir, prefix, b.name, remote)
	return remote, held, err
}

// gtPushedHere is the head this repository last pushed branch to, read off the
// remote-tracking ref's reflog, or empty when a fetch moved that ref last — a
// fetch can carry in a push from anywhere.
func gtPushedHere(ctx context.Context, dir render.Dir, prefix, branch string) (string, error) {
	ref := "refs/remotes/origin/" + branch
	exists, err := gitRefExists(ctx, dir, prefix, ref)
	if err != nil || !exists {
		return "", err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"reflog", "show", "-n", "1", "--format=%H %gs", ref, "--"})
	if err != nil {
		return "", fmt.Errorf("%s: git reflog %s: %w", prefix, ref, err)
	}
	sha, subject, _ := strings.Cut(strings.TrimSpace(out), " ")
	if subject != "update by push" {
		return "", nil
	}
	return sha, nil
}

func gtPlanNames(plan []gtSubmitBranch) []string {
	names := make([]string, 0, len(plan))
	for _, b := range plan {
		names = append(names, b.name)
	}
	return names
}

// gtSubmitPRs renders the plan into the submit call's branches. An update
// omits title and body deliberately: they are optional there so a re-submit
// need not restate them, and sending them would overwrite a description a
// human edited.
func gtSubmitPRs(plan []gtSubmitBranch, draft bool) []gtapi.SubmitPR {
	prs := make([]gtapi.SubmitPR, 0, len(plan))
	for _, b := range plan {
		pr := gtapi.SubmitPR{
			Head:    b.name,
			HeadSha: b.head,
			Base:    b.base,
			BaseSha: b.baseSha,
			Draft:   &draft,
		}
		if b.pr != 0 {
			pr.Action, pr.PRNumber = gtapi.SubmitUpdate, b.pr
		} else {
			pr.Action, pr.Title, pr.Body = gtapi.SubmitCreate, b.title, b.body
		}
		prs = append(prs, pr)
	}
	return prs
}

// gtSubmitFailure maps a failed API submit to a recovery step, keeping the
// typed failure reachable as the advice's cause. A per-branch refusal names
// both the branches that landed and the ones that did not, so a partial submit
// is reported exactly.
func gtSubmitFailure(err error, s gtSubmit) error {
	var submitErr *gtapi.SubmitError
	switch {
	case errors.Is(err, gtapi.ErrUnauthorized) || errors.Is(err, gtapi.ErrNoToken):
		return &gtAdvice{advice: gtStuck(s.prefix, "graphite auth required — run gt auth", s.suffix), cause: err}
	case errors.As(err, &submitErr):
		return &gtAdvice{advice: gtStuck(s.prefix, gtSubmitSplit(submitErr), s.suffix), cause: err}
	default:
		return fmt.Errorf("%s: %w", s.prefix, err)
	}
}

// gtSubmitPartial folds the pull requests earlier entries opened into the
// failure that stopped the submit loop, so one refusal reports both sides the
// way the batched call's own *gtapi.SubmitError did. An auth failure keeps its
// identity, since it routes to the gt auth advice rather than to a per-branch
// report.
func gtSubmitPartial(err error, head string, landed []gtapi.SubmittedPR) error {
	var submitErr *gtapi.SubmitError
	switch {
	case errors.Is(err, gtapi.ErrUnauthorized) || errors.Is(err, gtapi.ErrNoToken):
		return err
	case errors.As(err, &submitErr):
		return &gtapi.SubmitError{Submitted: append(landed, submitErr.Submitted...), Failed: submitErr.Failed}
	default:
		return &gtapi.SubmitError{Submitted: landed, Failed: []gtapi.BranchSubmitError{{Head: head, Message: err.Error()}}}
	}
}

func gtSubmitSplit(e *gtapi.SubmitError) string {
	failed := make([]string, 0, len(e.Failed))
	for _, f := range e.Failed {
		failed = append(failed, f.Head+" ("+f.Message+")")
	}
	problem := "graphite refused " + strings.Join(failed, ", ") + "; every branch is already pushed"
	if len(e.Submitted) == 0 {
		return problem
	}
	landed := make([]string, 0, len(e.Submitted))
	for _, s := range e.Submitted {
		landed = append(landed, fmt.Sprintf("%s → PR #%d", s.Head, s.PRNumber))
	}
	return problem + "; landed " + strings.Join(landed, ", ")
}

// gtStackNames orders a downstack trunk-first, the direction a stack reads in.
func gtStackNames(chain []string) []string {
	names := make([]string, len(chain))
	for i, b := range chain {
		names[len(chain)-1-i] = b
	}
	return names
}

// gtAnnounceStack names the branches a submit is about to force-push, which for
// a stack deeper than one branch is more than the one being shipped. Branches
// arrive bottom-up, the direction a stack reads in.
func gtAnnounceStack(errW io.Writer, prefix string, branches []string) error {
	if len(branches) < 2 {
		return nil
	}
	if _, err := fmt.Fprintf(errW, "%s: submitting %d branches: %s\n", prefix, len(branches), strings.Join(branches, ", ")); err != nil {
		return fmt.Errorf("%s: name the stack: %w", prefix, err)
	}
	return nil
}

// gtAnnounceContained names the branches a submit dropped because the remote
// trunk already holds them; dropping them silently would read as a submit that
// carried the whole stack.
func gtAnnounceContained(errW io.Writer, prefix string, tr vcs.Trunk, contained []string) error {
	if len(contained) == 0 {
		return nil
	}
	noun := "branches"
	if len(contained) == 1 {
		noun = "branch"
	}
	line := fmt.Sprintf("%s: skipping %d %s already in %s/%s: %s\n", prefix, len(contained), noun, tr.Remote(), tr.Name(), strings.Join(contained, ", "))
	if _, err := fmt.Fprint(errW, line); err != nil {
		return fmt.Errorf("%s: name the skipped branches: %w", prefix, err)
	}
	return nil
}

func gtPRSegment(branch string, chain []string, meta map[string]prMeta, entries map[string]stackEntry) (submitted string, bodyless []string, stack []stackEntry) {
	stackSeg := ""
	if len(chain) > 1 {
		stackSeg = fmt.Sprintf(" (stack of %d: %s)", len(chain), strings.Join(gtStackNames(chain), ", "))
	}
	submitted = "submitted " + branch + stackSeg
	stack = gtSubmitDownstack(chain, entries)
	for _, entry := range stack {
		if entry.PR == 0 {
			continue
		}
		if entry.Branch == branch {
			submitted = fmt.Sprintf("submitted %s → PR #%d %s%s", branch, entry.PR, entry.URL, stackSeg)
		}
		if !entry.HasBody && !meta[entry.Branch].writesBody() {
			bodyless = append(bodyless, fmt.Sprintf("bodyless PR #%d %s", entry.PR, entry.Branch))
		}
	}
	return submitted, bodyless, stack
}

// gtSubmitDownstack lays the submitted stack's pull requests out trunk-first
// from what Graphite answered — the open pull requests it knew of and the ones
// the submit opened — so reporting a submit spends no GitHub API call. A branch
// with neither has no pull request.
func gtSubmitDownstack(chain []string, entries map[string]stackEntry) []stackEntry {
	stack := make([]stackEntry, 0, len(chain))
	for _, branch := range gtStackNames(chain) {
		entry, ok := entries[branch]
		if !ok {
			entry = stackEntry{Branch: branch}
		}
		stack = append(stack, entry)
	}
	return stack
}
