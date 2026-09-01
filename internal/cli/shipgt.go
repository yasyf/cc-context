package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

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

// gtStateQuery reads the stack gt tracks out of gt's own SQLite metadata rather
// than from gt state, which costs six to nine seconds in a large repository
// because gt revalidates every ref on every invocation. gtmeta answers the same
// question from one query and one for-each-ref; its conformance test is what
// keeps the two answers the same.
func gtStateQuery(ctx context.Context, dir render.Dir, prefix string) (gtState, error) {
	commonDir, err := gtCommonDir(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	return gtStateAt(ctx, commonDir, prefix)
}

// gtStateAt is gtStateQuery for a caller already holding the git common dir.
func gtStateAt(ctx context.Context, commonDir, prefix string) (gtState, error) {
	tracked, err := gtmeta.Read(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	state := make(gtState, len(tracked))
	for branch, s := range tracked {
		entry := gtBranchState{Trunk: s.Trunk, NeedsRestack: s.NeedsRestack, Head: s.Head}
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
			return nil, fmt.Errorf("%s: gt state has no parent for %s, an ancestor of %s — the stack is unresolvable; run gt track %s, or gt restack", prefix, cur, branch, cur)
		}
	}
	return chain, nil
}

func gtStackChain(ctx context.Context, dir render.Dir, prefix, branch string) (gtState, []string, error) {
	state, err := gtStateQuery(ctx, dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, nil, err
	}
	if branch == trunk {
		return state, nil, nil
	}
	chain, err := gtDownstack(prefix, state, branch, trunk)
	if err != nil {
		return nil, nil, err
	}
	return state, chain, nil
}

// stackBranches lists the current downstack chain — current branch first, up
// to (excluding) trunk — or nil when the current branch is trunk.
func stackBranches(ctx context.Context, dir render.Dir, prefix string) ([]string, error) {
	branch, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", prefix)
	}
	_, chain, err := gtStackChain(ctx, dir, prefix, branch)
	return chain, err
}

func shipPreflightGT(ctx context.Context, errW io.Writer, l lane, o shipOpts) (branchPlan, string, error) {
	branch, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return branchPlan{}, "", err
	}
	state, err := gtStateQuery(ctx, l.dir(), "ship")
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
			if state, seg, err = gtTrack(ctx, l.dir(), errW, o, branch); err != nil {
				return branchPlan{}, "", err
			}
		}
		chain, err := gtDownstack("ship", state, branch, trunk)
		if err != nil {
			return branchPlan{}, "", err
		}
		needsRestack = slices.ContainsFunc(chain, func(b string) bool { return state[b].NeedsRestack })
	}

	if o.noCommit && branch == trunk {
		return branchPlan{}, "", errors.New("ship: --no-commit on trunk is refused in the graphite lane — there is no stacked branch to submit")
	}

	if o.amend && branch == trunk {
		return branchPlan{}, "", errors.New("ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)")
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
	prefix   string
	suffix   string
	draft    bool
	noVerify bool
}

func gtStuck(prefix, problem, suffix string) string {
	return prefix + ": " + problem + suffix
}

func gtRestack(ctx context.Context, errW io.Writer, l lane, suffix, branch string) (string, error) {
	_, chain, err := gtStackChain(ctx, l.dir(), "ship", branch)
	if err != nil {
		return "", err
	}
	classify := func(dir render.Dir, r gtResult, cause error) error {
		if strings.Contains(r.Output, gtSyncConflict) {
			return &gtAdvice{advice: gtStuck("ship", gtRestackConflict(l.dir(), dir), suffix), cause: cause}
		}
		return fmt.Errorf("ship: %w", cause)
	}
	lanes, declined, err := gtLaneRestack(ctx, errW, "ship", l.checkout, gtBottomUp(chain), classify)
	if err != nil {
		return "", err
	}
	here, err := gtRestackAt(ctx, l.dir(), errW, classify)
	if err != nil {
		return "", err
	}
	for branch, reason := range gtSyncSkipped(here) {
		declined[branch] = reason
	}
	state, chain, err := gtStackChain(ctx, l.dir(), "ship", branch)
	if err != nil {
		return "", err
	}
	for _, b := range chain {
		if state[b].NeedsRestack {
			return "", errors.New(gtStuck("ship", gtOffParent(b, declined[b]), suffix))
		}
	}
	return gtLaneSegment(lanes), nil
}

// gtRestackConflict names the working copy a conflicted restack left mid-rebase,
// since a sweep across lanes can stop in one nobody is looking at.
func gtRestackConflict(here, dir render.Dir) string {
	if dir == here {
		return "gt restack hit a conflict — resolve the listed files, then gt continue (or gt abort, then gt restack)"
	}
	return fmt.Sprintf("gt restack hit a conflict in %s — resolve the listed files there, then gt continue (or gt abort, then gt restack)", dir)
}

// gtOffParent explains a branch the sweep could not restack. gt says why on
// stdout, which a non-streamed run never shows anyone, so the reason is carried
// into the refusal rather than pointed at.
func gtOffParent(branch, reason string) string {
	if reason == gtSkipMerged {
		return branch + " is already merged, so there is nothing to submit — drop it with gt untrack " + branch
	}
	problem := "gt restack left " + branch + " off its parent"
	if reason == "" {
		return problem + " — see gt's output above"
	}
	return problem + " (" + reason + ")"
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
func gtTrack(ctx context.Context, dir render.Dir, errW io.Writer, o shipOpts, branch string) (gtState, string, error) {
	argv := []string{"track", branch, "-f", "--no-interactive"}
	if o.parent != "" {
		argv = []string{"track", branch, "--parent", o.parent, "--no-interactive"}
	}
	untracked := fmt.Errorf("ship: branch %s is not tracked by graphite — run gt track %s, or pass --no-gt", branch, branch)
	r, runErr := gtRun(ctx, dir, argv, gtZeroFatal, errW)
	if err := gtReport(ctx, errW, r); err != nil {
		return nil, "", err
	}
	if runErr != nil {
		return nil, "", &gtAdvice{advice: untracked.Error(), cause: runErr}
	}
	state, err := gtStateQuery(ctx, dir, "ship")
	if err != nil {
		return nil, "", err
	}
	s, tracked := state[branch]
	if !tracked {
		return nil, "", untracked
	}
	seg := "tracked " + branch
	if len(s.Parents) > 0 {
		seg += " onto " + s.Parents[0].Ref
	}
	return state, seg, nil
}

// gtCommitArgv picks modify vs create from the branch plan; amend always
// modifies. A create always names the branch explicitly — gt would otherwise
// "generate a branch name from the commit message", which by then carries the
// Claude-Session-Id trailer — and gets --no-ai (modify has no --ai flag to pin).
func gtCommitArgv(o shipOpts, plan branchPlan) []string {
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"modify", "-m", o.message}
	case o.amend:
		argv = []string{"modify"}
	case plan.action == branchCreate:
		argv = []string{"create", plan.name}
		if plan.parent != "" {
			argv = append(argv, "--onto", plan.parent)
		}
		argv = append(argv, "-m", o.message, "--no-ai")
	default:
		argv = []string{"modify", "-c", "-m", o.message}
	}
	argv = append(argv, "--no-interactive")
	if o.noVerify || o.hooksRan {
		argv = append(argv, "--no-verify")
	}
	return argv
}

// shipCommitGT stages, refuses an empty commit, runs pre-commit hooks (or
// reports "hooks hunk-skip" for a hunk selection), then commits through gt.
// It never passes -a to gt: staging is shipGitAdd's job on both lanes, since
// gt add is a git-add passthrough that costs a whole gt startup.
func shipCommitGT(ctx context.Context, dir render.Dir, errW io.Writer, o shipOpts, sel *shipSelection, plan branchPlan) (string, error) {
	o.message = withSessionTrailer(o.message)
	if sel != nil {
		seg := ""
		if !o.noVerify && shipHasHookConfig(string(dir)) {
			seg = "hooks hunk-skip"
		}
		return seg, shipCommitGTSelect(ctx, dir, errW, o, sel, plan)
	}
	if err := shipGitAdd(ctx, dir, o); err != nil {
		return "", err
	}
	if !o.amend {
		if err := shipRefuseEmptyGit(ctx, dir, o, plan); err != nil {
			return "", err
		}
	}
	hookSeg, hooksRan, err := shipRunHooks(ctx, errW, dir, vcs.Git, o)
	if err != nil {
		return "", err
	}
	o.hooksRan = hooksRan
	r, runErr := gtRun(ctx, dir, gtCommitArgv(o, plan), gtZeroFatal, errW)
	if err := gtReport(ctx, errW, r); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", fmt.Errorf("ship: %w", runErr)
	}
	return hookSeg, nil
}

// shipCommitGTSelect commits a hunk selection through the same throwaway-index
// technique as shipCommitGitSelect — gt shells out to git, which honors
// GIT_INDEX_FILE, so running gt's verb under the same env commits only the temp
// index. gt's only hunk surface is interactive -p, so staging stays on git.
func shipCommitGTSelect(ctx context.Context, dir render.Dir, errW io.Writer, o shipOpts, sel *shipSelection, plan branchPlan) error {
	idxFile, err := os.CreateTemp("", "ccx-ship-index-*")
	if err != nil {
		return fmt.Errorf("ship: create temp index: %w", err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	defer func() { _ = os.Remove(idxPath) }()
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := render.RunCLIEnv(ctx, dir, "git", []string{"read-tree", "HEAD"}, env); err != nil {
		return fmt.Errorf("ship: git read-tree: %w", err)
	}
	if addArgv, ok := gitSelectAddArgv(o.rootPaths, sel); ok {
		if _, err := render.RunCLIEnv(ctx, dir, "git", addArgv, env); err != nil {
			return fmt.Errorf("ship: git add: %w", err)
		}
	}
	for _, path := range sortedSelectionFiles(sel) {
		if err := gitStageSelected(ctx, dir, path, sel, env); err != nil {
			return err
		}
	}
	argv := gtCommitArgv(o, plan)
	r, runErr := gtRun(ctx, dir, argv, gtZeroFatal, errW, env...)
	if err := gtReport(ctx, errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("ship: %w", runErr)
	}

	restoreArgv := append([]string{"restore", "--staged", "--"}, gitRestorePaths(o.rootPaths)...)
	if _, err := render.RunCLI(ctx, dir, "git", restoreArgv); err != nil {
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
// branch to it. It never fetches, rebases, or retries — gt owns restacking.
// The resolved downstack it returns is the one the pull request step then
// backfills into, so the stack is walked and its pull requests fetched once.
func shipPushGT(ctx context.Context, errW io.Writer, l lane, o shipOpts, meta map[string]prMeta, branch, suffix string) (submitted string, bodyless []string, stack []stackEntry, err error) {
	commonDir, err := gtCommonDir(ctx, l.dir(), "ship")
	if err != nil {
		return "", nil, nil, err
	}
	state, err := gtStateAt(ctx, commonDir, "ship")
	if err != nil {
		return "", nil, nil, err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", nil, nil, err
	}
	chain, err := gtDownstack("ship", state, branch, trunk)
	if err != nil {
		return "", nil, nil, err
	}
	sub := gtSubmit{prefix: "ship", suffix: suffix, draft: o.draft, noVerify: o.noVerify}
	if err := gtAnnounceStack(errW, sub.prefix, gtBottomUp(chain)); err != nil {
		return "", nil, nil, err
	}
	if err := gtSubmitStack(ctx, l, sub, commonDir, state, trunk, gtBottomUp(chain)); err != nil {
		return "", nil, nil, err
	}
	submitted, bodyless, stack = gtPRSegment(ctx, l, branch, chain, meta)
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
}

// gtSubmitStack drives one submit over Graphite's API: confirm the repo is
// synced, learn each branch's open PR, announce the submit, force-push every
// branch, then post the whole downstack in one submit call. The pushes come
// before the submit so the headSha Graphite records is always the one already
// on the remote.
func gtSubmitStack(ctx context.Context, l lane, s gtSubmit, commonDir string, state gtState, trunk string, branches []string) error {
	owner, name, err := gtRepoOwnerName(ctx, l, s.prefix)
	if err != nil {
		return err
	}
	client := gtAPIClient()
	sync, err := client.IsRepoSynced(ctx, owner, name)
	if err != nil {
		return gtSubmitFailure(err, s)
	}
	if sync.Status != gtapi.RepoSynced {
		problem := fmt.Sprintf("graphite does not sync %s/%s (%s) — add the repo at app.graphite.dev, or pass --no-gt", owner, name, sync.Status)
		return &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: fmt.Errorf("gtapi: is-repo-synced: %s %s", sync.Status, sync.Message)}
	}

	infos, err := client.PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{
		RepoOwner:        owner,
		RepoName:         name,
		PRNumbers:        []int{},
		PRHeadRefNames:   branches,
		TrunkBranchNames: []string{trunk},
		Callsite:         "ccx",
	})
	if err != nil {
		return gtSubmitFailure(err, s)
	}
	open := map[string]int{}
	for _, pr := range infos {
		if pr.State == gtapi.PROpen {
			open[pr.HeadRefName] = pr.PRNumber
		}
	}

	last, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return fmt.Errorf("%s: %w", s.prefix, err)
	}
	plan, err := gtSubmitPlan(ctx, l.dir(), s.prefix, state, branches, open, last)
	if err != nil {
		return err
	}

	pre := make([]gtapi.PreSubmitBranch, 0, len(plan))
	for _, b := range plan {
		pre = append(pre, gtapi.PreSubmitBranch{HeadRefName: b.name, PRNumber: b.pr})
	}
	if _, err := client.PreSubmitPullRequests(ctx, owner, name, pre); err != nil {
		return gtSubmitFailure(err, s)
	}

	// Each lease is recorded right after its own push — the irreversible step —
	// so no later failure leaves it behind the remote head this run just moved.
	for _, b := range plan {
		if err := gtPushBranch(ctx, l.dir(), s, b); err != nil {
			return err
		}
		version := gtmeta.Version{HeadSha: b.head, BaseSha: b.baseSha, BaseName: b.base}
		if err := gtmeta.RecordSubmitted(ctx, commonDir, b.name, version); err != nil {
			return gtSubmitFailure(err, s)
		}
	}

	if _, err := client.SubmitPullRequests(ctx, gtapi.SubmitRequest{
		RepoOwner:       owner,
		RepoName:        name,
		TrunkBranchName: trunk,
		PRs:             gtSubmitPRs(plan, s.draft),
	}); err != nil {
		return gtSubmitFailure(err, s)
	}
	return nil
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
// head, its parent and the sha it is stacked on, its open PR, and the lease of
// its last submitted version. A branch with no PR gets the title and body a
// create requires.
func gtSubmitPlan(ctx context.Context, dir render.Dir, prefix string, state gtState, branches []string, open map[string]int, last map[string]gtmeta.Version) ([]gtSubmitBranch, error) {
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
		if b.pr == 0 {
			title, body, err := gtCreateMeta(ctx, dir, prefix, name, b.base)
			if err != nil {
				return nil, err
			}
			b.title, b.body = title, body
		}
		plan = append(plan, b)
	}
	return plan, nil
}

// gtCreateMeta derives a created PR's title and body the way gt submit
// --no-edit does: from the branch's first commit above its base. The
// Claude-Session-Id trailer is dropped from the body, the same line the
// non-graphite lane keeps out of descriptions by never passing --fill.
func gtCreateMeta(ctx context.Context, dir render.Dir, prefix, branch, base string) (string, string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--reverse", "--format=%s%x00%b%x00", base + ".." + branch})
	if err != nil {
		return "", "", fmt.Errorf("%s: git log %s..%s: %w", prefix, base, branch, err)
	}
	fields := strings.Split(out, "\x00")
	if len(fields) < 3 {
		return "", "", fmt.Errorf("%s: %s holds no commit above %s to derive a PR title from", prefix, branch, base)
	}
	return strings.TrimPrefix(fields[0], "\n"), gtStripSessionTrailer(fields[1]), nil
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

// gtPushArgv is the push gt submit itself makes, recovered from its traces.
// The hook decision rides along: without --no-verify a repository's pre-push
// hook runs the suite the commit was told to skip.
func gtPushArgv(s gtSubmit, b gtSubmitBranch) []string {
	lease := "--force-with-lease"
	if b.lease != "" {
		lease += "=refs/heads/" + b.name + ":" + b.lease
	}
	argv := []string{"push", "origin", lease, "--progress", b.head + ":refs/heads/" + b.name}
	if s.noVerify {
		argv = append(argv, "--no-verify")
	}
	return append(argv, "--atomic")
}

// gtPushBranch force-pushes one branch under the lease of its last submitted
// version, so a remote someone else advanced is refused rather than
// overwritten. No retry: a force-with-lease push is never rejected as
// non-fast-forward, so a refusal here is terminal.
func gtPushBranch(ctx context.Context, dir render.Dir, s gtSubmit, b gtSubmitBranch) error {
	_, err := render.RunCLI(ctx, dir, "git", gtPushArgv(s, b))
	switch {
	case err == nil:
		return nil
	case gitPushStaleLease(err):
		problem := "remote " + b.name + " changed since last submit — reconcile manually (gt sync)"
		return &gtAdvice{advice: gtStuck(s.prefix, problem, s.suffix), cause: err}
	default:
		return fmt.Errorf("%s: git push %s: %w", s.prefix, b.name, err)
	}
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

func gtSubmitSplit(e *gtapi.SubmitError) string {
	failed := make([]string, 0, len(e.Failed))
	for _, f := range e.Failed {
		failed = append(failed, f.Head+" ("+f.Message+")")
	}
	problem := "graphite refused " + strings.Join(failed, ", ")
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

func gtPRSegment(ctx context.Context, l lane, branch string, chain []string, meta map[string]prMeta) (submitted string, bodyless []string, stack []stackEntry) {
	stackSeg := ""
	if len(chain) > 1 {
		stackSeg = fmt.Sprintf(" (stack of %d: %s)", len(chain), strings.Join(gtStackNames(chain), ", "))
	}
	submitted = "submitted " + branch + stackSeg
	stack = infoDownstack(ctx, l, chain)
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
