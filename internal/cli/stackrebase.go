package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	stackRebasePrefix   = "stack rebase"
	stackRebaseStateDir = "ccx-stack-rebase"
	stackRebaseState    = "state.json"
	stackBriefLines     = 25
	stackCulprits       = 10
	stackVerdictTries   = 4
	stackStaleAfter     = 5 * time.Minute
)

// stackGitNoRerere keeps every rebase this command drives from replaying a
// recorded resolution, which silently resolves a conflict with a stale side,
// and from moving branch refs ahead of the one transaction that writes them.
var stackGitNoRerere = []string{"-c", "rerere.enabled=false", "-c", "rebase.updateRefs=false", "-c", "core.editor=true"}

type stackPR struct {
	Number    int      `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	Base      string   `json:"base"`
	Head      string   `json:"head"`
	Mergeable string   `json:"mergeable"`
	Labels    []string `json:"labels,omitempty"`
	Landed    bool     `json:"landed"`
}

func (p *stackPR) String() string {
	return fmt.Sprintf("#%d %q", p.Number, p.Title)
}

type stackRebaseBranch struct {
	Name      string   `json:"name"`
	Parent    string   `json:"parent"`
	WasParent string   `json:"was_parent"`
	Local     string   `json:"local"`
	Remote    string   `json:"remote,omitempty"`
	Head      string   `json:"head"`
	HeadRef   string   `json:"head_ref"`
	OldBase   string   `json:"old_base"`
	Landed    string   `json:"landed,omitempty"`
	PR        *stackPR `json:"pr,omitempty"`
	NewBase   string   `json:"new_base,omitempty"`
	NewHead   string   `json:"new_head,omitempty"`
}

type stackConflict struct {
	Branch    string `json:"branch"`
	Workspace string `json:"workspace"`
	Brief     string `json:"brief"`
}

type stackRebaseRun struct {
	Trunk    string              `json:"trunk"`
	Pin      string              `json:"pin"`
	NoPush   bool                `json:"no_push"`
	Applied  bool                `json:"applied,omitempty"`
	Roots    []string            `json:"roots"`
	Pid      int                 `json:"pid"`
	Host     string              `json:"host"`
	Branches []stackRebaseBranch `json:"branches"`
	Conflict *stackConflict      `json:"conflict,omitempty"`
	dir      string
	saved    time.Time
}

func (r *stackRebaseRun) branch(name string) *stackRebaseBranch {
	for i := range r.Branches {
		if r.Branches[i].Name == name {
			return &r.Branches[i]
		}
	}
	return nil
}

func (r *stackRebaseRun) headOf(name string) string {
	if name == r.Trunk {
		return r.Pin
	}
	return r.branch(name).NewHead
}

type stackRebaseOpts struct {
	parents   []string
	linearize []string
	landed    []string
	dryRun    bool
	noPush    bool
}

// stackPRLookup is a var so tests answer for GitHub.
var stackPRLookup = stackQueryPRs

func newStackRebaseCmd() *cobra.Command {
	var o stackRebaseOpts
	cmd := &cobra.Command{
		Use:   "rebase",
		Short: "Rebase the whole stack onto trunk and push it, stopping in a workspace on conflict",
		Long: `Rebase every branch of the stack onto its parent and trunk, then push it.

Every branch's local and remote head is recorded before anything moves, and each
branch is replayed from the base it was recorded on (--onto <new parent>
<recorded old parent>), so a push partway through never changes what a child is
rebased from. A branch whose pull request landed is dropped and its children
move onto what it sat on, leaving its squashed commits behind. Branches held by
other working copies are rewritten without a checkout, and those working copies
are reset onto the new heads with their uncommitted work re-applied.

A conflict stops the run before any ref moves: the rebase is left in progress
in a workspace of its own, with both sides' intent written out, and
ccx vcs stack continue resumes the rest of the stack from there
(ccx vcs stack abort drops it). rerere is off for every rebase it drives.

After the rewrite, gt's parents are recorded, the stack is force-pushed under
the remote heads recorded at the start, and one verdict line per pull request
names its pushed head, parent, and mergeability. Labels are never touched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackRebase(cmd, o)
		},
	}
	cmd.Flags().StringArrayVar(&o.parents, "parent", nil, "restack <branch>=<parent> onto a new parent (repeatable)")
	cmd.Flags().StringSliceVar(&o.linearize, "linearize", nil, "chain these branches in this order, each onto the one before it")
	cmd.Flags().StringArrayVar(&o.landed, "landed", nil, "treat <branch> as landed and drop it (repeatable)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan and move nothing")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "rewrite the local stack and gt's record, but push nothing")
	return cmd
}

func newStackContinueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "continue",
		Short: "Resume a stack rebase after resolving its conflict workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackContinue(cmd)
		},
	}
}

func newStackAbortCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abort",
		Short: "Drop a stopped stack rebase; nothing it planned has moved",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackAbort(cmd)
		},
	}
}

func runStackRebase(cmd *cobra.Command, o stackRebaseOpts) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, stackRebasePrefix, workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack rebase: this repository is not on the graphite lane, and a stack is Graphite's — rebase the branch with ccx vcs stack restack instead")
	}
	commonDir, err := gtCommonDir(ctx, l.dir(), stackRebasePrefix)
	if err != nil {
		return err
	}
	runs, err := stackRuns(commonDir)
	if err != nil {
		return err
	}
	for _, other := range runs {
		if other.Conflict != nil && other.Conflict.Workspace == l.root {
			return stackInProgress(other)
		}
	}
	run, err := stackPlan(ctx, l, commonDir, o)
	if err != nil {
		return err
	}
	for _, other := range runs {
		if !stackOverlaps(run, other) {
			continue
		}
		if !stackStale(other) {
			return stackInProgress(other)
		}
		if o.dryRun {
			cmd.Println(fmt.Sprintf("would reclaim the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
			continue
		}
		if err := stackReclaim(ctx, l, commonDir, other); err != nil {
			return err
		}
		cmd.Println(fmt.Sprintf("reclaimed the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
	}
	cmd.Println(strings.Join(stackPlanLines(run), "\n"))
	if o.dryRun {
		return nil
	}
	if err := stackClaim(commonDir, run); err != nil {
		return err
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return stackDrive(ctx, cmd, l, commonDir, run)
}

func stackInProgress(run *stackRebaseRun) error {
	where := stackHolder(run)
	if run.Conflict != nil {
		where = fmt.Sprintf("stopped on %s in %s, %s", run.Conflict.Branch, run.Conflict.Workspace, where)
	}
	return fmt.Errorf("stack rebase: a stack rebase of %s is already in progress (%s) — ccx vcs stack continue, or ccx vcs stack abort, from one of its branches", strings.Join(run.Roots, ", "), where)
}

func stackOverlaps(run, other *stackRebaseRun) bool {
	return slices.ContainsFunc(run.Branches, func(b stackRebaseBranch) bool { return other.branch(b.Name) != nil }) ||
		slices.ContainsFunc(run.Roots, func(root string) bool { return slices.Contains(other.Roots, root) })
}

// stackReclaim takes a stale run's state aside with one rename, so of two
// callers reclaiming the same run only one proceeds, and a run that saved
// after the caller judged it stale is put back rather than dropped.
func stackReclaim(ctx context.Context, l lane, commonDir string, run *stackRebaseRun) error {
	roots := strings.Join(run.Roots, ", ")
	tomb := fmt.Sprintf("%s.reclaim-%d", run.dir, os.Getpid())
	if err := os.Rename(run.dir, tomb); err != nil {
		return fmt.Errorf("stack rebase: reclaim the stale run of %s (another caller may have taken it — re-run): %w", roots, err)
	}
	info, err := os.Stat(stackStatePath(tomb))
	if err != nil || !info.ModTime().Equal(run.saved) {
		if err := os.Rename(tomb, run.dir); err != nil {
			return fmt.Errorf("stack rebase: restore the run of %s from %s: %w", roots, tomb, err)
		}
		return fmt.Errorf("stack rebase: the run of %s saved again while it was being reclaimed — re-run", roots)
	}
	if err := stackDropTempRefs(ctx, l.dir(), run); err != nil {
		return err
	}
	for _, root := range run.Roots[1:] {
		if err := os.RemoveAll(stackRunDir(commonDir, root)); err != nil {
			return fmt.Errorf("stack rebase: reclaim the stale run of %s: %w", roots, err)
		}
	}
	if err := os.RemoveAll(tomb); err != nil {
		return fmt.Errorf("stack rebase: reclaim the stale run of %s: %w", roots, err)
	}
	return nil
}

func stackHolder(run *stackRebaseRun) string {
	state := "exited"
	if stackPidAlive(run) {
		state = "running"
	}
	return fmt.Sprintf("pid %d on %s %s, last saved %s ago", run.Pid, run.Host, state, time.Since(run.saved).Round(time.Second))
}

// stackStale reports a run whose process died mid-replay: a run stopped on a
// conflict has exited by design and waits on its workspace, and an applied run
// has moved refs that only continue records.
func stackStale(run *stackRebaseRun) bool {
	host, _ := os.Hostname()
	if run.Host != host || run.Applied || stackPidAlive(run) || time.Since(run.saved) < stackStaleAfter {
		return false
	}
	if run.Conflict == nil {
		return true
	}
	_, err := os.Stat(run.Conflict.Workspace)
	return errors.Is(err, fs.ErrNotExist)
}

func stackPidAlive(run *stackRebaseRun) bool {
	err := syscall.Kill(run.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func stackPlan(ctx context.Context, l lane, commonDir string, o stackRebaseOpts) (*stackRebaseRun, error) {
	prefix := stackRebasePrefix
	state, err := gtStateAt(ctx, commonDir, prefix)
	if err != nil {
		return nil, err
	}
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return nil, err
	}
	overrides, err := stackOverrides(o)
	if err != nil {
		return nil, err
	}
	current, err := gitCurrentBranch(ctx, l.dir(), prefix)
	if err != nil {
		return nil, err
	}
	seeds := slices.Sorted(maps.Keys(overrides))
	for _, child := range seeds {
		if parent := overrides[child]; parent != trunk {
			seeds = append(seeds, parent)
		}
	}
	seeds = append(seeds, o.landed...)
	if current != "" && current != trunk {
		seeds = append([]string{current}, seeds...)
	}
	if len(seeds) == 0 {
		return nil, errors.New("stack rebase: HEAD is not on a stack branch — run it from a working copy holding one, or name the branches with --parent/--linearize")
	}
	members, roots, err := stackMembers(state, trunk, seeds)
	if err != nil {
		return nil, err
	}
	for child, parent := range overrides {
		if parent != trunk && !slices.Contains(members, parent) {
			return nil, fmt.Errorf("stack rebase: --parent %s=%s names a parent gt does not track", child, parent)
		}
	}

	prs, err := stackPRLookup(ctx, l.dir(), trunk, members)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read the stack's pull requests: %w", err)
	}
	tr, err := gtTrunkRef(ctx, l.dir(), prefix, trunk)
	if err != nil {
		return nil, err
	}
	pin, err := gtTrunkHead(ctx, l.dir(), prefix, tr)
	if err != nil {
		return nil, err
	}
	remotes, err := stackRemoteHeads(ctx, l.dir(), tr.Remote(), members)
	if err != nil {
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}
	run := &stackRebaseRun{Trunk: trunk, Pin: pin, NoPush: o.noPush, Roots: roots, Pid: os.Getpid(), Host: host}
	byName := map[string]*stackRebaseBranch{}
	for _, name := range members {
		b, err := stackSnapshot(ctx, l.dir(), tr, state[name], name, remotes[name], prs[name], slices.Contains(o.landed, name), pin)
		if err != nil {
			return nil, err
		}
		byName[name] = &b
	}
	for name, b := range byName {
		b.Parent = b.WasParent
		if p, ok := overrides[name]; ok {
			b.Parent = p
		}
	}
	for _, b := range byName {
		for seen := map[string]bool{}; b.Parent != trunk && byName[b.Parent].Landed != ""; {
			if seen[b.Parent] {
				return nil, fmt.Errorf("stack rebase: the parents of %s cycle", b.Name)
			}
			seen[b.Parent] = true
			b.Parent = byName[b.Parent].Parent
		}
	}
	order, err := stackOrder(trunk, byName)
	if err != nil {
		return nil, err
	}
	for _, name := range order {
		b := byName[name]
		if b.Landed == "" {
			if b.OldBase, err = stackOldBase(ctx, l.dir(), trunk, pin, state[name], b, byName); err != nil {
				return nil, err
			}
			if err := stackOwnWork(ctx, l.dir(), tr, pin, b); err != nil {
				return nil, err
			}
		}
		run.Branches = append(run.Branches, *b)
	}
	return run, nil
}

func stackOverrides(o stackRebaseOpts) (map[string]string, error) {
	overrides := map[string]string{}
	for _, pair := range o.parents {
		child, parent, ok := strings.Cut(pair, "=")
		if !ok || child == "" || parent == "" || child == parent {
			return nil, fmt.Errorf("stack rebase: --parent %q is not <branch>=<parent>", pair)
		}
		overrides[child] = parent
	}
	for i := 1; i < len(o.linearize); i++ {
		overrides[o.linearize[i]] = o.linearize[i-1]
	}
	for _, name := range o.landed {
		if _, ok := overrides[name]; ok {
			return nil, fmt.Errorf("stack rebase: %s is both landed and given a parent", name)
		}
	}
	return overrides, nil
}

func stackMembers(state gtState, trunk string, seeds []string) ([]string, []string, error) {
	var members, roots []string
	for _, seed := range seeds {
		if seed == trunk {
			return nil, nil, fmt.Errorf("stack rebase: %s is trunk, not a stack branch", seed)
		}
		if slices.Contains(members, seed) {
			continue
		}
		down, err := gtDownstack(stackRebasePrefix, state, seed, trunk)
		if err != nil {
			return nil, nil, err
		}
		bottom := down[len(down)-1]
		roots = append(roots, bottom)
		up, err := gtUpstack(stackRebasePrefix, state, bottom)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range append([]string{bottom}, up...) {
			if !slices.Contains(members, name) {
				members = append(members, name)
			}
		}
	}
	return members, roots, nil
}

func stackRemoteHeads(ctx context.Context, dir render.Dir, remote string, branches []string) (map[string]string, error) {
	argv := make([]string, 0, 2+len(branches))
	argv = append(argv, "ls-remote", remote)
	for _, b := range branches {
		argv = append(argv, gtRestackRef(b))
	}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: git ls-remote %s: %w", remote, err)
	}
	heads := map[string]string{}
	fetch := []string{"fetch", "--quiet", remote}
	for line := range strings.Lines(out) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		heads[name] = sha
		fetch = append(fetch, "+"+ref+":refs/remotes/"+remote+"/"+name)
	}
	if len(heads) > 0 {
		if _, err := render.RunCLI(ctx, dir, "git", fetch); err != nil {
			return nil, fmt.Errorf("stack rebase: git fetch %s: %w", remote, err)
		}
	}
	return heads, nil
}

func stackSnapshot(ctx context.Context, dir render.Dir, tr vcs.Trunk, s gtBranchState, name, remote string, pr *stackPR, declared bool, pin string) (stackRebaseBranch, error) {
	if s.State != "" {
		return stackRebaseBranch{}, fmt.Errorf("stack rebase: %s is %s in gt — unfreeze it, or rebase a stack without it", name, s.State)
	}
	b := stackRebaseBranch{
		Name: name, WasParent: s.Parents[0].Ref, Local: s.Head, Remote: remote,
		Head: s.Head, HeadRef: gtRestackRef(name), PR: pr,
	}
	if remote != "" && remote != s.Head {
		ahead, err := gitIsAncestor(ctx, dir, stackRebasePrefix, remote, s.Head)
		if err != nil {
			return b, err
		}
		behind, err := gitIsAncestor(ctx, dir, stackRebasePrefix, s.Head, remote)
		if err != nil {
			return b, err
		}
		switch {
		case behind:
			b.Head, b.HeadRef = remote, stackTempRef(name)
		case !ahead:
			return b, fmt.Errorf("stack rebase: %s has diverged from %s/%s (local %.12s, remote %.12s) — someone pushed to it; reconcile the two by hand, then re-run", name, tr.Remote(), name, s.Head, remote)
		}
	}
	contained, err := gitIsAncestor(ctx, dir, stackRebasePrefix, b.Head, pin)
	if err != nil {
		return b, err
	}
	switch {
	case declared:
		b.Landed = "declared landed"
	case pr != nil && pr.Landed:
		if pr.Head != "" && pr.Head != b.Head {
			within, err := gitIsAncestor(ctx, dir, stackRebasePrefix, b.Head, pr.Head)
			if err != nil {
				return b, err
			}
			if !within {
				return b, fmt.Errorf("stack rebase: %s's pull request #%d landed at %.12s, but the branch holds commits past it (%.12s) — move them to a new branch, or pass --landed %s to drop them too", name, pr.Number, pr.Head, b.Head, name)
			}
		}
		b.Landed = fmt.Sprintf("#%d landed", pr.Number)
	case contained:
		b.Landed = "already in " + tr.Name()
	case pr != nil && pr.State == "CLOSED":
		return b, fmt.Errorf("stack rebase: %s's pull request #%d closed without landing — reopen it, drop the branch with ccx vcs stack drop %s, or pass --landed %s if it did land", name, pr.Number, name, name)
	}
	return b, nil
}

func stackOrder(trunk string, byName map[string]*stackRebaseBranch) ([]string, error) {
	children := map[string][]string{}
	var landed []string
	for name, b := range byName {
		if b.Landed != "" {
			landed = append(landed, name)
			continue
		}
		children[b.Parent] = append(children[b.Parent], name)
	}
	slices.Sort(landed)
	order := slices.Clone(landed)
	for queue := []string{trunk}; len(queue) > 0; queue = queue[1:] {
		kids := children[queue[0]]
		slices.Sort(kids)
		order = append(order, kids...)
		queue = append(queue, kids...)
	}
	if len(order) != len(byName) {
		return nil, errors.New("stack rebase: the requested parents form a cycle")
	}
	return order, nil
}

// stackOldBase is the commit a branch's own work starts after: the furthest of
// its parent's recorded head and gt's recorded parent revision that the branch
// contains. Both are read before anything moves, so a push mid-run never
// changes the answer, and a squash-landed parent's commits stay behind.
func stackOldBase(ctx context.Context, dir render.Dir, trunk, pin string, s gtBranchState, self *stackRebaseBranch, byName map[string]*stackRebaseBranch) (string, error) {
	if s.Parents[0].Ref == trunk {
		return stackMergeBase(ctx, dir, self.Head, pin)
	}
	head := byName[s.Parents[0].Ref]
	best := ""
	for _, candidate := range []string{head.Head, head.Local, s.Parents[0].SHA} {
		if candidate == "" || candidate == best {
			continue
		}
		ok, err := gitIsAncestor(ctx, dir, stackRebasePrefix, candidate, self.Head)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if best == "" {
			best = candidate
			continue
		}
		further, err := gitIsAncestor(ctx, dir, stackRebasePrefix, best, candidate)
		if err != nil {
			return "", err
		}
		if further {
			best = candidate
		}
	}
	if best != "" {
		return best, nil
	}
	return stackMergeBase(ctx, dir, self.Head, head.Head)
}

func stackMergeBase(ctx context.Context, dir render.Dir, a, b string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", a, b})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git merge-base %.12s %.12s: %w", a, b, err)
	}
	return strings.TrimSpace(out), nil
}

func stackOwnWork(ctx context.Context, dir render.Dir, tr vcs.Trunk, pin string, b *stackRebaseBranch) error {
	span := b.OldBase + ".." + b.Head
	replayed, err := gtRevCount(ctx, stackRebasePrefix, dir, span)
	if err != nil {
		return err
	}
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, span, "--not", pin)
	if err != nil {
		return err
	}
	if replayed == own {
		return nil
	}
	return fmt.Errorf("stack rebase: %s would replay %d commits but owns %d — the rest are already in %s; name its real parent with --parent %s=<branch>",
		b.Name, replayed, own, tr.Name(), b.Name)
}

func stackPlanLines(run *stackRebaseRun) []string {
	lines := make([]string, 0, 1+len(run.Branches))
	lines = append(lines, fmt.Sprintf("plan · trunk %s@%.12s", run.Trunk, run.Pin))
	for _, b := range run.Branches {
		fields := []string{b.Name}
		if b.Landed != "" {
			fields = append(fields, "drop ("+b.Landed+")")
		} else {
			parent := "onto " + b.Parent
			if b.Parent != b.WasParent {
				parent += " (was " + b.WasParent + ")"
			}
			fields = append(fields, parent, fmt.Sprintf("from %.12s", b.OldBase))
			if b.Head != b.Local {
				fields = append(fields, fmt.Sprintf("taking the remote head %.12s", b.Head))
			}
		}
		if b.PR != nil {
			fields = append(fields, b.PR.String())
		}
		lines = append(lines, strings.Join(fields, shipSep))
	}
	return lines
}

func stackDrive(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed != "" || b.NewHead != "" {
			continue
		}
		b.NewBase = run.headOf(b.Parent)
		if b.NewBase == b.OldBase {
			b.NewHead = b.Head
			continue
		}
		head, err := stackReplay(ctx, l.dir(), b)
		if errors.Is(err, errReplayConflict) {
			return stackOpenConflict(ctx, cmd, l, commonDir, run, b)
		}
		if err != nil {
			return err
		}
		b.NewHead = head
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return stackFinish(ctx, cmd, l, commonDir, run)
}

// stackReplay computes a branch's rebased head without moving any ref:
// replay.refAction=print makes git 2.55 print the update it would otherwise
// apply, and every earlier git prints it regardless of the key.
func stackReplay(ctx context.Context, dir render.Dir, b *stackRebaseBranch) (string, error) {
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, b.OldBase+".."+b.Head)
	if err != nil {
		return "", err
	}
	if own == 0 {
		return b.NewBase, nil
	}
	if b.HeadRef != gtRestackRef(b.Name) {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"update-ref", b.HeadRef, b.Head}); err != nil {
			return "", fmt.Errorf("stack rebase: pin %s's remote head: %w", b.Name, err)
		}
	}
	span := b.OldBase + ".." + b.HeadRef
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"-c", "replay.refAction=print", "replay", "--onto", b.NewBase, span})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git replay: %w", err)
	}
	if code != 0 {
		if strings.TrimSpace(stderr) == "" {
			return "", errReplayConflict
		}
		return "", fmt.Errorf("stack rebase: git replay --onto %.12s %s: %s", b.NewBase, span, strings.TrimSpace(stderr))
	}
	for line := range strings.Lines(out) {
		if fields := strings.Fields(line); len(fields) == 4 && fields[0] == "update" && fields[1] == b.HeadRef {
			return fields[2], nil
		}
	}
	return "", fmt.Errorf("stack rebase: git replay printed no update for %s over %d commit(s): %q", b.HeadRef, own, strings.TrimSpace(out))
}

// stackTempRef pins a remote head that is ahead of the local branch under a
// branch-namespace ref, the only kind git replay reports an update for.
func stackTempRef(branch string) string {
	return "refs/heads/" + stackRebaseStateDir + "/" + branch
}

func stackDropTempRefs(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	for _, b := range run.Branches {
		if b.HeadRef == stackTempRef(b.Name) {
			if _, err := render.RunCLI(ctx, dir, "git", []string{"update-ref", "-d", b.HeadRef}); err != nil {
				return fmt.Errorf("stack rebase: delete %s: %w", b.HeadRef, err)
			}
		}
	}
	return nil
}

func stackOpenConflict(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, b *stackRebaseBranch) error {
	ws, err := mintWorktreePath(stackRebasePrefix, l.checkout, "conflict-"+strings.ReplaceAll(b.Name, "/", "-"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(ws); err == nil {
		return fmt.Errorf("stack rebase: %s already exists — remove it (ccx vcs worktree rm %s) and re-run", ws, filepath.Base(ws))
	}
	if err := os.MkdirAll(filepath.Dir(ws), 0o750); err != nil {
		return fmt.Errorf("stack rebase: mint the conflict workspace: %w", err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "add", "--detach", ws, b.Head}); err != nil {
		return fmt.Errorf("stack rebase: git worktree add %s: %w", ws, err)
	}
	argv := append(slices.Clone(stackGitNoRerere), "rebase", "--onto", b.NewBase, b.OldBase)
	_, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(ws), "git", argv)
	if err != nil {
		return fmt.Errorf("stack rebase: git rebase in %s: %w", ws, err)
	}
	run.Conflict = &stackConflict{Branch: b.Name, Workspace: ws, Brief: stackBriefPath(run.dir, b.Name)}
	if code == 0 {
		return stackResume(ctx, cmd, l, commonDir, run)
	}
	unmerged, err := stackUnmerged(ctx, ws)
	if err != nil {
		return err
	}
	if len(unmerged) == 0 {
		return fmt.Errorf("stack rebase: git rebase in %s stopped without a conflict: %s", ws, strings.TrimSpace(stderr))
	}
	return stackStopped(ctx, run, b, unmerged)
}

func stackStopped(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch, unmerged []string) error {
	brief := stackBrief(ctx, run, b, unmerged)
	if err := os.WriteFile(run.Conflict.Brief, []byte(brief), 0o600); err != nil {
		return fmt.Errorf("stack rebase: write the conflict brief: %w", err)
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return errors.New(brief)
}

func stackUnmerged(ctx context.Context, ws string) ([]string, error) {
	out, err := render.RunCLI(ctx, render.Dir(ws), "git", []string{"diff", "--name-only", "--diff-filter=U"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: list the conflicted files in %s: %w", ws, err)
	}
	return strings.Fields(out), nil
}

func stackBrief(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch, unmerged []string) string {
	ws := render.Dir(run.Conflict.Workspace)
	var s strings.Builder
	fmt.Fprintf(&s, "stack rebase: conflict — %s does not rebase onto %s cleanly; nothing has moved\n", b.Name, b.Parent)
	fmt.Fprintf(&s, "workspace: %s (detached, rebase in progress, rerere off)\n", ws)
	if stopped, err := render.RunCLI(ctx, ws, "git", []string{"log", "-1", "--format=%h %s", "REBASE_HEAD"}); err == nil {
		fmt.Fprintf(&s, "stopped at: %s\n", strings.TrimSpace(stopped))
	}
	s.WriteString("conflicted files:\n")
	for _, f := range unmerged {
		fmt.Fprintf(&s, "  %s\n", f)
	}
	stackBriefIntent(&s, "this branch", b.Name, b.PR)
	if parent := run.branch(b.Parent); parent != nil {
		stackBriefIntent(&s, "the side it lands on", parent.Name, parent.PR)
	}
	argv := append([]string{"log", "--format=%h %s", fmt.Sprintf("-%d", stackCulprits), b.OldBase + ".." + b.NewBase, "--"}, unmerged...)
	if culprits, err := render.RunCLI(ctx, ws, "git", argv); err == nil && strings.TrimSpace(culprits) != "" {
		fmt.Fprintf(&s, "upstream commits touching these files (%.12s..%.12s):\n", b.OldBase, b.NewBase)
		for line := range strings.Lines(strings.TrimSpace(culprits)) {
			fmt.Fprintf(&s, "  %s\n", strings.TrimRight(line, "\n"))
		}
	}
	fmt.Fprintf(&s, "next: resolve the files in %s and git add them, then run ccx vcs stack continue from any working copy of this repository; ccx vcs stack abort drops the run.\n", ws)
	s.WriteString("never git rebase --continue by hand: continue drives it with rerere off, then rebases and pushes the rest of the stack.")
	return s.String()
}

func stackBriefIntent(s *strings.Builder, side, name string, pr *stackPR) {
	if pr == nil {
		fmt.Fprintf(s, "%s: %s (no pull request)\n", side, name)
		return
	}
	fmt.Fprintf(s, "%s: %s %s %s\n", side, name, pr.String(), pr.URL)
	lines := strings.Split(strings.TrimSpace(pr.Body), "\n")
	if len(lines) > stackBriefLines {
		lines = append(lines[:stackBriefLines], "…")
	}
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(s, "  | %s\n", line)
		}
	}
}

func runStackContinue(cmd *cobra.Command) error {
	ctx := cmd.Context()
	l, commonDir, run, err := stackResolveRun(ctx)
	if err != nil {
		return err
	}
	if run.Conflict == nil {
		return stackDrive(ctx, cmd, l, commonDir, run)
	}
	return stackResume(ctx, cmd, l, commonDir, run)
}

func stackResume(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	c := run.Conflict
	ws := render.Dir(c.Workspace)
	b := run.branch(c.Branch)
	if b.NewHead != "" {
		return stackCloseWorkspace(ctx, cmd, l, commonDir, run)
	}
	unmerged, err := stackUnmerged(ctx, c.Workspace)
	if err != nil {
		return err
	}
	if len(unmerged) > 0 {
		return fmt.Errorf("stack rebase: %s still has unresolved files: %s — resolve them and git add them first", c.Workspace, strings.Join(unmerged, ", "))
	}
	for stackRebasing(ctx, ws) {
		argv := append(slices.Clone(stackGitNoRerere), "rebase", "--continue")
		_, code, stderr, err := render.RunCLIExitCode(ctx, ws, "git", argv)
		if err != nil {
			return fmt.Errorf("stack rebase: git rebase --continue in %s: %w", ws, err)
		}
		if code == 0 {
			continue
		}
		if unmerged, err = stackUnmerged(ctx, c.Workspace); err != nil {
			return err
		}
		if len(unmerged) == 0 {
			return fmt.Errorf("stack rebase: git rebase --continue in %s failed: %s", ws, strings.TrimSpace(stderr))
		}
		return stackStopped(ctx, run, b, unmerged)
	}
	head, err := stackRevParse(ctx, ws, "HEAD")
	if err != nil {
		return err
	}
	onto, err := gitIsAncestor(ctx, ws, stackRebasePrefix, b.NewBase, head)
	if err != nil {
		return err
	}
	if !onto {
		return fmt.Errorf("stack rebase: %s is not on %s's new head %.12s — its rebase was aborted or reset; ccx vcs stack abort, then re-run", c.Workspace, b.Parent, b.NewBase)
	}
	b.NewHead = head
	if err := stackSaveRun(run); err != nil {
		return err
	}
	cmd.Println(fmt.Sprintf("resolved %s at %.12s", b.Name, head))
	return stackCloseWorkspace(ctx, cmd, l, commonDir, run)
}

func stackCloseWorkspace(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	c := run.Conflict
	if _, err := os.Stat(c.Workspace); err == nil {
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "remove", c.Workspace}); err != nil {
			return fmt.Errorf("stack rebase: remove the conflict workspace %s (clear what it holds, then continue again): %w", c.Workspace, err)
		}
	}
	if err := os.Remove(c.Brief); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stack rebase: %w", err)
	}
	run.Conflict = nil
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return stackDrive(ctx, cmd, l, commonDir, run)
}

func stackRebasing(ctx context.Context, ws render.Dir) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path, err := render.RunCLI(ctx, ws, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", dir})
		if err != nil {
			continue
		}
		if _, err := os.Stat(strings.TrimSpace(path)); err == nil {
			return true
		}
	}
	return false
}

func stackRevParse(ctx context.Context, dir render.Dir, rev string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", rev})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

func runStackAbort(cmd *cobra.Command) error {
	ctx := cmd.Context()
	l, commonDir, run, err := stackResolveRun(ctx)
	if err != nil {
		return err
	}
	if run.Applied {
		return errors.New("stack abort: the rewritten stack is already written locally, so there is nothing left to abort — ccx vcs stack continue finishes recording and pushing it")
	}
	if c := run.Conflict; c != nil {
		if stackRebasing(ctx, render.Dir(c.Workspace)) {
			if _, err := render.RunCLI(ctx, render.Dir(c.Workspace), "git", []string{"rebase", "--abort"}); err != nil {
				return fmt.Errorf("stack abort: git rebase --abort in %s: %w", c.Workspace, err)
			}
		}
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "remove", "--force", c.Workspace}); err != nil {
			return fmt.Errorf("stack abort: remove %s: %w", c.Workspace, err)
		}
	}
	if err := stackDropTempRefs(ctx, l.dir(), run); err != nil {
		return err
	}
	if err := stackClearRun(commonDir, run); err != nil {
		return fmt.Errorf("stack abort: %w", err)
	}
	cmd.Println("aborted · no branch moved")
	return nil
}

func stackResolveRun(ctx context.Context) (lane, string, *stackRebaseRun, error) {
	l, err := resolveLane(ctx, stackRebasePrefix, workingDir(ctx), false)
	if err != nil {
		return lane{}, "", nil, err
	}
	commonDir, err := gtCommonDir(ctx, l.dir(), stackRebasePrefix)
	if err != nil {
		return lane{}, "", nil, err
	}
	runs, err := stackRuns(commonDir)
	if err != nil {
		return lane{}, "", nil, err
	}
	current, err := gitCurrentBranch(ctx, l.dir(), stackRebasePrefix)
	if err != nil {
		return lane{}, "", nil, err
	}
	run, err := stackRunFor(runs, l.root, current)
	if err != nil {
		return lane{}, "", nil, err
	}
	if run.Host, err = os.Hostname(); err != nil {
		return lane{}, "", nil, fmt.Errorf("stack rebase: %w", err)
	}
	run.Pid = os.Getpid()
	if err := stackSaveRun(run); err != nil {
		return lane{}, "", nil, err
	}
	if run.Conflict != nil && l.root == run.Conflict.Workspace {
		if l, err = resolveLane(ctx, stackRebasePrefix, l.checkout.MainRoot, false); err != nil {
			return lane{}, "", nil, err
		}
	}
	return l, commonDir, run, nil
}

func stackFinish(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	prefix := stackRebasePrefix
	errW := cmd.ErrOrStderr()
	var moves []restackMove
	var movers, live, dropped []string
	reparent := map[string]string{}
	revisions := map[string]string{}
	leases := map[string]string{}
	for _, b := range run.Branches {
		if b.Landed != "" {
			dropped = append(dropped, b.Name)
			continue
		}
		live = append(live, b.Name)
		leases[b.Name] = b.Remote
		revisions[b.Name] = b.NewBase
		if b.Parent != b.WasParent {
			reparent[b.Name] = b.Parent
		}
		if b.NewHead != b.Local {
			movers = append(movers, b.Name)
			moves = append(moves, restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase})
		}
	}
	var realigned []string
	var alignErr error
	if !run.Applied {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		snapshots, err := gtRestackSnapshots(ctx, prefix, movers, holders)
		if err != nil {
			return err
		}
		if err := stackWriteRefs(ctx, l.dir(), run); err != nil {
			return err
		}
		run.Applied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
		realigned, alignErr = gtRestackAlign(ctx, prefix, holders, snapshots, moves)
	}
	if err := errors.Join(
		gtmeta.Reparent(ctx, commonDir, reparent),
		gtmeta.RecordRestacked(ctx, commonDir, revisions),
		gtmeta.Forget(ctx, commonDir, dropped),
		stackDropTempRefs(ctx, l.dir(), run),
	); err != nil {
		return fmt.Errorf("%s: the branches are rewritten, but recording the stack in gt failed — fix the cause and run ccx vcs stack continue: %w", prefix, errors.Join(err, alignErr))
	}
	state, err := gtStateAt(ctx, commonDir, prefix)
	if err != nil {
		return err
	}
	tr, err := gtTrunkRefOffline(ctx, l.dir(), prefix, run.Trunk)
	if err != nil {
		return err
	}
	if trunkPin, err := gtTrunkPin(ctx, prefix, l.checkout, l.dir(), tr, state[run.Trunk].Head); err != nil {
		return errors.Join(err, alignErr)
	} else if trunkPin.diverged > 0 {
		_, _ = fmt.Fprintf(errW, "%s: warning: local %s holds %d commit(s) %s does not; the stack sits on %s, not on them\n", prefix, run.Trunk, trunkPin.diverged, tr.Ref(), tr.Ref())
	}
	summary := []string{fmt.Sprintf("rebased %s onto %s@%.12s", gtBranchCount(len(movers)), run.Trunk, run.Pin)}
	if len(dropped) > 0 {
		summary = append(summary, "dropped landed "+strings.Join(dropped, ", "))
	}
	if len(realigned) > 0 {
		summary = append(summary, "reset "+strings.Join(realigned, ", "))
	}
	cmd.Println(strings.Join(summary, shipSep))
	if alignErr != nil {
		return alignErr
	}
	if !run.NoPush {
		state, err = gtStateAt(ctx, commonDir, prefix)
		if err != nil {
			return err
		}
		sub := gtSubmit{prefix: prefix, suffix: " — the local stack is rewritten; reconcile the remote, then run ccx vcs stack continue", leases: leases, trunkHead: run.Pin}
		if _, _, err := gtSubmitStack(ctx, l, errW, sub, commonDir, state, tr, live); err != nil {
			return err
		}
	}
	if err := stackClearRun(commonDir, run); err != nil {
		return fmt.Errorf("%s: clear the run state: %w", prefix, err)
	}
	if run.NoPush {
		cmd.Println("not pushed (--no-push)")
		return nil
	}
	return stackVerdict(ctx, cmd, l.dir(), run, live)
}

// stackWriteRefs moves every rewritten branch in one transaction and verifies
// every unmoved one, so a branch that changed locally while the run was stopped
// fails the whole write rather than leaving a child on a parent it never saw.
func stackWriteRefs(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		switch {
		case b.Landed != "":
		case b.NewHead != b.Local:
			fmt.Fprintf(&tx, "update %s %s %s\n", gtRestackRef(b.Name), b.NewHead, b.Local)
		default:
			fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), b.Local)
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack rebase: a branch moved locally since the run started, so nothing was written — ccx vcs stack abort, then re-run: %w", err)
	}
	return nil
}

func stackVerdict(ctx context.Context, cmd *cobra.Command, dir render.Dir, run *stackRebaseRun, live []string) error {
	var prs map[string]*stackPR
	var err error
	for try := range stackVerdictTries {
		if prs, err = stackPRLookup(ctx, dir, run.Trunk, live); err != nil {
			return fmt.Errorf("stack rebase: pushed, but the verdict could not read the pull requests: %w", err)
		}
		if !stackAnyUnknown(prs) || try == stackVerdictTries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(statusMergeableRetry):
		}
	}
	for _, name := range live {
		b := run.branch(name)
		fields := []string{name}
		pr := prs[name]
		if pr == nil {
			fields = append(fields, "no pull request", fmt.Sprintf("head %.12s", b.NewHead), "parent "+b.Parent)
			cmd.Println(strings.Join(fields, shipSep))
			continue
		}
		fields = append(fields, fmt.Sprintf("#%d", pr.Number), fmt.Sprintf("head %.12s", pr.Head), "parent "+b.Parent, strings.ToLower(pr.Mergeable))
		if pr.Head != b.NewHead {
			fields = append(fields, fmt.Sprintf("GitHub still shows %.12s, pushed %.12s", pr.Head, b.NewHead))
		}
		if pr.Base != b.Parent {
			fields = append(fields, "base "+pr.Base+" ≠ parent")
		}
		if len(pr.Labels) > 0 {
			fields = append(fields, "labels "+strings.Join(pr.Labels, ","))
		}
		cmd.Println(strings.Join(fields, shipSep))
	}
	return nil
}

func stackAnyUnknown(prs map[string]*stackPR) bool {
	for _, pr := range prs {
		if pr != nil && pr.State == "OPEN" && pr.Mergeable == statusUnknown {
			return true
		}
	}
	return false
}

func stackRunDir(commonDir, root string) string {
	return filepath.Join(commonDir, stackRebaseStateDir, strings.ReplaceAll(root, "/", "-"))
}

func stackStatePath(dir string) string {
	return filepath.Join(dir, stackRebaseState)
}

func stackBriefPath(dir, branch string) string {
	return filepath.Join(dir, strings.ReplaceAll(branch, "/", "-")+".md")
}

func stackClaim(commonDir string, run *stackRebaseRun) error {
	if err := os.MkdirAll(filepath.Join(commonDir, stackRebaseStateDir), 0o750); err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	for i, root := range run.Roots {
		dir := stackRunDir(commonDir, root)
		if err := stackReclaimAbandoned(dir); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o750); err != nil {
			for _, claimed := range run.Roots[:i] {
				_ = os.Remove(stackRunDir(commonDir, claimed))
			}
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("stack rebase: another stack rebase of %s started meanwhile — ccx vcs stack continue, or ccx vcs stack abort", root)
			}
			return fmt.Errorf("stack rebase: %w", err)
		}
	}
	run.dir = stackRunDir(commonDir, run.Roots[0])
	return nil
}

func stackReclaimAbandoned(dir string) error {
	info, err := os.Stat(dir)
	if err != nil || time.Since(info.ModTime()) < stackStaleAfter {
		return nil
	}
	if _, err := os.Stat(stackStatePath(dir)); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	tomb := fmt.Sprintf("%s.reclaim-%d", dir, os.Getpid())
	if err := os.Rename(dir, tomb); err != nil {
		return nil
	}
	moved, err := os.Stat(tomb)
	if err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	if _, err := os.Stat(stackStatePath(tomb)); !errors.Is(err, fs.ErrNotExist) || !moved.ModTime().Equal(info.ModTime()) {
		if err := os.Rename(tomb, dir); err != nil {
			return fmt.Errorf("stack rebase: restore %s from %s: %w", dir, tomb, err)
		}
		return nil
	}
	return os.RemoveAll(tomb)
}

func stackClearRun(commonDir string, run *stackRebaseRun) error {
	for _, root := range run.Roots {
		if err := os.RemoveAll(stackRunDir(commonDir, root)); err != nil {
			return err
		}
	}
	return nil
}

func stackRuns(commonDir string) ([]*stackRebaseRun, error) {
	entries, err := os.ReadDir(filepath.Join(commonDir, stackRebaseStateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}
	var runs []*stackRebaseRun
	for _, e := range entries {
		if !e.IsDir() || strings.Contains(e.Name(), ".reclaim-") {
			continue
		}
		dir := filepath.Join(commonDir, stackRebaseStateDir, e.Name())
		info, err := os.Stat(stackStatePath(dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stack rebase: %w", err)
		}
		data, err := os.ReadFile(stackStatePath(dir))
		if err != nil {
			return nil, fmt.Errorf("stack rebase: %w", err)
		}
		run := &stackRebaseRun{dir: dir, saved: info.ModTime()}
		if err := json.Unmarshal(data, run); err != nil {
			return nil, fmt.Errorf("stack rebase: read %s: %w", stackStatePath(dir), err)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func stackRunFor(runs []*stackRebaseRun, root, branch string) (*stackRebaseRun, error) {
	if len(runs) == 0 {
		return nil, errors.New("stack rebase: no stack rebase is in progress in this repository")
	}
	for _, run := range runs {
		if run.Conflict != nil && run.Conflict.Workspace == root {
			return run, nil
		}
	}
	for _, run := range runs {
		if branch != "" && run.branch(branch) != nil {
			return run, nil
		}
	}
	if len(runs) == 1 {
		return runs[0], nil
	}
	var stacks []string
	for _, run := range runs {
		stacks = append(stacks, strings.Join(run.Roots, "+"))
	}
	return nil, fmt.Errorf("stack rebase: %d stack rebases are in progress (%s) — run this from a working copy on one of the stack's branches, or from its conflict workspace", len(runs), strings.Join(stacks, ", "))
}

func stackSaveRun(run *stackRebaseRun) error {
	path := stackStatePath(run.dir)
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("stack rebase: encode the run state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("stack rebase: write %s: %w", path, err)
	}
	return nil
}

func stackQueryPRs(ctx context.Context, dir render.Dir, trunk string, branches []string) (map[string]*stackPR, error) {
	argv := make([]string, 0, 8+2*len(branches))
	argv = append(argv, "api", "graphql", "-F", "owner={owner}", "-F", "repo={repo}")
	for i, branch := range branches {
		argv = append(argv, "-f", downstackPRAlias(i)+"="+branch)
	}
	argv = append(argv, "-f", "query="+stackPRQuery(len(branches)))
	out, err := render.RunCLI(ctx, dir, "gh", argv)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	var resp struct {
		Data struct {
			Repository map[string]struct {
				Nodes []struct {
					Number      int    `json:"number"`
					URL         string `json:"url"`
					Title       string `json:"title"`
					Body        string `json:"body"`
					BaseRefName string `json:"baseRefName"`
					HeadRefOid  string `json:"headRefOid"`
					Mergeable   string `json:"mergeable"`
					Labels      struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					prLanding
				} `json:"nodes"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parse gh api graphql: %w", err)
	}
	prs := map[string]*stackPR{}
	var closes []prQueueClose
	byNumber := map[int]*stackPR{}
	for i, branch := range branches {
		nodes := resp.Data.Repository[downstackPRAlias(i)].Nodes
		if len(nodes) == 0 {
			continue
		}
		n := nodes[0]
		pr := &stackPR{
			Number: n.Number, URL: n.URL, Title: n.Title, Body: n.Body, State: n.State,
			Base: n.BaseRefName, Head: n.HeadRefOid, Mergeable: n.Mergeable,
		}
		for _, label := range n.Labels.Nodes {
			pr.Labels = append(pr.Labels, label.Name)
		}
		switch n.verdict(true) {
		case prLanded:
			pr.Landed = true
		case prQueueClosed:
			closes = append(closes, prQueueClose{Number: n.Number, Base: trunk})
			byNumber[n.Number] = pr
		}
		prs[branch] = pr
	}
	for number, landed := range resolveQueueLandings(ctx, dir, closes) {
		byNumber[number].Landed = landed
	}
	return prs, nil
}

func stackPRQuery(n int) string {
	decls := make([]string, 0, 2+n)
	decls = append(decls, "$owner: String!", "$repo: String!")
	var fields strings.Builder
	for i := range n {
		alias := downstackPRAlias(i)
		decls = append(decls, "$"+alias+": String!")
		fmt.Fprintf(&fields, "    %s: pullRequests(headRefName: $%s, first: 1, orderBy: {field: CREATED_AT, direction: DESC})"+
			" { nodes { number url title body baseRefName headRefOid mergeable labels(first: 20) { nodes { name } } %s } }\n",
			alias, alias, prLandingFields)
	}
	return fmt.Sprintf("query(%s) {\n  repository(owner: $owner, name: $repo) {\n%s  }\n}", strings.Join(decls, ", "), fields.String())
}
