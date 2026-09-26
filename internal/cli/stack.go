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
files unmaterialized instead. --path names a new location outside both checkouts.
Sparse and no-checkout creation require a Git checkout.

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
	cmd.Flags().StringVar(&options.path, "path", "", "new destination outside the source and main checkout")
	cmd.MarkFlagsMutuallyExclusive("sparse", "no-checkout")
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
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Restack every lane, then submit the whole stack",
		Long: `Rebase this lane's branches, then push and submit them.

Submit runs ccx vcs stack rebase over this lane's branches: it fetches the remote
trunk and replays each branch from its recorded base onto that trunk. The local
trunk branch and other working copies are left untouched. A branch whose pull
request landed through a merge queue squash is dropped, and its children move
onto what it sat on, leaving its squashed commits behind.

A conflict stops the run before any ref moves, in a conflict workspace with
rerere off. After resolution, ccx vcs stack continue finishes the rebase, pushes,
and submits; ccx vcs stack abort drops the run. A moved branch held by another
working copy, or uncommitted work in the invoking checkout, stops publication
before any branch moves.

A branch another working copy has checked out is that lane's, so it is skipped
and named with the working copy holding it, along with every branch stacked
above it; --include submits it anyway.

Above the branch checked out here, a branch belongs to another lane when the
rest of the record contradicts its gt parent: its open pull request is based on
neither that parent nor the branch a landed parent leaves it on, or its history
carries none of the parent's own commits. It and everything stacked on it are
left alone and named on stderr with the step to re-record the parent.

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
			return runStackSubmit(cmd, o, include)
		},
	}
	cmd.Flags().BoolVar(&o.draft, "draft", false, "open new PRs as drafts")
	cmd.Flags().StringArrayVar(&o.prTitle, "pr-title", nil, "title for a pull request: <branch>=<title>, or a bare title for the branch checked out here (repeatable)")
	cmd.Flags().StringArrayVar(&o.prBodyFile, "pr-body-file", nil, "body file for a pull request: <branch>=<path>, or a bare path for the branch checked out here; - reads stdin (repeatable)")
	cmd.Flags().StringArrayVar(&include, "include", nil, "submit this branch even though another working copy has it checked out (repeatable)")
	cmd.Flags().StringArrayVar(&o.landed, "landed", nil, "treat <branch> as landed and drop it (repeatable)")
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
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
	state, err := gtStateQuery(ctx, dir, prefix)
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

func runStackSubmit(cmd *cobra.Command, o shipOpts, include []string) error {
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
	intent, cleanup, err := stackSubmitIntent(cmd, l, o, stack)
	defer cleanup()
	if err != nil {
		return err
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("stack submit: %w", err)
	}
	chain, skipped, err := stackOwnBranches(stack, stackState, holders, l.checkout.Root, include)
	if err != nil {
		return err
	}
	if err := stackAnnounceSkipped(errW, skipped); err != nil {
		return err
	}
	return runStackRebase(cmd, stackRebaseOpts{members: chain, landed: o.landed, draft: o.draft, ship: intent, submit: true, dropCommits: o.dropCommits})
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

type stackSkip struct {
	branch string
	holder string
	on     string
}

func stackOwnBranches(stack []string, state gtState, holders map[string]string, root string, include []string) ([]string, []stackSkip, error) {
	for _, name := range include {
		if !slices.Contains(stack, name) {
			return nil, nil, fmt.Errorf("stack submit: --include %s names no branch of this stack (%s)", name, strings.Join(stack, ", "))
		}
	}
	skip := map[string]bool{}
	var own []string
	var skipped []stackSkip
	for _, branch := range stack {
		parent := state[branch].Parents[0].Ref
		holder := holders[branch]
		switch {
		case skip[parent]:
			skip[branch] = true
			skipped = append(skipped, stackSkip{branch: branch, on: parent})
		case holder != "" && holder != root && !slices.Contains(include, branch):
			skip[branch] = true
			skipped = append(skipped, stackSkip{branch: branch, holder: holder})
		default:
			own = append(own, branch)
		}
	}
	return own, skipped, nil
}

func stackAnnounceSkipped(errW io.Writer, skipped []stackSkip) error {
	if len(skipped) == 0 {
		return nil
	}
	named := make([]string, 0, len(skipped))
	for _, s := range skipped {
		if s.holder != "" {
			named = append(named, fmt.Sprintf("%s (checked out in %s)", s.branch, s.holder))
			continue
		}
		named = append(named, fmt.Sprintf("%s (stacked on %s)", s.branch, s.on))
	}
	if _, err := fmt.Fprintf(errW, "stack submit: skipping %s — another lane owns them; pass --include <branch> to submit one anyway\n", strings.Join(named, ", ")); err != nil {
		return fmt.Errorf("stack submit: name the skipped branches: %w", err)
	}
	return nil
}
