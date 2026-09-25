package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	var parent string
	cmd := &cobra.Command{
		Use:   "new <branch>",
		Short: "Cut a branch stacked on this one, in a working copy of its own",
		Long: `Cut a branch named <branch> stacked on this one, in a working copy of its own.

The branch is created directly in the new working copy, so the one you run this
from never changes branch — which is what makes a stack workable by several
agents at once, one lane each. The path is minted under the repository's pool,
gt adopts the branch onto --parent (the branch checked out here by default), and
the new working copy's path is the last thing printed, ready to hand to whoever
works the lane. A slash in the branch name, as in owner/topic, becomes a dash in
the working copy's name.

A lane cut onto trunk starts at the remote trunk, fetched first, and the local
trunk branch is fast-forwarded onto it; a local trunk holding commits the remote
does not is refused.

In a jj repository the lane is a git worktree carrying its own colocated jj, cut
with "jj git init --git-repo .": every lane then answers to git, gt and jj alike.
A jj workspace would not — it has no .git for gt to read.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStackNew(cmd, args[0], parent)
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "branch to stack the new one on (default: the branch checked out here)")
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
	cmd := &cobra.Command{
		Use:   "submit",
		Short: "Restack every lane, then submit the whole stack",
		Long: `Restack every lane, then submit the whole stack.

A submit pushes each branch onto the parent gt records for it, so a stack spread
across working copies has to be restacked in each of them first — which is the
sweep ccx vcs stack restack runs. This does both, in that order, so the submit
meets a stack that is already in the shape Graphite expects.

The trunk is fetched once, up front, and that one commit both restacks every
lane and anchors the submit: the local trunk branch is fast-forwarded onto it,
since gt measures a restack against the local ref, and the report names the
commit pinned. A local trunk holding commits the remote does not is refused
rather than restacked onto, because a restack would splice them into every
branch of the stack. No ref moves until every branch kept has replayed. A branch
that conflicts is left where it was, along with everything stacked on it, and
named with the ccx vcs stack rebase that resolves it; the rest of the stack is
restacked and submitted, and the command still exits non-zero.

A branch whose pull request already landed — the merge queue's squash leaves its
head out of trunk's history — is dropped: its children move onto its parent and
replay their own commits alone, and gt forgets it. A branch rebased outside gt
has its recorded base moved to where it sits, and a replay leaves out the trunk
commits a stale record reaches back over; one carrying a copy of its parent's
commit under another sha is refused.

The submit itself is ccx vcs ship's: dropping the branches that trunk already
holds and naming them, and the ones whose open pull request already carries
exactly this head and base, then one atomic push moving every branch left, each
under the lease of its last submitted version, then one post to Graphite's API
per branch, bottom-up.

A branch above this one whose open pull request targets another base, or that
carries none of its recorded parent's commits, belongs to another lane: it is
named and left alone, along with everything stacked on it. A tracked branch
with no commit of its own yet is skipped rather than refused.

--pr-title and --pr-body-file restate the pull requests the submit leaves open,
as ccx vcs ship's do: <branch>=<value> names a branch of the stack, and a bare
value names the branch checked out here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackSubmit(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.draft, "draft", false, "open new PRs as drafts")
	cmd.Flags().StringArrayVar(&o.prTitle, "pr-title", nil, "title for a pull request: <branch>=<title>, or a bare title for the branch checked out here (repeatable)")
	cmd.Flags().StringArrayVar(&o.prBodyFile, "pr-body-file", nil, "body file for a pull request: <branch>=<path>, or a bare path for the branch checked out here; - reads stdin (repeatable)")
	return cmd
}

func runStackNew(cmd *cobra.Command, name, parent string) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "stack new", workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack new: this repository is not on the graphite lane, and a stack is Graphite's — run gt init, or cut a plain working copy with ccx vcs worktree add")
	}
	if parent == "" {
		if parent, err = gitCurrentBranch(ctx, l.dir(), "stack new"); err != nil {
			return err
		}
	}
	if parent == "" {
		return errors.New("stack new: HEAD is detached here, so there is no branch to stack on — check one out, or name it with --parent")
	}
	start, err := stackNewStart(ctx, l, parent)
	if err != nil {
		return err
	}
	path, err := mintWorktreePath("stack new", l.checkout, strings.ReplaceAll(name, "/", "-"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("stack new: mint pool for %q: %w", name, err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "add", "-b", name, path, start}); err != nil {
		return fmt.Errorf("stack new: git worktree add %s: %w", path, err)
	}
	if err := stackFormLane(ctx, cmd.ErrOrStderr(), l, render.Dir(path), name, parent); err != nil {
		return errors.Join(err, stackUnwindLane(ctx, l.dir(), path, name))
	}
	cmd.Println(strings.Join([]string{"cut " + name + " onto " + parent, path}, shipSep))
	return nil
}

// stackNewStart is the commit a new lane is cut from. A lane on trunk starts at
// the freshly fetched remote trunk, with the local trunk fast-forwarded onto it,
// so gt tracks it without a restack pending; a local trunk the remote cannot
// fast-forward is refused rather than cut from.
func stackNewStart(ctx context.Context, l lane, parent string) (string, error) {
	state, err := gtStateQuery(ctx, l.dir(), "stack new")
	if err != nil {
		return "", err
	}
	trunk, err := gtTrunkBranch("stack new", state)
	if err != nil {
		return "", err
	}
	if parent != trunk {
		return parent, nil
	}
	tr, err := gtTrunkRef(ctx, l.dir(), "stack new", trunk)
	if err != nil {
		return "", err
	}
	pin, err := gtTrunkPin(ctx, "stack new", l.checkout, l.dir(), tr, state[trunk].Head)
	if err != nil {
		return "", fmt.Errorf("stack new: %w", err)
	}
	if pin.diverged > 0 {
		return "", fmt.Errorf("stack new: %w", &errTrunkDiverged{Trunk: trunk, Remote: string(tr.Ref()), Ahead: pin.diverged})
	}
	return pin.sha, nil
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
	return gtTrackAt(ctx, path, errW, parent)
}

// stackUnwindLane takes back a lane that only half formed. Left in place it
// holds both the name and the branch, so the same stack new refuses on a retry
// and the fix is two git commands nobody was told about.
func stackUnwindLane(ctx context.Context, root render.Dir, path, name string) error {
	if _, err := render.RunCLI(ctx, root, "git", []string{"worktree", "remove", "--force", path}); err != nil {
		return fmt.Errorf("stack new: remove the half-formed lane at %s: %w", path, err)
	}
	if _, err := render.RunCLI(ctx, root, "git", []string{"branch", "-D", name}); err != nil {
		return fmt.Errorf("stack new: delete the half-formed branch %s: %w", name, err)
	}
	return nil
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
	r, runErr := gtRun(ctx, dir, []string{"track", "--parent", parent, "--no-interactive"}, gtZeroFatal, errW)
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

func runStackSubmit(cmd *cobra.Command, o shipOpts) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()
	l, err := resolveLane(ctx, "stack submit", workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack submit: this repository is not on the graphite lane, and a stack is Graphite's — ship the branch with ccx vcs ship instead")
	}
	chain, _, err := gtStackAll(ctx, l.dir(), "stack submit")
	if err != nil {
		return err
	}
	prCleanup, err := materializePRBodyStdin(cmd, &o)
	defer prCleanup()
	if err != nil {
		return err
	}
	current, err := gitCurrentBranch(ctx, l.dir(), "stack submit")
	if err != nil {
		return err
	}
	meta, err := resolvePRMeta(cmd, o, current)
	if err != nil {
		return err
	}
	for branch, m := range meta {
		if len(m.stated()) > 0 && !slices.Contains(chain, branch) {
			return fmt.Errorf("stack submit: --pr-title/--pr-body-file named %s, which is not in this stack", branch)
		}
	}
	sub := gtSubmit{prefix: "stack submit", draft: o.draft}
	pass, err := gtStackRestack(ctx, errW, l, sub, chain)
	if err != nil {
		return err
	}
	for branch, m := range meta {
		if len(m.stated()) > 0 && !slices.Contains(pass.chain, branch) && !pass.refuses(branch) {
			return fmt.Errorf("stack submit: --pr-title/--pr-body-file named %s, which this submit leaves out", branch)
		}
	}
	commits, files, err := gtSubmitWidth(ctx, "stack submit", l.dir(), pass.tr, pass.chain)
	if err != nil {
		return err
	}
	submitted, entries, err := gtSubmitStack(ctx, l, errW, sub, pass.commonDir, pass.state, pass.tr, pass.chain, pass.prs, "")
	if err != nil {
		return err
	}
	stack := make([]stackEntry, 0, len(pass.chain))
	for _, branch := range pass.chain {
		entry := entries[branch]
		entry.Branch = branch
		stack = append(stack, entry)
	}
	restated, err := shipPRGT(ctx, pass.prs.owner+"/"+pass.prs.name, meta, stack)
	if err != nil {
		return err
	}
	segments := []string{gtRestackSegment(pass.result)}
	if seg := gtLandedSegment(pass.landed); seg != "" {
		segments = append(segments, seg)
	}
	if restated != "" {
		segments = append(segments, restated)
	}
	segments = append(segments,
		fmt.Sprintf("submitted %d branches", len(submitted)),
		"trunk "+pass.pin.String(),
		fmt.Sprintf("proposing %d commit(s), %d file(s)", commits, files),
	)
	cmd.Println(strings.Join(segments, shipSep))
	if len(pass.refused) > 0 {
		return gtRefusedErr("stack submit", pass.refused)
	}
	return nil
}
