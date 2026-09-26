package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	shipSweptNamed   = 5
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

// errShipAlreadyCommitted is the empty-commit refusals' other answer: the work
// this ship would have committed is already committed, so ship submits the
// branch as it stands instead of refusing. Only runShip reads it, which is
// where the switch to --no-commit's path is made.
var errShipAlreadyCommitted = errors.New("ship: the branch already carries its commits")

func shipLandedSegment(o shipOpts) string {
	return "nothing to commit" + shipScope(o) + " — shipping as --no-commit"
}

func shipScope(o shipOpts) string {
	if len(o.paths) == 0 {
		return ""
	}
	return " in " + strings.Join(o.paths, ", ")
}

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
	// messages is every -m the caller passed, in order; message is the commit
	// message they make, one blank line between them, joined once in runShip
	// before anything downstream reads it.
	messages []string
	message  string

	noPush       bool
	noCommit     bool
	noWatch      bool
	expectRemote string

	// noVerify carries --no-verify until runShip resolves it against --verify and
	// the branch plan, after which it is the run's whole answer to "run the
	// repository's hooks". hooksRan is a separate fact: ccx ran prek itself, so
	// the commit verb is told not to run the same suite again.
	noVerify bool
	verify   bool
	hooksRan bool

	yolo   bool
	amend  bool
	dryRun bool
	budget int
	// paths is the caller's own spelling, cwd-relative; rootPaths is the same
	// set rebased onto the repository root, which is where every child runs.
	paths     []string
	rootPaths []string
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

	landed      []string
	tipOnly     bool
	dropCommits bool
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

Ship refuses an empty working copy only when the branch carries nothing above trunk either. Where it does carry commits trunk does not — work a delegate's worktree, a hand-made commit, or a codex lane already landed — there is nothing to cut and everything to submit, so ship skips the commit and goes on to push and the pull request, reporting "nothing to commit — shipping as --no-commit". A path-scoped ship whose scoped paths are clean takes that same path, naming them: scoping already declared everything else out, so finding them clean over a branch ahead of trunk means the work landed. --no-commit states the path outright and refuses a working copy holding changes to tracked files, which would otherwise be left out of the branch and the pull request this same run updates; git's untracked paths do not refuse it, since a run that cuts no commit was never going to carry a worktree's scratch into one, and the report names them as "left untracked: <paths>" instead. Under jj there is nothing to exempt: a new file is already part of the working-copy commit --no-commit pushes. --no-commit is refused alongside paths, where the two spellings contradict each other. Where the branch carries nothing above trunk there is nothing to submit either, and the refusal says so. Ship resolves the push target before committing, so a refusal leaves the working copy untouched. After committing, ship fetches from the remote first and, when the target is no longer an ancestor of the local stack, rebases the stack onto it (jj: the target bookmark; git: origin/<branch>); a rebase that would conflict is rolled back and reported instead of pushed. Uncommitted work the rebase has to move — every hunk a scoped ship deliberately left in the tree — is kept as a commit of its own rather than on refs/stash, which every working copy of a repository shares, and put back afterwards; work that will not go back leaves the commit holding it and the files in it named in the refusal. A push the remote rejects because it advanced again mid-ship re-fetches, re-rebases, and retries up to 3 attempts before failing with the manual recovery steps. --amend never retries a rejected push: the force-with-lease refusal is reported for manual reconciliation instead of overwriting the concurrent push.

Where the commit goes is one decision, resolved before any mutation and reported as a branch <name> or created <name> segment. On a non-trunk branch or bookmark, ship appends to it. On trunk it appends in your own repositories — direct-to-main is deliberate there — and starts a branch named from the commit subject when GitHub says the repository is someone else's, since an org trunk rejects the commit through its protect-<trunk> hook and leaves it dangling; the graphite lane always starts a branch on trunk, because gt has no verb that commits onto it. A detached HEAD is refused rather than guessed at, unless --new-branch names a branch to cut there, and so are several trunk candidates unless --branch names one of them. --branch <name> commits onto that branch, creating it here when it does not exist and refusing when it exists somewhere else, since ship does not check branches out; --new-branch[=<name>] always starts one, deriving the name from the commit subject when bare (an explicit name must be spelled --new-branch=name, because cobra parses "--new-branch name" as a path operand to commit); --append refuses on trunk; --allow-trunk lets --branch advance a trunk you do not own. --bookmark is a jj-only alias of --branch, --create a deprecated alias of --new-branch. A new branch is cut with gt create (graphite), git switch -c (git), or jj bookmark create -r @- (jj).

Hooks follow that same decision. Ship runs the repository's prek suite only where the commit lands straight on trunk — the one position no pull request and no CI ever grades — and on a repository that names no trunk at all, where nothing downstream grades a commit either. Every other position is bound for a pull request whose CI is the check, so the suite is skipped there rather than paid twice, and git's own hooks go with it: the commit verb and every push ship makes carry --no-verify, since a push would otherwise run the pre-push half of the suite the commit just skipped. --verify runs them wherever the commit lands, --no-verify skips them on trunk too, and either flag passed explicitly beats the default.

A live Graphite config (.git/.graphite_repo_config, or the git common dir's copy in a linked worktree) routes ship to the gt lane instead, even in colocated jj repos; --no-gt falls back to the jj/git detection above. Ship declines that lane, reporting a leading lane <kind> (<reason>) segment, when the repo sets ccx.nogt or when GitHub says the repo is someone else's — a public repo you neither administer nor share an owner or organization with. A lookup that cannot be made at all (gh off PATH, not signed in, no GitHub remote) keeps the gt lane. Ship also asks Graphite itself, before any commit forms, whether this repo is submittable — a live config only proves gt init once ran — and declines the lane when Graphite answers no (no auth token, no permissions); a probe that times out or cannot reach the Graphite server is asked once more and then fails the ship without committing, since an unanswered probe says nothing about the lane and demoting on one would drop a stacked branch out of its stack. Every verdict is cached between ships: a yes for a day, a no for an hour, an unanswered probe for a minute. The gt lane commits through gt: gt create <name> starts a stacked branch, gt modify -c appends to one, and --amend amends the branch tip; an untracked branch is adopted first with gt track -f, or gt track --parent when --parent names one, and a tracked branch --parent names another parent for is moved onto it; a parent no longer in the branch's history, rewritten after the branch was cut from it, first has the branch's own commits replayed onto its head, and a branch whose own commits sit among copies of the parent's is refused instead; the resolved parent is reported. gt track -f takes the most recent tracked ancestor, which in a repository carrying stale branches is routinely one the remote trunk already contains, and every submit built on it proposes a pull request holding no commits, which Graphite refuses; so a parent gt track -f picked that the remote trunk contains is replaced by trunk and named in the report, and a --parent naming one is refused before any commit forms, pointing at ccx vcs prune. That check reads the remote-tracking trunk as it stands, without a fetch, since containment only grows and a --no-push ship stays off the network; a repository with no such ref at all has nothing to measure against, and the submit fetches one before it needs the answer. Instead of a plain push, the lane submits the downstack itself, over Graphite's API, anchored on the remote trunk: the submit fetches that one ref first — origin/<trunk>, moving no local branch and no working copy — because the local trunk can sit behind it, and against a trunk left behind a branch whose commits are already upstream reads as a stack member, and Graphite refuses the empty pull request that follows. Every branch of the downstack whose head the remote trunk already contains is therefore dropped from the submit and named as skipped, having nothing left to push and no pull request to open, and the ship refuses outright when the branch being shipped is itself already contained, pointing at ccx vcs prune. A branch left carrying a commit whose patch the remote trunk already holds is refused there too, before the push, since its pull request would propose work the branch does not own. Of the rest, one atomic git push carries every branch's refspec at once, each under the lease of its last submitted version, so every branch moves or none does, and then each branch goes to Graphite in a submit call of its own, in stack order, published by default; --draft submits drafts, --publish makes the default explicit. A branch that already has an open pull request is updated without restating its title or body, so a description someone hand-edited survives; a branch without one gets a pull request titled and bodied from its first commit above the base — for a branch stacked on trunk, or on a branch this submit dropped, that base is the remote trunk's head, which is also the base sha the submit records for it in place of the one gt's local state holds. A submit deeper than one branch names every branch it is about to force-push before it runs, since a submit force-pushes every branch of the downstack it kept. Past that one trunk ref ship never fetches in the gt lane, and it never retries there. A pushing ship whose branch needs a restack or whose base is off the fetched trunk runs the stack rebase machinery over its downstack after the commit, reported as "restacked in isolation". A --no-push ship still restacks with the older in-place replay. A branch already sitting on its parent is left exactly where it is, keeping the shas its pull request was pushed as. The working copy holding a moved branch must be clean and is moved onto its new head; a branch another working copy holds is that lane's, so it is left where it is, with the branches above it, and named in the report. A branch gt is holding — gt freeze, or a merge in progress — is left where it is whatever its parent did, and a refusal names the hold rather than calling it a branch off its parent. A conflict during a pushing ship stops in a conflict workspace with rerere off; ccx vcs stack continue finishes the rebase, pushes, submits, and restates the PR flags this invocation carried. A --no-push ship's conflict refusal names ccx vcs stack rebase. A branch whose commits are already in its parent is named as merged instead, with the gt untrack that clears it. The lane still refuses up front on a branch gt track cannot adopt and on --amend on trunk; a failed submit reports the recovery step instead of retrying — gt auth for a rejected token — and a submit Graphite refused in part names both the branches that landed and the ones it refused. The report names the submitted branch and its PR, from Graphite's own answer rather than a GitHub lookup: submitted <branch> → PR #<n> <url>.

Ship owns the pull request in every lane. --pr-title and --pr-body-file are repeatable and branch-scoped — <branch>=<value>, a bare value applying to the tip — because one submit opens a PR for every branch in the downstack; --pr-body-file takes "-" once to read the body from piped stdin. Outside the graphite lane ship opens the branch's PR when there is none (never with --fill, which would publish the commit's Claude-Session-Id trailer into the description) and edits exactly the fields this invocation restated when there is, so a description someone hand-edited survives a re-ship that does not mention it; a body given is replaced wholesale, never merged. On trunk it reports no PR (on trunk). In the graphite lane the submit opens the PRs and ship restates the named branches afterwards, which is the only way a downstack PR gets a body at all. Every body file is read before the commit forms, so an unreadable path refuses with the working copy untouched, and a ship that names no PR flag makes no gh pr call. --draft and --publish apply in every lane, converting an existing PR in either direction; --no-pr skips the step. -m is optional when an unscoped --pr-title is given: the title becomes the commit subject, and an unscoped --pr-body-file its body, with the <details> wrapper dropped, each ## Heading folded into a Heading: paragraph, and blank runs collapsed. Only an unscoped value can feed it — the tip's name is the branch plan, which that message is an input to.

--dry-run is the opposite switch, and between them they spell the whole risk axis. It prints the decisions the run has already resolved and then stops before the first mutation: the branch the commit lands on, the parent a track would record and why that branch and not another, the named paths beside the staged ones the graphite commit takes with them, every ref a restack would rewrite and the working copy holding it, the open pull request heads a submit would force-push over, the titles it would derive for the ones it opens, and the paths the replay resolves anew. It fetches nothing, runs no gt verb and writes nothing, so the refs and the working copy are byte for byte what they were; --budget caps the report.

--yolo is the one switch for "skip the checks": it implies --no-verify, so ship's own prek pass never runs and the commit gt cuts carries --no-verify, and it drops every guard ship adds of its own, now and as more are added. It drops none today — the multi-branch gt submit --dry-run probe it was written for is gone, replaced by the branch list ship already holds — so against this version it is exactly --no-verify. It never drops a refusal git or gt would make anyway, and never the auto-restack, which is recovery rather than a guard.

--reviews keeps listening after the CI watch: each new review comment on the pushed branch's PR — every submitted PR, in the gt lane — streams to stdout until all are merged or closed. The standalone surface, with attach and replay knobs (--since, --interval, --budget, --stack), is ccx vcs reviews.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.paths = args
			return runShip(cmd, o)
		},
	}
	cmd.Flags().StringArrayVarP(&o.messages, "message", "m", nil, "commit message; repeatable, each one its own paragraph")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "commit only; do not push or watch CI")
	cmd.Flags().BoolVar(&o.noCommit, "no-commit", false, "push and update the PR for the commit already in place; cut no commit, and refuse a dirty working copy — implied when there is nothing to commit and the branch is ahead of trunk")
	cmd.Flags().StringVar(&o.expectRemote, "expect-remote", "", "publish an existing plain Git commit only if the remote branch still has this full commit ID; requires --no-commit")
	cmd.Flags().BoolVar(&o.noWatch, "no-watch", false, "push but do not watch CI")
	cmd.Flags().BoolVar(&o.noVerify, "no-verify", false, "skip the repository's hooks (uvx prek, and git's own) — the default everywhere a pull request's CI is the check")
	cmd.Flags().BoolVar(&o.verify, "verify", false, "run the repository's hooks — the default when the commit lands straight on trunk, or on a repository that names no trunk")
	cmd.Flags().BoolVar(&o.yolo, "yolo", false, "skip every hook and ship-side guard: implies --no-verify, and drops any guard ship adds of its own")
	cmd.Flags().BoolVar(&o.amend, "amend", false, "fold the working copy into the parent commit")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "report what this ship would do — resolved parent, commit scope, the refs and pull request heads it would move — and do none of it")
	cmd.Flags().IntVar(&o.budget, "budget", shipLogBudget, "token budget for the CI failure log excerpt, and for the --dry-run report (0 = uncapped)")
	cmd.Flags().StringArrayVar(&o.skipHunks, "skip-hunk", nil, "commit everything except this hunk ref (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringArrayVar(&o.onlyHunks, "only-hunk", nil, "commit only this hunk ref in its file (repeatable; refs from ccx vcs hunks)")
	cmd.Flags().StringVar(&o.branch, "branch", "", "commit onto this branch, creating it here when it does not exist")
	cmd.Flags().StringVar(&o.branch, "bookmark", "", "jj-only alias of --branch")
	cmd.Flags().StringVar(&o.newBranch, "new-branch", "", "start a new branch for this commit; bare --new-branch derives the name from the message, an explicit name must be spelled --new-branch=name")
	cmd.Flags().Lookup("new-branch").NoOptDefVal = branchNoOptDefVal
	cmd.Flags().StringVar(&o.newBranch, "create", "", "deprecated alias of --new-branch")
	cmd.Flags().Lookup("create").NoOptDefVal = branchNoOptDefVal
	cmd.Flags().BoolVar(&o.appendOnly, "append", false, "append the commit to the branch already checked out, refusing on trunk")
	cmd.Flags().StringVar(&o.parent, "parent", "", "parent of the stacked branch: recorded for a new or untracked branch, and a tracked one moves onto it (graphite lane only)")
	cmd.Flags().BoolVar(&o.allowTrunk, "allow-trunk", false, "let --branch advance the trunk of a repository you do not own")
	cmd.Flags().BoolVar(&o.draft, "draft", false, "open new PRs as drafts, and convert an existing one to a draft")
	cmd.Flags().BoolVar(&o.publish, "publish", false, "publish new PRs, and mark an existing draft ready (the default when neither is passed)")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	cmd.Flags().BoolVar(&o.reviews, "reviews", false, "after the CI watch, keep streaming new PR review comments until every submitted PR is merged or closed")
	cmd.Flags().StringArrayVar(&o.prTitle, "pr-title", nil, "set the pull request title; repeatable as <branch>=<title>, bare applies to the tip")
	cmd.Flags().StringArrayVar(&o.prBodyFile, "pr-body-file", nil, `set the pull request body from a file; repeatable as <branch>=<path>, bare applies to the tip ("-" reads stdin)`)
	cmd.Flags().BoolVar(&o.noPR, "no-pr", false, "push only; never create or update a pull request")
	cmd.Flags().StringArrayVar(&o.landed, "landed", nil, "treat <branch> as landed and drop it from the downstack (repeatable; graphite lane only)")
	cmd.Flags().BoolVar(&o.tipOnly, "tip-only", false, "ship only this branch, onto its parent's published head, pushing no ancestor (graphite lane only)")
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
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
		{"verify", "no-verify"},
		{"verify", "yolo"},
	} {
		cmd.MarkFlagsMutuallyExclusive(group...)
	}
	return cmd
}

func runShip(cmd *cobra.Command, o shipOpts) (err error) {
	ctx := cmd.Context()
	o.message = strings.Join(o.messages, "\n\n")
	if err := checkBranchFlags(cmd, o); err != nil {
		return err
	}
	l, err := resolveLane(ctx, "ship", workingDir(ctx), o.noGT)
	if err != nil {
		return err
	}
	dir, gtLane := l.dir(), l.gt
	kind := l.kind
	if gtLane {
		kind = vcs.Git
	}
	if !gtLane && o.parent != "" {
		return errors.New("ship: --parent applies only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop it")
	}
	if cmd.Flags().Changed("expect-remote") {
		if err := validateShipExpectedRemote(o, kind, gtLane); err != nil {
			return err
		}
		o.expectRemote = strings.ToLower(o.expectRemote)
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
	if o.rootPaths, err = rootRelPaths(ctx, string(dir), o.paths); err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	prCleanup, err := materializePRBodyStdin(cmd, &o)
	defer prCleanup()
	if err != nil {
		return err
	}
	if !o.amend && !o.noCommit && len(o.messages) == 0 {
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
	var gtc *gtCache
	if gtLane {
		gtc = newGTCache(dir, "ship")
	}
	if o.dryRun {
		return runShipDryRun(ctx, cmd, l, o, gtc)
	}
	if o.newBranch != "" && !o.amend && gitBacked(l) {
		var undo func() error
		if o, undo, err = shipCutFromDetached(ctx, dir, o); err != nil {
			return err
		}
		if undo != nil {
			defer func() {
				if err != nil {
					err = errors.Join(err, undo())
				}
			}()
		}
	}
	plan, planSeg, err := shipResolvePlan(ctx, cmd.ErrOrStderr(), l, o, gtc)
	if err != nil {
		return err
	}
	// Must follow the preflight, whose landed-parent guard reads the ref this
	// fetch moves.
	var trunkFetch *gtTrunkFetch
	if gtLane && !o.noPush {
		trunkFetch = gtStartTrunkFetch(ctx, dir, "ship", plan.trunk)
		defer trunkFetch.stop()
	}
	o.noVerify = !shipVerify(cmd, o, plan)
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
	landedSeg := ""
	untrackedSeg := ""
	if o.noCommit {
		if untrackedSeg, err = shipRefuseDirty(ctx, dir, kind, o); err != nil {
			return err
		}
	} else if kind == vcs.JJ && sel == nil && !o.amend {
		switch err := shipRefuseEmptyJJ(ctx, dir, o, plan); {
		case errors.Is(err, errShipAlreadyCommitted):
			o.noCommit, landedSeg = true, shipLandedSegment(o)
		case err != nil:
			return err
		}
	}

	if plan.moveOntoParent {
		if planSeg, plan.needsRestack, err = gtMoveOntoParent(ctx, cmd.ErrOrStderr(), l, o, gtc); err != nil {
			return err
		}
	}

	var preAmendSHA string
	if !o.noPush && o.amend && (gtLane || kind == vcs.Git) {
		if preAmendSHA, err = gitRevParse(ctx, dir, "ship", "HEAD"); err != nil {
			return err
		}
	}

	var hookSeg string
	if !o.noCommit {
		if gtLane {
			if hookSeg, err = shipCommitGT(ctx, l, cmd.ErrOrStderr(), o, sel, plan); err != nil && preAmendSHA != "" {
				err = shipAmendKept(ctx, dir, preAmendSHA, err)
			}
		} else {
			hookSeg, err = shipCommitLocal(ctx, cmd.ErrOrStderr(), dir, kind, o, sel, plan)
		}
		switch {
		case errors.Is(err, errShipAlreadyCommitted):
			o.noCommit, landedSeg = true, shipLandedSegment(o)
		case err != nil:
			return err
		case kind == vcs.JJ && plan.action == branchCreate:
			if _, err := render.RunCLI(ctx, dir, "jj", []string{"bookmark", "create", plan.name, "-r", "@-"}); err != nil {
				return fmt.Errorf("ship: jj bookmark create %s: %w", plan.name, err)
			}
		}
	}

	branch, healSeg, err := shipBranchAfterCommit(ctx, dir, l.checkout, kind, plan)
	if err != nil {
		return err
	}
	if gtLane && !o.noCommit {
		gtc.forget()
	}

	stuckOpts := asGiven
	stuckOpts.noCommit = o.noCommit
	stuck := gtStuckSuffix(stuckOpts)
	restackSeg := ""
	if gtLane && !o.noPush {
		tr, err := trunkFetch.join()
		if err != nil {
			return err
		}
		state, chain, err := gtStackChain(ctx, gtc, branch)
		if err != nil {
			return err
		}
		contains, err := gitIsAncestor(ctx, l.dir(), "ship", string(tr.Ref()), state[branch].Head)
		if err != nil {
			return err
		}
		published, err := stackHasPublication(ctx, l.dir(), chain)
		if err != nil {
			return err
		}
		if plan.needsRestack || !contains || published || len(o.landed) > 0 || o.tipOnly {
			intent, err := stackShipOptions(o, meta, prNWO, branch)
			if err != nil {
				return err
			}
			if err := runStackRebase(cmd, stackRebaseOpts{members: gtBottomUp(chain), landed: o.landed, draft: o.draft, noVerify: o.noVerify, deferPush: true, result: &gtc.restack, ship: intent, tip: branch, tipOnly: o.tipOnly, dropCommits: o.dropCommits || o.yolo}); err != nil {
				if preAmendSHA != "" && gtc.restack == nil {
					return shipAmendKept(ctx, dir, preAmendSHA, err)
				}
				return err
			}
			gtc.forget()
			restackSeg = "restacked in isolation"
		}
	} else if plan.needsRestack && !o.tipOnly {
		if restackSeg, err = gtRestack(ctx, l, stuck, branch, gtc); err != nil {
			return err
		}
	}

	short, subject, err := shipDescribe(ctx, dir, kind)
	if err != nil {
		return shipSettleRestack(ctx, l, gtc, err)
	}
	segments := make([]string, 0, 8)
	if l.note != "" {
		segments = append(segments, fmt.Sprintf("lane %s (%s)", kindLabel(l.kind), l.note))
	}
	for _, seg := range []string{planSeg, landedSeg, hookSeg} {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	committedSegment := len(segments)
	segments = append(segments, shipCommitSegment(o.noCommit, short, subject))
	if untrackedSeg != "" {
		segments = append(segments, untrackedSeg)
	}
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
			if err := jjMoveBookmark(ctx, dir, branch); err != nil {
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
		prSeg, bodylessSegs, gtStack, err = shipPushGT(ctx, cmd.ErrOrStderr(), l, o, meta, trunkFetch, branch, stuck, gtc)
	} else {
		remote, rebased, err = shipPush(ctx, dir, kind, o, branch, preAmendSHA)
	}
	if err != nil {
		return shipSettleRestack(ctx, l, gtc, err)
	}
	if gtLane && gtc.restack != nil {
		common, err := gtc.common(ctx)
		if err != nil {
			return err
		}
		if err := stackCompletePublication(ctx, dir, common, gtc.restack); err != nil {
			return err
		}
	}
	if rebased > 0 {
		short, subject, err = shipDescribe(ctx, dir, kind)
		if err != nil {
			return err
		}
		segments[committedSegment] = shipCommitSegment(o.noCommit, short, subject)
		segments = append(segments, fmt.Sprintf("rebased %d commit(s) onto %s", rebased, branch))
	}
	if gtLane {
		if gtc.restack != nil {
			segments = append(segments, fmt.Sprintf("published %.12s · source checkouts unchanged", gtc.restack.branch(branch).NewHead))
		}
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
			reviewBranches, err = stackBranches(ctx, gtc)
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

	var ciSeg string
	var report []string
	var ciErr error
	if gtLane && gtc.restack != nil {
		ciSeg, report, ciErr = shipWatchCIHead(ctx, cmd.ErrOrStderr(), dir, gtc.restack.branch(branch).NewHead, o.budget)
	} else {
		ciSeg, report, ciErr = shipWatchCI(ctx, cmd.ErrOrStderr(), dir, kind, o.budget)
	}
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

func shipSettleRestack(ctx context.Context, l lane, c *gtCache, cause error) error {
	if c == nil || c.restack == nil {
		return cause
	}
	common, err := c.common(ctx)
	if err != nil {
		return errors.Join(cause, err)
	}
	_, err = stackSettle(ctx, l, common, c.restack)
	return errors.Join(cause, err)
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

func withSessionTrailer(ctx context.Context, message string) string {
	id := render.Getenv(ctx, envClaudeSessionKey)
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
func shipCommitLocal(ctx context.Context, errW io.Writer, dir render.Dir, kind vcs.Kind, o shipOpts, sel *shipSelection, plan branchPlan) (seg string, err error) {
	if kind == vcs.Git && plan.action == branchCreate {
		if _, serr := render.RunCLI(ctx, dir, "git", []string{"switch", "-c", plan.name}); serr != nil {
			return "", fmt.Errorf("ship: git switch -c %s: %w", plan.name, serr)
		}
		defer func() {
			if err != nil {
				err = errors.Join(err, shipRestoreBranch(ctx, dir, plan.from, plan.name))
			}
		}()
	}
	return shipCommit(ctx, errW, dir, kind, o, sel, plan)
}

// shipCutFromDetached cuts --new-branch at a detached HEAD before any lane reads
// the position, so every lane then ships an ordinary branch. The undo it returns
// puts HEAD back detached and deletes the branch, and does nothing once a commit
// has landed on it.
func shipCutFromDetached(ctx context.Context, dir render.Dir, o shipOpts) (shipOpts, func() error, error) {
	current, err := gitCurrentBranch(ctx, dir, "ship")
	if err != nil || current != "" {
		return o, nil, err
	}
	name, err := newBranchName(o)
	if err != nil {
		return o, nil, err
	}
	head, err := stackRevParse(ctx, dir, "HEAD")
	if err != nil {
		return o, nil, err
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"switch", "-qc", name}); err != nil {
		return o, nil, fmt.Errorf("ship: git switch -c %s: %w", name, err)
	}
	o.newBranch = ""
	undo := func() error {
		now, err := stackRevParse(ctx, dir, "HEAD")
		if err != nil || now != head {
			return err
		}
		if _, err := render.RunCLI(ctx, dir, "git", []string{"switch", "-q", "--detach", head}); err != nil {
			return fmt.Errorf("ship: rollback: git switch --detach %.12s: %w — the working copy is left on %s", head, err, name)
		}
		if _, err := render.RunCLI(ctx, dir, "git", []string{"branch", "-D", name}); err != nil {
			return fmt.Errorf("ship: rollback: git branch -D %s: %w — the branch ship cut is left behind", name, err)
		}
		return nil
	}
	return o, undo, nil
}

// shipRestoreBranch puts the working copy back on from and deletes created. -D
// is not a force: git switch -c refuses an existing name, so created is one this
// run cut, with no commit on it. A step that fails names what it left behind.
func shipRestoreBranch(ctx context.Context, dir render.Dir, from, created string) error {
	if _, err := render.RunCLI(ctx, dir, "git", []string{"switch", from}); err != nil {
		return fmt.Errorf("ship: rollback: git switch %s: %w — the working copy is left on %s", from, err, created)
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"branch", "-D", created}); err != nil {
		return fmt.Errorf("ship: rollback: git branch -D %s: %w — the branch ship cut is left behind", created, err)
	}
	return nil
}

// shipCommit stages, runs pre-commit hooks, and commits. Hunk-scoped selections
// report "hooks hunk-skip" instead: external prek would inspect full worktree
// files, not the partial content being committed through a throwaway index.
// It returns the hook summary segment to prepend to the ship summary.
func shipCommit(ctx context.Context, errW io.Writer, dir render.Dir, kind vcs.Kind, o shipOpts, sel *shipSelection, plan branchPlan) (string, error) {
	o.message = withSessionTrailer(ctx, o.message)
	segs := make([]string, 0, 2)
	if kind == vcs.Git && sel == nil {
		sweptSeg, err := shipGitAdd(ctx, dir, o)
		if err != nil {
			return "", err
		}
		if sweptSeg != "" {
			segs = append(segs, sweptSeg)
		}
	}
	if sel != nil && !o.noVerify && shipHasHookConfig(string(dir)) {
		segs = append(segs, "hooks hunk-skip")
	}
	if sel == nil {
		hookSeg, ran, err := shipRunHooks(ctx, errW, dir, kind, o)
		if err != nil {
			return "", err
		}
		o.hooksRan = ran
		if hookSeg != "" {
			segs = append(segs, hookSeg)
		}
	}
	seg := strings.Join(segs, shipSep)
	switch kind {
	case vcs.JJ:
		return seg, shipCommitJJ(ctx, dir, o, sel)
	case vcs.Git:
		return seg, shipCommitGit(ctx, dir, o, sel, plan)
	default:
		return "", errors.New("ship: commit: unsupported vcs")
	}
}

// shipGitAdd stages the ship's paths (or everything, when unscoped) into the real
// index ahead of hook attempts and the commit. An unscoped add runs --verbose and
// returns the segment naming what it took: a checkout several sessions share can
// hold another lane's work, and a commit that carried it off reads exactly like
// one that did not. Scoped adds name their paths already, so they report nothing.
func shipGitAdd(ctx context.Context, dir render.Dir, o shipOpts) (string, error) {
	addArgv := []string{"add", "-A"}
	if len(o.rootPaths) > 0 {
		addArgv = append(addArgv, "--")
		addArgv = append(addArgv, o.rootPaths...)
	} else {
		addArgv = append(addArgv, "--verbose")
	}
	out, err := render.RunCLI(ctx, dir, "git", addArgv)
	if err != nil {
		return "", fmt.Errorf("ship: git add: %w", err)
	}
	if len(o.rootPaths) > 0 {
		return "", nil
	}
	return sweptSegment(parseAddVerbose(out)), nil
}

// parseAddVerbose reads the paths out of `git add --verbose`, whose every line is
// a verb and a single-quoted path: add 'a/b.txt', remove 'a/c.txt'. A line in any
// other shape is git talking about something other than a path, and is skipped.
func parseAddVerbose(out string) []string {
	paths := make([]string, 0, 8)
	for _, line := range strings.Split(out, "\n") {
		open := strings.IndexByte(line, '\'')
		if open < 0 || !strings.HasSuffix(line, "'") || len(line) < open+2 {
			continue
		}
		paths = append(paths, line[open+1:len(line)-1])
	}
	return paths
}

// sweptSegment names the paths an unscoped add staged, capped so a wide commit
// still reports in one line.
func sweptSegment(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	named := paths
	if len(named) > shipSweptNamed {
		named = named[:shipSweptNamed]
	}
	seg := fmt.Sprintf("swept %d path(s): %s", len(paths), strings.Join(named, ", "))
	if len(paths) > len(named) {
		seg += fmt.Sprintf(", and %d more", len(paths)-len(named))
	}
	return seg
}

func shipCommitJJ(ctx context.Context, dir render.Dir, o shipOpts, sel *shipSelection) error {
	if sel != nil {
		return shipCommitJJSelect(ctx, dir, o, sel)
	}
	argv := make([]string, 0, 4+len(o.rootPaths))
	switch {
	case o.amend && o.message != "":
		argv = append(argv, "squash", "-m", o.message)
	case o.amend:
		argv = append(argv, "squash", "--use-destination-message")
	default:
		argv = append(argv, "commit", "-m", o.message)
	}
	if len(o.rootPaths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.rootPaths...)
	}
	if _, err := render.RunCLI(ctx, dir, "jj", argv); err != nil {
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

// shipCommitJJSelect commits a hunk selection through jj's diff-editor protocol:
// it writes a plan tempfile plus a sidecar, points a throwaway merge tool at
// ccx's own apply-selection subcommand, and lets jj drive the partial commit
// inside its transaction. On failure it prefers the sidecar's structured reason
// over raw jj stderr.
func shipCommitJJSelect(ctx context.Context, dir render.Dir, o shipOpts, sel *shipSelection) error {
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
	if _, err := render.RunCLI(ctx, dir, "jj", argv); err != nil {
		if reason := readSidecar(sidecarPath); reason != "" {
			return fmt.Errorf("ship: %s: %w", reason, err)
		}
		return fmt.Errorf("ship: jj %s: %w", argv[0], err)
	}
	return nil
}

func shipCommitGit(ctx context.Context, dir render.Dir, o shipOpts, sel *shipSelection, plan branchPlan) error {
	if sel != nil {
		return shipCommitGitSelect(ctx, dir, o, sel)
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
	if len(o.rootPaths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.rootPaths...)
	}
	if _, cerr := render.RunCLI(ctx, dir, "git", argv); cerr != nil {
		if !o.amend {
			if err := shipRefuseEmptyGit(ctx, dir, o, plan); err != nil {
				return err
			}
		}
		return fmt.Errorf("ship: git commit: %w", cerr)
	}
	return nil
}

func shipDescribe(ctx context.Context, dir render.Dir, kind vcs.Kind) (short, subject string, err error) {
	switch kind {
	case vcs.Git:
		out, rerr := render.RunCLI(ctx, dir, "git", []string{"log", "-1", "--format=%h%x00%s"})
		if rerr != nil {
			return "", "", fmt.Errorf("ship: git log: %w", rerr)
		}
		return splitDescribe(out, "\x00")
	case vcs.JJ:
		out, rerr := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate})
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

// shipRefusal is ship declining on purpose, as opposed to a probe that failed.
// The two are indistinguishable by message, and --dry-run has to tell them
// apart: a refusal is a line in its report, a failed probe is its exit.
type shipRefusal struct{ msg string }

func (e *shipRefusal) Error() string { return e.msg }

func refuse(format string, a ...any) error { return &shipRefusal{msg: fmt.Sprintf(format, a...)} }

// shipRefuseDirty refuses --no-commit over changes to files git tracks, which
// the mode would leave out of the branch and pull request this run updates, and
// returns the untracked paths it exempted. A commit already in place carries no
// scratch; jj has none to exempt, where a new file is already in @.
func shipRefuseDirty(ctx context.Context, dir render.Dir, kind vcs.Kind, o shipOpts) (string, error) {
	var items, untracked []string
	if kind == vcs.JJ {
		paths, err := shipChangedPaths(ctx, dir, vcs.JJ, o)
		if err != nil {
			return "", err
		}
		items = paths
	} else {
		out, err := render.RunCLI(ctx, dir, "git", []string{"status", "--porcelain"})
		if err != nil {
			return "", fmt.Errorf("ship: git status: %w", err)
		}
		for _, line := range strings.Split(out, "\n") {
			switch entry := strings.TrimSpace(line); {
			case entry == "":
			case strings.HasPrefix(line, "?? "):
				untracked = append(untracked, strings.TrimPrefix(line, "?? "))
			default:
				items = append(items, entry)
			}
		}
	}
	if len(items) > 0 {
		return "", refuse("ship: --no-commit needs a clean working copy — uncommitted: %s; drop --no-commit to ship that work, or stash it", strings.Join(items, ", "))
	}
	return shipUntrackedSegment(untracked), nil
}

// shipUntrackedNameCap caps the paths the segment names before it counts the
// rest: a worktree carrying scratch carries more than a report should hold.
const shipUntrackedNameCap = 3

// shipUntrackedSegment names the untracked paths a --no-commit ship left out,
// so the report says what did not reach the pull request.
func shipUntrackedSegment(paths []string) string {
	switch {
	case len(paths) == 0:
		return ""
	case len(paths) <= shipUntrackedNameCap:
		return "left untracked: " + strings.Join(paths, ", ")
	}
	return fmt.Sprintf("left untracked: %s and %d more", strings.Join(paths[:shipUntrackedNameCap], ", "), len(paths)-shipUntrackedNameCap)
}

func shipRefuseEmptyJJ(ctx context.Context, dir render.Dir, o shipOpts, plan branchPlan) error {
	paths, err := shipChangedPaths(ctx, dir, vcs.JJ, o)
	if err != nil {
		return err
	}
	if len(paths) > 0 {
		return nil
	}
	state, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate})
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
	landed, standing, err := shipAlreadyCommitted(ctx, dir, vcs.JJ, plan)
	if err != nil {
		return err
	}
	if landed {
		return errShipAlreadyCommitted
	}
	short, subject, err := shipDescribe(ctx, dir, vcs.JJ)
	if err != nil {
		return err
	}
	if standing != "" {
		return fmt.Errorf("ship: nothing to commit%s, and %s — nothing to submit", shipScope(o), standing)
	}
	hint := ""
	if plan.name != "" {
		pat := shellSingleQuote(vcs.JJExactPattern(plan.name))
		hint = fmt.Sprintf(" push it: jj bookmark move %s --to @- && jj git push --bookmark %s", pat, pat)
	}
	return fmt.Errorf("ship: nothing to commit%s — did a prior ship already land %s %q?%s", shipScope(o), short, subject, hint)
}

// shipRefuseEmptyGit refuses a commit over an empty index. Unlike git commit,
// gt create happily creates an empty branch on one, so the graphite lane asks
// before committing and the git lane asks once git has refused — which is also
// what tells a genuinely empty index from git's other refusals.
func shipRefuseEmptyGit(ctx context.Context, dir render.Dir, o shipOpts, plan branchPlan) error {
	argv := []string{"diff", "--cached", "--quiet"}
	if len(o.rootPaths) > 0 {
		argv = append(argv, "--")
		argv = append(argv, o.rootPaths...)
	}
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", argv)
	if err != nil {
		return fmt.Errorf("ship: git diff --cached --quiet: %w", err)
	}
	if code == 1 {
		return nil
	}
	if code != 0 {
		return fmt.Errorf("ship: git diff --cached --quiet: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	landed, standing, err := shipAlreadyCommitted(ctx, dir, vcs.Git, plan)
	if err != nil {
		return err
	}
	if landed {
		return errShipAlreadyCommitted
	}
	short, subject, err := shipDescribe(ctx, dir, vcs.Git)
	if err != nil {
		return err
	}
	if standing != "" {
		return fmt.Errorf("ship: nothing to commit%s, and %s — nothing to submit", shipScope(o), standing)
	}
	return fmt.Errorf("ship: nothing to commit%s — did a prior ship already land %s %q?", shipScope(o), short, subject)
}

// shipAlreadyCommitted reports work some other path already committed: nothing
// left to commit, scoped to paths or not, over a branch carrying commits trunk
// does not. Ship submits that branch as it stands rather than refusing a commit
// it cannot cut. Where it does not, standing is the clause the refusal prints
// for why there is nothing to submit either, empty when no trunk resolved.
func shipAlreadyCommitted(ctx context.Context, dir render.Dir, kind vcs.Kind, plan branchPlan) (landed bool, standing string, err error) {
	if plan.trunk == "" || plan.action == branchCreate {
		return false, "", nil
	}
	switch kind {
	case vcs.JJ:
		stack, err := jjLogLines(ctx, dir, "ship", jjStackRevset(plan.trunk))
		if err != nil {
			return false, "", err
		}
		if len(stack) == 0 {
			return false, "@- carries nothing above " + plan.trunk, nil
		}
		conflicts, err := jjLogLines(ctx, dir, "ship", jjConflictRevset(plan.trunk))
		if err != nil {
			return false, "", err
		}
		if len(conflicts) > 0 {
			return false, "the commits above " + plan.trunk + ", or their descendants, are conflicted", nil
		}
		return true, "", nil
	case vcs.Git:
		out, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--count", plan.trunk + "..HEAD"})
		if err != nil {
			return false, "", fmt.Errorf("ship: git rev-list --count %s..HEAD: %w", plan.trunk, err)
		}
		ahead, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return false, "", fmt.Errorf("ship: malformed rev-list count %q: %w", out, err)
		}
		if ahead == 0 {
			return false, "the branch carries nothing above " + plan.trunk, nil
		}
		return true, "", nil
	default:
		return false, "", errors.New("ship: unsupported vcs")
	}
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
		if _, err := os.Stat(filepath.Join(workingDir(cmd.Context()), path)); err != nil {
			return fmt.Errorf("ship: %q is not a path — did you mean --new-branch=%s?", path, path)
		}
	}
	return nil
}

// shipVerify decides whether this ship runs the repository's hooks, from the
// branch decision it already made: they run where the commit lands straight on
// trunk, or on a repository that names no trunk — the positions no pull request
// and no CI grades. Everywhere else a pull request's CI is the check, so a suite
// that takes minutes is not paid twice for the same verdict.
//
// Either flag, passed explicitly, overrides that, as does the --yolo that
// implies --no-verify; --no-verify=false reads as --verify.
func shipVerify(cmd *cobra.Command, o shipOpts, plan branchPlan) bool {
	switch {
	case cmd.Flags().Changed("verify"):
		return o.verify
	case cmd.Flags().Changed("no-verify") || o.yolo:
		return !o.noVerify
	default:
		return plan.action == branchAppend && (plan.trunk == "" || plan.name == plan.trunk)
	}
}

// shipResolvePlan resolves where the commit goes, before any mutation, on
// whichever lane is live. The second return is the preflight's own report
// segment, which today only the graphite lane's auto-track produces. errW is the
// graphite preflight's reporting channel: an auto-track gt declined says so in
// gt's own words, which ship's recovery step would otherwise replace.
func shipResolvePlan(ctx context.Context, errW io.Writer, l lane, o shipOpts, c *gtCache) (branchPlan, string, error) {
	if l.gt {
		return shipPreflightGT(ctx, errW, l, o, c)
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
	trunkNames, err := jjTrunkBookmarkNames(ctx, l.dir(), "ship")
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

	names, err := jjBookmarkNames(ctx, l.dir(), "ship", jjNearestBookmarkRevset)
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
		explicitHeads, err = jjBookmarkHeads(ctx, l.dir(), o.branch)
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
		heads, err := jjBookmarkHeads(ctx, l.dir(), plan.name)
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
func jjBookmarkHeads(ctx context.Context, dir render.Dir, name string) (int, error) {
	heads, err := jjLogLines(ctx, dir, "ship", jjBookmarksRevset(name))
	if err != nil {
		return 0, err
	}
	if len(heads) > 1 {
		return 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", name, len(heads))
	}
	return len(heads), nil
}

func shipPreflightGitLane(ctx context.Context, l lane, o shipOpts) (branchPlan, error) {
	current, err := gitCurrentBranch(ctx, l.dir(), "ship")
	if err != nil {
		return branchPlan{}, err
	}
	trunk, err := gitTrunkBranch(ctx, l.dir())
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
	return plan, refuseExistingBranch(ctx, l.dir(), o, plan)
}

// refuseExistingBranch refuses a --branch that names an existing branch other
// than the one checked out: reaching it would mean checking it out mid-ship,
// which strands the working copy's changes on the branch being left.
func refuseExistingBranch(ctx context.Context, dir render.Dir, o shipOpts, plan branchPlan) error {
	if o.branch == "" || plan.action != branchCreate {
		return nil
	}
	exists, err := gitRefExists(ctx, dir, "ship", "refs/heads/"+plan.name)
	if err != nil {
		return err
	}
	if exists {
		return refuse("ship: branch %s already exists — check it out first; ship does not switch branches mid-commit", plan.name)
	}
	return nil
}

// gitCurrentBranch names the checked-out branch, empty on a detached HEAD.
func gitCurrentBranch(ctx context.Context, dir render.Dir, prefix string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"branch", "--show-current"})
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
func gitTrunkBranch(ctx context.Context, dir render.Dir) (string, error) {
	ref := "refs/remotes/origin/HEAD"
	out, code, _, err := render.RunCLIExitCode(ctx, dir, "git", []string{"symbolic-ref", "--short", ref})
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
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
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
func shipBranchAfterCommit(ctx context.Context, dir render.Dir, ck vcs.Checkout, kind vcs.Kind, plan branchPlan) (string, string, error) {
	if kind != vcs.Git {
		return plan.name, "", nil
	}
	branch, err := gitCurrentBranch(ctx, dir, "ship")
	if err != nil {
		return "", "", err
	}
	if branch != "" {
		return branch, "", nil
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "HEAD"})
	if err != nil {
		return "", "", fmt.Errorf("ship: git rev-parse HEAD: %w", err)
	}
	sha := strings.TrimSpace(out)
	if _, err := render.RunCLI(ctx, dir, "git", []string{"checkout", "-B", plan.name, sha}); err != nil {
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

func shipPush(ctx context.Context, dir render.Dir, kind vcs.Kind, o shipOpts, target, preAmendSHA string) (remote string, rebased int, err error) {
	switch kind {
	case vcs.JJ:
		rebased, err = shipPushJJ(ctx, dir, target, o.amend)
		return "origin", rebased, err
	case vcs.Git:
		return shipPushGit(ctx, dir, o, target, preAmendSHA)
	default:
		return "", 0, errors.New("ship: push: unsupported vcs")
	}
}

func shipPushJJ(ctx context.Context, dir render.Dir, target string, amend bool) (int, error) {
	if err := jjTrackUntrackedTarget(ctx, dir, target); err != nil {
		return 0, err
	}
	pat := shellSingleQuote(vcs.JJExactPattern(target))
	hint := fmt.Sprintf("jj git fetch && jj rebase -b @- --destination %s && jj bookmark move %s --to @- && jj git push --bookmark %s", shellSingleQuote(jjBookmarksRevset(target)), pat, pat)
	return shipPushRetry(ctx, target, hint, func(ctx context.Context) (int, error) {
		return shipPushJJOnce(ctx, dir, target, amend)
	})
}

// jjMoveBookmark advances target onto the commit ship just made: a jj bookmark
// does not follow jj commit or jj squash the way a git branch ref follows git
// commit, so every lane that leaves a commit behind moves it explicitly.
func jjMoveBookmark(ctx context.Context, dir render.Dir, target string) error {
	if _, err := render.RunCLI(ctx, dir, "jj", []string{"bookmark", "move", vcs.JJExactPattern(target), "--to", "@-"}); err != nil {
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
func jjTrackUntrackedTarget(ctx context.Context, dir render.Dir, target string) error {
	remotes, err := jjUntrackedTargetRemotes(ctx, dir, target)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		return nil
	}
	remote := remotes[0]
	if len(remotes) > 1 {
		remote, err = jjPushRemote(ctx, dir)
		if err != nil {
			return err
		}
	}
	if _, err := render.RunCLI(ctx, dir, "jj", []string{"bookmark", "track", vcs.JJExactPattern(target), "--remote=" + remote}); err != nil {
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
func jjUntrackedTargetRemotes(ctx context.Context, dir render.Dir, target string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "jj", []string{"bookmark", "list", vcs.JJExactPattern(target), "--all-remotes", "-T", jjRemoteBookmarkTemplate})
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
func jjPushRemote(ctx context.Context, dir render.Dir) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "jj", []string{"--ignore-working-copy", "config", "get", "git.push"})
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
func shipPushJJOnce(ctx context.Context, dir render.Dir, target string, amend bool) (int, error) {
	if _, err := render.RunCLI(ctx, dir, "jj", []string{"git", "fetch"}); err != nil {
		return 0, fmt.Errorf("ship: jj git fetch: %w", err)
	}

	heads, err := jjLogLines(ctx, dir, "ship", jjBookmarksRevset(target))
	if err != nil {
		return 0, err
	}
	switch {
	case len(heads) == 0:
		return 0, fmt.Errorf("ship: bookmark %q not found", target)
	case len(heads) > 1:
		return 0, fmt.Errorf("ship: bookmark %q is conflicted (%d heads); resolve it (jj bookmark list --conflicted) before shipping", target, len(heads))
	}

	ancestors, err := jjBookmarkNames(ctx, dir, "ship", jjAncestorRevset(target))
	if err != nil {
		return 0, err
	}
	rebased := 0
	if len(ancestors) == 0 {
		rebased, err = jjRebaseOnto(ctx, dir, target)
		if err != nil {
			return 0, err
		}
	}

	if err := jjMoveBookmark(ctx, dir, target); err != nil {
		return 0, err
	}
	moveOp, err := jjOpID(ctx, dir)
	if err != nil {
		return 0, err
	}
	if _, err := render.RunCLI(ctx, dir, "jj", []string{"git", "push", "--bookmark", vcs.JJExactPattern(target)}); err != nil {
		return rebased, shipPushJJReject(ctx, dir, target, moveOp, amend, err)
	}
	return rebased, nil
}

// shipPushJJReject classifies a failed jj push. A remote-advanced rejection undoes
// only the bookmark move (jj op revert moveOp, uncancellable so a cancelled ctx
// leaves no advanced bookmark) and returns a *pushRejectedError to replay; a
// non-rejection, a failed undo, or a rejected amend is terminal.
func shipPushJJReject(ctx context.Context, dir render.Dir, target, moveOp string, amend bool, pushErr error) error {
	raw := fmt.Errorf("ship: jj git push: %w", pushErr)
	if !jjPushRejected(raw) {
		return raw
	}
	cleanup := context.WithoutCancel(ctx)
	if _, uerr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", moveOp}); uerr != nil {
		return fmt.Errorf("ship: jj git push rejected (%w); reverting the bookmark move also failed: %w — run: jj op revert %s", pushErr, uerr, moveOp)
	}
	if amend {
		return fmt.Errorf("ship: origin advanced past the commit you amended on %q — someone else pushed; not force-retrying over their work; inspect with jj log and jj op log, then reconcile manually: %w", target, pushErr)
	}
	return &pushRejectedError{err: raw}
}

func shipPushGit(ctx context.Context, dir render.Dir, o shipOpts, branch, preAmendSHA string) (string, int, error) {
	remote, err := gitRemoteFor(ctx, dir, "ship", branch)
	if err != nil {
		return "", 0, err
	}
	if o.expectRemote != "" {
		return remote, 0, shipPushGitExpected(ctx, dir, remote, branch, o.expectRemote, o.noVerify)
	}
	if o.amend {
		return remote, 0, shipPushGitAmend(ctx, dir, remote, branch, preAmendSHA, o.noVerify)
	}
	hint := fmt.Sprintf("git fetch %s && git rebase --autostash %s/%s && git push %s %s", remote, remote, branch, remote, branch)
	rebased, err := shipPushRetry(ctx, branch, hint, func(ctx context.Context) (int, error) {
		return shipPushGitOnce(ctx, dir, remote, branch, o.noVerify)
	})
	return remote, rebased, err
}

// pushArgv opens every push ccx runs. Following tags makes git resolve every
// remote branch tip, and in a partial clone each tip it lacks is a lazy fetch
// of that tip's whole commit and tree history. Quiet skips the status table,
// whose column width abbreviates both oids of every advertised ref against
// every pack; git still prints it when the push fails.
func pushArgv(args ...string) []string {
	return append([]string{"push", "--no-follow-tags", "--quiet"}, args...)
}

// gitPushArgv builds a push argv, carrying the run's hook decision to the
// pre-push hook a push of its own would otherwise run — the same suite the
// commit already skipped, over the same files.
func gitPushArgv(noVerify bool, args ...string) []string {
	argv := pushArgv()
	if noVerify {
		argv = append(argv, "--no-verify")
	}
	return append(argv, args...)
}

// gitRemoteFor resolves the remote that branch.<branch>.remote configures, so a
// triangular or non-origin-only repo fetches, rebases, and pushes against the
// same remote. git config --get exits 1 when unset; that and an empty value both
// default to origin. Any other exit is an error, prefixed with the command that
// asked — ship, restack, and info all do.
func gitRemoteFor(ctx context.Context, dir render.Dir, prefix, branch string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", "branch." + branch + ".remote"})
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

func shipAmendKept(ctx context.Context, dir render.Dir, preAmendSHA string, err error) error {
	head, headErr := gitRevParse(ctx, dir, "ship", "HEAD")
	if headErr != nil {
		return errors.Join(err, headErr)
	}
	if head == preAmendSHA {
		return err
	}
	return fmt.Errorf("%w\nship: the amend stays committed as %s and unpublished — publish it with ccx vcs stack submit once that is resolved, or undo it with git reset --soft %s", err, shortOID(head), preAmendSHA)
}

// shipPushGitAmend pushes an amended commit without ever fetching. It tries a
// plain push first (an amend of an unpushed commit fast-forwards, no force) and
// only on a non-fast-forward rejection force-pushes with a lease pinned to
// preAmendSHA, so the force lands iff the remote still sits on the rewritten
// commit. A stale or rejected lease is terminal.
func shipPushGitAmend(ctx context.Context, dir render.Dir, remote, branch, preAmendSHA string, noVerify bool) error {
	_, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, branch))
	if err == nil {
		return nil
	}
	if !gitPushRejected(err) {
		return fmt.Errorf("ship: git push: %w", err)
	}
	lease := fmt.Sprintf("--force-with-lease=%s:%s", branch, preAmendSHA)
	if _, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, lease, branch)); err != nil {
		if gitPushStaleLease(err) || gitPushRejected(err) {
			return fmt.Errorf("ship: %s/%s does not match the pre-amend head — someone may have built on the commit you amended, or it was rebased locally; inspect and reconcile the remote, then verify its exact commit before running ccx vcs ship --no-gt --no-commit --expect-remote <full-remote-oid>: %w", remote, branch, err)
		}
		return fmt.Errorf("ship: git push: %w", err)
	}
	return nil
}

// shipPushGitOnce is one non-amend push attempt: fetch the remote, rebase onto
// <remote>/<branch> when it advanced past HEAD, then push. A rejected push moves
// no local ref, so it re-enters as a *pushRejectedError with no rollback.
func shipPushGitOnce(ctx context.Context, dir render.Dir, remote, branch string, noVerify bool) (int, error) {
	if err := gitFetch(ctx, dir, remote); err != nil {
		return 0, fmt.Errorf("ship: git fetch %s: %w", remote, err)
	}
	remoteRef := "refs/remotes/" + remote + "/" + branch
	present, err := gitRefExists(ctx, dir, "ship", remoteRef)
	if err != nil {
		return 0, err
	}
	rebased := 0
	if present {
		ancestor, err := gitIsAncestor(ctx, dir, "ship", remoteRef, "HEAD")
		if err != nil {
			return 0, err
		}
		if !ancestor {
			rebased, err = gitRebaseOnto(ctx, dir, "ship", remote, branch)
			if err != nil {
				return 0, err
			}
		}
	}
	if _, err := render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, branch)); err != nil {
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
func gitRefExists(ctx context.Context, dir render.Dir, prefix, ref string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"rev-parse", "--verify", "--quiet", ref})
	if err != nil {
		return false, fmt.Errorf("%s: git rev-parse %s: %w", prefix, ref, err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("%s: git rev-parse %s: exit %d: %s", prefix, ref, code, strings.TrimSpace(stderr))
	}
}

// gitIsAncestor reports whether maybe is an ancestor of ref (git merge-base
// --is-ancestor: exit 0 yes, exit 1 no). Any other exit is an error.
func gitIsAncestor(ctx context.Context, dir render.Dir, prefix, maybe, ref string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"merge-base", "--is-ancestor", maybe, ref})
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

// gitRebaseOnto rebases HEAD onto <remote>/<branch>, returning the number of
// local commits replayed. The worktree is dirty here by design — a hunk-scoped
// ship leaves every excluded hunk in the tree — so the rebase moves work this
// ship deliberately did not take, and gitHoldWorktree gives it back rather than
// --autostash.
func gitRebaseOnto(ctx context.Context, dir render.Dir, prefix, remote, branch string) (int, error) {
	remoteRef := "refs/remotes/" + remote + "/" + branch
	countOut, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--count", remoteRef + "..HEAD"})
	if err != nil {
		return 0, fmt.Errorf(prefix+": git rev-list --count: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		return 0, fmt.Errorf(prefix+": malformed rev-list count %q: %w", countOut, err)
	}

	held, err := gitHoldWorktree(ctx, dir, prefix)
	if err != nil {
		return 0, err
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"rebase", remoteRef}); err != nil {
		return 0, errors.Join(gitRebaseFailure(ctx, dir, prefix, remote, branch, err), held.restore(ctx, dir, prefix))
	}
	if err := held.restore(ctx, dir, prefix); err != nil {
		return 0, err
	}
	return count, nil
}

// worktreeHold is the uncommitted work a rebase had to move out of the way: the
// paths it covers, and the commit it is kept as.
type worktreeHold struct {
	paths []string
	sha   string
}

// gitHoldWorktree clears the worktree for a rebase, keeping its uncommitted work
// as a commit. git stash create, never git stash push: refs/stash is shared by
// every working copy, so an entry taken back by position hands one holder
// another's work, and a bare "autostash" row names neither the branch it came
// from nor the files in it.
func gitHoldWorktree(ctx context.Context, dir render.Dir, prefix string) (worktreeHold, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"diff", "--name-only", "HEAD"})
	if err != nil {
		return worktreeHold{}, fmt.Errorf("%s: git diff --name-only HEAD: %w", prefix, err)
	}
	paths := strings.Fields(out)
	if len(paths) == 0 {
		return worktreeHold{}, nil
	}
	out, err = render.RunCLI(ctx, dir, "git", []string{"stash", "create", "ccx " + prefix + " rebase"})
	if err != nil {
		return worktreeHold{}, fmt.Errorf("%s: snapshot the uncommitted work before rebasing: %w", prefix, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return worktreeHold{}, nil
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"reset", "--hard"}); err != nil {
		return worktreeHold{}, fmt.Errorf("%s: clear the worktree for the rebase (the work is kept at %s): %w", prefix, sha, err)
	}
	return worktreeHold{paths: paths, sha: sha}, nil
}

// restore puts the held work back, and names where it is when it will not go.
func (h worktreeHold) restore(ctx context.Context, dir render.Dir, prefix string) error {
	if h.sha == "" {
		return nil
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"stash", "apply", "--index", h.sha}); err != nil {
		return fmt.Errorf("%s: the uncommitted work in %s does not apply to the rebased branch — it is kept as %s, so put it back with git stash apply %s, never git stash pop, which takes whatever sits on top of a stack every working copy shares: %w",
			prefix, strings.Join(h.paths, ", "), h.sha, h.sha, err)
	}
	return nil
}

// gitRebaseFailure classifies a failed rebase. A rebase in progress
// (REBASE_HEAD resolves) conflicted mid-replay: list, abort, report. Otherwise
// it failed before starting (hook, dirty index) — return the raw error, no
// abort. The caller puts back the work it held either way. Cleanup runs
// uncancellable.
func gitRebaseFailure(ctx context.Context, dir render.Dir, prefix, remote, branch string, rebaseErr error) error {
	cleanup := context.WithoutCancel(ctx)
	inProgress, err := gitRefExists(cleanup, dir, "ship", "REBASE_HEAD")
	if err != nil {
		return err
	}
	if !inProgress {
		return fmt.Errorf(prefix+": git rebase onto %s/%s: %w", remote, branch, rebaseErr)
	}
	files, lerr := render.RunCLI(cleanup, dir, "git", []string{"diff", "--name-only", "--diff-filter=U"})
	if _, aerr := render.RunCLI(cleanup, dir, "git", []string{"rebase", "--abort"}); aerr != nil {
		return fmt.Errorf(prefix+": rebase onto %s/%s conflicted (%w) and abort failed: %w — run: git rebase --abort, then resolve manually", remote, branch, rebaseErr, aerr)
	}
	if lerr != nil {
		return fmt.Errorf(prefix+": rebase onto %s/%s conflicted (%w); aborted back to the pre-rebase state; listing the conflicted files also failed: %w — %s", remote, branch, rebaseErr, lerr, gitRebaseRecovery(remote, branch))
	}
	conflicted := strings.Join(strings.Fields(files), ", ")
	return fmt.Errorf(prefix+": rebase onto %s/%s conflicts in: %s; aborted back to the pre-rebase state (%w) — %s", remote, branch, conflicted, rebaseErr, gitRebaseRecovery(remote, branch))
}

// gitRebaseRecovery names the way out of a rebase ccx rolled back. Every rebase
// it reports replays a branch onto its own remote counterpart, so the second
// clause holds for all of them: the conflict is spurious when the local history
// is a deliberate rewrite, and ccx vcs push moves the remote rather than
// replaying it back over the work that replaced it.
func gitRebaseRecovery(remote, branch string) string {
	return fmt.Sprintf("resolve manually: git fetch %s && git rebase --autostash %s/%s, fix the conflicts (git status), then git push %s %s; if you rewrote %s on purpose, ccx vcs push moves %s/%s onto your head under a lease instead of replaying onto it",
		remote, remote, branch, remote, branch, branch, remote, branch)
}

func jjBookmarkNames(ctx context.Context, dir render.Dir, prefix, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", jjBookmarkTemplate})
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

func jjTrunkBookmarkNames(ctx context.Context, dir render.Dir, prefix string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate})
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

func jjLogLines(ctx context.Context, dir render.Dir, prefix, rev string) ([]string, error) {
	out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", rev, "--no-graph", "-T", jjStackLineTemplate})
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

func jjOpID(ctx context.Context, dir render.Dir) (string, error) {
	out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate})
	if err != nil {
		return "", fmt.Errorf("ship: jj op log: %w", err)
	}
	opID := strings.TrimSpace(out)
	if opID == "" {
		return "", fmt.Errorf("ship: malformed jj operation ID %q", out)
	}
	return opID, nil
}

func jjRebaseOnto(ctx context.Context, dir render.Dir, target string) (int, error) {
	stack, err := jjLogLines(ctx, dir, "ship", jjStackRevset(target))
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("ship: %q..@- is empty — the commit already landed on %q; refusing to move the bookmark backwards", target, target)
	}

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"rebase", "-b", "@-", "--destination", jjBookmarksRevset(target)}); err != nil {
		return 0, fmt.Errorf("ship: jj rebase onto %q: %w", target, err)
	}
	rebaseOp, err := jjOpID(ctx, dir)
	if err != nil {
		return 0, err
	}

	// rebase -b @- rewrites every descendant of the stack, including siblings
	// of @; check the whole rewritten set without including conflicts below it.
	conflicts, err := jjLogLines(ctx, dir, "ship", jjConflictRevset(target))
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, uerr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp})
		if uerr == nil {
			return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed (rebase rolled back): %w", target, err)
		}
		return 0, fmt.Errorf("ship: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", target, err, uerr, rebaseOp)
	}
	if len(conflicts) > 0 {
		// Undo only the rebase so a conflicted @ rolls back without touching a
		// concurrent session's operations.
		if _, uerr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp}); uerr != nil {
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
func shipWatchCI(ctx context.Context, errW io.Writer, dir render.Dir, kind vcs.Kind, budget int) (string, []string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "CI gh-missing", nil, nil
	}
	sha, err := shipHeadSHA(ctx, dir, kind)
	if err != nil {
		return "", nil, err
	}
	return shipWatchCIHead(ctx, errW, dir, sha, budget)
}

func shipWatchCIHead(ctx context.Context, errW io.Writer, dir render.Dir, sha string, budget int) (string, []string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "CI gh-missing", nil, nil
	}
	runs, err := findCIRuns(ctx, dir, sha)
	if err != nil {
		report := []string{fmt.Sprintf("check: gh run list --commit %s", sha)}
		return "CI error", report, err
	}
	if len(runs) == 0 {
		hasWorkflows, err := shipHasWorkflows(dir)
		if err != nil {
			return "CI error", nil, err
		}
		if !hasWorkflows {
			return "CI no-run", nil, nil
		}
		return "CI unconfirmed", nil, fmt.Errorf("ship: no CI run was registered for the pushed commit; workflows may be paths-filtered or dispatch-only (on: workflow_dispatch); confirm manually: gh run list --commit %s", sha)
	}
	return reportCIRuns(ctx, errW, dir, sha, runs, budget)
}

// reportCIRuns watches every run for sha and, after each batch, re-lists to catch
// workflows that registered late; a run is watched, viewed, and reported exactly
// once. A settle-time re-list failure takes the infra path with the report so far
// preserved.
func reportCIRuns(ctx context.Context, errW io.Writer, dir render.Dir, sha string, runs []ciRun, budget int) (string, []string, error) {
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
			_ = watchCIRun(ctx, errW, dir, id)
			view, err := viewCIRun(ctx, dir, id)
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
		more, err := findCIRuns(ctx, dir, sha)
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
			report = append(report, ciFailureDetail(ctx, dir, r.id, r.view, per)...)
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
func watchCIRun(ctx context.Context, errW io.Writer, dir render.Dir, id string) error {
	watchCtx, cancel := context.WithTimeout(ctx, shipCIWatchTimeout)
	defer cancel()
	if shipStreamCI(errW) {
		return render.RunCLIStream(watchCtx, dir, "gh", []string{"run", "watch", id, "--exit-status", "--compact"}, errW)
	}
	_, err := render.RunCLI(watchCtx, dir, "gh", []string{"run", "watch", id, "--exit-status"})
	return err
}

func viewCIRun(ctx context.Context, dir render.Dir, id string) (ciView, error) {
	out, err := render.RunCLI(ctx, dir, "gh", []string{"run", "view", id, "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"})
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
func ciFailureDetail(ctx context.Context, dir render.Dir, id string, view ciView, budget int) []string {
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
	if log, err := render.RunCLI(ctx, dir, "gh", []string{"run", "view", id, "--log-failed"}); err != nil {
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

func shipHasWorkflows(dir render.Dir) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(string(dir), ".github", "workflows"))
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

func shipHeadSHA(ctx context.Context, dir render.Dir, kind vcs.Kind) (string, error) {
	switch kind {
	case vcs.Git:
		out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "HEAD"})
		if err != nil {
			return "", fmt.Errorf("ship: git rev-parse HEAD: %w", err)
		}
		return strings.TrimSpace(out), nil
	case vcs.JJ:
		out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id"})
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
func findCIRuns(ctx context.Context, dir render.Dir, sha string) ([]ciRun, error) {
	var lastErr error
	for i := 0; i < shipCIPollTries; i++ {
		out, err := render.RunCLI(ctx, dir, "gh", []string{"run", "list", "--commit", sha, "--limit", "50", "--json", "databaseId,workflowName,status,url"})
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
