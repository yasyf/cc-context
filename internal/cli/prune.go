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
		Short: "Delete local branches already merged into trunk, and forget their graphite rows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrune(cmd, o)
		},
	}
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print what would be deleted and delete nothing")
	cmd.Flags().BoolVar(&o.noGT, "no-gt", false, "ignore a live graphite config and leave its metadata alone")
	return cmd
}

// prunePlan is what one prune would do: merged branches to delete, graphite
// rows for branches that no longer exist, and the branches gt reports as
// diverged, which prune names and never touches — their remedy is gt track or
// gt untrack, and guessing between them loses work.
type prunePlan struct {
	merged   []string
	stale    []string
	diverged []string
	held     []string
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
	return plan, nil
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
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	segs := []string{fmt.Sprintf("%s %d branches merged into %s%s", verb, len(plan.merged), trunk.Name(), pruneNames(plan.merged))}
	if len(plan.stale) > 0 {
		segs = append(segs, fmt.Sprintf("forgot %d graphite rows for deleted branches", len(plan.stale)))
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

const pruneNameCap = 10
