package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// gtStateQuery runs gt state and parses its JSON.
func gtStateQuery(ctx context.Context, prefix string) (gtState, error) {
	out, err := render.RunCLI(ctx, "gt", []string{"state"})
	if err != nil {
		return nil, fmt.Errorf("%s: gt state: %w", prefix, err)
	}
	var state gtState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
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

// stackBranches lists the current downstack chain — current branch first, up
// to (excluding) trunk — or nil when the current branch is trunk.
func stackBranches(ctx context.Context, prefix string) ([]string, error) {
	branch, err := gitCurrentBranch(ctx)
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", prefix)
	}
	state, err := gtStateQuery(ctx, prefix)
	if err != nil {
		return nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, err
	}
	if branch == trunk {
		return nil, nil
	}
	return gtDownstack(prefix, state, branch, trunk)
}

// shipPreflightGT resolves the branch decision and validates the current branch
// against graphite's tracked state. Unlike the jj/git preflights it always
// runs, even under --no-push, so an unrestacked stack still refuses a commit.
// An untracked branch is auto-adopted first; only a track that still leaves the
// branch untracked refuses.
func shipPreflightGT(ctx context.Context, l lane, o shipOpts) (branchPlan, string, error) {
	branch, err := gitCurrentBranch(ctx)
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
	if branch != "" && branch != trunk {
		if _, tracked := state[branch]; !tracked {
			if state, seg, err = gtTrack(ctx, o, branch); err != nil {
				return branchPlan{}, "", err
			}
		}
		chain, err := gtDownstack("ship", state, branch, trunk)
		if err != nil {
			return branchPlan{}, "", err
		}
		for _, b := range chain {
			if state[b].NeedsRestack {
				return branchPlan{}, "", errors.New("ship: stack needs restack — run gt restack (gt continue / gt abort on conflict), then re-run ship")
			}
		}
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
	return plan, seg, nil
}

// gtTrack adopts an untracked branch, reporting the parent it landed on. gt
// track -f "sets the parent to the most recent tracked ancestor of the branch
// being tracked to skip prompts" and takes precedence over --parent, so a branch
// cut off another feature branch is adopted onto it and gt submit then publishes
// that unrelated branch too; --parent therefore drops -f, and either way the
// resolved parent is read back out of gt state and named in the report.
func gtTrack(ctx context.Context, o shipOpts, branch string) (gtState, string, error) {
	argv := []string{"track", "-f", "--no-interactive"}
	if o.parent != "" {
		argv = []string{"track", "--parent", o.parent, "--no-interactive"}
	}
	untracked := fmt.Errorf("ship: branch %s is not tracked by graphite — run gt track, or pass --no-gt", branch)
	if _, err := render.RunCLI(ctx, "gt", argv); err != nil {
		return nil, "", untracked
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
func shipGTAdd(ctx context.Context, o shipOpts) error {
	addArgv := []string{"add", "--no-interactive", "-A"}
	if len(o.paths) > 0 {
		addArgv = append(addArgv, "--")
		addArgv = append(addArgv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "gt", addArgv); err != nil {
		return fmt.Errorf("ship: gt add: %w", err)
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
		return seg, shipCommitGTSelect(ctx, o, sel, plan)
	}
	if err := shipGTAdd(ctx, o); err != nil {
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
	argv := gtCommitArgv(o, plan)
	if _, err := render.RunCLI(ctx, "gt", argv); err != nil {
		return "", fmt.Errorf("ship: gt %s: %w", argv[0], err)
	}
	return hookSeg, nil
}

// shipCommitGTSelect commits a hunk selection through the same throwaway-index
// technique as shipCommitGitSelect — gt shells out to git, which honors
// GIT_INDEX_FILE, so running gt's verb under the same env commits only the temp
// index. gt's only hunk surface is interactive -p, so staging stays on git.
func shipCommitGTSelect(ctx context.Context, o shipOpts, sel *shipSelection, plan branchPlan) error {
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
	if _, err := render.RunCLIEnv(ctx, "gt", argv, env); err != nil {
		return fmt.Errorf("ship: gt %s: %w", argv[0], err)
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

// classifyGTSubmit maps a failed gt submit's stderr to a specific recovery
// step when gt's wording is recognized, or wraps the raw error otherwise.
func classifyGTSubmit(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, gtRestackNeeded1) || strings.Contains(msg, gtRestackNeeded2):
		return errors.New("ship: stack drifted since preflight — run gt restack, then re-run ship")
	case strings.Contains(msg, gtTrunkStale):
		return errors.New("ship: trunk is out of sync — run gt sync (or ccx vcs restack), then re-run ship")
	case strings.Contains(msg, gtRemoteChanged1) || strings.Contains(msg, gtRemoteChanged2):
		return errors.New("ship: remote branch changed since last submit — reconcile manually (gt sync), then re-run ship")
	case strings.Contains(msg, gtAuthRequired1) || strings.Contains(msg, gtAuthRequired2):
		return errors.New("ship: graphite auth required — run gt auth")
	default:
		return fmt.Errorf("ship: gt submit: %w", err)
	}
}

// shipPushGT submits the downstack of the branch the commit landed on. The
// downstack is re-read here, after the commit, because a gt create adds a
// branch to it. It never fetches, rebases, or retries — gt owns restacking, and
// a rejected submit reports gt's own recovery step.
//
// gt submit force-pushes "all branches in the current stack from trunk to the
// current branch"; --no-stack only drops the upstack. So a submit that will
// touch more than the current branch runs --dry-run first ("Reports the PRs
// that would be submitted and terminates") and names every branch it will
// publish in the report.
// The resolved downstack it returns is the one the pull request step then
// backfills into, so the stack is walked and its pull requests fetched once.
func shipPushGT(ctx context.Context, root string, o shipOpts, meta map[string]prMeta, branch string) (submitted string, bodyless []string, stack []stackEntry, err error) {
	state, err := gtStateQuery(ctx, "ship")
	if err != nil {
		return "", nil, nil, err
	}
	trunk, err := gtTrunkBranch("ship", state)
	if err != nil {
		return "", nil, nil, err
	}
	var chain []string
	if branch != trunk {
		if chain, err = gtDownstack("ship", state, branch, trunk); err != nil {
			return "", nil, nil, err
		}
	}
	if len(chain) > 1 {
		if _, err := render.RunCLI(ctx, "gt", []string{"submit", "--dry-run", "--no-interactive"}); err != nil {
			return "", nil, nil, classifyGTSubmit(err)
		}
	}
	if _, err := render.RunCLI(ctx, "gt", gtSubmitArgv(o)); err != nil {
		return "", nil, nil, classifyGTSubmit(err)
	}
	submitted, bodyless, stack = gtPRSegment(ctx, root, branch, chain, meta)
	return submitted, bodyless, stack, nil
}

func gtPRSegment(ctx context.Context, root, branch string, chain []string, meta map[string]prMeta) (submitted string, bodyless []string, stack []stackEntry) {
	stackSeg := ""
	if len(chain) > 1 {
		names := make([]string, len(chain))
		for i, b := range chain {
			names[len(chain)-1-i] = b
		}
		stackSeg = fmt.Sprintf(" (stack of %d: %s)", len(chain), strings.Join(names, ", "))
	}
	submitted = "submitted " + branch + stackSeg
	stack = infoDownstack(ctx, root, chain)
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
