package cli

import (
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
	commonDir, err := gtCommonDir(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	return gtStateAt(ctx, commonDir, prefix)
}

func gtStateAt(ctx context.Context, commonDir, prefix string) (gtState, error) {
	tracked, err := gtmeta.ReadOrigin(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
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
	state, err := gtStateAt(ctx, commonDir, c.prefix)
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
	_, chain, err := gtStackChain(ctx, c, branch)
	return chain, err
}

func shipPreflightGT(ctx context.Context, errW io.Writer, l lane, o shipOpts, c *gtCache) (branchPlan, string, error) {
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return branchPlan{}, "", err
	}
	state, err := c.at(ctx)
	if err != nil {
		return branchPlan{}, "", err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return branchPlan{}, "", err
	}

	var seg string
	needsRestack := false
	if branch != "" && branch != trunk {
		if _, tracked := state[branch]; !tracked {
			if state, seg, err = gtTrack(ctx, errW, o, branch, c); err != nil {
				return branchPlan{}, "", err
			}
		}
		chain, err := gtDownstack("ship", state, branch, trunk)
		if err != nil {
			return branchPlan{}, "", err
		}
		needsRestack = slices.ContainsFunc(chain, func(b string) bool { return state[b].NeedsRestack })
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
	plan.needsRestack = needsRestack
	return plan, seg, nil
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
	result, err := gtRestackChain(ctx, "ship", l.checkout, l.dir(), commonDir, state, gtBottomUp(chain))
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
	for _, b := range chain {
		if state[b].NeedsRestack {
			return "", errors.New(gtStuck("ship", gtOffParent(b, result.held[b]), suffix))
		}
	}
	return gtRestackSegment(result), nil
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
func gtTrack(ctx context.Context, errW io.Writer, o shipOpts, branch string, c *gtCache) (gtState, string, error) {
	argv := []string{"track", branch, "-f", "--no-interactive"}
	if o.parent != "" {
		argv = []string{"track", branch, "--parent", o.parent, "--no-interactive"}
	}
	untracked := fmt.Errorf("ship: branch %s is not tracked by graphite — run gt track %s, or pass --no-gt", branch, branch)
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
	seg := "tracked " + branch
	if len(s.Parents) > 0 {
		parent := s.Parents[0].Ref
		if err := gtRefuseLandedParent(ctx, c.dir, state, branch, parent); err != nil {
			return nil, "", err
		}
		seg += " onto " + parent
	}
	return state, seg, nil
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

// gtCommit places the commit on the gt lane. A modify — an amend or an appended
// commit — moves one ref and reparents nothing, so git commits it and the
// branches above are replayed onto it carrying the parent revisions gt would
// have recorded. A create still runs gt, for the branch_metadata row gtmeta
// cannot insert.
func gtCommit(ctx context.Context, l lane, errW io.Writer, o shipOpts, plan branchPlan, env []string) error {
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
	return gtModifyRestack(ctx, l, o, plan.from)
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
	state, err := gtStateAt(ctx, commonDir, "ship")
	if err != nil {
		return err
	}
	up, err := gtUpstack("ship", state, branch)
	if err != nil {
		return err
	}
	if _, err := gtRestackChain(ctx, "ship", l.checkout, l.dir(), commonDir, state, up); err != nil {
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

// gtAPIClient is the Graphite API client the submit path calls; tests point it
// at an httptest server.
var gtAPIClient = gtapi.Default

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
	}
	commonDir, err := c.common(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	_, entries, err := gtSubmitStack(ctx, l, errW, sub, commonDir, state, tr, gtBottomUp(chain), branch)
	if err != nil {
		return "", nil, nil, err
	}
	submitted, bodyless, stack = gtPRSegment(branch, chain, meta, entries)
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

	last, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", s.prefix, err)
	}
	plan, err := gtSubmitPlan(ctx, l.dir(), s.prefix, state, tr, branches, held, open, last)
	if err != nil {
		return nil, nil, err
	}
	for i, b := range plan {
		if lease, ok := s.leases[b.name]; ok {
			plan[i].lease, plan[i].leaseSet = lease, true
		}
	}
	plan, unchanged := gtDropUnchanged(plan, last, known, tip)
	if err := gtAnnounceUnchanged(errW, s.prefix, unchanged); err != nil {
		return nil, nil, err
	}
	if err := gtAnnounceStack(errW, s.prefix, gtPlanNames(plan)); err != nil {
		return nil, nil, err
	}
	if err := gtRefuseInherited(ctx, s.prefix, l.dir(), tr, s.trunkHead, plan); err != nil {
		return nil, nil, err
	}
	for _, branch := range branches {
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
	if len(plan) == 0 {
		return nil, entries, nil
	}

	pre := make([]gtapi.PreSubmitBranch, 0, len(plan))
	for _, b := range plan {
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
	versions := make(map[string]gtmeta.Version, len(plan))
	for _, b := range plan {
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
	}

	var landed []gtapi.SubmittedPR
	for i, pr := range gtSubmitPRs(plan, s.draft) {
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
		if plan[i].pr != 0 {
			continue
		}
		for _, created := range out {
			entries[created.Head] = stackEntry{Branch: created.Head, PR: created.PRNumber, URL: created.PRURL, HasBody: strings.TrimSpace(plan[i].body) != "", State: string(gtapi.PROpen)}
		}
	}
	return gtPlanNames(plan), entries, nil
}

// gtDropUnchanged splits off every branch below tip whose open pull request
// both gt's record and Graphite's newest version hold at exactly the head,
// base and base sha it would be submitted at now: submitting it again changes
// nothing, and a Graphite refusal on it would fail the tip's submit.
func gtDropUnchanged(plan []gtSubmitBranch, last map[string]gtmeta.Version, known map[string]gtapi.PullRequestInfo, tip string) (submit []gtSubmitBranch, unchanged []string) {
	for _, b := range plan {
		now := gtmeta.Version{HeadSha: b.head, BaseSha: b.baseSha, BaseName: b.base}
		pr, open := known[b.name]
		newest := pr.Newest()
		if b.name != tip && b.pr != 0 && open && last[b.name] == now && pr.BaseRefName == b.base && newest.HeadSha == b.head && newest.BaseSha == b.baseSha {
			unchanged = append(unchanged, b.name)
			continue
		}
		submit = append(submit, b)
	}
	return submit, unchanged
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
	if _, err := render.RunCLI(ctx, dir, "git", []string{"fetch", remote, trunk}); err != nil {
		return vcs.Trunk{}, fmt.Errorf("%s: git fetch %s %s: %w", prefix, remote, trunk, err)
	}
	return gtTrunkRefAt(ctx, dir, prefix, remote, trunk)
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
// fetch would only widen the answer, and a --no-push ship stays off the network.
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
// no PR gets the title and body a create requires. One not stacked on another
// branch of this submit is anchored on the remote trunk, not on gt's local sha.
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
		case slices.Contains(held, b.base):
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
	}
	argv = append(argv, "--progress")
	for _, b := range plan {
		argv = append(argv, b.head+":refs/heads/"+b.name)
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
// push is retried once on the head that push left, and refused for any branch
// whose remote moved by other hands.
func gtPushStack(ctx context.Context, dir render.Dir, s gtSubmit, plan []gtSubmitBranch) error {
	_, err := render.RunCLI(ctx, dir, "git", gtPushArgv(s, plan))
	if err == nil || !gitPushStaleLease(err) {
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
		pushed, err := gtPushedHere(ctx, dir, s.prefix, name)
		if err != nil {
			return err
		}
		if pushed == "" || pushed == plan[i].lease {
			moved = append(moved, name)
			continue
		}
		plan[i].lease = pushed
	}
	if len(moved) == 0 {
		_, err = render.RunCLI(ctx, dir, "git", gtPushArgv(s, plan))
		if err == nil || !gitPushStaleLease(err) {
			return gtPushFailure(s, plan, err)
		}
		moved = gtStaleRefs(err)
	}
	problem := "remote " + strings.Join(moved, ", ") + " changed since last submit, by a push this repository did not make — fetch it and fold in what it added, then submit again"
	return &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: err}
}

func gtPushFailure(s gtSubmit, plan []gtSubmitBranch, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: git push %s: %w", s.prefix, strings.Join(gtPlanNames(plan), ", "), err)
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

// gtPushedHere is the head this repository last pushed branch to, read off the
// remote-tracking ref's reflog, or empty when a fetch moved that ref last — a
// fetch can carry in a push from anywhere.
func gtPushedHere(ctx context.Context, dir render.Dir, prefix, branch string) (string, error) {
	ref := "refs/remotes/origin/" + branch
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
