package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	// gtSyncConflict through gtSyncAuthRequired2 are gt 1.8.6's own wording
	// for classifyGTRestack; version-dependent, kept as lone constants so an
	// upgrade is a one-line change (precedent: gtRestackNeeded1).
	gtSyncConflict      = "Hit conflict restacking"
	gtSyncAuthRequired1 = "Please authenticate your Graphite CLI"
	gtSyncAuthRequired2 = "Your Graphite auth token is invalid/expired"

	// gtSyncSkippedPrefix and gtSyncSkippedReason bracket the branch name in the
	// lines gt 1.8.6 prints — on stdout, at exit 0 — for a branch it declined to
	// restack. Three reasons follow: gtSyncSkippedWorktree and a path, "frozen.",
	// or "merging.". Only the first names a working copy gtLaneRestack can drive
	// gt from; the other two are reasons no lane resolves.
	gtSyncSkippedPrefix   = "Did not restack branch "
	gtSyncSkippedReason   = " because it is "
	gtSyncSkippedWorktree = "checked out in worktree "

	jjRestackAncestorRevset = "trunk() & ::@"
	jjRestackStackRevset    = "trunk()..@"
	jjRestackConflictRevset = "conflicts() & @::"
)

var errRestackDetached = errors.New("restack: detached HEAD — check out a branch before restacking")

type restackOpts struct {
	noGT bool
}

func newRestackCmd() *cobra.Command {
	var o restackOpts
	cmd := &cobra.Command{
		Use:     "restack",
		Aliases: []string{"rebase"},
		Short:   "Fetch and restack the working-copy stack onto trunk",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRestack(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and fall back to the jj/git detection")
	return cmd
}

func runRestack(cmd *cobra.Command, o restackOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "restack", workingDir(), o.noGT)
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
		summary, err = restackJJ(ctx)
	case vcs.Git:
		summary, err = restackGit(ctx)
	default:
		panic(fmt.Sprintf("restack: unsupported vcs kind %d", l.kind))
	}
	if err != nil {
		return err
	}
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
}

// restackGT reads the stack twice: the preflight refuses over the stack as it
// stands, while the verdict measures the one sync left behind. gt sync deletes
// the branches whose PRs merged or closed and reparents their children onto
// trunk, so a verdict over the pre-sync list probes refs sync just deleted —
// git merge-base exits 128 on a name that no longer resolves, turning a
// successful sync into a failure.
func restackGT(ctx context.Context, l lane, errW io.Writer) (string, error) {
	state, err := gtStateQuery(ctx, "restack")
	if err != nil {
		return "", err
	}
	trunk, err := gtTrunkBranch("restack", state)
	if err != nil {
		return "", err
	}
	stack, err := gtRestackStack(ctx, state, trunk)
	if err != nil {
		return "", err
	}
	trunkHolder, err := gtRestackTrunkHolder(ctx, l, stack, trunk)
	if err != nil {
		return "", err
	}

	output, err := gtSync(ctx, errW)
	if err != nil {
		return "", err
	}

	synced, err := gtStateQuery(ctx, "restack")
	if err != nil {
		return "", err
	}
	stack, err = gtRestackStack(ctx, synced, trunk)
	if err != nil {
		return "", err
	}

	classify := func(dir string, r gtResult, cause error) error {
		if strings.Contains(r.Output, gtSyncConflict) {
			return &gtAdvice{advice: gtLaneConflict("restack", dir), cause: cause}
		}
		return classifyGTRestack(r, cause)
	}
	lanes, laneDeclined, err := gtLaneRestack(ctx, errW, "restack", l.checkout, gtBottomUp(stack), classify)
	if err != nil {
		return "", err
	}
	// gt sync restacked this working copy before the sweep, since it also fetches
	// trunk and prunes merged branches. A sibling lane moving afterwards leaves
	// the branches above it off their parents again, so this one restacks a
	// second time — but only when a sibling actually moved.
	if len(lanes) > 0 {
		if _, err := gtRestackAt(ctx, errW, "", classify); err != nil {
			return "", err
		}
	}
	swept, err := gtStateQuery(ctx, "restack")
	if err != nil {
		return "", err
	}
	declined := gtLaneStanding(swept, stack, mergeDeclines(gtSyncSkipped(output), laneDeclined))

	remote, err := vcs.GitRemoteFor(ctx, "", trunk)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	trunkRef, err := vcs.TrunkFromName(ctx, "", remote, trunk)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	restacked, skipped, err := gtRestackVerdict(ctx, trunkRef, stack, declined)
	if err != nil {
		return "", err
	}
	return gtRestackSummary(trunk, trunkHolder, len(stack), restacked, skipped), nil
}

// gtRestackStack lists the branches gt sync is asked to restack: the current
// downstack, trunk excluded.
func gtRestackStack(ctx context.Context, state gtState, trunk string) ([]string, error) {
	branch, err := gitCurrentBranch(ctx, "restack")
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
// gtLaneRestack drives gt from that working copy instead.
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

// mergeDeclines folds one sweep's declines over another's, later runs last.
func mergeDeclines(first, second map[string]string) map[string]string {
	merged := make(map[string]string, len(first)+len(second))
	for branch, reason := range first {
		merged[branch] = reason
	}
	for branch, reason := range second {
		merged[branch] = reason
	}
	return merged
}

// gtSync runs gt sync and returns everything it printed, both streams
// interleaved. gt 1.8.6 splits one exit-0 sync across the two: it names a
// branch it declined on stdout, and warns on stderr about one it could not
// restack — "WARNING: <b> could not be restacked cleanly." — while stdout's
// restack section stays empty. A caller that keeps one stream sees half the
// sync.
//
// Exit 0 stays a success — gtZeroSurfaces — because the verdict re-measures the
// stack's ancestry itself, so a diagnostic explains a report ccx already made
// rather than deciding it. A trunk gt could neither pull nor fast-forward exits
// 1 and reaches classifyGTRestack instead.
func gtSync(ctx context.Context, errW io.Writer) (string, error) {
	r, err := gtRun(ctx, []string{"sync", "--no-interactive"}, gtZeroSurfaces, errW)
	if err != nil {
		return "", classifyGTRestack(r, err)
	}
	if report := r.Diagnostics(); report != "" {
		if _, werr := io.WriteString(errW, report); werr != nil {
			return "", fmt.Errorf("restack: report gt sync diagnostics: %w", werr)
		}
	}
	return r.Output, nil
}

func gtSyncSkipped(output string) map[string]string {
	skipped := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		_, named, ok := strings.Cut(line, gtSyncSkippedPrefix)
		if !ok {
			continue
		}
		branch, reason, ok := strings.Cut(named, gtSyncSkippedReason)
		if !ok {
			continue
		}
		skipped[branch] = gtSkipReason(reason)
	}
	return skipped
}

// gtSkipReason renders gt's sentence tail as a label note: the period dropped,
// and the worktree variant shortened to the path that answers it.
func gtSkipReason(reason string) string {
	reason = strings.TrimSuffix(strings.TrimSpace(reason), ".")
	if worktree, ok := strings.CutPrefix(reason, gtSyncSkippedWorktree); ok {
		return "checked out in " + worktree
	}
	return reason
}

// gtRestackVerdict counts the stack branches that ended up on trunk and labels
// the rest, then appends every branch gt declined that the stack never named. A
// branch gt declined never counts as restacked, however the ancestry compares:
// gt reports what it did, while ancestry infers it. Where the two disagree the
// label says so, since a decline over a branch already on trunk is a different
// fact from one over a branch left behind it.
//
// The measurement is against the remote-tracking trunk, never the local branch.
// gt sync writes refs/remotes/<remote>/<trunk> from the fetch before it tries to
// move the local branch, and that second step is the one that fails — a sibling
// working copy holding trunk with conflicting unstaged changes, or a trunk that
// cannot fast-forward, leaves the local branch stale while gt still exits 0
// without declining a single branch. A stack measured against that ref reads as
// current while it sits behind the trunk everyone else sees.
func gtRestackVerdict(ctx context.Context, trunk vcs.Trunk, stack []string, declined map[string]string) (int, []string, error) {
	restacked := 0
	named := make(map[string]bool, len(stack))
	var skipped []string
	for _, branch := range stack {
		named[branch] = true
		on, err := gitIsAncestor(ctx, "restack", string(trunk.Ref()), branch)
		if err != nil {
			return 0, nil, fmt.Errorf("restack: check %s sits on %s: %w", branch, trunk.Ref(), err)
		}
		reason, refused := declined[branch]
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
	return restacked, skipped, nil
}

func gtSkipLabel(branch string, notes ...string) string {
	notes = slices.DeleteFunc(notes, func(note string) bool { return note == "" })
	if len(notes) == 0 {
		return branch
	}
	return branch + " (" + strings.Join(notes, "; ") + ")"
}

func gtRestackSummary(trunk, trunkHolder string, total, restacked int, skipped []string) string {
	held := trunk
	if trunkHolder != "" {
		held = trunk + " (checked out in " + trunkHolder + ")"
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

// classifyGTRestack reads a failed sync's whole interleaved output, not the
// error text: gt writes its conflict banner to stdout and its ERROR:-led
// diagnostics to stderr, so a classifier matching either stream alone matches
// nothing the other one carried. gtAdvice replaces gt's sentence without
// discarding it, so the run stays reachable through errors.As.
func classifyGTRestack(r gtResult, cause error) error {
	switch {
	case strings.Contains(r.Output, gtSyncConflict):
		return &gtAdvice{advice: "restack: conflict — resolve the listed files, then gt continue (or gt abort); see the output above", cause: cause}
	case strings.Contains(r.Output, gtSyncAuthRequired1) || strings.Contains(r.Output, gtSyncAuthRequired2):
		return &gtAdvice{advice: "restack: graphite auth required — run gt auth", cause: cause}
	default:
		return fmt.Errorf("restack: %w", cause)
	}
}

func restackJJ(ctx context.Context) (string, error) {
	trunkNames, err := jjTrunkBookmarkNames(ctx, "restack")
	if err != nil {
		return "", err
	}
	if len(trunkNames) != 1 {
		return "", fmt.Errorf("restack: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", trunkNames)
	}
	trunk := trunkNames[0]

	if _, err := render.RunCLI(ctx, "jj", []string{"git", "fetch"}); err != nil {
		return "", fmt.Errorf("restack: jj git fetch: %w", err)
	}
	ancestors, err := jjLogLines(ctx, "restack", jjRestackAncestorRevset)
	if err != nil {
		return "", err
	}
	if len(ancestors) > 0 {
		return "fetched · already up to date", nil
	}

	rebased, err := jjRestackOntoTrunk(ctx, trunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fetched · rebased %d commit(s) onto %s", rebased, trunk), nil
}

func jjRestackOntoTrunk(ctx context.Context, trunk string) (int, error) {
	stack, err := jjLogLines(ctx, "restack", jjRestackStackRevset)
	if err != nil {
		return 0, err
	}
	if len(stack) == 0 {
		return 0, fmt.Errorf("restack: trunk %q is not an ancestor of @ but trunk()..@ is empty", trunk)
	}

	if _, err := render.RunCLI(ctx, "jj", []string{"rebase", "-b", "@", "--destination", "trunk()"}); err != nil {
		return 0, fmt.Errorf("restack: jj rebase onto trunk %q: %w — retry manually: jj rebase -b @ --destination 'trunk()'", trunk, err)
	}
	rebaseOp, err := jjOpID(ctx)
	if err != nil {
		return 0, fmt.Errorf("restack: read jj rebase operation: %w", err)
	}

	conflicts, err := jjLogLines(ctx, "restack", jjRestackConflictRevset)
	cleanup := context.WithoutCancel(ctx)
	if err != nil {
		_, revertErr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp})
		if revertErr == nil {
			return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed (rebase rolled back): %w", trunk, err)
		}
		return 0, fmt.Errorf("restack: conflict check after rebase onto %q failed: %w; rollback also failed: %w — run: jj op revert %s", trunk, err, revertErr, rebaseOp)
	}
	if len(conflicts) > 0 {
		if _, revertErr := render.RunCLI(cleanup, "jj", []string{"op", "revert", rebaseOp}); revertErr != nil {
			return 0, fmt.Errorf("restack: rebase onto %q conflicted and rollback failed: %w — run: jj op revert %s, then resolve manually", trunk, revertErr, rebaseOp)
		}
		return 0, fmt.Errorf("restack: rebase onto %q conflicts in %d commit(s); rolled back to the pre-rebase state\nconflicted:\n  %s\nresolve manually: jj rebase -b @ --destination 'trunk()', then fix the conflicts (jj status)", trunk, len(conflicts), strings.Join(conflicts, "\n  "))
	}
	return len(stack), nil
}

func restackGit(ctx context.Context) (string, error) {
	branch, err := gitCurrentBranch(ctx, "restack")
	if err != nil {
		return "", err
	}
	if branch == "" {
		return "", errRestackDetached
	}
	remote, err := vcs.GitRemoteFor(ctx, "", branch)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	if _, err := render.RunCLI(ctx, "git", []string{"fetch", remote}); err != nil {
		return "", fmt.Errorf("restack: git fetch %s: %w", remote, err)
	}

	trunk, err := vcs.ResolveTrunk(ctx, "", remote)
	if err != nil {
		return "", fmt.Errorf("restack: %w", err)
	}
	upToDate, err := gitIsAncestor(ctx, "restack", string(trunk.Ref()), "HEAD")
	if err != nil {
		return "", fmt.Errorf("restack: compare HEAD with %s: %w", trunk.Ref(), err)
	}
	if upToDate {
		return "fetched · already up to date", nil
	}

	if branch == trunk.Name() {
		if _, err := render.RunCLI(ctx, "git", []string{"merge", "--ff-only", string(trunk.Ref())}); err != nil {
			return "", fmt.Errorf("restack: fast-forward %s to %s: %w — resolve manually: git fetch %s && git merge --ff-only %s", branch, trunk.Ref(), err, remote, trunk.Ref())
		}
		return "fetched · fast-forwarded " + trunk.Name(), nil
	}

	if _, err := gitRebaseOnto(ctx, "restack", trunk.Remote(), trunk.Name()); err != nil {
		return "", err
	}
	return "fetched · rebased onto " + trunk.Name(), nil
}
