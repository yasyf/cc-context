package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	// gtSkipMerged is the decline that means the branch has nowhere left to go.
	gtSkipMerged = "already merged"

	jjRestackAncestorRevset = "trunk() & ::@"
	jjRestackStackRevset    = "trunk()..@"
	jjRestackConflictRevset = "conflicts() & @::"
)

var errRestackDetached = errors.New("restack: detached HEAD — check out a branch before restacking")

// errRestackBehind is a pass that ended with a branch still off trunk, which
// exit 0 would let a caller take for a current base.
type errRestackBehind struct {
	Trunk    string
	Branches []string
	Summary  string
}

func (e *errRestackBehind) Error() string {
	return fmt.Sprintf("restack: %s still behind %s: %s — %s; re-run once the cause above is cleared, or move them by hand with gt restack --only --branch <b>",
		gtBranchCount(len(e.Branches)), e.Trunk, strings.Join(e.Branches, ", "), e.Summary)
}

func gtBranchCount(n int) string {
	if n == 1 {
		return "1 branch"
	}
	return fmt.Sprintf("%d branches", n)
}

type restackOpts struct {
	noGT bool
}

func newRestackCmd() *cobra.Command {
	var o restackOpts
	cmd := &cobra.Command{
		Use:   "restack",
		Short: "Fetch and restack the working-copy stack onto trunk",
		Long: `Fetch and restack the working-copy stack onto trunk.

On the gt lane every branch of the stack is replayed off its parent. jj rebases
the working-copy stack onto trunk() and rolls a conflict back. On plain git,
trunk itself is fast-forwarded, and any other branch is replayed onto the
fetched trunk without a checkout, then every working copy holding it is reset
onto the new head with its uncommitted work carried over. A conflict moves
nothing: the rebase stops in a conflict-<branch> workspace with rerere off, and
ccx vcs stack continue finishes it once the files are resolved and added, or
ccx vcs stack abort drops it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestack(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	return cmd
}

func runRestack(cmd *cobra.Command, o restackOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "restack", workingDir(ctx), o.noGT)
	if err != nil {
		return err
	}
	if l.gt {
		summary, err := restackGT(ctx, l, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		cmd.Println(summary)
		return nil
	}

	var summary string
	switch l.kind {
	case vcs.JJ:
		summary, err = restackJJ(ctx, l.dir())
	case vcs.Git:
		summary, err = restackGit(ctx, cmd, l)
	default:
		panic(fmt.Sprintf("restack: unsupported vcs kind %d", l.kind))
	}
	if err != nil || summary == "" {
		return err
	}
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
}

// restackGT re-reads the stack after gtRestackLanded, which moves every branch
// stacked on a landed parent onto the first ancestor that has not landed.
func restackGT(ctx context.Context, l lane, errW io.Writer) (string, error) {
	commonDir, err := gtCommonDir(ctx, l.dir(), "restack")
	if err != nil {
		return "", err
	}
	state, err := gtStateAt(ctx, commonDir, "restack")
	if err != nil {
		return "", err
	}
	trunk, err := gtTrunkBranch("restack", state)
	if err != nil {
		return "", err
	}
	stack, err := gtRestackStack(ctx, l.dir(), state, trunk)
	if err != nil {
		return "", err
	}
	trunkHolder, err := gtRestackTrunkHolder(ctx, l, stack, trunk)
	if err != nil {
		return "", err
	}
	declined, err := gtRestackLanded(ctx, l, commonDir, trunk, stack)
	if err != nil {
		return "", err
	}
	remote, err := vcs.GitRemoteFor(ctx, l.dir(), trunk)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"fetch", remote, trunk}); err != nil {
		return "", fmt.Errorf("restack: git fetch %s %s: %w", remote, trunk, err)
	}
	trunkRef, err := gtTrunkRefAt(ctx, l.dir(), "restack", remote, trunk)
	if err != nil {
		return "", err
	}

	landed, err := gtStateAt(ctx, commonDir, "restack")
	if err != nil {
		return "", err
	}
	stack, err = gtRestackStack(ctx, l.dir(), landed, trunk)
	if err != nil {
		return "", err
	}
	pin, err := gtTrunkPin(ctx, "restack", l.checkout, l.dir(), trunkRef, landed[trunk].Head)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	// Re-read because the pin moves refs/heads/<trunk>, the ref needs-restack is
	// measured against.
	pinned, err := gtStateAt(ctx, commonDir, "restack")
	if err != nil {
		return "", err
	}
	for _, branch := range stack {
		if held := pinned[branch].State; held != "" {
			declined[branch] = held
		}
	}
	chain := gtBottomUp(slices.DeleteFunc(slices.Clone(stack), func(branch string) bool {
		_, skip := declined[branch]
		return skip
	}))
	if err := gtTrunkDrift(errW, "restack", pinned, chain, pin, string(trunkRef.Ref())); err != nil {
		return "", err
	}

	result, err := gtRestackChain(ctx, "restack", l.checkout, l.dir(), commonDir, pinned, chain)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	for branch, held := range result.held {
		declined[branch] = held
	}

	restacked, skipped, behind, err := gtRestackVerdict(ctx, l.dir(), trunkRef, stack, declined)
	if err != nil {
		return "", err
	}
	summary := gtRestackSummary(pin, trunkHolder, len(stack), restacked, skipped)
	if len(behind) > 0 {
		return "", &errRestackBehind{Trunk: pin.String(), Branches: behind, Summary: summary}
	}
	return summary, nil
}

// gtRestackLanded asks Graphite about the stack's own branches only: gt sync asks
// about every tracked branch, which times Graphite out in a large repository. It
// runs before the trunk fetch, so every merge it reports is in the fetched trunk,
// and compares against heads read after the answer. A parent merged at another
// head keeps its children, which may build on commits the merge never took.
func gtRestackLanded(ctx context.Context, l lane, commonDir, trunk string, stack []string) (map[string]string, error) {
	merged, err := gtMergedHeads(ctx, l, "restack", trunk, stack)
	if err != nil {
		return nil, err
	}
	state, err := gtStateAt(ctx, commonDir, "restack")
	if err != nil {
		return nil, err
	}
	declined := make(map[string]string, len(merged))
	landed := make(map[string]bool, len(merged))
	for branch, head := range merged {
		declined[branch] = gtSkipMerged
		if state[branch].Head == head {
			landed[branch] = true
		}
	}
	if len(landed) == 0 {
		return declined, nil
	}
	rows, err := gtmeta.Rows(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("restack: %w", err)
	}
	moves, err := pruneReparent(rows, landed, trunk)
	if err != nil {
		return nil, err
	}
	if err := gtmeta.Reparent(ctx, commonDir, moves); err != nil {
		return nil, fmt.Errorf("restack: %w", err)
	}
	return declined, nil
}

// gtRestackStack lists the current downstack, trunk excluded.
func gtRestackStack(ctx context.Context, dir render.Dir, state gtState, trunk string) ([]string, error) {
	branch, err := gitCurrentBranch(ctx, dir, "restack")
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return nil, errRestackDetached
	}
	if branch == trunk {
		return nil, nil
	}
	return gtDownstack("restack", state, branch, trunk)
}

// gtRestackTrunkHolder names the working copy holding trunk, which gt declines
// to say anything about: a held trunk cannot be pulled, so the whole stack reads
// as behind with nothing explaining why. An empty string means nobody else holds
// it — BranchHolders names only the branches some working copy has checked out,
// so a trunk no entry covers is one this summary must not claim anything about.
//
// A stack branch some other working copy holds is no longer a refusal:
// gtRestackChain replays it without a checkout and realigns its holder.
func gtRestackTrunkHolder(ctx context.Context, l lane, stack []string, trunk string) (string, error) {
	if len(stack) == 0 {
		return "", nil
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if holder := holders[trunk]; holder != l.checkout.Root {
		return holder, nil
	}
	return "", nil
}

// gtRestackVerdict counts the stack branches that ended up on trunk and labels
// the rest, then appends every declined branch the stack never named. It
// measures against the remote-tracking trunk, since a local trunk the pin could
// not fast-forward would read a stale stack as current.
func gtRestackVerdict(ctx context.Context, dir render.Dir, trunk vcs.Trunk, stack []string, declined map[string]string) (int, []string, []string, error) {
	restacked := 0
	named := make(map[string]bool, len(stack))
	var skipped, behind []string
	for _, branch := range stack {
		named[branch] = true
		on, err := gitIsAncestor(ctx, dir, "restack", string(trunk.Ref()), branch)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("restack: check %s sits on %s: %w", branch, trunk.Ref(), err)
		}
		reason, refused := declined[branch]
		if !on && !gtRestackHold(reason) {
			behind = append(behind, gtSkipLabel(branch, reason))
		}
		switch {
		case !refused && on:
			restacked++
		case !refused:
			skipped = append(skipped, gtSkipLabel(branch))
		case on:
			skipped = append(skipped, gtSkipLabel(branch, reason, "already on "+trunk.Name()))
		default:
			skipped = append(skipped, gtSkipLabel(branch, reason))
		}
	}

	var elsewhere []string
	for branch := range declined {
		if !named[branch] {
			elsewhere = append(elsewhere, branch)
		}
	}
	slices.Sort(elsewhere)
	for _, branch := range elsewhere {
		skipped = append(skipped, gtSkipLabel(branch, declined[branch]))
	}
	return restacked, skipped, behind, nil
}

// gtRestackHold reports whether a branch left behind trunk was left there on
// purpose. gt freeze and a merge in progress are holds the operator asked for,
// and a branch already merged has nowhere to go; everything else off trunk after
// a restack is a restack that did not happen.
func gtRestackHold(reason string) bool {
	return reason == "frozen" || reason == "merging" || reason == gtSkipMerged
}

func gtSkipLabel(branch string, notes ...string) string {
	notes = slices.DeleteFunc(notes, func(note string) bool { return note == "" })
	if len(notes) == 0 {
		return branch
	}
	return branch + " (" + strings.Join(notes, "; ") + ")"
}

// gtRestackSummary names the commit the pass pinned, not just the branch: a
// trunk that lands every few minutes makes "trunk main" a different base from
// one minute to the next, and the sha is what makes the result reproducible.
func gtRestackSummary(pin gtTrunkPinned, trunkHolder string, total, restacked int, skipped []string) string {
	held := pin.String()
	if trunkHolder != "" {
		held += " (checked out in " + trunkHolder + ")"
	}
	summary := "synced · trunk " + held
	if total > 0 {
		summary = fmt.Sprintf("restacked %d of %d · trunk %s", restacked, total, held)
	}
	if len(skipped) > 0 {
		summary += shipSep + "skipped " + strings.Join(skipped, ", ")
	}
	return summary
}

func restackJJ(ctx context.Context, dir render.Dir) (string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if len(trunkNames) != 1 {
		return "", fmt.Errorf("restack: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", trunkNames)
	}
	trunk := trunkNames[0]

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"git", "fetch"}); err != nil {
		return "", fmt.Errorf("restack: jj git fetch: %w", err)
	}
	ancestors, err := jjLogLines(ctx, dir, "restack", jjRestackAncestorRevset)
	if err != nil {
		return "", err
	}
	if len(ancestors) > 0 {
		return "fetched · already up to date", nil
	}

	rebased, err := jjRestackOntoTrunk(ctx, dir, trunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched · rebased %d commit(s) onto %s", rebased, trunk), nil
}

func jjRestackOntoTrunk(ctx context.Context, dir render.Dir, trunk string) (int, error) {
	stack, err := jjLogLines(ctx, dir, "restack", jjRestackStackRevset)
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("restack: trunk %q is not an ancestor of @ but trunk()..@ is empty", trunk)
	}

	if _, err := render.RunCLI(ctx, dir, "jj", []string{"rebase", "-b", "@", "--destination", "trunk()"}); err != nil {
		return 0, fmt.Errorf("restack: jj rebase onto trunk %q: %w — retry manually: jj rebase -b @ --destination 'trunk()'", trunk, err)
	}
	rebaseOp, err := jjOpID(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("restack: read jj rebase operation: %w", err)
	}

	conflicts, err := jjLogLines(ctx, dir, "restack", jjRestackConflictRevset)
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp})
		if revertErr == nil {
			return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed (rebase rolled back): %w", trunk, err)
		}
		return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", trunk, err, revertErr, rebaseOp)
	}
	if len(conflicts) > 0 {
		if _, revertErr := render.RunCLI(cleanup, dir, "jj", []string{"op", "revert", rebaseOp}); revertErr != nil {
			return 0, fmt.Errorf("restack: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", trunk, revertErr, rebaseOp)
		}
		return 0, fmt.Errorf("restack: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @ --destination 'trunk()', then fix the conflicts (jj status)", trunk, len(conflicts), strings.Join(conflicts, "\n  "))
	}
	return len(stack), nil
}

// restackGit fast-forwards trunk in place, and rebases any other branch as a
// one-branch stack rebase: a conflict stops in a workspace of its own, with
// rerere off, for ccx vcs stack continue or ccx vcs stack abort. A rebase
// finished here prints its own summary and returns an empty one.
func restackGit(ctx context.Context, cmd *cobra.Command, l lane) (string, error) {
	dir := l.dir()
	branch, err := gitCurrentBranch(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errRestackDetached
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, branch)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"fetch", remote}); err != nil {
		return "", fmt.Errorf("restack: git fetch %s: %w", remote, err)
	}

	trunk, err := vcs.ResolveTrunk(ctx, dir, remote)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	upToDate, err := gitIsAncestor(ctx, dir, "restack", string(trunk.Ref()), "HEAD")
	if err != nil {
		return "", fmt.Errorf("restack: compare HEAD with %s: %w", trunk.Ref(), err)
	}
	if upToDate {
		return "fetched · already up to date", nil
	}

	if branch == trunk.Name() {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"merge", "--ff-only", string(trunk.Ref())}); err != nil {
			return "", fmt.Errorf("restack: fast-forward %s to %s: %w — resolve manually: git fetch %s && git merge --ff-only %s", branch, trunk.Ref(), err, remote, trunk.Ref())
		}
		return "fetched · fast-forwarded " + trunk.Name(), nil
	}

	run, err := restackGitRun(ctx, dir, branch, trunk)
	if err != nil {
		return "", err
	}
	commonDir, err := gtCommonDir(ctx, dir, "restack")
	if err != nil {
		return "", err
	}
	runs, err := stackRuns(commonDir)
	if err != nil {
		return "", err
	}
	if err := stackAdmit(ctx, cmd, l, commonDir, runs, run, false); err != nil {
		return "", err
	}
	if err := stackClaim(commonDir, run); err != nil {
		return "", err
	}
	if err := stackSaveRun(run); err != nil {
		return "", err
	}
	return "", stackDrive(ctx, cmd, l, commonDir, run)
}

// restackGitRun plans branch as a one-branch stack on the fetched trunk,
// replayed from where it forked.
func restackGitRun(ctx context.Context, dir render.Dir, branch string, trunk vcs.Trunk) (*stackRebaseRun, error) {
	pin, err := stackRevParse(ctx, dir, string(trunk.Ref()))
	if err != nil {
		return nil, err
	}
	head, err := stackRevParse(ctx, dir, gtRestackRef(branch))
	if err != nil {
		return nil, err
	}
	base, err := stackMergeBase(ctx, dir, pin, head)
	if err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("restack: %w", err)
	}
	return &stackRebaseRun{
		Trunk:  trunk.Name(),
		Pin:    pin,
		NoPush: true,
		Git:    true,
		Roots:  []string{branch},
		Pid:    os.Getpid(),
		Host:   host,
		Branches: []stackRebaseBranch{{
			Name:      branch,
			Parent:    trunk.Name(),
			WasParent: trunk.Name(),
			Local:     head,
			Head:      head,
			HeadRef:   gtRestackRef(branch),
			OldBase:   base,
		}},
	}, nil
}
