package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	// gtRestackNeeded{1,2} through gtAuthRequired{1,2} are gt 1.8.6's own
	// wording for classifyGTSubmit; version-dependent, kept as lone constants
	// so an upgrade is a one-line change (precedent: jjPushMovedSubstr).
	gtRestackNeeded1 = "You must restack before submitting this stack."
	gtRestackNeeded2 = "You must restack and resolve conflicts with "

	gtTrunkStale = "Aborting submit because trunk branch is out of date"

	gtRemoteChanged1 = "This branch has been updated remotely since you last submitted"
	gtRemoteChanged2 = "Force-with-lease push failed due to external changes to the remote branch"

	gtAuthRequired1 = "Please authenticate your Graphite CLI"
	gtAuthRequired2 = "Your Graphite auth token is invalid/expired"
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
func gtReport(errW io.Writer, r gtResult) error {
	report := r.Diagnostics()
	if report == "" {
		return nil
	}
	if _, err := io.WriteString(errW, report); err != nil {
		return fmt.Errorf("ship: report gt diagnostics: %w", err)
	}
	return nil
}

// gtStateQuery runs gt state and parses its JSON. It is the one gt run whose
// streams stay apart — a diagnostic interleaved into the payload would break the
// unmarshal — and the one with no channel to report on, so a WARNING: gt exits 0
// with is dropped. An ERROR: at exit 0 is not: gt's own words are the only
// evidence that the state it printed is not the state on disk, so the policy is
// fatal and the whole output rides the error.
func gtStateQuery(ctx context.Context, prefix string) (gtState, error) {
	payload, _, err := gtCapture(ctx, []string{"state"}, gtZeroFatal)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	var state gtState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return nil, fmt.Errorf("%s: parse gt state: %w", prefix, err)
	}
	return state, nil
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

func gtStackChain(ctx context.Context, prefix, branch string) (gtState, []string, error) {
	state, err := gtStateQuery(ctx, prefix)
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
func stackBranches(ctx context.Context, prefix string) ([]string, error) {
	branch, err := gitCurrentBranch(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", prefix)
	}
	_, chain, err := gtStackChain(ctx, prefix, branch)
	return chain, err
}

func shipPreflightGT(ctx context.Context, errW io.Writer, l lane, o shipOpts) (branchPlan, string, error) {
	branch, err := gitCurrentBranch(ctx, "ship")
	if err != nil {
		return branchPlan{}, "", err
	}
	state, err := gtStateQuery(ctx, "ship")
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
			if state, seg, err = gtTrack(ctx, errW, o, branch); err != nil {
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

func gtLandedSuffix(resume string) string {
	if resume == "" {
		return ". The commit already landed and nothing was pushed, so there is nothing left to re-run."
	}
	return ". The commit already landed, so a plain re-run refuses as an empty commit — submit it with: " + resume
}

func gtStuckAfterCommit(problem, resume string) string {
	return "ship: " + problem + gtLandedSuffix(resume)
}

func gtRestack(ctx context.Context, errW io.Writer, l lane, resume, branch string) (string, error) {
	_, chain, err := gtStackChain(ctx, "ship", branch)
	if err != nil {
		return "", err
	}
	classify := func(dir string, r gtResult, cause error) error {
		if strings.Contains(r.Output, gtSyncConflict) {
			return &gtAdvice{advice: gtStuckAfterCommit(gtRestackConflict(dir), resume), cause: cause}
		}
		return fmt.Errorf("ship: %w", cause)
	}
	lanes, declined, err := gtLaneRestack(ctx, errW, "ship", l.checkout, gtBottomUp(chain), classify)
	if err != nil {
		return "", err
	}
	here, err := gtRestackAt(ctx, errW, "", classify)
	if err != nil {
		return "", err
	}
	for branch, reason := range gtSyncSkipped(here) {
		declined[branch] = reason
	}
	state, chain, err := gtStackChain(ctx, "ship", branch)
	if err != nil {
		return "", err
	}
	standing := gtLaneStanding(state, chain, declined)
	for _, b := range chain {
		if state[b].NeedsRestack {
			return "", errors.New(gtStuckAfterCommit(gtOffParent(b, standing[b]), resume))
		}
	}
	return gtLaneSegment(lanes), nil
}

// gtRestackConflict names the working copy a conflicted restack left mid-rebase,
// since a sweep across lanes can stop in one nobody is looking at.
func gtRestackConflict(dir string) string {
	if dir == "" {
		return "gt restack hit a conflict — resolve the listed files, then gt continue (or gt abort, then gt restack)"
	}
	return "gt restack hit a conflict in " + dir + " — resolve the listed files there, then gt continue (or gt abort, then gt restack)"
}

// gtOffParent explains a branch the sweep could not restack. gt says why on
// stdout, which a non-streamed run never shows anyone, so the reason is carried
// into the refusal rather than pointed at.
func gtOffParent(branch, reason string) string {
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
func gtTrack(ctx context.Context, errW io.Writer, o shipOpts, branch string) (gtState, string, error) {
	argv := []string{"track", "-f", "--no-interactive"}
	if o.parent != "" {
		argv = []string{"track", "--parent", o.parent, "--no-interactive"}
	}
	untracked := fmt.Errorf("ship: branch %s is not tracked by graphite — run gt track, or pass --no-gt", branch)
	r, runErr := gtRun(ctx, argv, gtZeroFatal, errW)
	if err := gtReport(errW, r); err != nil {
		return nil, "", err
	}
	if runErr != nil {
		return nil, "", &gtAdvice{advice: untracked.Error(), cause: runErr}
	}
	state, err := gtStateQuery(ctx, "ship")
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

// shipRefuseEmptyGT refuses a non-amend gt commit when the real index has no
// staged changes: unlike git commit, gt create happily creates an empty
// branch on an empty index.
func shipRefuseEmptyGT(ctx context.Context, o shipOpts) error {
	_, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"diff", "--cached", "--quiet"})
	if err != nil {
		return fmt.Errorf("ship: git diff --cached --quiet: %w", err)
	}
	if code == 1 {
		return nil
	}
	if code != 0 {
		return fmt.Errorf("ship: git diff --cached --quiet: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	short, subject, err := shipDescribe(ctx, vcs.Git)
	if err != nil {
		return err
	}
	scope := ""
	if len(o.paths) > 0 {
		scope = " in " + strings.Join(o.paths, ", ")
	}
	return fmt.Errorf("ship: nothing to commit%s — did a prior ship already land %s %q?", scope, short, subject)
}

// shipGTAdd stages the ship's paths (or everything, when unscoped) into the
// real index through gt add — gt's own git-add passthrough — so the plain
// staging step stays on the gt binary like every other gt-lane mutation.
func shipGTAdd(ctx context.Context, errW io.Writer, o shipOpts) error {
	addArgv := []string{"add", "--no-interactive", "-A"}
	if len(o.paths) > 0 {
		addArgv = append(addArgv, "--")
		addArgv = append(addArgv, o.paths...)
	}
	r, runErr := gtRun(ctx, addArgv, gtZeroFatal, errW)
	if err := gtReport(errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("ship: %w", runErr)
	}
	return nil
}

// shipCommitGT stages, refuses an empty commit, runs pre-commit hooks (or
// reports "hooks hunk-skip" for a hunk selection), then commits through gt.
// It never passes -a to gt: staging is shipGTAdd's job, same as the git lane
// is shipGitAdd's. The branch name in plan was derived before this call appends
// the session trailer, which is what keeps the trailer out of it.
func shipCommitGT(ctx context.Context, errW io.Writer, root string, o shipOpts, sel *shipSelection, plan branchPlan) (string, error) {
	o.message = withSessionTrailer(o.message)
	if sel != nil {
		seg := ""
		if !o.noVerify && shipHasHookConfig(root) {
			seg = "hooks hunk-skip"
		}
		return seg, shipCommitGTSelect(ctx, errW, o, sel, plan)
	}
	if err := shipGTAdd(ctx, errW, o); err != nil {
		return "", err
	}
	if !o.amend {
		if err := shipRefuseEmptyGT(ctx, o); err != nil {
			return "", err
		}
	}
	hookSeg, hooksRan, err := shipRunHooks(ctx, errW, root, vcs.Git, o)
	if err != nil {
		return "", err
	}
	o.hooksRan = hooksRan
	r, runErr := gtRun(ctx, gtCommitArgv(o, plan), gtZeroFatal, errW)
	if err := gtReport(errW, r); err != nil {
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
func shipCommitGTSelect(ctx context.Context, errW io.Writer, o shipOpts, sel *shipSelection, plan branchPlan) error {
	idxFile, err := os.CreateTemp("", "ccx-ship-index-*")
	if err != nil {
		return fmt.Errorf("ship: create temp index: %w", err)
	}
	idxPath := idxFile.Name()
	_ = idxFile.Close()
	defer func() { _ = os.Remove(idxPath) }()
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := render.RunCLIEnv(ctx, "git", []string{"read-tree", "HEAD"}, env); err != nil {
		return fmt.Errorf("ship: git read-tree: %w", err)
	}
	if addArgv, ok := gitSelectAddArgv(o.paths, sel); ok {
		if _, err := render.RunCLIEnv(ctx, "git", addArgv, env); err != nil {
			return fmt.Errorf("ship: git add: %w", err)
		}
	}
	for _, path := range sortedSelectionFiles(sel) {
		if err := gitStageSelected(ctx, path, sel, env); err != nil {
			return err
		}
	}
	argv := gtCommitArgv(o, plan)
	r, runErr := gtRun(ctx, argv, gtZeroFatal, errW, env...)
	if err := gtReport(errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("ship: %w", runErr)
	}

	restoreArgv := append([]string{"restore", "--staged", "--"}, gitRestorePaths(o.paths)...)
	if _, err := render.RunCLI(ctx, "git", restoreArgv); err != nil {
		return fmt.Errorf("ship: git restore --staged: %w", err)
	}
	return nil
}

// gtSubmitArgv builds the gt submit argv. --no-stack narrows to the current
// branch's downstack and skips the upstack-inclusion prompt.
func gtSubmitArgv(o shipOpts) []string {
	argv := []string{"submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack"}
	if o.draft {
		argv = append(argv, "--draft")
	} else {
		argv = append(argv, "--publish")
	}
	return argv
}

// classifyGTSubmit maps a failed gt submit to a specific recovery step when gt's
// wording is recognized, or wraps the failure verbatim otherwise. It reads the
// whole interleaved output, not the error's text: gt splits one submit's report
// across both streams, so a recognized sentence arrives on whichever one gt
// picked. A recognized failure keeps gt's own error as the advice's cause, so
// errors.As still reaches it behind the sentence that replaced it.
func classifyGTSubmit(r gtResult, cause error) error {
	switch {
	case strings.Contains(r.Output, gtRestackNeeded1) || strings.Contains(r.Output, gtRestackNeeded2):
		return &gtAdvice{advice: "ship: stack drifted since preflight — run gt restack", cause: cause}
	case strings.Contains(r.Output, gtTrunkStale):
		return &gtAdvice{advice: "ship: trunk is out of sync — run gt sync (or ccx vcs restack)", cause: cause}
	case strings.Contains(r.Output, gtRemoteChanged1) || strings.Contains(r.Output, gtRemoteChanged2):
		return &gtAdvice{advice: "ship: remote branch changed since last submit — reconcile manually (gt sync)", cause: cause}
	case strings.Contains(r.Output, gtAuthRequired1) || strings.Contains(r.Output, gtAuthRequired2):
		return &gtAdvice{advice: "ship: graphite auth required — run gt auth", cause: cause}
	default:
		return fmt.Errorf("ship: %w", cause)
	}
}

// gtSubmit runs one submit — the dry run or the real one — and reports whether
// gt published anything. Exit 0 is not consent: gt exits 0 while printing an
// ERROR: naming a submit it refused, and ccx has no second oracle for a push it
// did not make, so the policy is fatal.
func gtSubmit(ctx context.Context, errW io.Writer, argv []string, resume string) error {
	r, runErr := gtRun(ctx, argv, gtZeroFatal, errW)
	if err := gtReport(errW, r); err != nil {
		return err
	}
	if runErr == nil {
		return nil
	}
	err := classifyGTSubmit(r, runErr)
	var advice *gtAdvice
	if !errors.As(err, &advice) {
		return err
	}
	return &gtAdvice{advice: advice.advice + gtLandedSuffix(resume), cause: advice.cause}
}

// shipPushGT submits the downstack of the branch the commit landed on. The
// downstack is re-read here, after the commit, because a gt create adds a
// branch to it. It never fetches, rebases, or retries — gt owns restacking, and
// a rejected submit reports gt's own recovery step.
//
// gt submit force-pushes "all branches in the current stack from trunk to the
// current branch", so a submit deeper than one branch names the chain first.
// That name comes from the downstack already resolved here rather than from a
// second gt submit --dry-run, which cost a full network pass to report a list
// this function is holding — and reported it wrong, since without --no-stack it
// covered the upstack the real submit drops.
// The resolved downstack it returns is the one the pull request step then
// backfills into, so the stack is walked and its pull requests fetched once.
func shipPushGT(ctx context.Context, errW io.Writer, l lane, o shipOpts, meta map[string]prMeta, branch, resume string) (submitted string, bodyless []string, stack []stackEntry, err error) {
	_, chain, err := gtStackChain(ctx, "ship", branch)
	if err != nil {
		return "", nil, nil, err
	}
	if err := gtAnnounceStack(errW, chain); err != nil {
		return "", nil, nil, err
	}
	if err := gtSubmit(ctx, errW, gtSubmitArgv(o), resume); err != nil {
		return "", nil, nil, err
	}
	submitted, bodyless, stack = gtPRSegment(ctx, l, branch, chain, meta)
	return submitted, bodyless, stack, nil
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
// a stack deeper than one branch is more than the one being shipped.
func gtAnnounceStack(errW io.Writer, chain []string) error {
	if len(chain) < 2 {
		return nil
	}
	if _, err := fmt.Fprintf(errW, "ship: submitting %d branches: %s\n", len(chain), strings.Join(gtStackNames(chain), ", ")); err != nil {
		return fmt.Errorf("ship: name the stack: %w", err)
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
