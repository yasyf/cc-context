package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type pruneOpts struct {
	dryRun bool
	noGT   bool
}

func newPruneCmd() *cobra.Command {
	var o pruneOpts
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete local branches already merged into trunk, forget their graphite rows, and reparent the rows they leave behind",
		Long: `Delete local branches already merged into trunk, forget their graphite rows, and reparent the rows they leave behind.

A forgotten row leaves its children naming a parent gt no longer knows, and gt
then refuses to resolve the whole stack above them, so every surviving row whose
recorded parent this prune forgets moves onto the first ancestor it keeps — trunk
once the chain runs out of tracked ancestors. gt has no verb for the move, so
prune writes it into gt's metadata itself, on both sides, since gt walks its tree
through each parent's list of children; the moved branch then reads as needing
the restack it does need. Branches gt reports as diverged are named and never
touched, since their remedy is gt track or gt untrack and guessing between them
loses work, and merged branches a worktree holds are named as held, since git
refuses to delete them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrune(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would be deleted and delete nothing")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and leave its metadata alone")
	return cmd
}

// prunePlan is what one prune would do: merged branches to delete, graphite
// rows for branches that no longer exist, the rows a forget would strand and
// the parent each moves onto, and the branches gt reports as diverged, which
// prune names and never touches — their remedy is gt track or gt untrack, and
// guessing between them loses work.
type prunePlan struct {
	merged   []string
	stale    []string
	diverged []string
	held     []string
	reparent map[string]string
}

func runPrune(cmd *cobra.Command, o pruneOpts) error {
	ctx := cmd.Context()
	dir := render.Dir(workingDir())
	l, err := resolveLane(ctx, "prune", workingDir(), o.noGT)
	if err != nil {
		return err
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	trunk, err := vcs.ResolveTrunk(ctx, dir, remote)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	plan, err := prunePlanFor(ctx, dir, l, trunk)
	if err != nil {
		return err
	}
	if o.dryRun {
		cmd.Println(prunePlanReport(plan, trunk, true))
		return nil
	}
	if err := pruneApply(ctx, dir, l, plan); err != nil {
		return err
	}
	cmd.Println(prunePlanReport(plan, trunk, false))
	return nil
}

func prunePlanFor(ctx context.Context, dir render.Dir, l lane, trunk vcs.Trunk) (prunePlan, error) {
	held, err := pruneHeldBranches(ctx, dir)
	if err != nil {
		return prunePlan{}, err
	}
	merged, err := pruneMergedBranches(ctx, dir, trunk)
	if err != nil {
		return prunePlan{}, err
	}
	plan := prunePlan{}
	for _, branch := range merged {
		if held[branch] {
			plan.held = append(plan.held, branch)
			continue
		}
		plan.merged = append(plan.merged, branch)
	}
	if !l.gt {
		return plan, nil
	}
	live, err := pruneLiveBranches(ctx, dir)
	if err != nil {
		return prunePlan{}, err
	}
	commonDir, err := gtCommonDir(ctx, dir, "prune")
	if err != nil {
		return prunePlan{}, err
	}
	rows, err := gtmeta.Rows(ctx, commonDir)
	if err != nil {
		return prunePlan{}, fmt.Errorf("prune: %w", err)
	}
	for _, row := range rows {
		switch {
		case row.Stale, !live[row.Branch]:
			plan.stale = append(plan.stale, row.Branch)
		case row.Diverged:
			plan.diverged = append(plan.diverged, row.Branch)
		}
	}
	sort.Strings(plan.stale)
	sort.Strings(plan.diverged)
	plan.reparent, err = pruneReparent(rows, pruneForgotten(plan), trunk.Name())
	if err != nil {
		return prunePlan{}, err
	}
	return plan, nil
}

// pruneForgotten names every graphite row this prune deletes: the branches it
// deletes from git, plus the rows whose branch is already gone.
func pruneForgotten(plan prunePlan) map[string]bool {
	forgotten := make(map[string]bool, len(plan.merged)+len(plan.stale))
	for _, branch := range plan.merged {
		forgotten[branch] = true
	}
	for _, branch := range plan.stale {
		forgotten[branch] = true
	}
	return forgotten
}

// pruneReparent moves every surviving row whose recorded parent this prune
// forgets onto the first ancestor it keeps: a forgotten parent leaves its
// children naming a branch that no longer exists, and gt then refuses to
// resolve the whole stack above them.
func pruneReparent(rows []gtmeta.Row, forgotten map[string]bool, trunk string) (map[string]string, error) {
	parents := make(map[string]string, len(rows))
	for _, row := range rows {
		parents[row.Branch] = row.Parent
	}
	moves := make(map[string]string)
	for _, row := range rows {
		if forgotten[row.Branch] || !forgotten[row.Parent] {
			continue
		}
		survivor, err := pruneSurvivor(parents, forgotten, trunk, row.Branch)
		if err != nil {
			return nil, err
		}
		moves[row.Branch] = survivor
	}
	return moves, nil
}

// pruneSurvivor walks branch's recorded parents past every row the prune
// forgets, to the first row it keeps — trunk once the chain runs out of tracked
// ancestors, since a branch gt does not track is no parent to record. A chain
// that revisits a branch is a cycle prune refuses rather than writes back.
func pruneSurvivor(parents map[string]string, forgotten map[string]bool, trunk, branch string) (string, error) {
	seen := map[string]bool{branch: true}
	for cur := branch; ; {
		next, tracked := parents[cur]
		if !tracked || next == "" {
			return trunk, nil
		}
		if seen[next] {
			return "", fmt.Errorf("prune: gt parent chain of %s cycles at %s — run gt track %s", branch, next, next)
		}
		if _, rowed := parents[next]; rowed && !forgotten[next] {
			return next, nil
		}
		seen[next] = true
		cur = next
	}
}

// pruneHeldBranches names every branch a worktree has checked out, including
// this one: git refuses to delete them, and a prune that tried would fail
// partway rather than report the exclusion.
func pruneHeldBranches(ctx context.Context, dir render.Dir) (map[string]bool, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"worktree", "list", "--porcelain"})
	if err != nil {
		return nil, fmt.Errorf("prune: git worktree list: %w", err)
	}
	held := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			held[strings.TrimSpace(name)] = true
		}
	}
	return held, nil
}

func pruneMergedBranches(ctx context.Context, dir render.Dir, trunk vcs.Trunk) ([]string, error) {
	argv := []string{"branch", "--merged", string(trunk.Ref()), "--format=%(refname:short)"}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return nil, fmt.Errorf("prune: git branch --merged: %w", err)
	}
	var merged []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" || name == trunk.Name() {
			continue
		}
		merged = append(merged, name)
	}
	sort.Strings(merged)
	return merged, nil
}

func pruneLiveBranches(ctx context.Context, dir render.Dir) (map[string]bool, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"for-each-ref", "--format=%(refname:short)", "refs/heads/"})
	if err != nil {
		return nil, fmt.Errorf("prune: git for-each-ref: %w", err)
	}
	live := make(map[string]bool)
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			live[name] = true
		}
	}
	return live, nil
}

// pruneApply deletes with git branch -d, never -D: every branch in the plan
// reached trunk, so a refusal means the plan went stale under a concurrent
// checkout and the branch keeps its commits.
func pruneApply(ctx context.Context, dir render.Dir, l lane, plan prunePlan) error {
	for _, batch := range pruneBatches(plan.merged, 200) {
		argv := append([]string{"branch", "-d"}, batch...)
		if _, err := render.RunCLI(ctx, dir, "git", argv); err != nil {
			return fmt.Errorf("prune: git branch -d: %w", err)
		}
	}
	forget := append(append([]string{}, plan.merged...), plan.stale...)
	if !l.gt || len(forget) == 0 {
		return nil
	}
	commonDir, err := gtCommonDir(ctx, dir, "prune")
	if err != nil {
		return err
	}
	if err := gtmeta.Reparent(ctx, commonDir, plan.reparent); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	if err := gtmeta.Forget(ctx, commonDir, forget); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

// pruneBatches keeps one git branch -d under the platform's argv limit, which a
// repository with hundreds of merged branches would otherwise blow past.
func pruneBatches(names []string, size int) [][]string {
	var batches [][]string
	for len(names) > size {
		batches = append(batches, names[:size])
		names = names[size:]
	}
	if len(names) > 0 {
		batches = append(batches, names)
	}
	return batches
}

func prunePlanReport(plan prunePlan, trunk vcs.Trunk, dryRun bool) string {
	deleted, forgot, reparented := "deleted", "forgot", "reparented"
	if dryRun {
		deleted, forgot, reparented = "would delete", "would forget", "would reparent"
	}
	segs := []string{fmt.Sprintf("%s %d branches merged into %s%s", deleted, len(plan.merged), trunk.Name(), pruneNames(plan.merged))}
	if len(plan.stale) > 0 {
		segs = append(segs, fmt.Sprintf("%s %d graphite rows for deleted branches", forgot, len(plan.stale)))
	}
	if len(plan.reparent) > 0 {
		segs = append(segs, fmt.Sprintf("%s %d graphite rows onto a surviving parent%s", reparented, len(plan.reparent), pruneNames(pruneMoveNames(plan.reparent))))
	}
	if len(plan.held) > 0 {
		segs = append(segs, fmt.Sprintf("%d merged branches held by a worktree", len(plan.held)))
	}
	if len(plan.diverged) > 0 {
		segs = append(segs, fmt.Sprintf("%d diverged, left alone — gt track or gt untrack each", len(plan.diverged)))
	}
	return strings.Join(segs, shipSep)
}

// pruneNames lists the branches a prune deletes, which is the one part worth
// reading in full — capped, because a repository that needs pruning has more of
// them than a report should carry.
func pruneNames(branches []string) string {
	if len(branches) == 0 {
		return ""
	}
	if len(branches) <= pruneNameCap {
		return ": " + strings.Join(branches, ", ")
	}
	return fmt.Sprintf(": %s and %d more", strings.Join(branches[:pruneNameCap], ", "), len(branches)-pruneNameCap)
}

// pruneMoveNames renders each reparented row as the move it is, sorted so one
// prune's report reads the same twice.
func pruneMoveNames(moves map[string]string) []string {
	names := make([]string, 0, len(moves))
	for branch, parent := range moves {
		names = append(names, branch+" → "+parent)
	}
	sort.Strings(names)
	return names
}

const pruneNameCap = 10
