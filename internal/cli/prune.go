package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/gtapi"
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

A squash landing leaves no ancestry for git to see, so on the graphite lane a
branch also counts as merged when Graphite reports its pull request MERGED at
exactly the branch's local head; a branch carrying a commit the merged pull
request did not is left alone, and the delete refuses if the head moved since.

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
	squashed map[string]string
	stale    []string
	diverged []string
	held     []string
	reparent map[string]string
}

func runPrune(cmd *cobra.Command, o pruneOpts) error {
	ctx := cmd.Context()
	dir := render.Dir(workingDir(ctx))
	l, err := resolveLane(ctx, "prune", workingDir(ctx), o.noGT)
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
	commonDir, err := gtCommonDir(ctx, dir, "prune")
	if err != nil {
		return err
	}
	plan, err := prunePlanFor(ctx, dir, l, trunk, commonDir)
	if err != nil {
		return err
	}
	if o.dryRun {
		cmd.Println(prunePlanReport(plan, trunk, true))
		return nil
	}
	if err := pruneApply(ctx, dir, l, plan, commonDir); err != nil {
		return err
	}
	cmd.Println(prunePlanReport(plan, trunk, false))
	return nil
}

func prunePlanFor(ctx context.Context, dir render.Dir, l lane, trunk vcs.Trunk, commonDir string) (prunePlan, error) {
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
	rows, err := gtmeta.Rows(ctx, commonDir)
	if err != nil {
		return prunePlan{}, fmt.Errorf("prune: %w", err)
	}
	diverged := map[string]bool{}
	for _, row := range rows {
		_, alive := live[row.Branch]
		switch {
		case row.Stale, !alive:
			plan.stale = append(plan.stale, row.Branch)
		case row.Diverged:
			plan.diverged = append(plan.diverged, row.Branch)
			diverged[row.Branch] = true
		}
	}
	sort.Strings(plan.stale)
	sort.Strings(plan.diverged)
	candidates := make(map[string]string, len(live))
	for branch, head := range live {
		if branch != trunk.Name() && !diverged[branch] {
			candidates[branch] = head
		}
	}
	for _, branch := range merged {
		delete(candidates, branch)
	}
	squashed, err := pruneSquashLanded(ctx, l, trunk, candidates)
	if err != nil {
		return prunePlan{}, err
	}
	for branch, head := range squashed {
		if held[branch] {
			plan.held = append(plan.held, branch)
			continue
		}
		if plan.squashed == nil {
			plan.squashed = map[string]string{}
		}
		plan.squashed[branch] = head
	}
	sort.Strings(plan.held)
	plan.reparent, err = pruneReparent(rows, pruneForgotten(plan), trunk.Name())
	if err != nil {
		return prunePlan{}, err
	}
	return plan, nil
}

// pruneForgotten names every graphite row this prune deletes: the branches it
// deletes from git, plus the rows whose branch is already gone.
func pruneForgotten(plan prunePlan) map[string]bool {
	forgotten := make(map[string]bool, len(plan.merged)+len(plan.squashed)+len(plan.stale))
	for _, branch := range plan.merged {
		forgotten[branch] = true
	}
	for branch := range plan.squashed {
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

// pruneLiveBranches maps every local branch to its head.
func pruneLiveBranches(ctx context.Context, dir render.Dir) (map[string]string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads/"})
	if err != nil {
		return nil, fmt.Errorf("prune: git for-each-ref: %w", err)
	}
	live := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, head, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			live[name] = head
		}
	}
	return live, nil
}

// pruneLandedBatch bounds the head refs one pull-request-info request names.
const pruneLandedBatch = 100

// pruneSquashLanded asks Graphite which candidates landed as a squash, and
// returns each one whose merged pull request's newest version is exactly the
// branch's local head: a branch that moved past what merged carries work the
// squash never took.
func pruneSquashLanded(ctx context.Context, l lane, trunk vcs.Trunk, heads map[string]string) (map[string]string, error) {
	merged, err := gtMergedHeads(ctx, l, "prune", trunk, slices.Sorted(maps.Keys(heads)))
	if err != nil {
		return nil, err
	}
	landed := map[string]string{}
	for branch, head := range merged {
		if heads[branch] == head {
			landed[branch] = head
		}
	}
	return landed, nil
}

func gtMergedHeads(ctx context.Context, l lane, prefix string, trunk vcs.Trunk, branches []string) (map[string]string, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	owner, name, err := gtRepoOwnerName(ctx, l, prefix)
	if err != nil {
		return nil, err
	}
	client := gtAPIClient()
	var mu sync.Mutex
	merged := map[string]string{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for batch := range slices.Chunk(branches, pruneLandedBatch) {
		g.Go(func() error {
			infos, err := client.PullRequestInfo(gctx, gtapi.PullRequestInfoRequest{
				RepoOwner:        owner,
				RepoName:         name,
				PRNumbers:        []int{},
				PRHeadRefNames:   batch,
				TrunkBranchNames: []string{trunk.Name()},
				Callsite:         "ccx",
			})
			if err != nil {
				return fmt.Errorf("%s: graphite pull-request-info: %w", prefix, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, pr := range infos {
				if pr.State == gtapi.PRMerged && slices.Contains(batch, pr.HeadRefName) {
					merged[pr.HeadRefName] = pruneMergedHead(pr)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return merged, nil
}

// pruneMergedHead is the head of a pull request's newest version, empty when
// Graphite recorded none.
func pruneMergedHead(pr gtapi.PullRequestInfo) string {
	newest := gtapi.PRVersion{}
	for _, v := range pr.Versions {
		if v.CreatedAt >= newest.CreatedAt {
			newest = v
		}
	}
	return newest.HeadSha
}

// pruneApply deletes with git branch -d, never -D: every merged branch in the
// plan reached trunk, so a refusal means the plan went stale under a concurrent
// checkout and the branch keeps its commits. A squash-landed branch never
// reached trunk, so git branch -d would refuse it; it goes in one git update-ref
// transaction that deletes each ref only at the head its merged pull request
// carried, and refuses them all if one moved. update-ref, unlike git branch,
// deletes a branch a worktree has out, so the holders are read again first.
func pruneApply(ctx context.Context, dir render.Dir, l lane, plan prunePlan, commonDir string) error {
	for _, batch := range pruneBatches(plan.merged, 200) {
		argv := append([]string{"branch", "-d"}, batch...)
		if _, err := render.RunCLI(ctx, dir, "git", argv); err != nil {
			return fmt.Errorf("prune: git branch -d: %w", err)
		}
	}
	squashed := pruneSquashedNames(plan)
	if len(squashed) > 0 {
		held, err := pruneHeldBranches(ctx, dir)
		if err != nil {
			return err
		}
		for _, branch := range squashed {
			if held[branch] {
				return fmt.Errorf("prune: a worktree checked out %s after the plan; re-run prune", branch)
			}
		}
		var tx strings.Builder
		for _, branch := range squashed {
			fmt.Fprintf(&tx, "delete refs/heads/%s %s\n", branch, plan.squashed[branch])
		}
		if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
			return fmt.Errorf("prune: git update-ref --stdin: %w", err)
		}
	}
	forget := slices.Concat(plan.merged, squashed, plan.stale)
	if !l.gt || len(forget) == 0 {
		return nil
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
	if squashed := pruneSquashedNames(plan); len(squashed) > 0 {
		segs = append(segs, fmt.Sprintf("%s %d branches whose pull request squash-landed%s", deleted, len(squashed), pruneNames(squashed)))
	}
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

func pruneSquashedNames(plan prunePlan) []string {
	names := make([]string, 0, len(plan.squashed))
	for branch := range plan.squashed {
		names = append(names, branch)
	}
	sort.Strings(names)
	return names
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
