package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackListEntry struct {
	Branch       string `json:"branch"`
	Path         string `json:"path"`
	Current      bool   `json:"current"`
	NeedsRestack bool   `json:"needs_restack"`
	State        string `json:"state"`
}

type stackListReport struct {
	Root     string           `json:"root"`
	Branches []stackListEntry `json:"branches"`
}

func newStackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Work a Graphite stack that spans one working copy per branch",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newStackNewCmd(),
		newStackRepairPublishedChildCmd(),
		newStackListCmd(),
		newRestackCmd(),
		newStackSubmitCmd(),
		newStackDropCmd(),
		newStackRebaseCmd(),
		newStackContinueCmd(),
		newStackRegenerateCmd(),
		newStackAbortCmd(),
	)
	return cmd
}

func newStackNewCmd() *cobra.Command {
	var options stackNewOpts
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Cut a branch stacked on this one, in a working copy of its own",
		Long: `Cut a branch named <name> stacked on this one, in a working copy of its own.

The branch is created directly in the new working copy, so the one you run this
from never changes branch — which is what makes a stack workable by several
agents at once, one lane each. The path is minted under the repository's pool,
gt adopts the branch onto --parent (the branch checked out here by default), and
the new working copy's path is the last thing printed, ready to hand to whoever
works the lane. A slash in the branch name becomes a dash in the working copy's
directory name: owner/topic becomes owner-topic. A lane cut onto trunk starts at
the freshly fetched remote trunk, leaving the local trunk branch where it is.

--published-parent uses the parent's recorded publication only when its source,
remote head, and submission metadata still match. --sparse copies this checkout's
per-worktree sparse configuration before populating the child. --no-checkout leaves
files unmaterialized instead. --include checks out more directories in a sparse
lane. --path names a new location outside the checkout you run this from, its
main checkout, and the thin store. Sparse, no-checkout, and thin creation require
a Git checkout.

--thin cuts the lane into the repository's thin store: one clone per repository
under $HOME/.claude/stores, with --depth commits of trunk history (256 unless the
store already exists), no blobs until a checkout needs them, no tags, a fetch of
trunk alone, and a sparse checkout of root files plus this checkout's sparse set.
The first thin lane creates the store; a lane cut from a checkout of the store is
a linked worktree of it whatever the flags say. A parent the store does not hold
needs --published-parent: its publication is verified against this checkout and
the remote, and the store takes it frozen at its published head, so no submit
from the store rewrites it. When its published base lies past the store's
history, --deepen fetches trunk history down to it, never more than --max-depth
commits; without it the lane is refused. --full-history cuts the lane from this
checkout's own full history instead, and is refused in a thin store.
CCX_STACK_NEW=thin or full picks the default when neither flag is given.

Outside the graphite lane the branch is cut the same way and nothing records
its parent: ship opens its pull request against trunk, and a restack replays it
onto its open pull request's base, so retargeting that base stacks it.

In a jj repository the lane is a git worktree carrying its own colocated jj, cut
with "jj git init --git-repo .": every lane then answers to git, gt and jj alike.
A jj workspace would not — it has no .git for gt to read.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackNew(cmd, args[0], options)
		},
	}
	cmd.Flags().StringVar(&options.parent, "parent", "", "branch to stack the new one on (default: the branch checked out here)")
	cmd.Flags().BoolVar(&options.published, "published-parent", false, "start at the parent's verified publication receipt")
	cmd.Flags().BoolVar(&options.sparse, "sparse", false, "inherit this checkout's sparse patterns before materializing files")
	cmd.Flags().BoolVar(&options.noCheckout, "no-checkout", false, "create the tracked child without materializing files")
	cmd.Flags().StringVar(&options.path, "path", "", "new destination outside the source checkout, its main checkout, and the thin store")
	cmd.Flags().BoolVar(&options.thin, "thin", false, "cut the lane into the repository's shallow, partial, sparse thin store")
	cmd.Flags().BoolVar(&options.fullHistory, "full-history", false, "cut the lane from this checkout's full history, overriding "+stackNewEnv)
	cmd.Flags().BoolVar(&options.deepen, "deepen", false, "fetch trunk history down to a published parent's base the thin store lacks")
	cmd.Flags().IntVar(&options.depth, "depth", thinDefaultDepth, "commits of trunk history a new thin store starts with")
	cmd.Flags().IntVar(&options.maxDepth, "max-depth", thinDefaultMaxDepth, "most commits --deepen may fetch")
	cmd.Flags().StringArrayVar(&options.includes, "include", nil, "also check out this directory in a sparse lane (repeatable)")
	cmd.MarkFlagsMutuallyExclusive("sparse", "no-checkout")
	cmd.MarkFlagsMutuallyExclusive("thin", "full-history")
	cmd.MarkFlagsMutuallyExclusive("thin", "no-checkout")
	cmd.PreRun = func(cmd *cobra.Command, _ []string) {
		options.depthSet = cmd.Flags().Changed("depth")
	}
	return cmd
}

func newStackListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the stack, naming the working copy holding each branch",
		Long: `List the stack, naming the working copy holding each branch.

With --json, emit root and a bottom-up branches array. Each branch carries its
name, path (empty when no working copy holds it), current flag, needs_restack
flag, and Graphite state (including frozen).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackList(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the listing as JSON")
	return cmd
}

func newStackSubmitCmd() *cobra.Command {
	var o shipOpts
	var include []string
	var to string
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Restack every lane, then submit the whole stack",
		Long: `Rebase this lane's branches, then push and submit them.

Submit runs ccx vcs stack rebase over this lane's branches: it fetches the remote
trunk and replays each branch from its recorded base onto that trunk. The local
trunk branch and other working copies are left untouched. A branch whose pull
request landed through a merge queue squash is dropped, and its children move
onto what it sat on, leaving its squashed commits behind.
A branch whose pull request was closed without landing is dropped the same way,
named in the plan with its pull request, and its own commits are never replayed.
A pull request GitHub closed because its base branch was deleted still carries
live work, so before the push the run puts that base back, reopens the pull
request, retargets it onto the branch's new parent, and deletes the base again.
A new parent the remote does not carry yet is refused, naming it.

A conflict stops the run before any ref moves, in a conflict workspace with
rerere off. After resolution, ccx vcs stack continue finishes the rebase, pushes,
and submits; ccx vcs stack abort drops the run. A moved branch held by another
working copy, or uncommitted work in the invoking checkout, stops publication
before any branch moves.

A branch another working copy has checked out is that lane's, so it is skipped
and named with the working copy holding it, along with every branch stacked
above it; --include submits it anyway. One whose pull request landed is no
lane's any more: it is dropped like any landed branch, and the branches on it
move onto trunk.

A branch whose name differs from the checked-out branch's before the last slash
is another lane's too: one this lane sits on is kept at its published head,
neither pushed nor submitted, and named on stderr; the rest are left out.
--all-lanes, or --include for one branch, submits them anyway.

Above the branch checked out here, a branch belongs to another lane when the
rest of the record contradicts its gt parent: its open pull request is based on
neither that parent nor the branch a landed parent leaves it on, or its history
carries none of the parent's own commits. It and everything stacked on it are
left alone and named on stderr with the step to re-record the parent.
--to <branch> stops the run at <branch>, leaving every branch stacked above it
out; a <branch> that is neither the one checked out here nor stacked above it
is refused.

A tracked branch with no commit past the parent revision gt recorded is an
empty lane nobody has committed to yet. It and everything stacked on it are
left where they are, neither dropped nor forgotten by gt, and reported as
"skipped empty <branch>".

Every remaining branch is force-pushed in one atomic push under the lease of
its last submitted version, then posted to Graphite's API one branch at a time,
bottom-up. --pr-title and --pr-body-file take ship's <branch>=<value> form; a bare
value names the branch checked out here. They restate those pull requests after
the push and survive a conflict stop. Naming a branch outside the stack, or one
the run leaves out, is refused before anything moves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackSubmit(cmd, o, include, to)
		},
	}
	cmd.Flags().BoolVar(&o.draft, "draft", false, "open new PRs as drafts")
	cmd.Flags().StringArrayVar(&o.prTitle, "pr-title", nil, "title for a pull request: <branch>=<title>, or a bare title for the branch checked out here (repeatable)")
	cmd.Flags().StringArrayVar(&o.prBodyFile, "pr-body-file", nil, "body file for a pull request: <branch>=<path>, or a bare path for the branch checked out here; - reads stdin (repeatable)")
	cmd.Flags().StringArrayVar(&include, "include", nil, "submit this branch even though another working copy has it checked out or it is another lane's (repeatable)")
	cmd.Flags().BoolVar(&o.allLanes, "all-lanes", false, "submit the branches of other lanes too, a lane being the branch name before its last slash")
	cmd.Flags().StringArrayVar(&o.landed, "landed", nil, "treat <branch> as landed and drop it (repeatable)")
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
	cmd.Flags().StringVar(&to, "to", "", stackToUsage)
	return cmd
}

// stackFormLane finishes a lane the worktree already exists for: its own
// colocated jj where the repository has one, then the Graphite adoption without
// which no restack or submit reaches the branch.
func stackFormLane(ctx context.Context, errW io.Writer, l lane, path render.Dir, name, parent string) error {
	if l.checkout.Kind == vcs.JJ {
		if err := stackColocateJJ(ctx, path, name); err != nil {
			return err
		}
	}
	if !l.gt {
		return nil
	}
	return gtTrackAt(ctx, path, errW, parent)
}

// stackColocateJJ gives a lane its own colocated jj. --colocate is refused
// inside a git worktree and a bare jj git init takes the same path, so the
// repository is named instead: --git-repo . resolves to the worktree's own
// gitdir pointer and colocates against the repository behind it, which is the
// one spelling jj accepts here.
//
// jj then points git's HEAD at the working-copy commit's parent, as colocation
// does everywhere, and gt has no branch to read from a detached HEAD. The files
// on disk are already the branch's, so HEAD is re-attached by name rather than
// by checkout.
func stackColocateJJ(ctx context.Context, path render.Dir, name string) error {
	if _, err := render.RunCLI(ctx, path, "jj", []string{"git", "init", "--git-repo", "."}); err != nil {
		return fmt.Errorf("stack new: jj git init --git-repo . in %s: %w", path, err)
	}
	if _, err := render.RunCLI(ctx, path, "git", []string{"symbolic-ref", "HEAD", "refs/heads/" + name}); err != nil {
		return fmt.Errorf("stack new: re-attach HEAD to %s in %s: %w", name, path, err)
	}
	return nil
}

// gtTrackAt adopts the branch a lane holds onto its parent, from inside that
// lane: gt reads the branch to track from the working copy it runs in.
func gtTrackAt(ctx context.Context, dir render.Dir, errW io.Writer, parent string) error {
	r, runErr := gtRun(ctx, dir, []string{"track", "--parent", parent, "--no-interactive"}, errW)
	if err := gtReport(ctx, errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("stack new: gt track --parent %s: %w", parent, runErr)
	}
	return nil
}

func runStackList(cmd *cobra.Command, asJSON bool) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "stack list", workingDir(ctx), true, false)
	if err != nil {
		return err
	}
	stack, state, err := gtStackAll(ctx, l.dir(), "stack list")
	if err != nil {
		return err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("stack list: %w", err)
	}
	if asJSON {
		report := stackListReport{Root: l.checkout.Root, Branches: make([]stackListEntry, 0, len(stack))}
		for _, branch := range stack {
			report.Branches = append(report.Branches, stackListEntry{
				Branch:       branch,
				Path:         holders[branch],
				Current:      holders[branch] == l.checkout.Root,
				NeedsRestack: state[branch].NeedsRestack,
				State:        state[branch].State,
			})
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("stack list: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	for _, branch := range stack {
		cmd.Println(stackListLine(branch, holders[branch], l.checkout.Root, state[branch]))
	}
	return nil
}

// gtStackAll lists the whole stack the current branch belongs to, bottom-up:
// its downstack to trunk, then everything tracked above it. The upstack half is
// gt state's parent map read backwards, and it is the half that matters here —
// with a working copy per branch, the branches above this one are exactly the
// ones this working copy cannot check out to ask about.
func gtStackAll(ctx context.Context, dir render.Dir, prefix string) ([]string, gtState, error) {
	branch, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, nil, err
	}
	if branch == "" {
		return nil, nil, fmt.Errorf("%s: detached HEAD; no stack to resolve", prefix)
	}
	state, err := gtStateQueryFocused(ctx, dir, prefix, branch)
	if err != nil {
		return nil, nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, nil, err
	}
	if branch == trunk {
		return nil, nil, fmt.Errorf("%s: %s is trunk, and every stack in the repository sits on it — check out a branch of the one you mean", prefix, trunk)
	}
	stack, err := gtDownstack(prefix, state, branch, trunk)
	if untracked := (*errGTUntracked)(nil); errors.As(err, &untracked) {
		return nil, nil, gtUntrackedRefusal(ctx, dir, prefix, state, trunk, branch)
	}
	if err != nil {
		return nil, nil, err
	}
	slices.Reverse(stack)
	up, err := gtUpstack(prefix, state, branch)
	if err != nil {
		return nil, nil, err
	}
	return append(stack, up...), state, nil
}

// gtUntrackedRefusal names the repair for a branch gt never tracked: the
// parent gt track would adopt it onto, so the refusal is one command from done.
func gtUntrackedRefusal(ctx context.Context, dir render.Dir, prefix string, state gtState, trunk, branch string) error {
	parent, err := gtNearestTracked(ctx, dir, state, trunk, branch)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s: %s is not tracked by Graphite, so it sits on no stack — adopt it with gt track %s --parent %s (its nearest tracked ancestor), then rerun", prefix, branch, branch, parent)
}

// gtStackUpTo narrows gtStackAll's stack to the branches from trunk up to and
// including to, refused unless to is the current branch or stacked above it.
func gtStackUpTo(ctx context.Context, dir render.Dir, prefix string, state gtState, to string) ([]string, error) {
	current, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, err
	}
	down, err := stackUpTo(prefix, state, trunk, []string{current}, to)
	if err != nil {
		return nil, err
	}
	return gtBottomUp(down), nil
}

// stackListLine reads bottom-up, one branch per line, naming the working copy
// holding it — the answer to which lane a branch has to be worked from. A branch
// no working copy holds is named as such rather than left blank, since "nowhere"
// is the fact that decides whether a lane has to be cut for it.
func stackListLine(branch, holder, root string, state gtBranchState) string {
	where := "no working copy"
	switch holder {
	case "":
	case root:
		where = "here"
	default:
		where = holder
	}
	fields := []string{branch, where}
	if state.NeedsRestack {
		fields = append(fields, "needs restack")
	}
	return strings.Join(fields, shipSep)
}

func runStackSubmit(cmd *cobra.Command, o shipOpts, include []string, to string) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()
	l, err := resolveLane(ctx, "stack submit", workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack submit: this repository is not on the graphite lane, and a stack is Graphite's — ship the branch with ccx vcs ship instead")
	}
	stack, stackState, err := gtStackAll(ctx, l.dir(), "stack submit")
	if err != nil {
		return err
	}
	if to != "" {
		if stack, err = gtStackUpTo(ctx, l.dir(), "stack submit", stackState, to); err != nil {
			return err
		}
	}
	intent, cleanup, err := stackSubmitIntent(cmd, l, o, stack)
	defer cleanup()
	if err != nil {
		return err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("stack submit: %w", err)
	}
	landed, err := stackLandedElsewhere(ctx, l, stack, stackState, holders, o.landed)
	if err != nil {
		return err
	}
	chain, pinned, skipped, err := stackOwnBranches(stack, stackState, holders, l.checkout.Root, append(slices.Clone(include), landed...))
	if err != nil {
		return err
	}
	if err := stackAnnounceSkipped(errW, pinned, skipped); err != nil {
		return err
	}
	return runStackRebase(cmd, stackRebaseOpts{members: chain, pinned: stackSkipNames(pinned), landed: o.landed, draft: o.draft, ship: intent, submit: true, dropCommits: o.dropCommits, to: to, include: include, otherLanes: o.allLanes})
}

// stackSubmitIntent carries --pr-title and --pr-body-file into the run as a ship
// intent, so the pull requests are restated once the stack is pushed, even
// when a conflict stops the run and ccx vcs stack continue finishes it. No
// field named means no intent and no restate.
func stackSubmitIntent(cmd *cobra.Command, l lane, o shipOpts, stack []string) (*stackShipIntent, func(), error) {
	ctx := cmd.Context()
	cleanup, err := materializePRBodyStdin(cmd, &o)
	if err != nil {
		return nil, cleanup, err
	}
	current, err := gitCurrentBranch(ctx, l.dir(), "stack submit")
	if err != nil {
		return nil, cleanup, err
	}
	meta, err := resolvePRMeta(cmd, o, current)
	if err != nil {
		return nil, cleanup, err
	}
	for _, branch := range slices.Sorted(maps.Keys(meta)) {
		if len(meta[branch].stated()) == 0 {
			delete(meta, branch)
			continue
		}
		if !slices.Contains(stack, branch) {
			return nil, cleanup, fmt.Errorf("stack submit: --pr-title/--pr-body-file named %s, which is not in this stack", branch)
		}
		m := meta[branch]
		m.draft = nil
		meta[branch] = m
	}
	if len(meta) == 0 {
		return nil, cleanup, nil
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if err != nil {
		return nil, cleanup, fmt.Errorf("stack submit: restating a pull request needs GitHub metadata: %w", err)
	}
	intent, err := stackShipOptions(o, meta, repo.NameWithOwner, current)
	if err != nil {
		return nil, cleanup, fmt.Errorf("stack submit: %w", err)
	}
	intent.NoWatch = true
	return intent, cleanup, nil
}

func stackLandedElsewhere(ctx context.Context, l lane, stack []string, state gtState, holders map[string]string, declared []string) ([]string, error) {
	var held []string
	for _, branch := range stack {
		if holder := holders[branch]; holder != "" && holder != l.checkout.Root {
			held = append(held, branch)
		}
	}
	if len(held) == 0 {
		return nil, nil
	}
	trunk, err := gtTrunkBranch("stack submit", state)
	if err != nil {
		return nil, err
	}
	prs, err := stackPRs(ctx, l.dir(), trunk, held)
	if err != nil {
		return nil, fmt.Errorf("stack submit: read the pull requests of the branches other working copies hold: %w", err)
	}
	return slices.DeleteFunc(held, func(branch string) bool {
		return !slices.Contains(declared, branch) && (prs[branch] == nil || !prs[branch].Landed)
	}), nil
}

type stackSkip struct {
	branch string
	holder string
}

func stackSkipNames(skips []stackSkip) []string {
	names := make([]string, 0, len(skips))
	for _, s := range skips {
		names = append(names, s.branch)
	}
	return names
}

// stackOwnBranches keeps a held branch an own branch sits on in the run,
// pinned at its published head, so the own branch lands on it without the run
// replaying or pushing another lane's work; a held branch nothing own sits on
// is left out.
func stackOwnBranches(stack []string, state gtState, holders map[string]string, root string, include []string) (members []string, pinned, skipped []stackSkip, err error) {
	for _, name := range include {
		if !slices.Contains(stack, name) {
			return nil, nil, nil, fmt.Errorf("stack submit: --include %s names no branch of this stack (%s)", name, strings.Join(stack, ", "))
		}
	}
	held := map[string]string{}
	for _, branch := range stack {
		if holder := holders[branch]; holder != "" && holder != root && !slices.Contains(include, branch) {
			held[branch] = holder
		}
	}
	pins := map[string]bool{}
	for _, branch := range stack {
		if held[branch] != "" {
			continue
		}
		for parent := state[branch].Parents[0].Ref; held[parent] != "" && !pins[parent]; parent = state[parent].Parents[0].Ref {
			pins[parent] = true
		}
	}
	for _, branch := range stack {
		switch {
		case pins[branch]:
			members = append(members, branch)
			pinned = append(pinned, stackSkip{branch: branch, holder: held[branch]})
		case held[branch] != "":
			skipped = append(skipped, stackSkip{branch: branch, holder: held[branch]})
		default:
			members = append(members, branch)
		}
	}
	return members, pinned, skipped, nil
}

func stackAnnounceSkipped(errW io.Writer, pinned, skipped []stackSkip) error {
	for _, group := range []struct {
		skips []stackSkip
		verb  string
	}{
		{pinned, "keeping %s at their published heads"},
		{skipped, "skipping %s"},
	} {
		if len(group.skips) == 0 {
			continue
		}
		named := make([]string, 0, len(group.skips))
		for _, s := range group.skips {
			named = append(named, fmt.Sprintf("%s (checked out in %s)", s.branch, s.holder))
		}
		if _, err := fmt.Fprintf(errW, "stack submit: "+group.verb+" — another lane owns them; pass --include <branch> to submit one anyway\n", strings.Join(named, ", ")); err != nil {
			return fmt.Errorf("stack submit: name the skipped branches: %w", err)
		}
	}
	return nil
}
