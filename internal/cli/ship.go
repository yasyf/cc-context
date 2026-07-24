package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	shipSep          = " · "
	shipLogBudget    = 2000
	shipCIQuietPolls = 2

	// Pre-commit @ is the commit-to-be, so ::@ here matches what ::@- matched
	// post-commit. A partial jj squash keeps bookmarks on the remainder @; the
	// push phase's post-commit ancestry and rebase checks handle that state.
	jjNearestBookmarkRevset = "heads(::@ & bookmarks())"
	jjDescribeTemplate      = `commit_id.short() ++ "\n" ++ description.first_line()`
	jjBookmarkTemplate      = `local_bookmarks.map(|b| b.name()).join(" ") ++ " "`
	jjTrunkBookmarkTemplate = `remote_bookmarks.map(|b| b.name()).join(" ") ++ " "`

	// jjRemoteBookmarkTemplate emits one "remote<TAB>tracked|untracked" line per
	// entry of jj bookmark list <name> --all-remotes. Filtering the list to the
	// exact bookmark name makes every line that bookmark's own remote counterpart,
	// so the remote and tracked fields alone disambiguate — the name is never parsed
	// back out of jj's template quoting.
	jjRemoteBookmarkTemplate = `remote ++ "\t" ++ if(tracked, "tracked", "untracked") ++ "\n"`
	jjAncestorRevsetFmt      = `bookmarks(exact:%q) & ::@-`
	jjStackRevsetFmt         = `bookmarks(exact:%q)..@-`
	jjConflictRevsetFmt      = `conflicts() & (bookmarks(exact:%q)..@-)::`
	jjStackLineTemplate      = `commit_id.short() ++ " " ++ description.first_line() ++ "\n"`
	jjOpIDTemplate           = `id`
	jjAtStateTemplate        = `parents.len()`
)

var (
	shipCIPollTries    = 12
	shipCIPollInterval = 5 * time.Second
)

// ansiRE matches CSI escape sequences (colour, cursor moves) so a captured log
// can be stripped to plain text before it is budget-capped.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// shipStreamCI reports whether CI watch output should stream live to w, which is
// true only when w is a real terminal. It mirrors stdinPiped's device check.
var shipStreamCI = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

type shipOpts struct {
	message   string
	noPush    bool
	noWatch   bool
	noVerify  bool
	amend     bool
	budget    int
	paths     []string
	bookmark  string
	skipHunks []string
	onlyHunks []string
	create    string
	draft     bool
	publish   bool
	noGT      bool
	reviews   bool
}

type ciRun struct {
	DatabaseID   int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	URL          string `json:"url"`
}

type ciView struct {
	WorkflowName string    `json:"workflowName"`
	Conclusion   string    `json:"conclusion"`
	StartedAt    time.Time `json:"startedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	URL          string    `json:"url"`
	Jobs         []ciJob   `json:"jobs"`
}

type ciJob struct {
	Name       string   `json:"name"`
	Conclusion string   `json:"conclusion"`
	Steps      []ciStep `json:"steps"`
}

type ciStep struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

func newShipCmd() *cobra.Command {
	var o shipOpts
	cmd := &cobra.Command{
		Use:   "ship [paths...]",
		Short: "Commit, push, and watch CI in one step",
		Long: `Commit, push, and watch CI in one step.

Ship refuses an empty working copy (the usual cause: a prior ship already landed the commit in @-) and resolves the push target before committing, so a refusal leaves the working copy untouched. After committing, ship fetches from the remote first and, when the target is no longer an ancestor of the local stack, rebases the stack onto it (jj: the target bookmark; git: origin/<branch>, autostashing uncommitted work); a rebase that would conflict is rolled back and reported instead of pushed. A push the remote rejects because it advanced again mid-ship re-fetches, re-rebases, and retries up to 3 attempts before failing with the manual recovery steps. --amend never retries a rejected push: the force-with-lease refusal is reported for manual reconciliation instead of overwriting the concurrent push.

A live Graphite config (.git/.graphite_repo_config) routes ship to the gt lane instead, even in colocated jj repos; --no-gt falls back to the jj/git detection above. The gt lane commits through gt: on trunk, -m auto-creates a stacked branch named from the message; on a stacked branch, -m appends a commit and --amend amends the branch tip; an untracked branch is adopted first with gt track -f (parented to its nearest tracked ancestor). --create starts a new stacked branch on top of the current one — bare --create derives the name from the message, and an explicit name must be spelled --create=name (cobra parses "--create name" as a path operand to commit). Instead of pushing, the lane submits the downstack with gt submit, published by default; --draft submits drafts, --publish makes the default explicit. Ship never fetches, rebases, or retries in the gt lane — gt owns restacking — so it refuses up front on needs_restack anywhere on the downstack (run gt restack, then re-run ship), on a branch gt track cannot adopt, and on --amend on trunk; a failed submit reports gt's own recovery step (gt restack, gt sync, or gt auth) instead of retrying. The report names the submitted branch and its PR: submitted <branch> → PR #<n> <url>.

--reviews keeps listening after the CI watch: each new review comment on the pushed branch's PR — every submitted PR, in the gt lane — streams to stdout until all are merged or closed. The standalone surface, with attach and replay knobs (--since, --interval, --budget, --stack), is ccx vcs reviews.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.paths = args
			return runShip(cmd, o)
		},
	}
	cmd.Flags().StringVarP(&o.message, "message", "m", "", "commit message")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "commit only; do not push or watch CI")
	cmd.Flags().BoolVar(&o.noWatch, "no-watch", false, "push but do not watch CI")
	cmd.Flags().BoolVar(&o.noVerify, "no-verify", false, "skip pre-commit hooks (uvx prek) before committing")
	cmd.Flags().BoolVar(&o.amend, "amend", false, "fold the working copy into the parent commit")
	cmd.Flags().IntVar(&o.budget, "budget", shipLogBudget, "token budget for the CI failure log excerpt (0 = uncapped)")
	cmd.Flags().StringVar(&o.bookmark, "bookmark", "", "jj bookmark to advance and push")
	cmd.Flags().StringArrayVar(&o.skipHunks, "skip-hunk", nil, "commit everything except this hunk ref (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringArrayVar(&o.onlyHunks, "only-hunk", nil, "commit only this hunk ref in its file (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringVar(&o.create, "create", "", "start a new stacked branch on top of the current one (graphite lane only); bare --create derives the name from the message, an explicit name must be spelled --create=name")
	cmd.Flags().Lookup("create").NoOptDefVal = gtNoOptCreate
	cmd.Flags().BoolVar(&o.draft, "draft", false, "submit new PRs as drafts (graphite lane only)")
	cmd.Flags().BoolVar(&o.publish, "publish", false, "publish new PRs (graphite lane only; the default when neither is passed)")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	cmd.Flags().BoolVar(&o.reviews, "reviews", false, "after the CI watch, keep streaming new PR review comments until every submitted PR is merged or closed")
	cmd.MarkFlagsMutuallyExclusive("create", "amend")
	cmd.MarkFlagsMutuallyExclusive("draft", "publish")
	return cmd
}

func runShip(cmd *cobra.Command, o shipOpts) error {
	ctx := cmd.Context()
	kind, root := vcs.DetectRoot(workingDir())
	if kind == vcs.None {
		return errors.New("ship: no git or jj repository in the working directory")
	}
	gtLane := !o.noGT && (kind == vcs.Git || kind == vcs.JJ) && vcs.GraphiteRepo(root)
	if gtLane {
		if _, gerr := exec.LookPath("gt"); gerr != nil {
			return errors.New("ship: graphite config found but gt not on PATH — install graphite (brew install graphite) or pass --no-gt")
		}
		kind = vcs.Git
	}
	if !gtLane && (o.create != "" || o.draft || o.publish) {
		return errors.New("ship: --create/--draft/--publish apply only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop these flags")
	}
	if cmd.Flags().Changed("create") && o.create == "" {
		return errors.New("ship: --create requires a branch name or no value")
	}
	if o.bookmark != "" && kind != vcs.JJ {
		return errors.New("ship: --bookmark applies only to jj repositories")
	}
	if !o.amend && o.message == "" {
		return errors.New("ship: -m/--message is required unless --amend")
	}
	if o.reviews && o.noPush {
		return errors.New("ship: --reviews requires push (drop --no-push)")
	}

	sel, err := parseShipSelection(ctx, kind, o)
	if err != nil {
		return err
	}
	if sel != nil {
		if err := resolveShipSelection(ctx, kind, sel); err != nil {
			return err
		}
	}
	var target string
	var plan gtPlan
	switch {
	case gtLane:
		plan, err = shipPreflightGT(ctx, o)
		if err != nil {
			return err
		}
	case !o.noPush:
		target, err = shipPreflight(ctx, kind, o)
		if err != nil {
			return err
		}
	}
	if kind == vcs.JJ && sel == nil && !o.amend {
		if err := shipRefuseEmptyJJ(ctx, root, o, target); err != nil {
			return err
		}
	}

	var preAmendSHA string
	if !o.noPush && kind == vcs.Git && o.amend && !gtLane {
		out, rerr := render.RunCLI(ctx, "git", []string{"rev-parse", "HEAD"})
		if rerr != nil {
			return fmt.Errorf("ship: git rev-parse HEAD: %w", rerr)
		}
		preAmendSHA = strings.TrimSpace(out)
	}

	var hookSeg string
	if gtLane {
		hookSeg, err = shipCommitGT(ctx, cmd.ErrOrStderr(), root, o, sel, plan)
	} else {
		hookSeg, err = shipCommit(ctx, cmd.ErrOrStderr(), root, kind, o, sel)
	}
	if err != nil {
		return err
	}

	short, subject, err := shipDescribe(ctx, kind)
	if err != nil {
		return err
	}
	segments := make([]string, 0, 5)
	if hookSeg != "" {
		segments = append(segments, hookSeg)
	}
	committedSegment := len(segments)
	segments = append(segments, fmt.Sprintf("committed %s %q", short, subject))

	if o.noPush {
		segments = append(segments, "not pushed")
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	var branch, remote string
	var rebased int
	if gtLane {
		branch, err = shipPushGT(ctx, o)
	} else {
		branch, remote, rebased, err = shipPush(ctx, kind, o, target, preAmendSHA)
	}
	if err != nil {
		return err
	}
	if rebased > 0 {
		short, subject, err = shipDescribe(ctx, kind)
		if err != nil {
			return err
		}
		segments[committedSegment] = fmt.Sprintf("committed %s %q", short, subject)
		segments = append(segments, fmt.Sprintf("rebased %d commit(s) onto %s", rebased, branch))
	}
	if gtLane {
		segments = append(segments, gtPRSegment(ctx, branch, plan.depth))
	} else {
		segments = append(segments, fmt.Sprintf("pushed %s → %s", branch, remote))
	}

	var reviewBranches []string
	if o.reviews {
		if gtLane {
			reviewBranches, err = stackBranches(ctx)
			if err != nil {
				return err
			}
		} else {
			reviewBranches = []string{branch}
		}
	}

	if o.noWatch {
		cmd.Println(strings.Join(segments, shipSep))
		if o.reviews {
			return shipReviewsWatch(ctx, cmd.OutOrStdout(), reviewBranches)
		}
		return nil
	}

	ciSeg, report, ciErr := shipWatchCI(ctx, cmd.ErrOrStderr(), kind, o.budget)
	if ciSeg == "" {
		cmd.Println(strings.Join(segments, shipSep))
		if o.reviews {
			return errors.Join(ciErr, shipReviewsWatch(ctx, cmd.OutOrStdout(), reviewBranches))
		}
		return ciErr
	}
	segments = append(segments, ciSeg)
	cmd.Println(strings.Join(segments, shipSep))
	for _, line := range report {
		cmd.Println(line)
	}
	if o.reviews {
		return errors.Join(ciErr, shipReviewsWatch(ctx, cmd.OutOrStdout(), reviewBranches))
	}
	return ciErr
}

// shipReviewsWatch wraps shipWatchReviews with %v, not %w: the watch's
// internal error categories must not steer ship's exit code — a co-occurring
// CI failure owns it.
func shipReviewsWatch(ctx context.Context, w io.Writer, branches []string) error {
	if err := shipWatchReviews(ctx, w, branches); err != nil {
		return fmt.Errorf("reviews: %v", err) //nolint:errorlint // deliberate: %w would let its category outlive this wrap
	}
	return nil
}

const envClaudeSessionKey = "CLAUDE_CODE_SESSION_ID"

func withSessionTrailer(message string) string {
	id := os.Getenv(envClaudeSessionKey)
	if id == "" || message == "" {
		return message
	}
	return message + "\n\nClaude-Session-Id: " + id
}

// shipCommit stages, runs pre-commit hooks, and commits. Hunk-scoped selections
// report "hooks hunk-skip" instead: external prek would inspect full worktree
// files, not the partial content being committed through a throwaway index.
// It returns the hook summary segment to prepend to the ship summary.
func shipCommit(ctx context.Context, errW io.Writer, root string, kind vcs.Kind, o shipOpts, sel *shipSelection) (string, error) {
	o.message = withSessionTrailer(o.message)
	var seg string
	if kind == vcs.Git && sel == nil {
		if err := shipGitAdd(ctx, o); err != nil {
			return "", err
		}
	}
	if sel != nil && !o.noVerify && shipHasHookConfig(root) {
		seg = "hooks hunk-skip"
	}
	if sel == nil {
		var err error
		seg, err = shipRunHooks(ctx, errW, root, kind, o)
		if err != nil {
			return "", err
		}
	}
	switch kind {
	case vcs.JJ:
		return seg, shipCommitJJ(ctx, o, sel)
	case vcs.Git:
		return seg, shipCommitGit(ctx, o, sel)
	default:
		return "", errors.New("ship: commit: unsupported vcs")
	}
}

// shipGitAdd stages the ship's paths (or everything, when unscoped) into the real
// index ahead of hook attempts and the commit.
func shipGitAdd(ctx context.Context, o shipOpts) error {
	addArgv := []string{"add", "-A"}
	if len(o.paths) > 0 {
		addArgv = append(addArgv, "--")
		addArgv = append(addArgv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "git", addArgv); err != nil {
		return fmt.Errorf("ship: git add: %w", err)
	}
	return nil
}

func shipCommitJJ(ctx context.Context, o shipOpts, sel *shipSelection) error {
	if sel != nil {
		return shipCommitJJSelect(ctx, o, sel)
	}
	argv := make([]string, 0, 4+len(o.paths))
	switch {
	case o.amend && o.message != "":
		argv = append(argv, "squash", "-m", o.message)
	case o.amend:
		argv = append(argv, "squash", "--use-destination-message")
	default:
		argv = append(argv, "commit", "-m", o.message)
	}
	if len(o.paths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "jj", argv); err != nil {
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

// shipCommitJJSelect commits a hunk selection through jj's diff-editor protocol:
// it writes a plan tempfile plus a sidecar, points a throwaway merge tool at
// ccx's own apply-selection subcommand, and lets jj drive the partial commit
// inside its transaction. On failure it prefers the sidecar's structured reason
// over raw jj stderr.
func shipCommitJJSelect(ctx context.Context, o shipOpts, sel *shipSelection) error {
	sidecar, err := os.CreateTemp("", "ccx-ship-result-*")
	if err != nil {
		return fmt.Errorf("ship: create result file: %w", err)
	}
	sidecarPath := sidecar.Name()
	_ = sidecar.Close()
	defer func() { _ = os.Remove(sidecarPath) }()

	planBytes, err := json.Marshal(buildSelectionPlan(sel, sidecarPath))
	if err != nil {
		return fmt.Errorf("ship: encode selection plan: %w", err)
	}
	planFile, err := os.CreateTemp("", "ccx-ship-plan-*.json")
	if err != nil {
		return fmt.Errorf("ship: create selection plan: %w", err)
	}
	planPath := planFile.Name()
	defer func() { _ = os.Remove(planPath) }()
	if _, err := planFile.Write(planBytes); err != nil {
		_ = planFile.Close()
		return fmt.Errorf("ship: write selection plan: %w", err)
	}
	if err := planFile.Close(); err != nil {
		return fmt.Errorf("ship: write selection plan: %w", err)
	}

	argv, err := jjSelectArgv(o, planPath)
	if err != nil {
		return err
	}
	if _, err := render.RunCLI(ctx, "jj", argv); err != nil {
		if reason := readSidecar(sidecarPath); reason != "" {
			return fmt.Errorf("ship: %s: %w", reason, err)
		}
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

func shipCommitGit(ctx context.Context, o shipOpts, sel *shipSelection) error {
	if sel != nil {
		return shipCommitGitSelect(ctx, o, sel)
	}
	var argv []string
	switch {
	case o.amend && o.message != "":
		argv = []string{"commit", "--amend", "-m", o.message}
	case o.amend:
		argv = []string{"commit", "--amend", "--no-edit"}
	default:
		argv = []string{"commit", "-m", o.message}
	}
	if o.noVerify {
		argv = append(argv, "--no-verify")
	}
	if len(o.paths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.paths...)
	}
	if _, err := render.RunCLI(ctx, "git", argv); err != nil {
		return fmt.Errorf("ship: git commit: %w", err)
	}
	return nil
}

func shipDescribe(ctx context.Context, kind vcs.Kind) (short, subject string, err error) {
	switch kind {
	case vcs.Git:
		out, rerr := render.RunCLI(ctx, "git", []string{"log", "-1", "--format=%h%x00%s"})
		if rerr != nil {
			return "", "", fmt.Errorf("ship: git log: %w", rerr)
		}
		return splitDescribe(out, "\x00")
	case vcs.JJ:
		out, rerr := render.RunCLI(ctx, "jj", []string{"log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate})
		if rerr != nil {
			return "", "", fmt.Errorf("ship: jj log: %w", rerr)
		}
		return splitDescribe(out, "\n")
	default:
		return "", "", errors.New("ship: describe: unsupported vcs")
	}
}

func splitDescribe(out, sep string) (string, string, error) {
	parts := strings.SplitN(out, sep, 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("ship: malformed commit description %q", out)
	}
	return strings.TrimRight(parts[0], "\n"), strings.TrimRight(parts[1], "\n"), nil
}

func shipRefuseEmptyJJ(ctx context.Context, root string, o shipOpts, target string) error {
	paths, err := shipChangedPaths(ctx, root, vcs.JJ, o)
	if err != nil {
		return err
	}
	if len(paths) > 0 {
		return nil
	}
	state, err := render.RunCLI(ctx, "jj", []string{"log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate})
	if err != nil {
		return fmt.Errorf("ship: jj working-copy state: %w", err)
	}
	var parents int
	if _, err := fmt.Sscan(state, &parents); err != nil {
		return fmt.Errorf("ship: malformed jj working-copy state %q: %w", state, err)
	}
	if parents > 1 {
		return nil
	}
	short, subject, err := shipDescribe(ctx, vcs.JJ)
	if err != nil {
		return err
	}
	scope := ""
	if len(o.paths) > 0 {
		scope = " in " + strings.Join(o.paths, ", ")
	}
	hint := ""
	if target != "" {
		hint = fmt.Sprintf(" push it: jj bookmark move exact:%s --to @- && jj git push --bookmark exact:%s", target, target)
	}
	return fmt.Errorf("ship: nothing to commit%s — did a prior ship already land %s %q?%s", scope, short, subject, hint)
}

func shipPreflight(ctx context.Context, kind vcs.Kind, o shipOpts) (string, error) {
	switch kind {
	case vcs.JJ:
		return shipPreflightJJ(ctx, o)
	case vcs.Git:
		return shipPreflightGit(ctx)
	default:
		return "", errors.New("ship: push: unsupported vcs")
	}
}

func shipPreflightJJ(ctx context.Context, o shipOpts) (string, error) {
	target := o.bookmark
	if target == "" {
		trunkNames, err := jjTrunkBookmarkNames(ctx)
		if err != nil {
			return "", err
		}
		if len(trunkNames) != 1 {
			return "", fmt.Errorf("ship: cannot resolve the trunk bookmark from %q; pass --bookmark <name>", trunkNames)
		}
		trunk := trunkNames[0]

		names, err := jjBookmarkNames(ctx, jjNearestBookmarkRevset)
		if err != nil {
			return "", err
		}
		switch len(names) {
		case 0:
			target = trunk
		case 1:
			if names[0] != trunk {
				return "", fmt.Errorf("ship: nearest bookmark %q is not trunk %q — pass --bookmark %s to advance it deliberately", names[0], trunk, names[0])
			}
			target = names[0]
		default:
			return "", fmt.Errorf("ship: multiple nearest bookmarks %q; pass --bookmark <name> to choose one", strings.Join(names, ", "))
		}
	}

	// jj treats a bare NAMES argument as a glob and no-ops with exit 0 on
	// zero matches, and a conflicted bookmark resolves to multiple commits,
	// which rebase would silently treat as a merge destination; resolve the
	// exact name up front so both fail loudly.
	heads, err := jjLogLines(ctx, fmt.Sprintf(`bookmarks(exact:%q)`, target))
	if err != nil {
		return "", err
	}
	switch {
	case len(heads) == 0:
		return "", fmt.Errorf("ship: bookmark %q not found", target)
	case len(heads) > 1:
		return "", fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", target, len(heads))
	}
	return target, nil
}

func shipPreflightGit(ctx context.Context) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"branch", "--show-current"})
	if err != nil {
		return "", fmt.Errorf("ship: git branch --show-current: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", errors.New("ship: detached HEAD; no branch to push")
	}
	return branch, nil
}

func shipPush(ctx context.Context, kind vcs.Kind, o shipOpts, target, preAmendSHA string) (branch, remote string, rebased int, err error) {
	switch kind {
	case vcs.JJ:
		rebased, err = shipPushJJ(ctx, target, o.amend)
		return target, "origin", rebased, err
	case vcs.Git:
		return shipPushGit(ctx, o.amend, preAmendSHA)
	default:
		return "", "", 0, errors.New("ship: push: unsupported vcs")
	}
}

func shipPushJJ(ctx context.Context, target string, amend bool) (int, error) {
	if err := jjTrackUntrackedTarget(ctx, target); err != nil {
		return 0, err
	}
	hint := fmt.Sprintf("jj git fetch && jj rebase -b @- --destination 'bookmarks(exact:%s)' && jj bookmark move exact:%s --to @- && jj git push --bookmark exact:%s", target, target, target)
	return shipPushRetry(ctx, target, hint, func(ctx context.Context) (int, error) {
		return shipPushJJOnce(ctx, target, amend)
	})
}

// jjTrackUntrackedTarget tracks target's untracked remote counterpart before a
// push — the fresh colocated-init state where jj git fetch never advances the
// local bookmark (leaving ship's divergence check blind) and jj git push refuses
// with "Non-tracking remote bookmark". It tracks the remote the counterpart
// actually sits on, and when several remotes carry an untracked counterpart the one
// the push targets. Tracking mutates no working-copy state, so a later push refusal
// still leaves the tree untouched.
func jjTrackUntrackedTarget(ctx context.Context, target string) error {
	remotes, err := jjUntrackedTargetRemotes(ctx, target)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return nil
	}
	remote := remotes[0]
	if len(remotes) > 1 {
		remote, err = jjPushRemote(ctx)
		if err != nil {
			return err
		}
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "track", jjExactPattern(target), "--remote=" + remote}); err != nil {
		return fmt.Errorf("ship: jj bookmark track %s --remote=%s: %w", target, remote, err)
	}
	return nil
}

// jjUntrackedTargetRemotes returns the remotes carrying a same-name bookmark for
// target that jj has not been told to track. It filters jj bookmark list to the
// exact name so every line is target's own remote counterpart, then reads the
// remote and tracked fields; the local-view line (empty remote) and the internal
// git remote (always tracked) both fall out of the untracked filter. A local-only
// bookmark pushed for the first time has no remote counterpart and is left alone.
func jjUntrackedTargetRemotes(ctx context.Context, target string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"bookmark", "list", jjExactPattern(target), "--all-remotes", "-T", jjRemoteBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj bookmark list %s --all-remotes: %w", target, err)
	}
	var remotes []string
	for _, line := range strings.Split(out, "\n") {
		remote, tracked, ok := strings.Cut(line, "\t")
		if !ok || remote == "" || tracked != "untracked" {
			continue
		}
		remotes = append(remotes, remote)
	}
	return remotes, nil
}

// jjPushRemote resolves the remote jj git push targets: the git.push setting, or
// origin when it is unset. jj derives the push remote from config, not from the
// tracked bookmarks, so this mirrors jj's own resolution — used to break a tie when
// more than one remote carries an untracked counterpart.
func jjPushRemote(ctx context.Context) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, "jj", []string{"config", "get", "git.push"})
	if err != nil {
		return "", fmt.Errorf("ship: jj config get git.push: %w", err)
	}
	switch code {
	case 0:
		if r := strings.TrimSpace(out); r != "" {
			return r, nil
		}
		return "origin", nil
	case 1:
		return "origin", nil
	default:
		return "", fmt.Errorf("ship: jj config get git.push: exit %d: %s", code, strings.TrimSpace(stderr))
	}
}

// jjExactPattern renders name as jj's exact string pattern, exact:"…", quoting it
// so a bookmark name carrying an '@' (or any character jj would otherwise read as a
// bookmark@remote symbol or a glob metacharacter) is matched literally.
func jjExactPattern(name string) string {
	return `exact:"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
}

// shipPushJJOnce is one push attempt: fetch, re-check the bookmark, rebase when
// the target diverged, advance the bookmark, then push. It snapshots the op log
// right after the bookmark move so a rejected push can undo exactly that move.
func shipPushJJOnce(ctx context.Context, target string, amend bool) (int, error) {
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "fetch"}); err != nil {
		return 0, fmt.Errorf("ship: jj git fetch: %w", err)
	}

	heads, err := jjLogLines(ctx, fmt.Sprintf(`bookmarks(exact:%q)`, target))
	if err != nil {
		return 0, err
	}
	switch {
	case len(heads) == 0:
		return 0, fmt.Errorf("ship: bookmark %q not found", target)
	case len(heads) > 1:
		return 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", target, len(heads))
	}

	ancestors, err := jjBookmarkNames(ctx, fmt.Sprintf(jjAncestorRevsetFmt, target))
	if err != nil {
		return 0, err
	}
	rebased := 0
	if len(ancestors) == 0 {
		rebased, err = jjRebaseOnto(ctx, target)
		if err != nil {
			return 0, err
		}
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "move", "exact:" + target, "--to", "@-"}); err != nil {
		return 0, fmt.Errorf("ship: advance bookmark %q: %w", target, err)
	}
	moveOp, err := jjOpID(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "push", "--bookmark", "exact:" + target}); err != nil {
		return rebased, shipPushJJReject(ctx, target, moveOp, amend, err)
	}
	return rebased, nil
}

// shipPushJJReject classifies a failed jj push. A remote-advanced rejection undoes
// only the bookmark move (jj op revert moveOp, uncancellable so a cancelled ctx
// leaves no advanced bookmark) and returns a *pushRejectedError to replay; a
// non-rejection, a failed undo, or a rejected amend is terminal.
func shipPushJJReject(ctx context.Context, target, moveOp string, amend bool, pushErr error) error {
	raw := fmt.Errorf("ship: jj git push: %w", pushErr)
	if !jjPushRejected(raw) {
		return raw
	}
	cleanup := context.WithoutCancel(ctx)
	if _, uerr := render.RunCLI(cleanup, "jj", []string{"op", "revert", moveOp}); uerr != nil {
		return fmt.Errorf("ship: jj git push rejected (%w); reverting the bookmark move also failed: %w — run: jj op revert %s", pushErr, uerr, moveOp)
	}
	if amend {
		return fmt.Errorf("ship: origin advanced past the commit you amended on %q — someone else pushed; not force-retrying over their work; inspect with jj log and jj op log, then reconcile manually: %w", target, pushErr)
	}
	return &pushRejectedError{err: raw}
}

func shipPushGit(ctx context.Context, amend bool, preAmendSHA string) (string, string, int, error) {
	out, err := render.RunCLI(ctx, "git", []string{"branch", "--show-current"})
	if err != nil {
		return "", "", 0, fmt.Errorf("ship: git branch --show-current: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return "", "", 0, errors.New("ship: detached HEAD; no branch to push")
	}
	remote, err := gitRemoteFor(ctx, branch)
	if err != nil {
		return "", "", 0, err
	}
	if amend {
		return branch, remote, 0, shipPushGitAmend(ctx, remote, branch, preAmendSHA)
	}
	hint := fmt.Sprintf("git fetch %s && git rebase --autostash %s/%s && git push %s %s", remote, remote, branch, remote, branch)
	rebased, err := shipPushRetry(ctx, branch, hint, func(ctx context.Context) (int, error) {
		return shipPushGitOnce(ctx, remote, branch)
	})
	return branch, remote, rebased, err
}

// gitRemoteFor resolves the remote that branch.<branch>.remote configures, so a
// triangular or non-origin-only repo fetches, rebases, and pushes against the
// same remote. git config --get exits 1 when unset; that and an empty value both
// default to origin. Any other exit is an error.
func gitRemoteFor(ctx context.Context, branch string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"config", "--get", "branch." + branch + ".remote"})
	if err != nil {
		return "", fmt.Errorf("ship: git config branch.%s.remote: %w", branch, err)
	}
	switch code {
	case 0:
		if r := strings.TrimSpace(out); r != "" {
			return r, nil
		}
		return "origin", nil
	case 1:
		return "origin", nil
	default:
		return "", fmt.Errorf("ship: git config branch.%s.remote: exit %d: %s", branch, code, strings.TrimSpace(stderr))
	}
}

// shipPushGitAmend pushes an amended commit without ever fetching. It tries a
// plain push first (an amend of an unpushed commit fast-forwards, no force) and
// only on a non-fast-forward rejection force-pushes with a lease pinned to
// preAmendSHA, so the force lands iff the remote still sits on the rewritten
// commit. A stale or rejected lease is terminal.
func shipPushGitAmend(ctx context.Context, remote, branch, preAmendSHA string) error {
	_, err := render.RunCLI(ctx, "git", []string{"push", remote, branch})
	if err == nil {
		return nil
	}
	if !gitPushRejected(err) {
		return fmt.Errorf("ship: git push: %w", err)
	}
	lease := fmt.Sprintf("--force-with-lease=%s:%s", branch, preAmendSHA)
	if _, err := render.RunCLI(ctx, "git", []string{"push", remote, lease, branch}); err != nil {
		if gitPushStaleLease(err) || gitPushRejected(err) {
			return fmt.Errorf("ship: %s/%s moved since your last sync — someone may have built on the commit you amended; fetch and reconcile manually before force-pushing: %w", remote, branch, err)
		}
		return fmt.Errorf("ship: git push: %w", err)
	}
	return nil
}

// shipPushGitOnce is one non-amend push attempt: fetch the remote, rebase onto
// <remote>/<branch> when it advanced past HEAD, then push. A rejected push moves
// no local ref, so it re-enters as a *pushRejectedError with no rollback.
func shipPushGitOnce(ctx context.Context, remote, branch string) (int, error) {
	if _, err := render.RunCLI(ctx, "git", []string{"fetch", remote}); err != nil {
		return 0, fmt.Errorf("ship: git fetch %s: %w", remote, err)
	}
	remoteRef := "refs/remotes/" + remote + "/" + branch
	present, err := gitRefExists(ctx, remoteRef)
	if err != nil {
		return 0, err
	}
	rebased := 0
	if present {
		ancestor, err := gitIsAncestor(ctx, remoteRef, "HEAD")
		if err != nil {
			return 0, err
		}
		if !ancestor {
			rebased, err = gitRebaseOnto(ctx, remote, branch)
			if err != nil {
				return 0, err
			}
		}
	}
	if _, err := render.RunCLI(ctx, "git", []string{"push", remote, branch}); err != nil {
		raw := fmt.Errorf("ship: git push: %w", err)
		if gitPushRejected(raw) {
			return rebased, &pushRejectedError{err: raw}
		}
		return rebased, raw
	}
	return rebased, nil
}

// gitRefExists reports whether ref resolves (git rev-parse --verify --quiet: exit
// 0 present, exit 1 missing). Any other exit is an error naming the code.
func gitRefExists(ctx context.Context, ref string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"rev-parse", "--verify", "--quiet", ref})
	if err != nil {
		return false, fmt.Errorf("ship: git rev-parse %s: %w", ref, err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("ship: git rev-parse %s: exit %d: %s", ref, code, strings.TrimSpace(stderr))
	}
}

// gitIsAncestor reports whether maybe is an ancestor of ref (git merge-base
// --is-ancestor: exit 0 yes, exit 1 no). Any other exit is an error.
func gitIsAncestor(ctx context.Context, maybe, ref string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"merge-base", "--is-ancestor", maybe, ref})
	if err != nil {
		return false, fmt.Errorf("ship: git merge-base --is-ancestor: %w", err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("ship: git merge-base --is-ancestor: exit %d: %s", code, strings.TrimSpace(stderr))
	}
}

// gitRebaseOnto rebases HEAD onto <remote>/<branch> with --autostash (the worktree
// is dirty after a hunk-scoped ship), returning the number of local commits
// replayed. A failed rebase is classified by gitRebaseFailure; an autostash pop
// left unapplied is surfaced as a warning, not a failure.
func gitRebaseOnto(ctx context.Context, remote, branch string) (int, error) {
	remoteRef := "refs/remotes/" + remote + "/" + branch
	countOut, err := render.RunCLI(ctx, "git", []string{"rev-list", "--count", remoteRef + "..HEAD"})
	if err != nil {
		return 0, fmt.Errorf("ship: git rev-list --count: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		return 0, fmt.Errorf("ship: malformed rev-list count %q: %w", countOut, err)
	}

	_, stderr, err := render.RunCLIKeepStderr(ctx, "git", []string{"rebase", "--autostash", remoteRef})
	if err != nil {
		return 0, gitRebaseFailure(ctx, remote, branch, err)
	}
	if strings.Contains(stderr, "resulted in conflicts") {
		slog.Warn("ship: rebase left autostashed changes unapplied — recover them with git stash pop", "branch", branch)
	}
	return count, nil
}

// gitRebaseFailure classifies a failed git rebase --autostash. A rebase in progress
// (REBASE_HEAD resolves) conflicted mid-replay: list, abort (restoring the
// autostash), report. Otherwise it failed before starting (hook, dirty index) —
// return the raw error, no abort. Cleanup runs uncancellable.
func gitRebaseFailure(ctx context.Context, remote, branch string, rebaseErr error) error {
	cleanup := context.WithoutCancel(ctx)
	inProgress, err := gitRefExists(cleanup, "REBASE_HEAD")
	if err != nil {
		return err
	}
	if !inProgress {
		return fmt.Errorf("ship: git rebase onto %s/%s: %w", remote, branch, rebaseErr)
	}
	files, lerr := render.RunCLI(cleanup, "git", []string{"diff", "--name-only", "--diff-filter=U"})
	if _, aerr := render.RunCLI(cleanup, "git", []string{"rebase", "--abort"}); aerr != nil {
		return fmt.Errorf("ship: rebase onto %s/%s conflicted (%w) and abort failed: %w — run: git rebase --abort, then resolve manually", remote, branch, rebaseErr, aerr)
	}
	if lerr != nil {
		return fmt.Errorf("ship: rebase onto %s/%s conflicted (%w); aborted back to the pre-rebase state; listing the conflicted files also failed: %w — resolve manually: git fetch %s && git rebase --autostash %s/%s, fix the conflicts (git status), then git push %s %s", remote, branch, rebaseErr, lerr, remote, remote, branch, remote, branch)
	}
	conflicted := strings.Join(strings.Fields(files), ", ")
	return fmt.Errorf("ship: rebase onto %s/%s conflicts in: %s; aborted back to the pre-rebase state (%w) — resolve manually: git fetch %s && git rebase --autostash %s/%s, fix the conflicts (git status), then git push %s %s", remote, branch, conflicted, rebaseErr, remote, remote, branch, remote, branch)
}

func jjBookmarkNames(ctx context.Context, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", rev, "--no-graph", "-T", jjBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj bookmarks at %q: %w", rev, err)
	}
	return strings.Fields(out), nil
}

func jjTrunkBookmarkNames(ctx context.Context) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj trunk bookmark: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, name := range strings.Fields(out) {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}

func jjLogLines(ctx context.Context, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", rev, "--no-graph", "-T", jjStackLineTemplate})
	if err != nil {
		return nil, fmt.Errorf("ship: jj log %q: %w", rev, err)
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func jjOpID(ctx context.Context) (string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate})
	if err != nil {
		return "", fmt.Errorf("ship: jj op log: %w", err)
	}
	opID := strings.TrimSpace(out)
	if opID == "" {
		return "", fmt.Errorf("ship: malformed jj operation ID %q", out)
	}
	return opID, nil
}

func jjRebaseOnto(ctx context.Context, target string) (int, error) {
	stack, err := jjLogLines(ctx, fmt.Sprintf(jjStackRevsetFmt, target))
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("ship: %q..@- is empty — the commit already landed on %q; refusing to move the bookmark backwards", target, target)
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"rebase", "-b", "@-", "--destination", fmt.Sprintf(`bookmarks(exact:%q)`, target)}); err != nil {
		return 0, fmt.Errorf("ship: jj rebase onto %q: %w", target, err)
	}
	rebaseOp, err := jjOpID(ctx)
	if err != nil {
		return 0, err
	}

	// rebase -b @- rewrites every descendant of the stack, including siblings
	// of @; check the whole rewritten set without including conflicts below it.
	conflicts, err := jjLogLines(ctx, fmt.Sprintf(jjConflictRevsetFmt, target))
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, uerr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp})
		if uerr == nil {
			return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed (rebase rolled back): %w", target, err)
		}
		return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", target, err, uerr, rebaseOp)
	}
	if len(conflicts) > 0 {
		// Undo only the rebase so a conflicted @ rolls back without touching a
		// concurrent session's operations.
		if _, uerr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp}); uerr != nil {
			return 0, fmt.Errorf("ship: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", target, uerr, rebaseOp)
		}
		return 0, fmt.Errorf("ship: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @- --destination 'bookmarks(exact:%q)', fix the conflicts (jj status), then: jj bookmark move exact:%s --to @- && jj git push --bookmark exact:%s", target, len(conflicts), strings.Join(conflicts, "\n  "), target, target, target)
	}
	return len(stack), nil
}

// shipWatchCI watches every CI run on the pushed commit and builds a per-run
// report. Only a shipHeadSHA failure yields an empty segment; infra failures
// return a segment so the summary still prints before the nonzero exit.
func shipWatchCI(ctx context.Context, errW io.Writer, kind vcs.Kind, budget int) (string, []string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "CI gh-missing", nil, nil
	}
	sha, err := shipHeadSHA(ctx, kind)
	if err != nil {
		return "", nil, err
	}
	runs, err := findCIRuns(ctx, sha)
	if err != nil {
		report := []string{fmt.Sprintf("check: gh run list --commit %s", sha)}
		return "CI error", report, err
	}
	if len(runs) == 0 {
		hasWorkflows, err := shipHasWorkflows()
		if err != nil {
			return "CI error", nil, err
		}
		if !hasWorkflows {
			return "CI no-run", nil, nil
		}
		return "CI unconfirmed", nil, fmt.Errorf("ship: no CI run was registered for the pushed commit; workflows may be paths-filtered or dispatch-only (on: workflow_dispatch); confirm manually: gh run list --commit %s", sha)
	}
	return reportCIRuns(ctx, errW, sha, runs, budget)
}

// reportCIRuns watches every run for sha and, after each batch, re-lists to catch
// workflows that registered late; a run is watched, viewed, and reported exactly
// once. A settle-time re-list failure takes the infra path with the report so far
// preserved.
func reportCIRuns(ctx context.Context, errW io.Writer, sha string, runs []ciRun, budget int) (string, []string, error) {
	type redRun struct {
		id   string
		view ciView
	}
	var report []string
	var reds []redRun
	seen := map[string]bool{}
	viewFailed := false

	process := func(batch []ciRun) int {
		n := 0
		for _, run := range batch {
			id := strconv.FormatInt(run.DatabaseID, 10)
			if seen[id] {
				continue
			}
			seen[id] = true
			n++
			// Watch drives live progress; gh run view is the authoritative conclusion,
			// so a dropped watch on a green run still passes.
			_ = watchCIRun(ctx, errW, id)
			view, err := viewCIRun(ctx, id)
			if err != nil {
				viewFailed = true
				report = append(report,
					strings.Join([]string{run.WorkflowName, "view-error", run.URL}, shipSep),
					fmt.Sprintf("view error: %v", err))
				continue
			}
			report = append(report, ciRunLine(view))
			if view.Conclusion == "" {
				// Watch exited early (transient): the run has no conclusion yet, which
				// is indeterminate, not red — take the infra path, not --log-failed.
				viewFailed = true
				report = append(report, fmt.Sprintf("run %s has not concluded; check: gh run view %s", id, id))
				continue
			}
			if !ciGreen(view.Conclusion) {
				reds = append(reds, redRun{id: id, view: view})
			}
		}
		return n
	}

	process(runs)
	quiet := 0
	for quiet < shipCIQuietPolls {
		if err := sleepCtx(ctx, shipCIPollInterval); err != nil {
			return "CI error", report, err
		}
		more, err := findCIRuns(ctx, sha)
		if err != nil {
			report = append(report, fmt.Sprintf("check: gh run list --commit %s", sha))
			return "CI error", report, err
		}
		if process(more) == 0 {
			quiet++
		} else {
			quiet = 0
		}
	}

	if len(reds) > 0 {
		per := budget / len(reds)
		if budget > 0 && per < 1 {
			per = 1
		}
		for _, r := range reds {
			report = append(report, ciFailureDetail(ctx, r.id, r.view, per)...)
		}
		return "CI failure", report, fmt.Errorf("ship: CI failed for %d run(s) on the pushed commit", len(reds))
	}
	if viewFailed {
		return "CI error", report, errors.New("ship: gh run view could not read the CI run conclusion")
	}
	return "CI success", report, nil
}

// watchCIRun blocks until run id concludes, streaming gh's progress to errW on a
// real terminal and otherwise buffering it away.
func watchCIRun(ctx context.Context, errW io.Writer, id string) error {
	if shipStreamCI(errW) {
		return render.RunCLIStream(ctx, "gh", []string{"run", "watch", id, "--exit-status", "--compact"}, errW)
	}
	_, err := render.RunCLI(ctx, "gh", []string{"run", "watch", id, "--exit-status"})
	return err
}

func viewCIRun(ctx context.Context, id string) (ciView, error) {
	out, err := render.RunCLI(ctx, "gh", []string{"run", "view", id, "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"})
	if err != nil {
		return ciView{}, fmt.Errorf("ship: gh run view %s: %w", id, err)
	}
	var view ciView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return ciView{}, fmt.Errorf("ship: parse gh run view %s: %w", id, err)
	}
	return view, nil
}

func ciRunLine(view ciView) string {
	parts := []string{view.WorkflowName, view.Conclusion}
	if d := ciDuration(view.StartedAt, view.UpdatedAt); d != "" {
		parts = append(parts, d)
	}
	parts = append(parts, view.URL)
	return strings.Join(parts, shipSep)
}

// ciFailureDetail names each red job and its failed steps, appends the
// ANSI-stripped, budget-capped --log-failed excerpt (fetch failure is non-fatal),
// and always ends with the full-log pointer plus the ci-triage agent handoff.
func ciFailureDetail(ctx context.Context, id string, view ciView, budget int) []string {
	var lines []string
	for _, job := range view.Jobs {
		if ciGreen(job.Conclusion) {
			continue
		}
		line := "failed: " + job.Name
		var steps []string
		for _, s := range job.Steps {
			if !ciGreen(s.Conclusion) {
				steps = append(steps, s.Name)
			}
		}
		if len(steps) > 0 {
			line += shipSep + strings.Join(steps, ", ")
		}
		lines = append(lines, line)
	}
	if log, err := render.RunCLI(ctx, "gh", []string{"run", "view", id, "--log-failed"}); err != nil {
		lines = append(lines, fmt.Sprintf("log unavailable: %v", err))
	} else if excerpt := strings.TrimRight(render.Cap(ansiRE.ReplaceAllString(log, ""), budget), "\n"); excerpt != "" {
		lines = append(lines, excerpt)
	}
	return append(lines, fmt.Sprintf("full log: gh run view %s --log-failed", id)+shipSep+"triage: spawn the cc-context:ci-triage agent with this run id")
}

// ciGreen reports whether a conclusion counts as passing; skipped and neutral
// (path-filtered workflows) are green, not failures.
func ciGreen(conclusion string) bool {
	switch conclusion {
	case "success", "skipped", "neutral":
		return true
	default:
		return false
	}
}

// ciDuration formats end-start as whole seconds, omitting it for a zero start or
// a negative span so the report never shows a negative duration.
func ciDuration(start, end time.Time) string {
	if start.IsZero() {
		return ""
	}
	d := end.Sub(start)
	if d < 0 {
		return ""
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

func shipHasWorkflows() (bool, error) {
	entries, err := os.ReadDir(filepath.Join(workingDir(), ".github", "workflows"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ship: read GitHub Actions workflows: %w", err)
	}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if !entry.IsDir() && (ext == ".yml" || ext == ".yaml") {
			return true, nil
		}
	}
	return false, nil
}

func shipHeadSHA(ctx context.Context, kind vcs.Kind) (string, error) {
	switch kind {
	case vcs.Git:
		out, err := render.RunCLI(ctx, "git", []string{"rev-parse", "HEAD"})
		if err != nil {
			return "", fmt.Errorf("ship: git rev-parse HEAD: %w", err)
		}
		return strings.TrimSpace(out), nil
	case vcs.JJ:
		out, err := render.RunCLI(ctx, "jj", []string{"log", "-r", "@-", "--no-graph", "-T", "commit_id"})
		if err != nil {
			return "", fmt.Errorf("ship: jj log commit_id: %w", err)
		}
		return strings.TrimSpace(out), nil
	default:
		return "", errors.New("ship: head sha: unsupported vcs")
	}
}

// findCIRuns polls gh for the runs on sha (server-side --commit filter, no
// client-side compare). Transient list or parse errors are tolerated across the
// window; an exhausted window returns the last error, or nil,nil for a clean
// no-run.
func findCIRuns(ctx context.Context, sha string) ([]ciRun, error) {
	var lastErr error
	for i := 0; i < shipCIPollTries; i++ {
		out, err := render.RunCLI(ctx, "gh", []string{"run", "list", "--commit", sha, "--limit", "50", "--json", "databaseId,workflowName,status,url"})
		switch {
		case err != nil:
			lastErr = fmt.Errorf("ship: gh run list: %w", err)
		default:
			var runs []ciRun
			if uerr := json.Unmarshal([]byte(out), &runs); uerr != nil {
				lastErr = fmt.Errorf("ship: parse gh run list: %w", uerr)
			} else if len(runs) > 0 {
				return runs, nil
			} else {
				lastErr = nil
			}
		}
		if i < shipCIPollTries-1 {
			if serr := sleepCtx(ctx, shipCIPollInterval); serr != nil {
				return nil, serr
			}
		}
	}
	return nil, lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
