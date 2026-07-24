package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// gtNoOptCreate is cobra's NoOptDefVal for a bare --create: "-" is not a
// legal git branch name, so it never collides with an explicit --create=name.
const gtNoOptCreate = "-"

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

// gtPlan is shipPreflightGT's routing decision: branch and trunk, whether
// ship auto-creates a stacked branch off trunk, the report's stack depth, and
// whether preflight had to auto-track an untracked branch via gt track -f.
type gtPlan struct {
	branch      string
	trunk       string
	onTrunk     bool
	autoCreate  bool
	depth       int
	autoTracked bool
}

// gtStateQuery runs gt state and parses its JSON.
func gtStateQuery(ctx context.Context) (gtState, error) {
	out, err := render.RunCLI(ctx, "gt", []string{"state"})
	if err != nil {
		return nil, fmt.Errorf("ship: gt state: %w", err)
	}
	var state gtState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return nil, fmt.Errorf("ship: parse gt state: %w", err)
	}
	return state, nil
}

// gtTrunkBranch returns the one branch state marks Trunk.
func gtTrunkBranch(state gtState) (string, error) {
	for name, s := range state {
		if s.Trunk {
			return name, nil
		}
	}
	return "", errors.New("ship: gt state named no trunk branch")
}

// gtDownstack walks branch to trunk via each entry's first parent, returning
// every branch visited (branch first), excluding trunk. It errors on a
// missing parent or a cycle — corrupt or unresolvable gt metadata.
func gtDownstack(state gtState, branch, trunk string) ([]string, error) {
	var chain []string
	seen := make(map[string]bool)
	cur := branch
	for cur != trunk {
		if seen[cur] {
			return nil, fmt.Errorf("ship: gt state parent chain cycles at %s", cur)
		}
		seen[cur] = true
		chain = append(chain, cur)
		s, ok := state[cur]
		if !ok || len(s.Parents) == 0 {
			return nil, fmt.Errorf("ship: gt state has no parent for %s", cur)
		}
		cur = s.Parents[0].Ref
	}
	return chain, nil
}

// stackBranches lists the current downstack chain — current branch first, up
// to (excluding) trunk — or nil when the current branch is trunk.
func stackBranches(ctx context.Context) ([]string, error) {
	branch, err := shipPreflightGit(ctx)
	if err != nil {
		return nil, err
	}
	state, err := gtStateQuery(ctx)
	if err != nil {
		return nil, err
	}
	trunk, err := gtTrunkBranch(state)
	if err != nil {
		return nil, err
	}
	if branch == trunk {
		return nil, nil
	}
	return gtDownstack(state, branch, trunk)
}

// shipPreflightGT validates the current branch against graphite's tracked
// state. Unlike the jj/git preflights it always runs, even under --no-push,
// so an unrestacked stack still refuses a commit. An untracked branch is
// auto-adopted via gt track -f (parented to its most recent tracked
// ancestor); only a track that still leaves the branch untracked refuses.
func shipPreflightGT(ctx context.Context, o shipOpts) (gtPlan, error) {
	branch, err := shipPreflightGit(ctx)
	if err != nil {
		return gtPlan{}, err
	}
	state, err := gtStateQuery(ctx)
	if err != nil {
		return gtPlan{}, err
	}
	trunk, err := gtTrunkBranch(state)
	if err != nil {
		return gtPlan{}, err
	}
	onTrunk := branch == trunk
	_, tracked := state[branch]
	var autoTracked bool
	if !tracked && !onTrunk {
		if _, terr := render.RunCLI(ctx, "gt", []string{"track", "-f", "--no-interactive"}); terr == nil {
			if state, err = gtStateQuery(ctx); err != nil {
				return gtPlan{}, err
			}
			_, tracked = state[branch]
		}
		if !tracked {
			return gtPlan{}, fmt.Errorf("ship: branch %s is not tracked by graphite — run gt track, or pass --no-gt", branch)
		}
		autoTracked = true
	}

	chain, err := gtDownstack(state, branch, trunk)
	if err != nil {
		return gtPlan{}, err
	}
	for _, b := range chain {
		if state[b].NeedsRestack {
			return gtPlan{}, errors.New("ship: stack needs restack — run gt restack (gt continue / gt abort on conflict), then re-run ship")
		}
	}

	if o.amend && onTrunk {
		return gtPlan{}, errors.New("ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)")
	}

	depth := len(chain)
	autoCreate := onTrunk && o.create == ""
	if autoCreate || o.create != "" {
		depth++
	}

	return gtPlan{branch: branch, trunk: trunk, onTrunk: onTrunk, autoCreate: autoCreate, depth: depth, autoTracked: autoTracked}, nil
}

// gtCommitArgv picks modify vs create by whether --create was passed and
// whether the branch is trunk; amend always modifies. create also gets
// --no-ai (modify has no --ai flag to pin).
func gtCommitArgv(o shipOpts, plan gtPlan) []string {
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"modify", "-m", o.message}
	case o.amend:
		argv = []string{"modify"}
	case o.create == gtNoOptCreate:
		argv = []string{"create", "-m", o.message, "--no-ai"}
	case o.create != "":
		argv = []string{"create", o.create, "-m", o.message, "--no-ai"}
	case plan.onTrunk:
		argv = []string{"create", "-m", o.message, "--no-ai"}
	default:
		argv = []string{"modify", "-c", "-m", o.message}
	}
	argv = append(argv, "--no-interactive")
	if o.noVerify {
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
// is shipGitAdd's. A preflight auto-track (plan.autoTracked) is folded into
// the same returned segment, ahead of any hook segment.
func shipCommitGT(ctx context.Context, errW io.Writer, root string, o shipOpts, sel *shipSelection, plan gtPlan) (string, error) {
	o.message = withSessionTrailer(o.message)
	segs := make([]string, 0, 2)
	if plan.autoTracked {
		segs = append(segs, "tracked "+plan.branch)
	}
	if sel != nil {
		if !o.noVerify && shipHasHookConfig(root) {
			segs = append(segs, "hooks hunk-skip")
		}
		return strings.Join(segs, shipSep), shipCommitGTSelect(ctx, o, sel, plan)
	}
	if err := shipGTAdd(ctx, o); err != nil {
		return "", err
	}
	if !o.amend {
		if err := shipRefuseEmptyGT(ctx, o); err != nil {
			return "", err
		}
	}
	hookSeg, err := shipRunHooks(ctx, errW, root, vcs.Git, o)
	if err != nil {
		return "", err
	}
	if hookSeg != "" {
		segs = append(segs, hookSeg)
	}
	argv := gtCommitArgv(o, plan)
	if _, err := render.RunCLI(ctx, "gt", argv); err != nil {
		return "", fmt.Errorf("ship: gt %s: %w", argv[0], err)
	}
	return strings.Join(segs, shipSep), nil
}

// shipCommitGTSelect commits a hunk selection through gt's real index (no
// throwaway index like shipCommitGitSelect — gt reads the real one) and runs
// the gt verb with no pathspec; no restore --staged, gt's commit consumes it.
// The staging plumbing below stays on git: gt's only hunk surface is
// interactive `-p`, which --no-interactive can't drive.
func shipCommitGTSelect(ctx context.Context, o shipOpts, sel *shipSelection, plan gtPlan) error {
	if _, err := render.RunCLI(ctx, "git", []string{"read-tree", "HEAD"}); err != nil {
		return fmt.Errorf("ship: git read-tree: %w", err)
	}
	if addArgv, ok := gitSelectAddArgv(o.paths, sel); ok {
		if _, err := render.RunCLI(ctx, "git", addArgv); err != nil {
			return fmt.Errorf("ship: git add: %w", err)
		}
	}
	for _, path := range sortedSelectionFiles(sel) {
		if err := gitStageSelected(ctx, path, sel, nil); err != nil {
			return err
		}
	}
	argv := gtCommitArgv(o, plan)
	if _, err := render.RunCLI(ctx, "gt", argv); err != nil {
		return fmt.Errorf("ship: gt %s: %w", argv[0], err)
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

// shipPushGT re-reads the branch after the commit (gt create switches HEAD)
// and submits the downstack. It never fetches, rebases, or retries — gt
// owns restacking, and a rejected submit reports gt's own recovery step.
func shipPushGT(ctx context.Context, o shipOpts) (string, error) {
	branch, err := shipPreflightGit(ctx)
	if err != nil {
		return "", err
	}
	if _, err := render.RunCLI(ctx, "gt", gtSubmitArgv(o)); err != nil {
		return branch, classifyGTSubmit(err)
	}
	return branch, nil
}

// gtPRSegment resolves branch's PR via gh (non-fatal when gh is missing or
// the lookup fails) and formats the report segment, naming the stack depth
// when > 1.
func gtPRSegment(ctx context.Context, branch string, depth int) string {
	base := "submitted " + branch
	if _, err := exec.LookPath("gh"); err != nil {
		return base
	}
	out, err := render.RunCLI(ctx, "gh", []string{"pr", "view", branch, "--json", "number,url"})
	if err != nil {
		return base
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return base
	}
	seg := fmt.Sprintf("%s → PR #%d %s", base, pr.Number, pr.URL)
	if depth > 1 {
		seg += fmt.Sprintf(" (stack of %d)", depth)
	}
	return seg
}
