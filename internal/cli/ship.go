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
	"slices"
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
	// jj renders a bookmark name it would otherwise reread as a symbol — one
	// carrying an '@', a space, a quote — in its own quoted string syntax, whose
	// escapes (\e, \xHH) are not JSON's. escape_json() emits the name as a JSON
	// string instead, so one name per line decodes back to exactly what jj holds.
	jjBookmarkTemplate = `local_bookmarks.map(|b| b.name().escape_json()).join("\n") ++ "\n"`
	// jj exposes every local git ref of a colocated repo as <name>@git, which an
	// unfiltered remote_bookmarks would make a trunk candidate of. escape_json()
	// carries the same reasoning as jjBookmarkTemplate above.
	jjTrunkBookmarkTemplate = `remote_bookmarks.filter(|b| b.remote() != "git").map(|b| b.name().escape_json()).join("\n") ++ "\n"`

	// jjRemoteBookmarkTemplate emits one "remote<TAB>tracked|untracked" line per
	// entry of jj bookmark list <name> --all-remotes. Filtering the list to the
	// exact bookmark name makes every line that bookmark's own remote counterpart,
	// so the remote and tracked fields alone disambiguate — the name is never parsed
	// back out of jj's template quoting.
	jjRemoteBookmarkTemplate = `remote ++ "\t" ++ if(tracked, "tracked", "untracked") ++ "\n"`
	jjStackLineTemplate      = `commit_id.short() ++ " " ++ description.first_line() ++ "\n"`
	jjOpIDTemplate           = `id`
	jjAtStateTemplate        = `parents.len()`
)

var (
	shipCIPollTries    = 12
	shipCIPollInterval = 5 * time.Second

	// shipCIWatchTimeout is the runaway guard on one gh run watch, which blocks
	// until the run concludes and so is legitimately slower than any generic
	// default. This repo runs only GitHub-hosted runners, which cap a job at 6h and
	// a whole run at 35 days, so 12h sits past twice the longest job that can run
	// and far short of the run's own cap: a watch still blocked here is a hung gh,
	// not a build.
	shipCIWatchTimeout = 12 * time.Hour
)

// ansiRE matches CSI escape sequences (colour, cursor moves) so a captured log
// can be stripped to plain text before it is budget-capped.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

var errShipMessageRequired = errors.New("ship: -m/--message is required unless --amend, --no-commit, or --pr-title")

// shipStreamCI reports whether a child's output should stream live to w, which is
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
	noCommit  bool
	noWatch   bool
	noVerify  bool
	hooksRan  bool
	yolo      bool
	amend     bool
	budget    int
	paths     []string
	skipHunks []string
	onlyHunks []string

	// branch, newBranch, appendOnly, parent, and allowTrunk are the caller's
	// stated intent for resolveBranchPlan; --bookmark and --create are aliases
	// bound to the same fields as --branch and --new-branch.
	branch     string
	newBranch  string
	appendOnly bool
	parent     string
	allowTrunk bool

	draft   bool
	publish bool
	noGT    bool
	reviews bool

	// prTitle and prBodyFile are repeatable and branch-scoped, because one gt
	// submit opens a pull request for every branch in the downstack.
	prTitle    []string
	prBodyFile []string
	noPR       bool
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

Where the commit goes is one decision, resolved before any mutation and reported as a branch <name> or created <name> segment. On a non-trunk branch or bookmark, ship appends to it. On trunk it appends in your own repositories — direct-to-main is deliberate there — and starts a branch named from the commit subject when GitHub says the repository is someone else's, since an org trunk rejects the commit through its protect-<trunk> hook and leaves it dangling; the graphite lane always starts a branch on trunk, because gt has no verb that commits onto it. A detached HEAD is refused rather than guessed at, and so are several trunk candidates unless --branch names one of them. --branch <name> commits onto that branch, creating it here when it does not exist and refusing when it exists somewhere else, since ship does not check branches out; --new-branch[=<name>] always starts one, deriving the name from the commit subject when bare (an explicit name must be spelled --new-branch=name, because cobra parses "--new-branch name" as a path operand to commit); --append refuses on trunk; --allow-trunk lets --branch advance a trunk you do not own. --bookmark is a jj-only alias of --branch, --create a deprecated alias of --new-branch. A new branch is cut with gt create (graphite), git switch -c (git), or jj bookmark create -r @- (jj).

A live Graphite config (.git/.graphite_repo_config, or the git common dir's copy in a linked worktree) routes ship to the gt lane instead, even in colocated jj repos; --no-gt falls back to the jj/git detection above. Ship declines that lane, reporting a leading lane <kind> (<reason>) segment, when the repo sets ccx.nogt or when GitHub says the repo is someone else's — a public repo you neither administer nor share an owner or organization with. A lookup that cannot be made at all (gh off PATH, not signed in, no GitHub remote) keeps the gt lane. Ship also asks Graphite itself, before any commit forms, whether this repo is submittable — a live config only proves gt init once ran — and declines the lane when Graphite answers no (no auth token, no permissions); a probe that times out or cannot reach the Graphite server declines it too, because a lane nobody could confirm is not one to ride — the cost is that a blip on a genuinely synced repo lands a plain branch outside the stack, which is the safer half of that trade. Every verdict is cached between ships: a yes for a day, a no for an hour, an unanswered probe for a minute, so an outage costs one probe a minute rather than one per command. The gt lane commits through gt: gt create <name> starts a stacked branch, gt modify -c appends to one, and --amend amends the branch tip; an untracked branch is adopted first with gt track -f, or gt track --parent when --parent names one, and the resolved parent is reported. Instead of pushing, the lane submits the downstack with gt submit, published by default; --draft submits drafts, --publish makes the default explicit. A submit deeper than one branch names every branch it is about to force-push before it runs, since gt submit always force-pushes the whole downstack. Ship never fetches or retries in the gt lane — gt owns restacking — but it does recover an unrestacked downstack itself: needs_restack anywhere on the chain runs gt restack after the commit and reports a restacked segment. After, not before, because gt restack rebases and git refuses a rebase over the dirty working copy every ship starts from. A stack spread across working copies is restacked in place: git will not move a branch a sibling checkout has checked out, so one gt restack declines those branches and still exits 0, and ship instead drives gt once from each working copy holding a branch of the chain, bottom-up, ending with this one — the segment then reads restacked across N working copies. A lane's uncommitted work survives the rebase; gt stashes it itself. A restack that conflicts or that gt state still reads as unrestacked is the one manual case left, and the refusal names the exact way out — including which working copy a conflict stopped in, and the reason gt gave for a branch it declined, since gt says that on stdout and a non-streamed run never shows anyone: the commit has already landed by then, so a plain re-run would refuse as an empty commit — resolve it, run gt continue (or gt abort, then gt restack), and submit the commit in place with the ccx vcs ship --no-commit line the refusal prints, which restates the PR flags this invocation carried. The lane still refuses up front on a branch gt track cannot adopt and on --amend on trunk; a failed submit reports gt's own recovery step (gt restack, gt sync, or gt auth) instead of retrying. The report names the submitted branch and its PR: submitted <branch> → PR #<n> <url>.

Ship owns the pull request in every lane. --pr-title and --pr-body-file are repeatable and branch-scoped — <branch>=<value>, a bare value applying to the tip — because one gt submit opens a PR for every branch in the downstack; --pr-body-file takes "-" once to read the body from piped stdin. Outside the graphite lane ship opens the branch's PR when there is none (never with --fill, which would publish the commit's Claude-Session-Id trailer into the description) and edits exactly the fields this invocation restated when there is, so a description someone hand-edited survives a re-ship that does not mention it; a body given is replaced wholesale, never merged. On trunk it reports no PR (on trunk). In the graphite lane gt submit opens the PRs and ship restates the named branches afterwards, which is the only way a downstack PR gets a body at all. Every body file is read before the commit forms, so an unreadable path refuses with the working copy untouched, and a ship that names no PR flag makes no gh pr call. --draft and --publish apply in every lane, converting an existing PR in either direction; --no-pr skips the step. -m is optional when an unscoped --pr-title is given: the title becomes the commit subject, and an unscoped --pr-body-file its body, with the <details> wrapper dropped, each ## Heading folded into a Heading: paragraph, and blank runs collapsed. Only an unscoped value can feed it — the tip's name is the branch plan, which that message is an input to.

--yolo is the one switch for "skip the checks": it implies --no-verify, so ship's own prek pass never runs and the commit gt cuts carries --no-verify, and it drops every guard ship adds of its own, now and as more are added. It drops none today — the multi-branch gt submit --dry-run probe it was written for is gone, replaced by the branch list ship already holds — so against this version it is exactly --no-verify. It never drops a refusal git or gt would make anyway, and never the auto-restack, which is recovery rather than a guard.

--reviews keeps listening after the CI watch: each new review comment on the pushed branch's PR — every submitted PR, in the gt lane — streams to stdout until all are merged or closed. The standalone surface, with attach and replay knobs (--since, --interval, --budget, --stack), is ccx vcs reviews.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.paths = args
			return runShip(cmd, o)
		},
	}
	cmd.Flags().StringVarP(&o.message, "message", "m", "", "commit message")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "commit only; do not push or watch CI")
	cmd.Flags().BoolVar(&o.noCommit, "no-commit", false, "push and update the PR for the commit already in place; cut no commit, and refuse a dirty working copy")
	cmd.Flags().BoolVar(&o.noWatch, "no-watch", false, "push but do not watch CI")
	cmd.Flags().BoolVar(&o.noVerify, "no-verify", false, "skip pre-commit hooks (uvx prek) before committing")
	cmd.Flags().BoolVar(&o.yolo, "yolo", false, "skip every hook and ship-side guard: implies --no-verify, and drops any guard ship adds of its own")
	cmd.Flags().BoolVar(&o.amend, "amend", false, "fold the working copy into the parent commit")
	cmd.Flags().IntVar(&o.budget, "budget", shipLogBudget, "token budget for the CI failure log excerpt (0 = uncapped)")
	cmd.Flags().StringArrayVar(&o.skipHunks, "skip-hunk", nil, "commit everything except this hunk ref (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringArrayVar(&o.onlyHunks, "only-hunk", nil, "commit only this hunk ref in its file (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringVar(&o.branch, "branch", "", "commit onto this branch, creating it here when it does not exist")
	cmd.Flags().StringVar(&o.branch, "bookmark", "", "jj-only alias of --branch")
	cmd.Flags().StringVar(&o.newBranch, "new-branch", "", "start a new branch for this commit; bare --new-branch derives the name from the message, an explicit name must be spelled --new-branch=name")
	cmd.Flags().Lookup("new-branch").NoOptDefVal = branchNoOptDefVal
	cmd.Flags().StringVar(&o.newBranch, "create", "", "deprecated alias of --new-branch")
	cmd.Flags().Lookup("create").NoOptDefVal = branchNoOptDefVal
	cmd.Flags().BoolVar(&o.appendOnly, "append", false, "append the commit to the branch already checked out, refusing on trunk")
	cmd.Flags().StringVar(&o.parent, "parent", "", "parent of a new or newly tracked stacked branch (graphite lane only)")
	cmd.Flags().BoolVar(&o.allowTrunk, "allow-trunk", false, "let --branch advance the trunk of a repository you do not own")
	cmd.Flags().BoolVar(&o.draft, "draft", false, "open new PRs as drafts, and convert an existing one to a draft")
	cmd.Flags().BoolVar(&o.publish, "publish", false, "publish new PRs, and mark an existing draft ready (the default when neither is passed)")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	cmd.Flags().BoolVar(&o.reviews, "reviews", false, "after the CI watch, keep streaming new PR review comments until every submitted PR is merged or closed")
	cmd.Flags().StringArrayVar(&o.prTitle, "pr-title", nil, "set the pull request title; repeatable as <branch>=<title>, bare applies to the tip")
	cmd.Flags().StringArrayVar(&o.prBodyFile, "pr-body-file", nil, `set the pull request body from a file; repeatable as <branch>=<path>, bare applies to the tip ("-" reads stdin)`)
	cmd.Flags().BoolVar(&o.noPR, "no-pr", false, "push only; never create or update a pull request")
	for _, group := range [][]string{
		{"new-branch", "amend"},
		{"create", "amend"},
		{"branch", "new-branch"},
		{"branch", "create"},
		{"branch", "bookmark"},
		{"append", "branch"},
		{"append", "new-branch"},
		{"append", "create"},
		{"draft", "publish"},
		{"no-pr", "pr-title"},
		{"no-pr", "pr-body-file"},
		{"no-commit", "message"},
		{"no-commit", "amend"},
		{"no-commit", "no-push"},
		{"no-commit", "new-branch"},
		{"no-commit", "create"},
		{"no-commit", "append"},
		{"no-commit", "skip-hunk"},
		{"no-commit", "only-hunk"},
	} {
		cmd.MarkFlagsMutuallyExclusive(group...)
	}
	return cmd
}

func runShip(cmd *cobra.Command, o shipOpts) error {
	ctx := cmd.Context()
	if err := checkBranchFlags(cmd, o); err != nil {
		return err
	}
	l, err := resolveLane(ctx, "ship", workingDir(), o.noGT)
	if err != nil {
		return err
	}
	root, gtLane := l.root, l.gt
	kind := l.kind
	if gtLane {
		kind = vcs.Git
	}
	if !gtLane && o.parent != "" {
		return errors.New("ship: --parent applies only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop it")
	}
	if cmd.Flags().Changed("bookmark") {
		if gtLane {
			return errors.New("ship: --bookmark does not apply in the graphite lane; pass --no-gt to advance a jj bookmark instead")
		}
		if l.kind != vcs.JJ {
			return errors.New("ship: --bookmark applies only to jj repositories")
		}
	}
	if o.yolo {
		o.noVerify = true
	}
	asGiven := o
	prCleanup, err := materializePRBodyStdin(cmd, &o)
	defer prCleanup()
	if err != nil {
		return err
	}
	if !o.amend && !o.noCommit && o.message == "" {
		if o.message, err = shipMessageFromPR(o); err != nil {
			return err
		}
	}
	if o.noCommit && len(o.paths) > 0 {
		return errors.New("ship: --no-commit takes no paths — a path scopes a commit, and --no-commit cuts none")
	}
	if o.noCommit && o.branch != "" && kind != vcs.JJ {
		return errors.New("ship: --branch does not apply with --no-commit outside jj — git pushes the branch that is checked out")
	}
	if o.reviews && o.noPush {
		return errors.New("ship: --reviews requires push (drop --no-push)")
	}
	if o.noPush && (len(o.prTitle) > 0 || len(o.prBodyFile) > 0) {
		return errors.New("ship: --pr-title/--pr-body-file require push (drop --no-push)")
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
	plan, planSeg, err := shipResolvePlan(ctx, cmd.ErrOrStderr(), l, o)
	if err != nil {
		return err
	}
	prRun := shipPRRequested(cmd, l, o)
	meta, err := resolvePRMeta(cmd, o, plan.name)
	if err != nil {
		return err
	}
	var prNWO string
	if prRun && !o.noPush {
		if prNWO, err = shipPRRepo(ctx, l, plan); err != nil {
			return err
		}
	}
	if o.noCommit {
		if err := shipRefuseDirty(ctx, root, kind, o); err != nil {
			return err
		}
	} else if kind == vcs.JJ && sel == nil && !o.amend {
		if err := shipRefuseEmptyJJ(ctx, root, o, plan); err != nil {
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
	if !o.noCommit {
		if gtLane {
			hookSeg, err = shipCommitGT(ctx, cmd.ErrOrStderr(), root, o, sel, plan)
		} else {
			hookSeg, err = shipCommitLocal(ctx, cmd.ErrOrStderr(), root, kind, o, sel, plan)
		}
		if err != nil {
			return err
		}
		if kind == vcs.JJ && plan.action == branchCreate {
			if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "create", plan.name, "-r", "@-"}); err != nil {
				return fmt.Errorf("ship: jj bookmark create %s: %w", plan.name, err)
			}
		}
	}

	branch, healSeg, err := shipBranchAfterCommit(ctx, l.checkout, kind, plan)
	if err != nil {
		return err
	}

	resume := gtResumeCmd(asGiven)
	restackSeg := ""
	if plan.needsRestack {
		if restackSeg, err = gtRestack(ctx, cmd.ErrOrStderr(), l, resume, branch); err != nil {
			return err
		}
	}

	short, subject, err := shipDescribe(ctx, kind)
	if err != nil {
		return err
	}
	segments := make([]string, 0, 8)
	if l.note != "" {
		segments = append(segments, fmt.Sprintf("lane %s (%s)", kindLabel(l.kind), l.note))
	}
	for _, seg := range []string{planSeg, hookSeg} {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	committedSegment := len(segments)
	segments = append(segments, shipCommitSegment(o.noCommit, short, subject))
	if seg := branchSegment(plan, branch, o.noPush); seg != "" {
		segments = append(segments, seg)
	}
	if healSeg != "" {
		segments = append(segments, healSeg)
	}
	if restackSeg != "" {
		segments = append(segments, restackSeg)
	}

	if o.noPush {
		if kind == vcs.JJ && plan.action != branchCreate && branch != "" {
			if err := jjMoveBookmark(ctx, branch); err != nil {
				return err
			}
		}
		segments = append(segments, "not pushed")
		cmd.Println(strings.Join(segments, shipSep))
		return nil
	}

	var remote string
	var rebased int
	var prSeg string
	var bodylessSegs []string
	var gtStack []stackEntry
	if gtLane {
		prSeg, bodylessSegs, gtStack, err = shipPushGT(ctx, cmd.ErrOrStderr(), l, o, meta, branch, resume)
	} else {
		remote, rebased, err = shipPush(ctx, kind, o, branch, preAmendSHA)
	}
	if err != nil {
		return err
	}
	if rebased > 0 {
		short, subject, err = shipDescribe(ctx, kind)
		if err != nil {
			return err
		}
		segments[committedSegment] = shipCommitSegment(o.noCommit, short, subject)
		segments = append(segments, fmt.Sprintf("rebased %d commit(s) onto %s", rebased, branch))
	}
	if gtLane {
		segments = append(segments, prSeg)
	} else {
		segments = append(segments, fmt.Sprintf("pushed %s → %s", branch, remote))
	}

	if prRun {
		seg, err := shipPR(ctx, l, prNWO, branch, plan.trunk, subject, meta, gtStack)
		if err != nil {
			return err
		}
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	segments = append(segments, bodylessSegs...)

	var reviewBranches []string
	if o.reviews {
		if gtLane {
			reviewBranches, err = stackBranches(ctx, "ship")
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

// shipCommitLocal commits on the lanes ship drives itself — jj and plain git —
// cutting the git lane's new branch first. Anything failing after that checkout
// switches back and deletes the branch, so a refusal leaves the working copy
// where it started. Deferred, so a new failure path inherits the rollback, and
// scoped here, so the commit landing disarms it.
func shipCommitLocal(ctx context.Context, errW io.Writer, root string, kind vcs.Kind, o shipOpts, sel *shipSelection, plan branchPlan) (seg string, err error) {
	if kind == vcs.Git && plan.action == branchCreate {
		if _, serr := render.RunCLI(ctx, "git", []string{"switch", "-c", plan.name}); serr != nil {
			return "", fmt.Errorf("ship: git switch -c %s: %w", plan.name, serr)
		}
		defer func() {
			if err != nil {
				err = errors.Join(err, shipRestoreBranch(ctx, plan.from, plan.name))
			}
		}()
	}
	return shipCommit(ctx, errW, root, kind, o, sel)
}

// shipRestoreBranch puts the working copy back on from and deletes created. -D
// is not a force: git switch -c refuses an existing name, so created is one this
// run cut, with no commit on it. A step that fails names what it left behind.
func shipRestoreBranch(ctx context.Context, from, created string) error {
	if _, err := render.RunCLI(ctx, "git", []string{"switch", from}); err != nil {
		return fmt.Errorf("ship: rollback: git switch %s: %w — the working copy is left on %s", from, err, created)
	}
	if _, err := render.RunCLI(ctx, "git", []string{"branch", "-D", created}); err != nil {
		return fmt.Errorf("ship: rollback: git branch -D %s: %w — the branch ship cut is left behind", created, err)
	}
	return nil
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
		seg, o.hooksRan, err = shipRunHooks(ctx, errW, root, kind, o)
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
	if o.noVerify || o.hooksRan {
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
		out, rerr := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate})
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

// shipCommitSegment names the commit ship is shipping: one it just cut, or,
// under --no-commit, one that was already in place.
func shipCommitSegment(noCommit bool, short, subject string) string {
	if noCommit {
		return fmt.Sprintf("already committed %s %q", short, subject)
	}
	return fmt.Sprintf("committed %s %q", short, subject)
}

// shipRefuseDirty refuses --no-commit over a working copy that still holds
// changes. The mode pushes a commit that is already in place, so uncommitted
// work would be left out of a branch and a pull request this same run updates,
// and the report that could name it prints only once both have happened.
func shipRefuseDirty(ctx context.Context, root string, kind vcs.Kind, o shipOpts) error {
	var items []string
	if kind == vcs.JJ {
		paths, err := shipChangedPaths(ctx, root, vcs.JJ, o)
		if err != nil {
			return err
		}
		items = paths
	} else {
		out, err := render.RunCLIDir(ctx, root, "git", []string{"status", "--porcelain"})
		if err != nil {
			return fmt.Errorf("ship: git status: %w", err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) != "" {
				items = append(items, strings.TrimSpace(line))
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	return fmt.Errorf("ship: --no-commit needs a clean working copy — uncommitted: %s; drop --no-commit to ship that work, or stash it", strings.Join(items, ", "))
}

func shipRefuseEmptyJJ(ctx context.Context, root string, o shipOpts, plan branchPlan) error {
	paths, err := shipChangedPaths(ctx, root, vcs.JJ, o)
	if err != nil {
		return err
	}
	if len(paths) > 0 {
		return nil
	}
	state, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate})
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
	if plan.name != "" {
		pat := shellSingleQuote(vcs.JJExactPattern(plan.name))
		hint = fmt.Sprintf(" push it: jj bookmark move %s --to @- && jj git push --bookmark %s", pat, pat)
	}
	return fmt.Errorf("ship: nothing to commit%s — did a prior ship already land %s %q?%s", scope, short, subject, hint)
}

// checkBranchFlags validates the branch-intent flags before any repository read.
// A bare --new-branch never consumes the next token (cobra's NoOptDefVal), and
// ship's ArbitraryArgs then files it as a path to commit, so "--new-branch docs"
// would silently commit only docs/; a positional that is not on disk is refused
// instead.
//
// An explicit name skips deriveBranchName's legality check, so it runs here.
func checkBranchFlags(cmd *cobra.Command, o shipOpts) error {
	for _, name := range []string{"new-branch", "create"} {
		if cmd.Flags().Changed(name) && o.newBranch == "" {
			return fmt.Errorf("ship: --%s requires a branch name or no value", name)
		}
	}
	if o.branch != "" && !legalBranchName(o.branch) {
		return fmt.Errorf("ship: --branch %q is not a legal branch name", o.branch)
	}
	if o.newBranch != "" && o.newBranch != branchNoOptDefVal && !legalBranchName(o.newBranch) {
		return fmt.Errorf("ship: --new-branch %q is not a legal branch name", o.newBranch)
	}
	if o.newBranch != branchNoOptDefVal {
		return nil
	}
	for _, path := range o.paths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("ship: %q is not a path — did you mean --new-branch=%s?", path, path)
		}
	}
	return nil
}

// shipResolvePlan resolves where the commit goes, before any mutation, on
// whichever lane is live. The second return is the preflight's own report
// segment, which today only the graphite lane's auto-track produces. errW is the
// graphite preflight's reporting channel: an auto-track gt declined says so in
// gt's own words, which ship's recovery step would otherwise replace.
func shipResolvePlan(ctx context.Context, errW io.Writer, l lane, o shipOpts) (branchPlan, string, error) {
	if l.gt {
		return shipPreflightGT(ctx, errW, l, o)
	}
	switch l.kind {
	case vcs.JJ:
		return shipPreflightJJ(ctx, l, o)
	case vcs.Git:
		plan, err := shipPreflightGitLane(ctx, l, o)
		return plan, "", err
	default:
		return branchPlan{}, "", errors.New("ship: unsupported vcs")
	}
}

func shipPreflightJJ(ctx context.Context, l lane, o shipOpts) (branchPlan, string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx, "ship")
	if err != nil {
		return branchPlan{}, "", err
	}
	trunk := ""
	switch {
	case len(trunkNames) == 1:
		trunk = trunkNames[0]
	// A --branch naming one of the candidates is the caller disambiguating, so it
	// resolves trunk rather than only silencing the refusal: every trunk guard
	// downstream compares against this name.
	case len(trunkNames) > 1 && slices.Contains(trunkNames, o.branch):
		trunk = o.branch
	case len(trunkNames) > 1:
		return branchPlan{}, "", fmt.Errorf("ship: cannot resolve the trunk bookmark from %q; pass --branch naming one of them", trunkNames)
	}

	names, err := jjBookmarkNames(ctx, "ship", jjNearestBookmarkRevset)
	if err != nil {
		return branchPlan{}, "", err
	}
	unheld, beat := "", ""
	if len(names) > 1 && o.newBranch == "" && !slices.Contains(names, o.branch) {
		if unheld, beat, err = shipUnheldCandidate(ctx, l.checkout, names); err != nil {
			return branchPlan{}, "", err
		}
	}
	current, seg := trunk, ""
	switch {
	case len(names) == 1:
		current = names[0]
	case len(names) > 1 && slices.Contains(names, o.branch):
		current = o.branch
	// --new-branch names the branch outright, so a tie among the nearest
	// bookmarks settles nothing ship goes on to use: refusing over it would
	// demand the caller disambiguate a branch they already said to leave.
	case len(names) > 1 && o.newBranch != "":
	case unheld != "":
		current = unheld
		seg = fmt.Sprintf("bookmark %s (chosen over %s)", unheld, beat)
	case len(names) > 1 && slices.Contains(names, trunk):
		others := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == trunk })
		current = trunk
		seg = fmt.Sprintf("bookmark %s (trunk, chosen over %s)", trunk, strings.Join(others, ", "))
	case len(names) > 1:
		aside := "no trunk bookmark resolves here"
		if trunk != "" {
			aside = fmt.Sprintf("trunk %s is not among them", trunk)
		}
		return branchPlan{}, "", fmt.Errorf("ship: multiple nearest bookmarks %s (%s); pass --branch <name> to choose one", strings.Join(names, ", "), aside)
	}

	// A --branch naming a bookmark that already exists is one ship appends to,
	// wherever it sits: moving a jj bookmark strands no working copy the way a
	// git checkout would.
	probed, explicitHeads := false, 0
	if o.branch != "" {
		explicitHeads, err = jjBookmarkHeads(ctx, o.branch)
		if err != nil {
			return branchPlan{}, "", err
		}
		probed = true
		if explicitHeads > 0 {
			current, seg = o.branch, ""
		}
	}

	repo, err := shipTrunkRepo(ctx, l, o, current, trunk)
	if err != nil {
		return branchPlan{}, "", err
	}
	plan, err := resolveBranchPlan(l, repo, o, current, trunk)
	if err != nil {
		return branchPlan{}, "", err
	}
	// jj bookmark move exits 0 on a bookmark that does not exist.
	if plan.action == branchAppend && probed && plan.name == o.branch && explicitHeads == 0 {
		return branchPlan{}, "", fmt.Errorf("ship: bookmark %q not found", o.branch)
	}
	// Only a push needs the target to resolve; a --no-push ship in a repository
	// with no bookmark at all still commits, exactly as it did before.
	if o.noPush || plan.action == branchCreate {
		return plan, seg, nil
	}
	if plan.name == "" {
		return branchPlan{}, "", fmt.Errorf("ship: cannot resolve the trunk bookmark from %q; pass --branch <name>", trunkNames)
	}
	if !probed || plan.name != o.branch {
		heads, err := jjBookmarkHeads(ctx, plan.name)
		if err != nil {
			return branchPlan{}, "", err
		}
		if heads == 0 {
			return branchPlan{}, "", fmt.Errorf("ship: bookmark %q not found", plan.name)
		}
	}
	return plan, seg, nil
}

// shipUnheldCandidate breaks a tie between nearest bookmarks by who has them
// checked out: a candidate another working copy holds is that copy's bookmark,
// not an equal alternative for this one. It answers only when exactly one
// candidate is left standing, because holder data is not total — git names a
// holder only while some checkout holds the branch, and a colocated jj working
// copy is detached, so most refs report none and the trunk alias still decides
// the tie. The second return names the candidates the answer beat; a jj
// repository with no git backing shares no refs and so has no holders at all.
func shipUnheldCandidate(ctx context.Context, ck vcs.Checkout, names []string) (string, string, error) {
	if ck.CommonDir == "" {
		return "", "", nil
	}
	holders, err := vcs.BranchHolders(ctx, ck)
	if err != nil {
		return "", "", fmt.Errorf("ship: %w", err)
	}
	var kept, held []string
	for _, name := range names {
		if holder := holders[name]; holder != "" && holder != ck.Root {
			held = append(held, name+" held in "+holder)
			continue
		}
		kept = append(kept, name)
	}
	if len(kept) != 1 {
		return "", "", nil
	}
	return kept[0], strings.Join(held, ", "), nil
}

// jjBookmarkHeads counts the commits name resolves to. jj treats a bare NAMES
// argument as a glob and no-ops with exit 0 on zero matches, and a conflicted
// bookmark resolves to several commits, which rebase would silently treat as a
// merge destination; resolving the exact name up front makes both fail loudly.
// Zero heads is a count, not an error — it is how ship tells "create it" from
// "append to it" — so only a conflicted bookmark refuses here.
func jjBookmarkHeads(ctx context.Context, name string) (int, error) {
	heads, err := jjLogLines(ctx, "ship", jjBookmarksRevset(name))
	if err != nil {
		return 0, err
	}
	if len(heads) > 1 {
		return 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", name, len(heads))
	}
	return len(heads), nil
}

func shipPreflightGitLane(ctx context.Context, l lane, o shipOpts) (branchPlan, error) {
	current, err := gitCurrentBranch(ctx, "ship")
	if err != nil {
		return branchPlan{}, err
	}
	trunk, err := gitTrunkBranch(ctx)
	if err != nil {
		return branchPlan{}, err
	}
	repo, err := shipTrunkRepo(ctx, l, o, current, trunk)
	if err != nil {
		return branchPlan{}, err
	}
	plan, err := resolveBranchPlan(l, repo, o, current, trunk)
	if err != nil {
		return branchPlan{}, err
	}
	return plan, refuseExistingBranch(ctx, o, plan)
}

// refuseExistingBranch refuses a --branch that names an existing branch other
// than the one checked out: reaching it would mean checking it out mid-ship,
// which strands the working copy's changes on the branch being left.
func refuseExistingBranch(ctx context.Context, o shipOpts, plan branchPlan) error {
	if o.branch == "" || plan.action != branchCreate {
		return nil
	}
	exists, err := gitRefExists(ctx, "refs/heads/"+plan.name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("ship: branch %s already exists — check it out first; ship does not switch branches mid-commit", plan.name)
	}
	return nil
}

// gitCurrentBranch names the checked-out branch, empty on a detached HEAD.
func gitCurrentBranch(ctx context.Context, prefix string) (string, error) {
	out, err := render.RunCLI(ctx, "git", []string{"branch", "--show-current"})
	if err != nil {
		return "", fmt.Errorf("%s: git branch --show-current: %w", prefix, err)
	}
	return strings.TrimSpace(out), nil
}

// gitTrunkBranch reads the remote's default branch, empty when origin/HEAD does
// not resolve — a local-only repository has no trunk, which reads as "not on
// trunk" and leaves ship appending where it stands. That empty answer is the
// unresolved case alone: origin/HEAD may be pointed at any ref, and
// `git symbolic-ref --short refs/remotes/origin/HEAD` prints a tag as readily as
// a branch at exit 0, so a target outside origin/ is an error the way
// vcs.ResolveTrunk already treats it — reporting "" there would hand ship a tag
// to ship onto, or drop a trunk git named.
func gitTrunkBranch(ctx context.Context) (string, error) {
	ref := "refs/remotes/origin/HEAD"
	out, code, _, err := render.RunCLIExitCode(ctx, "git", []string{"symbolic-ref", "--short", ref})
	if err != nil {
		return "", fmt.Errorf("ship: git symbolic-ref %s: %w", ref, err)
	}
	if code != 0 {
		return "", nil
	}
	target := strings.TrimSpace(out)
	trunk, ok := strings.CutPrefix(target, "origin/")
	if !ok || trunk == "" {
		return "", fmt.Errorf("ship: %s points at %q, which names no branch of origin — run git remote set-head origin -a", ref, target)
	}
	return trunk, nil
}

// shipTrunkRepo looks the repository up only when the decision turns on who
// owns it — a ship that already sits off trunk never touches GitHub. An
// unanswerable lookup returns the zero Repo, which resolveBranchPlan reads as
// the viewer's own, the same way the lane gate only ever demotes on a positive
// answer.
func shipTrunkRepo(ctx context.Context, l lane, o shipOpts, current, trunk string) (vcs.Repo, error) {
	if trunk == "" || (current != trunk && o.branch != trunk) {
		return vcs.Repo{}, nil
	}
	repo, err := vcs.LookupRepo(ctx, l.root, false)
	if errors.Is(err, vcs.ErrNoGitHub) {
		return vcs.Repo{}, nil
	}
	if err != nil {
		return vcs.Repo{}, fmt.Errorf("ship: %w", err)
	}
	return repo, nil
}

// shipBranchAfterCommit re-reads the branch the commit actually landed on and
// self-heals a detached HEAD: a gt-lane commit in a linked worktree has twice
// left HEAD detached with the branch ref lagging the true tip, and the recovery
// was git checkout -B <branch> <sha> after reflog archaeology. jj has no
// checkout to lose, so its target is the plan's.
func shipBranchAfterCommit(ctx context.Context, ck vcs.Checkout, kind vcs.Kind, plan branchPlan) (string, string, error) {
	if kind != vcs.Git {
		return plan.name, "", nil
	}
	branch, err := gitCurrentBranch(ctx, "ship")
	if err != nil {
		return "", "", err
	}
	if branch != "" {
		return branch, "", nil
	}
	out, err := render.RunCLI(ctx, "git", []string{"rev-parse", "HEAD"})
	if err != nil {
		return "", "", fmt.Errorf("ship: git rev-parse HEAD: %w", err)
	}
	sha := strings.TrimSpace(out)
	if _, err := render.RunCLI(ctx, "git", []string{"checkout", "-B", plan.name, sha}); err != nil {
		return "", "", shipHealRefused(ctx, ck, plan.name, sha, err)
	}
	return plan.name, "healed detached HEAD onto " + plan.name, nil
}

// shipHealRefused names the sibling working copy holding the branch a heal
// could not take — the one reason git refuses the checkout that nothing about
// this working copy's own state explains. A holder lookup that fails rides
// along rather than replacing the failure it was asked to explain.
func shipHealRefused(ctx context.Context, ck vcs.Checkout, branch, sha string, cause error) error {
	refused := fmt.Errorf("ship: HEAD is detached at %s and git checkout -B %s failed: %w", sha, branch, cause)
	holders, err := vcs.BranchHolders(ctx, ck)
	if err != nil {
		return errors.Join(refused, err)
	}
	holder := holders[branch]
	if holder == "" || holder == ck.Root {
		return refused
	}
	return fmt.Errorf("ship: HEAD is detached at %s and git checkout -B %s failed — that branch is checked out in %s: %w", sha, branch, holder, cause)
}

// branchSegment reports the branch a commit landed on. A created branch is
// always named; an appended one only when nothing downstream will name it,
// which is every ship that does not push.
func branchSegment(plan branchPlan, branch string, noPush bool) string {
	switch {
	case branch == "":
		return ""
	case plan.action == branchCreate:
		return "created " + branch
	case noPush:
		return "branch " + branch
	default:
		return ""
	}
}

func shipPush(ctx context.Context, kind vcs.Kind, o shipOpts, target, preAmendSHA string) (remote string, rebased int, err error) {
	switch kind {
	case vcs.JJ:
		rebased, err = shipPushJJ(ctx, target, o.amend)
		return "origin", rebased, err
	case vcs.Git:
		return shipPushGit(ctx, o.amend, target, preAmendSHA)
	default:
		return "", 0, errors.New("ship: push: unsupported vcs")
	}
}

func shipPushJJ(ctx context.Context, target string, amend bool) (int, error) {
	if err := jjTrackUntrackedTarget(ctx, target); err != nil {
		return 0, err
	}
	pat := shellSingleQuote(vcs.JJExactPattern(target))
	hint := fmt.Sprintf("jj git fetch && jj rebase -b @- --destination %s && jj bookmark move %s --to @- && jj git push --bookmark %s", shellSingleQuote(jjBookmarksRevset(target)), pat, pat)
	return shipPushRetry(ctx, target, hint, func(ctx context.Context) (int, error) {
		return shipPushJJOnce(ctx, target, amend)
	})
}

// jjMoveBookmark advances target onto the commit ship just made: a jj bookmark
// does not follow jj commit or jj squash the way a git branch ref follows git
// commit, so every lane that leaves a commit behind moves it explicitly.
func jjMoveBookmark(ctx context.Context, target string) error {
	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "move", vcs.JJExactPattern(target), "--to", "@-"}); err != nil {
		return fmt.Errorf("ship: advance bookmark %q: %w", target, err)
	}
	return nil
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
	if _, err := render.RunCLI(ctx, "jj", []string{"bookmark", "track", vcs.JJExactPattern(target), "--remote=" + remote}); err != nil {
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
// Deliberately runs without --ignore-working-copy: that flag suppresses jj's
// implicit git import, and a counterpart fetched git-side would then go unseen,
// leaving jj git push to refuse the bookmark it was this call's job to track.
func jjUntrackedTargetRemotes(ctx context.Context, target string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"bookmark", "list", vcs.JJExactPattern(target), "--all-remotes", "-T", jjRemoteBookmarkTemplate})
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
	out, code, stderr, err := render.RunCLIExitCode(ctx, "jj", []string{"--ignore-working-copy", "config", "get", "git.push"})
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

// jjBookmarksRevset resolves name to the commit its bookmark points at, matching
// the name exactly rather than as the glob a bare argument would be. It spells
// the pattern through vcs.JJExactPattern rather than Go's %q, whose \a, \b, \f,
// \v, \u, and \U escapes jj's revset grammar rejects outright: a bookmark
// carrying a character Go considers unprintable but git allows in a ref — a
// non-breaking space, a zero-width joiner — would render as a revset jj 0.43
// cannot parse.
func jjBookmarksRevset(name string) string {
	return `bookmarks(` + vcs.JJExactPattern(name) + `)`
}

// jjAncestorRevset selects name's bookmark when it already sits under @-.
func jjAncestorRevset(name string) string {
	return jjBookmarksRevset(name) + " & ::@-"
}

// jjStackRevset selects the commits @- carries above name's bookmark.
func jjStackRevset(name string) string {
	return jjBookmarksRevset(name) + "..@-"
}

// jjConflictRevset selects the conflicted commits in and above that stack.
func jjConflictRevset(name string) string {
	return "conflicts() & (" + jjStackRevset(name) + ")::"
}

// shellSingleQuote renders s as one shell word, so a recovery hint the operator
// pastes reaches jj carrying the argument ship itself would have passed. A jj
// pattern spells its own quotes, and a shell eats them: pasted bare,
// exact:"foo@bar" arrives as exact:foo@bar, which jj refuses to parse as a
// pattern at all.
func shellSingleQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// shipPushJJOnce is one push attempt: fetch, re-check the bookmark, rebase when
// the target diverged, advance the bookmark, then push. It snapshots the op log
// right after the bookmark move so a rejected push can undo exactly that move.
func shipPushJJOnce(ctx context.Context, target string, amend bool) (int, error) {
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "fetch"}); err != nil {
		return 0, fmt.Errorf("ship: jj git fetch: %w", err)
	}

	heads, err := jjLogLines(ctx, "ship", jjBookmarksRevset(target))
	if err != nil {
		return 0, err
	}
	switch {
	case len(heads) == 0:
		return 0, fmt.Errorf("ship: bookmark %q not found", target)
	case len(heads) > 1:
		return 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", target, len(heads))
	}

	ancestors, err := jjBookmarkNames(ctx, "ship", jjAncestorRevset(target))
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

	if err := jjMoveBookmark(ctx, target); err != nil {
		return 0, err
	}
	moveOp, err := jjOpID(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := render.RunCLI(ctx, "jj", []string{"git", "push", "--bookmark", vcs.JJExactPattern(target)}); err != nil {
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

func shipPushGit(ctx context.Context, amend bool, branch, preAmendSHA string) (string, int, error) {
	remote, err := gitRemoteFor(ctx, "ship", branch)
	if err != nil {
		return "", 0, err
	}
	if amend {
		return remote, 0, shipPushGitAmend(ctx, remote, branch, preAmendSHA)
	}
	hint := fmt.Sprintf("git fetch %s && git rebase --autostash %s/%s && git push %s %s", remote, remote, branch, remote, branch)
	rebased, err := shipPushRetry(ctx, branch, hint, func(ctx context.Context) (int, error) {
		return shipPushGitOnce(ctx, remote, branch)
	})
	return remote, rebased, err
}

// gitRemoteFor resolves the remote that branch.<branch>.remote configures, so a
// triangular or non-origin-only repo fetches, rebases, and pushes against the
// same remote. git config --get exits 1 when unset; that and an empty value both
// default to origin. Any other exit is an error, prefixed with the command that
// asked — ship, restack, and info all do.
func gitRemoteFor(ctx context.Context, prefix, branch string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"config", "--get", "branch." + branch + ".remote"})
	if err != nil {
		return "", fmt.Errorf("%s: git config branch.%s.remote: %w", prefix, branch, err)
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
		return "", fmt.Errorf("%s: git config branch.%s.remote: exit %d: %s", prefix, branch, code, strings.TrimSpace(stderr))
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
		ancestor, err := gitIsAncestor(ctx, "ship", remoteRef, "HEAD")
		if err != nil {
			return 0, err
		}
		if !ancestor {
			rebased, err = gitRebaseOnto(ctx, "ship", remote, branch)
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
func gitIsAncestor(ctx context.Context, prefix, maybe, ref string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, "git", []string{"merge-base", "--is-ancestor", maybe, ref})
	if err != nil {
		return false, fmt.Errorf("%s: git merge-base --is-ancestor: %w", prefix, err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("%s: git merge-base --is-ancestor: exit %d: %s", prefix, code, strings.TrimSpace(stderr))
	}
}

// gitRebaseOnto rebases HEAD onto <remote>/<branch> with --autostash (the worktree
// is dirty after a hunk-scoped ship), returning the number of local commits
// replayed. A failed rebase is classified by gitRebaseFailure; an autostash pop
// left unapplied is surfaced as a warning, not a failure.
func gitRebaseOnto(ctx context.Context, prefix, remote, branch string) (int, error) {
	remoteRef := "refs/remotes/" + remote + "/" + branch
	countOut, err := render.RunCLI(ctx, "git", []string{"rev-list", "--count", remoteRef + "..HEAD"})
	if err != nil {
		return 0, fmt.Errorf(prefix+": git rev-list --count: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		return 0, fmt.Errorf(prefix+": malformed rev-list count %q: %w", countOut, err)
	}

	_, stderr, err := render.RunCLIKeepStderr(ctx, "git", []string{"rebase", "--autostash", remoteRef})
	if err != nil {
		return 0, gitRebaseFailure(ctx, prefix, remote, branch, err)
	}
	if strings.Contains(stderr, "resulted in conflicts") {
		slog.Warn(prefix+": rebase left autostashed changes unapplied — recover them with git stash pop", "branch", branch)
	}
	return count, nil
}

// gitRebaseFailure classifies a failed git rebase --autostash. A rebase in progress
// (REBASE_HEAD resolves) conflicted mid-replay: list, abort (restoring the
// autostash), report. Otherwise it failed before starting (hook, dirty index) —
// return the raw error, no abort. Cleanup runs uncancellable.
func gitRebaseFailure(ctx context.Context, prefix, remote, branch string, rebaseErr error) error {
	cleanup := context.WithoutCancel(ctx)
	inProgress, err := gitRefExists(cleanup, "REBASE_HEAD")
	if err != nil {
		return err
	}
	if !inProgress {
		return fmt.Errorf(prefix+": git rebase onto %s/%s: %w", remote, branch, rebaseErr)
	}
	files, lerr := render.RunCLI(cleanup, "git", []string{"diff", "--name-only", "--diff-filter=U"})
	if _, aerr := render.RunCLI(cleanup, "git", []string{"rebase", "--abort"}); aerr != nil {
		return fmt.Errorf(prefix+": rebase onto %s/%s conflicted (%w) and abort failed: %w — run: git rebase --abort, then resolve manually", remote, branch, rebaseErr, aerr)
	}
	if lerr != nil {
		return fmt.Errorf(prefix+": rebase onto %s/%s conflicted (%w); aborted back to the pre-rebase state; listing the conflicted files also failed: %w — resolve manually: git fetch %s && git rebase --autostash %s/%s, fix the conflicts (git status), then git push %s %s", remote, branch, rebaseErr, lerr, remote, remote, branch, remote, branch)
	}
	conflicted := strings.Join(strings.Fields(files), ", ")
	return fmt.Errorf(prefix+": rebase onto %s/%s conflicts in: %s; aborted back to the pre-rebase state (%w) — resolve manually: git fetch %s && git rebase --autostash %s/%s, fix the conflicts (git status), then git push %s %s", remote, branch, conflicted, rebaseErr, remote, remote, branch, remote, branch)
}

func jjBookmarkNames(ctx context.Context, prefix, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", jjBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("%s: jj bookmarks at %q: %w", prefix, rev, err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		var name string
		if err := json.Unmarshal([]byte(line), &name); err != nil {
			return nil, fmt.Errorf("%s: malformed jj bookmark name %q at %q: %w", prefix, line, rev, err)
		}
		names = append(names, name)
	}
	return names, nil
}

func jjTrunkBookmarkNames(ctx context.Context, prefix string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate})
	if err != nil {
		return nil, fmt.Errorf("%s: jj trunk bookmark: %w", prefix, err)
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		var name string
		if err := json.Unmarshal([]byte(line), &name); err != nil {
			return nil, fmt.Errorf("%s: malformed jj trunk bookmark name %q: %w", prefix, line, err)
		}
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	return names, nil
}

func jjLogLines(ctx context.Context, prefix, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", jjStackLineTemplate})
	if err != nil {
		return nil, fmt.Errorf("%s: jj log %q: %w", prefix, rev, err)
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
	out, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate})
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
	stack, err := jjLogLines(ctx, "ship", jjStackRevset(target))
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("ship: %q..@- is empty — the commit already landed on %q; refusing to move the bookmark backwards", target, target)
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"rebase", "-b", "@-", "--destination", jjBookmarksRevset(target)}); err != nil {
		return 0, fmt.Errorf("ship: jj rebase onto %q: %w", target, err)
	}
	rebaseOp, err := jjOpID(ctx)
	if err != nil {
		return 0, err
	}

	// rebase -b @- rewrites every descendant of the stack, including siblings
	// of @; check the whole rewritten set without including conflicts below it.
	conflicts, err := jjLogLines(ctx, "ship", jjConflictRevset(target))
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
		pat := shellSingleQuote(vcs.JJExactPattern(target))
		return 0, fmt.Errorf("ship: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @- --destination %s, fix the conflicts (jj status), then: jj bookmark move %s --to @- && jj git push --bookmark %s", target, len(conflicts), strings.Join(conflicts, "\n  "), shellSingleQuote(jjBookmarksRevset(target)), pat, pat)
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
// real terminal and otherwise buffering it away. The wait is bounded by
// shipCIWatchTimeout, an explicit deadline render's generic guard defers to.
func watchCIRun(ctx context.Context, errW io.Writer, id string) error {
	watchCtx, cancel := context.WithTimeout(ctx, shipCIWatchTimeout)
	defer cancel()
	if shipStreamCI(errW) {
		return render.RunCLIStream(watchCtx, "gh", []string{"run", "watch", id, "--exit-status", "--compact"}, errW)
	}
	_, err := render.RunCLI(watchCtx, "gh", []string{"run", "watch", id, "--exit-status"})
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
		out, err := render.RunCLI(ctx, "jj", []string{"--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id"})
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
