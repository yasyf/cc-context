package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/gtapi"
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
	Name        string            `json:"name"`
	Parent      string            `json:"parent"`
	WasParent   string            `json:"was_parent"`
	Local       string            `json:"local"`
	Remote      string            `json:"remote,omitempty"`
	Head        string            `json:"head"`
	HeadRef     string            `json:"head_ref"`
	OldBase     string            `json:"old_base"`
	SourceBase  string            `json:"source_base"`
	Publication *stackPublication `json:"publication,omitempty"`
	Landed      string            `json:"landed,omitempty"`
	Held        string            `json:"held,omitempty"`
	Kept        bool              `json:"kept,omitempty"`
	PR          *stackPR          `json:"pr,omitempty"`
	NewBase     string            `json:"new_base,omitempty"`
	NewHead     string            `json:"new_head,omitempty"`
}

type stackConflict struct {
	Branch    string   `json:"branch"`
	Workspace string   `json:"workspace"`
	Brief     string   `json:"brief"`
	Generated []string `json:"generated,omitempty"`
}

type stackRebaseRun struct {
	Trunk       string `json:"trunk"`
	Pin         string `json:"pin"`
	NoPush      bool   `json:"no_push"`
	Git         bool   `json:"git,omitempty"`
	Origin      string `json:"origin"`
	Draft       bool   `json:"draft,omitempty"`
	NoVerify    bool   `json:"no_verify,omitempty"`
	Tip         string `json:"tip,omitempty"`
	TipOnly     bool   `json:"tip_only,omitempty"`
	DropCommits bool   `json:"drop_commits,omitempty"`
	deferPush   bool
	Ship        *stackShipIntent         `json:"ship,omitempty"`
	Aligned     bool                     `json:"aligned,omitempty"`
	Applied     bool                     `json:"applied,omitempty"`
	Publishing  bool                     `json:"publishing,omitempty"`
	PushTargets []stackPublicationTarget `json:"push_targets,omitempty"`
	Pushed      bool                     `json:"pushed,omitempty"`
	Receipted   bool                     `json:"receipted,omitempty"`
	Branches    []stackRebaseBranch      `json:"branches"`
	Conflict    *stackConflict           `json:"conflict,omitempty"`
	Roots       []string                 `json:"roots"`
	Pid         int                      `json:"pid"`
	Started     string                   `json:"started"`
	Host        string                   `json:"host"`
	dir         string
	saved       time.Time
	left        []stackLeft
}

// stackLeft is a branch of the stack a run leaves exactly where it is: an
// empty lane nobody has committed to yet, another lane's branch gt parents on
// this stack, or a branch stacked on either.
type stackLeft struct {
	branch string
	empty  bool
	why    string
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
	parents     []string
	linearize   []string
	landed      []string
	dryRun      bool
	noPush      bool
	members     []string
	draft       bool
	noVerify    bool
	deferPush   bool
	vetted      map[string]string
	replayed    map[string]stackRebaseBranch
	result      **stackRebaseRun
	ship        *stackShipIntent
	tip         string
	tipOnly     bool
	dropCommits bool
}

const stackDropCommitsUsage = "publish a branch whose local head drops commits its published head carries"

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
move onto what it sat on, leaving its squashed commits behind. Other working
copies and local trunk are left untouched. A branch held by another working copy,
or uncommitted work in the invoking checkout, stops publication before any branch
moves. Empty lanes and another lane's branches above the one checked out here are
left where they are and named rather than rebased.

A conflict stops the run before any ref moves: the rebase is left in progress
in a workspace of its own, with both sides' intent written out, and
ccx vcs stack continue resumes the rest of the stack from there
(ccx vcs stack abort drops it). rerere is off for every rebase it drives.
A stop whose conflicts are all files .ccx.toml lists under [[generated]] does
not wait: each owning command runs once in the workspace and the rebase
continues.

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
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
	return cmd
}

func newStackContinueCmd() *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "continue",
		Short: "Resume a stack rebase after resolving its conflict workspace",
		Long: `Resume a stack rebase after resolving its conflict workspace.

With no stack rebase in progress, continue finishes a rebase stopped in this
working copy — one a hand-run gt restack left behind after losing its own
operation, which gt continue then refuses. rerere is off. Every file rerere had
already filled from a recorded resolution is named first as a warning, since a
stale recording silently drops a branch's own changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackContinue(cmd, stack)
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "the run to resume, named by its stack's bottom branch")
	return cmd
}

func newStackAbortCmd() *cobra.Command {
	var stack string
	cmd := &cobra.Command{
		Use:   "abort",
		Short: "Drop a stopped stack rebase; nothing it planned has moved",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackAbort(cmd, stack)
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "the run to drop, named by its stack's bottom branch")
	return cmd
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
	current, err := gitCurrentBranch(ctx, l.dir(), stackRebasePrefix)
	if err != nil {
		return err
	}
	var gated []*stackRebaseRun
	for _, other := range runs {
		if (other.Conflict != nil && other.Conflict.Workspace == l.root) || (current != "" && other.branch(current) != nil) {
			if err := stackGate(ctx, cmd, l, commonDir, other, o.dryRun); err != nil {
				return err
			}
			gated = append(gated, other)
		}
	}
	rest := slices.DeleteFunc(slices.Clone(runs), func(other *stackRebaseRun) bool { return slices.Contains(gated, other) })
	return stackBegin(ctx, cmd, l, commonDir, rest, o)
}

func stackBegin(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, others []*stackRebaseRun, o stackRebaseOpts) error {
	run, err := stackPlan(ctx, l, commonDir, o)
	if err != nil {
		return err
	}
	if o.result != nil {
		*o.result = run
	}
	if err := stackAdmit(ctx, cmd, l, commonDir, others, run, o.dryRun); err != nil {
		return err
	}
	if err := stackShipCovers(run, o.ship); err != nil {
		return err
	}
	if err := stackAnnounceLeft(cmd, run.left); err != nil {
		return err
	}
	cmd.Println(strings.Join(stackPlanLines(run), "\n"))
	regen, err := stackRegenPlan(ctx, l.dir(), run)
	if err != nil {
		return err
	}
	if len(regen) > 0 {
		cmd.Println(strings.Join(regen, "\n"))
	}
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

// stackGate refuses a rebase over a run in progress, or reclaims the run when
// it is stale.
func stackGate(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, other *stackRebaseRun, dryRun bool) error {
	if !stackStale(other) {
		return stackInProgress(other)
	}
	if dryRun {
		cmd.Println(fmt.Sprintf("would reclaim the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
		return nil
	}
	if err := stackReclaim(ctx, l, commonDir, other); err != nil {
		return err
	}
	cmd.Println(fmt.Sprintf("reclaimed the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
	return nil
}

func stackAdmit(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, runs []*stackRebaseRun, run *stackRebaseRun, dryRun bool) error {
	for _, other := range runs {
		if !stackOverlaps(run, other) {
			continue
		}
		if err := stackGate(ctx, cmd, l, commonDir, other, dryRun); err != nil {
			return err
		}
	}
	return nil
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
	if err := stackDropPublicationPins(ctx, l.dir(), run); err != nil {
		return err
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
// conflict has exited by design and waits on its workspace, and an applied
// --no-push run has moved refs that only continue records.
func stackStale(run *stackRebaseRun) bool {
	host, _ := os.Hostname()
	if run.Host != host || (run.Applied && !stackLegacyApplied(run)) || run.Publishing || stackPidAlive(run) || time.Since(run.saved) < stackStaleAfter {
		return false
	}
	if run.Conflict == nil {
		return true
	}
	_, err := os.Stat(run.Conflict.Workspace)
	return errors.Is(err, fs.ErrNotExist)
}

// stackPidAlive also matches the process's start time, so a pid the kernel
// reused for another process after the run's own exited reads as exited.
func stackPidAlive(run *stackRebaseRun) bool {
	if err := syscall.Kill(run.Pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return stackProcStart(run.Pid) == run.Started
}

func stackProcStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // a fixed ps argv around a numeric pid
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
	retargeted := stackRetargeted(state, overrides)
	members, roots, err := stackMembers(retargeted, trunk, seeds)
	if err != nil {
		return nil, err
	}
	if o.members != nil {
		members = o.members
	}
	if !o.noPush {
		if members, roots, err = stackWithPublishedParents(ctx, l.dir(), retargeted, trunk, members, roots, overrides); err != nil {
			return nil, err
		}
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
	members, left, err := stackKept(ctx, l.dir(), state, tr, current, members, prs, overrides, o.landed)
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
	submitted, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}
	run := &stackRebaseRun{Trunk: trunk, Pin: pin, NoPush: o.noPush, Origin: l.checkout.Root, Draft: o.draft, NoVerify: o.noVerify, Tip: o.tip, TipOnly: o.tipOnly, DropCommits: o.dropCommits, deferPush: o.deferPush, Ship: o.ship, Roots: roots, Pid: os.Getpid(), Started: stackProcStart(os.Getpid()), Host: host, left: left}
	own := map[string]bool{}
	if current != "" && current != trunk {
		down, err := gtDownstack(stackRebasePrefix, retargeted, current, trunk)
		if err != nil {
			return nil, err
		}
		for _, name := range down {
			own[name] = true
		}
	}
	outside := map[string]bool{}
	byName := map[string]*stackRebaseBranch{}
	for _, name := range members {
		if parent := retargeted[name].Parents[0].Ref; outside[parent] {
			outside[name] = true
			run.left = append(run.left, stackLeft{branch: name, why: "it sits on " + parent + ", which is left where it is"})
			continue
		}
		ours := submitted[name].HeadSha
		if vetted := o.vetted[name]; vetted != "" && vetted == remotes[name] {
			ours = vetted
		}
		source := state[name]
		effective := source
		var receipt *stackPublication
		superseded := false
		if !o.noPush {
			receipt, err = stackReadPublication(ctx, l.dir(), name)
			if err != nil {
				return nil, err
			}
			if receipt != nil && remotes[name] != "" && remotes[name] != receipt.Head {
				if superseded, err = stackOwnRemote(ctx, l.dir(), name, source.Head, remotes[name], pin); err != nil {
					return nil, err
				}
			}
			if receipt != nil && !superseded && receipt.Source == source.Head {
				effective.Head = receipt.Head
			}
		}
		prepared, replayed := o.replayed[name]
		if replayed {
			if source.Head != prepared.Local {
				return nil, fmt.Errorf("stack rebase: %s source changed during replanning; checkout untouched", name)
			}
			effective.Head = prepared.NewHead
		}
		b, err := stackSnapshot(ctx, l.dir(), tr, effective, name, remotes[name], ours, prs[name], slices.Contains(o.landed, name), pin, o.dropCommits)
		if err != nil && len(own) > 0 && !own[name] {
			outside[name] = true
			run.left = append(run.left, stackLeft{branch: name, why: strings.TrimPrefix(err.Error(), stackRebasePrefix+": ")})
			continue
		}
		if err != nil {
			return nil, err
		}
		b.Local = source.Head
		if !o.noPush {
			b.HeadRef = stackTempRef(name)
			if superseded {
				b.Publication = receipt
			} else if err := stackUsePublication(ctx, l.dir(), &b, receipt, submitted[name]); err != nil {
				return nil, err
			}
		}
		if replayed {
			b.Head = prepared.NewHead
			b.WasParent = prepared.Parent
			b.SourceBase = cmp.Or(prepared.SourceBase, prepared.OldBase)
			b.OldBase = prepared.NewBase
			if b.Landed != "" {
				b.NewHead = prepared.NewHead
			}
		}
		byName[name] = &b
	}
	for name, b := range byName {
		b.Parent = b.WasParent
		if _, member := byName[b.Parent]; b.Parent != trunk && !member {
			b.Parent = state[name].Parents[0].Ref
		}
		if p, ok := overrides[name]; ok {
			b.Parent = p
		}
		if _, member := byName[b.Parent]; b.Parent != trunk && !member {
			return nil, fmt.Errorf("stack rebase: %s was published onto %s and gt records it on %s, and neither is in this run — re-record it with gt track --parent <branch> %s, or name it with ccx vcs stack rebase --parent %s=<branch>", name, b.WasParent, state[name].Parents[0].Ref, name, name)
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
	if err := stackCheckHeld(ctx, l.dir(), trunk, byName); err != nil {
		return nil, err
	}
	order, err := stackOrder(trunk, byName)
	if err != nil {
		return nil, err
	}
	queued, err := stackQueuedBranches(ctx, l, o.noPush, byName)
	if err != nil {
		return nil, err
	}
	moving, err := stackMovingBranches(retargeted, o, overrides)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{trunk: true}
	for _, name := range order {
		b := byName[name]
		if b.Landed != "" || b.Held != "" {
			continue
		}
		_, named := overrides[name]
		switch {
		case queued[name] && named:
			return nil, fmt.Errorf("stack rebase: %s is in the merge queue as %s, and moving it would evict it — take it out of the queue first", name, b.PR)
		case queued[name] || (moving != nil && !moving[name]):
			b.Kept = stackPinPublished(b)
		case o.tip != "" && name != o.tip:
			if b.Kept, err = stackKeepsAncestor(ctx, l.dir(), pin, b, kept[b.Parent], o.tipOnly); err != nil {
				return nil, err
			}
		}
		kept[name] = b.Kept
	}
	for _, name := range order {
		b := byName[name]
		if b.Landed == "" && b.Held == "" {
			if b.OldBase == "" {
				if b.OldBase, err = stackOldBase(ctx, l.dir(), trunk, pin, state, b, byName); err != nil {
					return nil, err
				}
			}
			if b.Kept {
				run.Branches = append(run.Branches, *b)
				continue
			}
			if err := stackOwnWork(ctx, l.dir(), tr, pin, b); err != nil {
				return nil, err
			}
		}
		run.Branches = append(run.Branches, *b)
	}
	return run, nil
}

// stackKept drops from members every branch the run must leave where it is,
// in members' parents-first order. A branch with no commit past the parent
// revision gt recorded is an empty lane, not a landed one: dropping it would
// make gt forget a lane nobody has committed to yet. A branch above the one
// checked out here whose gt parent the rest of the record contradicts belongs
// to another lane: gt track adopts a branch cut at the same commit as another
// onto it, and a run that trusted that record replayed another lane's work
// onto this stack and pushed it. Everything stacked on either goes with it.
func stackKept(ctx context.Context, dir render.Dir, state gtState, tr vcs.Trunk, current string, members []string, prs map[string]*stackPR, overrides map[string]string, landed []string) ([]string, []stackLeft, error) {
	isLanded := func(name string) bool {
		return slices.Contains(landed, name) || (prs[name] != nil && prs[name].Landed)
	}
	above := map[string]bool{}
	if current != "" && current != tr.Name() {
		if err := stackRefuseForeignBelow(ctx, dir, stackRetargeted(state, overrides), tr, current, overrides, prs, isLanded); err != nil {
			return nil, nil, err
		}
		up, err := gtUpstack(stackRebasePrefix, state, current)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range up {
			above[name] = true
		}
	}
	gone := map[string]bool{}
	var kept []string
	var left []stackLeft
	for _, name := range members {
		s := state[name]
		parent := s.Parents[0].Ref
		override, overridden := overrides[name]
		if overridden {
			parent = override
		}
		switch {
		case gone[parent]:
			left = append(left, stackLeft{branch: name, why: "it sits on " + parent + ", which is left where it is"})
		case !isLanded(name) && s.Parents[0].SHA == s.Head:
			left = append(left, stackLeft{branch: name, empty: true})
		case above[name] && !overridden:
			effective := parent
			for effective != tr.Name() && isLanded(effective) {
				effective = state[effective].Parents[0].Ref
			}
			why, err := stackStrayReason(ctx, dir, state, tr, name, parent, effective, prs[name])
			if err != nil {
				return nil, nil, err
			}
			if why == "" {
				kept = append(kept, name)
				continue
			}
			left = append(left, stackLeft{branch: name, why: why})
		default:
			kept = append(kept, name)
			continue
		}
		gone[name] = true
	}
	for _, child := range slices.Sorted(maps.Keys(overrides)) {
		for _, name := range []string{child, overrides[child]} {
			if gone[name] {
				return nil, nil, fmt.Errorf("stack rebase: --parent %s=%s names %s, which this run leaves where it is", child, overrides[child], name)
			}
		}
	}
	return kept, left, nil
}

// stackStrayReason names the evidence that branch belongs to another lane: an
// open pull request based on neither its gt parent nor the ancestor that
// parent's landing leaves it on, or a history carrying none of the parent's own
// commits under any sha.
func stackStrayReason(ctx context.Context, dir render.Dir, state gtState, tr vcs.Trunk, branch, parent, effective string, pr *stackPR) (string, error) {
	if pr != nil && pr.State == "OPEN" && pr.Base != "" && pr.Base != parent && pr.Base != effective {
		return fmt.Sprintf("gt records its parent as %s, but its pull request #%d is based on %s — re-record it with gt track --parent %s %s", parent, pr.Number, pr.Base, pr.Base, branch), nil
	}
	if parent == tr.Name() {
		return "", nil
	}
	none, _, err := stackCarriesNone(ctx, dir, state, tr, branch, branch, parent)
	if err != nil || !none {
		return "", err
	}
	below := state[parent].Parents[0].Ref
	return fmt.Sprintf("gt records its parent as %s, but it carries none of %s's commits — re-record it with gt track --parent %s %s, or run ccx vcs stack submit from %s's working copy to put it on %s", parent, parent, below, branch, branch, parent), nil
}

// stackCarriesNone reports whether branch, with work of its own, carries none
// of parent's own commits under any sha, where gt stacks child on parent. A
// branch still carrying the parent revision gt recorded for child, with work of
// the parent's own in it, was cut from the parent before a rewrite and carries
// it; fromEmpty says that revision held no work of the parent's, which a branch
// cut from a parent before its first commit shares with a foreign one.
func stackCarriesNone(ctx context.Context, dir render.Dir, state gtState, tr vcs.Trunk, branch, child, parent string) (none, fromEmpty bool, err error) {
	below := state[parent].Parents[0].Ref
	outside := []string{"^" + string(tr.Ref())}
	if below != tr.Name() {
		outside = append(outside, "^"+gtRestackRef(below))
	}
	parentOwn, err := gtRevCount(ctx, stackRebasePrefix, dir, gtRestackRef(parent), outside...)
	if err != nil || parentOwn == 0 {
		return false, false, err
	}
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, gtRestackRef(branch), outside...)
	if err != nil || own == 0 {
		return false, false, err
	}
	recorded := state[child].Parents[0].SHA
	carried, err := gitIsAncestor(ctx, dir, stackRebasePrefix, recorded, gtRestackRef(branch))
	if err != nil {
		return false, false, err
	}
	if carried {
		recordedOwn, err := gtRevCount(ctx, stackRebasePrefix, dir, recorded, outside...)
		if err != nil || recordedOwn > 0 {
			return false, false, err
		}
	}
	missing, err := gtRevCount(ctx, stackRebasePrefix, dir, gtRestackRef(parent)+"..."+gtRestackRef(branch), append([]string{"--left-only", "--cherry-pick"}, outside...)...)
	return err == nil && missing >= parentOwn, carried, err
}

// stackRefuseForeignBelow refuses a branch gt stacks on another lane's work.
// gt track --force takes the most recent tracked ancestor over --parent, often
// an empty branch another lane just cut on the same trunk commit, so an edge
// cut from an empty parent counts as foreign only when the pull request of the
// branch carrying it is based elsewhere. Each edge is judged by the nearest
// branch above it with work of its own.
func stackRefuseForeignBelow(ctx context.Context, dir render.Dir, state gtState, tr vcs.Trunk, current string, overrides map[string]string, prs map[string]*stackPR, isLanded func(string) bool) error {
	down, err := gtDownstack(stackRebasePrefix, state, current, tr.Name())
	if err != nil {
		return err
	}
	carrier := 0
	for i := 1; i < len(down); i++ {
		child, parent := down[i-1], down[i]
		if i > 1 && !isLanded(child) {
			own, err := gtRevCount(ctx, stackRebasePrefix, dir, gtRestackRef(child), "^"+string(tr.Ref()), "^"+gtRestackRef(parent))
			if err != nil {
				return err
			}
			if own > 0 {
				carrier = i - 1
			}
		}
		if _, named := overrides[child]; named || isLanded(parent) {
			continue
		}
		none, fromEmpty, err := stackCarriesNone(ctx, dir, state, tr, down[carrier], child, parent)
		if err != nil {
			return err
		}
		pr := prs[down[carrier]]
		disowned := pr != nil && pr.State == "OPEN" && pr.Base != "" && !slices.Contains(down[carrier+1:i+1], pr.Base)
		if !none || (fromEmpty && !disowned) {
			continue
		}
		foreign := slices.DeleteFunc(slices.Clone(down[i:]), isLanded)
		return fmt.Errorf("stack rebase: %s carries none of %s's commits, yet gt stacks it on %s (%s) — this run would restack and push another lane's %s; gt track --force takes the most recent tracked ancestor over --parent, so name %s's real parent with ccx vcs stack rebase --parent %s=<branch>",
			current, parent, parent, strings.Join(append(down, tr.Name()), " → "), strings.Join(foreign, ", "), current, current)
	}
	return nil
}

// stackAnnounceLeft names every branch a run leaves where it is: the empty
// lanes in the report, the rest with the evidence behind them.
func stackAnnounceLeft(cmd *cobra.Command, left []stackLeft) error {
	var empty []string
	for _, l := range left {
		if l.empty {
			empty = append(empty, l.branch)
			continue
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "stack rebase: left %s alone: %s\n", l.branch, l.why); err != nil {
			return err
		}
	}
	if len(empty) > 0 {
		cmd.Println("skipped empty " + strings.Join(empty, ", ") + ", with no commit of its own yet")
	}
	return nil
}

// stackShipCovers refuses, before anything moves, a --pr-title or
// --pr-body-file naming a branch the run leaves out: its pull request is one
// nothing will push to.
func stackShipCovers(run *stackRebaseRun, ship *stackShipIntent) error {
	if ship == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(ship.Meta)) {
		if b := run.branch(name); b == nil || b.Landed != "" {
			return fmt.Errorf("stack rebase: --pr-title/--pr-body-file named %s, which this run leaves out", name)
		}
	}
	return nil
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

func stackRetargeted(state gtState, overrides map[string]string) gtState {
	out := maps.Clone(state)
	for child, parent := range overrides {
		s, tracked := out[child]
		if !tracked || len(s.Parents) == 0 {
			continue
		}
		s.Parents = slices.Clone(s.Parents)
		s.Parents[0].Ref = parent
		out[child] = s
	}
	return out
}

// stackWithPublishedParents adds back the parent a member was last published
// onto when the run left it out, by --parent or because gt records another
// parent: gt state keeps the old parent until the source checkout moves, and
// the publication is what gets replayed.
func stackWithPublishedParents(ctx context.Context, dir render.Dir, state gtState, trunk string, members, roots []string, overrides map[string]string) ([]string, []string, error) {
	for {
		var missing []string
		for _, name := range members {
			if _, named := overrides[name]; named {
				continue
			}
			receipt, err := stackReadPublication(ctx, dir, name)
			if err != nil {
				return nil, nil, err
			}
			if receipt == nil || receipt.Parent == trunk || slices.Contains(members, receipt.Parent) || slices.Contains(missing, receipt.Parent) {
				continue
			}
			if _, tracked := state[receipt.Parent]; tracked {
				missing = append(missing, receipt.Parent)
			}
		}
		if len(missing) == 0 {
			return members, roots, nil
		}
		more, moreRoots, err := stackMembers(state, trunk, missing)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range members {
			if !slices.Contains(more, name) {
				more = append(more, name)
			}
		}
		for _, root := range roots {
			if !slices.Contains(moreRoots, root) {
				moreRoots = append(moreRoots, root)
			}
		}
		members, roots = more, moreRoots
	}
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

// stackSnapshot takes a remote that equals the branch's last submitted head
// as ours even when local no longer contains it: every rewrite a rebase or
// restack makes diverges from the head it last pushed.
func stackSnapshot(ctx context.Context, dir render.Dir, tr vcs.Trunk, s gtBranchState, name, remote, submitted string, pr *stackPR, declared bool, pin string, dropCommits bool) (stackRebaseBranch, error) {
	b := stackRebaseBranch{
		Name: name, WasParent: s.Parents[0].Ref, Local: s.Head, Remote: remote,
		Head: s.Head, HeadRef: gtRestackRef(name), PR: pr, Held: s.State,
	}
	if b.Held == "" && remote != "" && remote != s.Head {
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
		case !ahead && remote != submitted:
			replay, err := stackRemoteReplays(ctx, dir, remote, submitted, pin)
			if err != nil {
				return b, err
			}
			if !replay {
				if replay, err = stackOwnRemote(ctx, dir, name, s.Head, remote, pin); err != nil {
					return b, err
				}
			}
			if !replay {
				if replay, err = stackRestackedOntoTrunk(ctx, dir, remote, s.Parents[0].SHA, s.Head, pin); err != nil {
					return b, err
				}
			}
			if replay {
				break
			}
			return b, fmt.Errorf("stack rebase: %s has diverged from %s/%s (local %.12s, remote %.12s) — someone pushed to it; reconcile the two by hand, then re-run", name, tr.Remote(), name, s.Head, remote)
		}
		if !ahead && !behind && !dropCommits {
			if err := stackRefuseDroppedCommits(ctx, dir, tr, name, s.Head, remote, pin); err != nil {
				return b, err
			}
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
				if within, err = stackRemoteReplays(ctx, dir, b.Head, pr.Head, pin); err != nil {
					return b, err
				}
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

// stackRefuseDroppedCommits refuses a local head that only loses work its
// published head carries: one sharing none of its commits, or one holding a
// strict subset of them, matched by subject so a rebase, an amend, or a rewrite
// of a commit still passes. A hard reset onto the wrong commit would otherwise
// force-push over the pull request's work.
func stackRefuseDroppedCommits(ctx context.Context, dir render.Dir, tr vcs.Trunk, name, local, remote, pin string) error {
	subjects := func(head string) ([]string, error) {
		out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--no-merges", "--format=%s", head, "^" + pin})
		if err != nil {
			return nil, fmt.Errorf("%s: git log %.12s: %w", stackRebasePrefix, head, err)
		}
		return slices.DeleteFunc(strings.Split(strings.TrimSpace(out), "\n"), func(s string) bool { return s == "" }), nil
	}
	kept, err := subjects(local)
	if err != nil {
		return err
	}
	published, err := subjects(remote)
	if err != nil {
		return err
	}
	var dropped []string
	shared := 0
	for _, subject := range published {
		if slices.Contains(kept, subject) {
			shared++
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%q", subject))
	}
	added := slices.ContainsFunc(kept, func(subject string) bool { return !slices.Contains(published, subject) })
	if len(dropped) == 0 || (shared > 0 && added) {
		return nil
	}
	return fmt.Errorf("stack rebase: %s's local head %.12s drops %d commit(s) its published head %s/%s (%.12s) carries: %s — restore them, or pass --drop-commits to publish the local head anyway",
		name, local, len(dropped), tr.Remote(), name, remote, strings.Join(dropped, ", "))
}

// stackKeepsAncestor leaves a ship's ancestor at the head its pull request
// already shows: a force-push of a pull request that does not conflict onto
// newer trunk dismisses its approvals for nothing. A local head that only
// replays the published commits onto newer trunk, as a restack leaves it, is
// kept at the published head. --tip-only keeps every published ancestor,
// whatever moved under it.
func stackKeepsAncestor(ctx context.Context, dir render.Dir, pin string, b *stackRebaseBranch, parentKept, tipOnly bool) (bool, error) {
	if b.Remote == "" {
		if tipOnly {
			return false, fmt.Errorf("stack rebase: --tip-only ships onto %s's published head, and it has none — push it first", b.Name)
		}
		return false, nil
	}
	if !tipOnly {
		pr := b.PR
		if !parentKept || b.Parent != b.WasParent || pr == nil || pr.State != "OPEN" || pr.Mergeable == "CONFLICTING" {
			return false, nil
		}
		if b.Head != b.Remote {
			replays, err := stackRemoteReplays(ctx, dir, b.Head, b.Remote, pin)
			if err != nil || !replays {
				return false, err
			}
		}
	}
	b.Head, b.HeadRef = b.Remote, stackTempRef(b.Name)
	return true, nil
}

// stackPinPublished keeps a branch the run must not push at the head its
// pull request already shows; one never pushed has nothing to keep.
func stackPinPublished(b *stackRebaseBranch) bool {
	if b.Remote == "" {
		return false
	}
	b.Head, b.HeadRef = b.Remote, stackTempRef(b.Name)
	return true
}

// stackMovingBranches is what a pushing --parent or --linearize run may move:
// the branches it names a parent for and everything stacked on them. The
// parents it names stay at their published heads, since a force-push to
// another lane's pull request is not what the run asked for. nil lets every
// branch move.
func stackMovingBranches(state gtState, o stackRebaseOpts, overrides map[string]string) (map[string]bool, error) {
	if o.noPush || o.tip != "" || len(overrides) == 0 {
		return nil, nil
	}
	moving := map[string]bool{}
	for child := range overrides {
		up, err := gtUpstack(stackRebasePrefix, state, child)
		if err != nil {
			return nil, err
		}
		moving[child] = true
		for _, name := range up {
			moving[name] = true
		}
	}
	return moving, nil
}

// stackQueuedBranches reads Graphite's merge queue for every branch with an
// open pull request a pushing run could move: a push to a queued pull request
// evicts it.
func stackQueuedBranches(ctx context.Context, l lane, noPush bool, byName map[string]*stackRebaseBranch) (map[string]bool, error) {
	var heads []string
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if b := byName[name]; b.Landed == "" && b.PR != nil && b.PR.State == "OPEN" {
			heads = append(heads, name)
		}
	}
	if noPush || len(heads) == 0 {
		return nil, nil
	}
	owner, name, err := gtRepoOwnerName(ctx, l, stackRebasePrefix)
	if err != nil {
		return nil, err
	}
	infos, err := gtAPIClient().PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: name, PRNumbers: []int{}, PRHeadRefNames: heads, Consistent: true, Callsite: "ccx"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read the merge queue before pushing: %w", err)
	}
	queued := map[string]bool{}
	for _, info := range infos {
		if info.State == gtapi.PROpen && info.MergeQueueStatus != nil && info.MergeQueueStatus.IsInGraphiteMq {
			queued[info.HeadRefName] = true
		}
	}
	return queued, nil
}

func stackOwnRemote(ctx context.Context, dir render.Dir, name, local, remote, pin string) (bool, error) {
	held, err := gitReflogHolds(ctx, dir, stackRebasePrefix, name, remote)
	if err != nil || held {
		return held, err
	}
	return stackRemoteReplays(ctx, dir, remote, local, pin)
}

func stackRemoteReplays(ctx context.Context, dir render.Dir, remote, submitted, pin string) (bool, error) {
	if submitted == "" {
		return false, nil
	}
	theirs, err := stackPatchSeries(ctx, dir, pin, remote)
	if err != nil {
		return false, err
	}
	ours, err := stackPatchSeries(ctx, dir, pin, submitted)
	if err != nil {
		return false, err
	}
	return theirs != nil && ours != nil && slices.Equal(theirs, ours), nil
}

func stackRestackedOntoTrunk(ctx context.Context, dir render.Dir, remote, base, head, pin string) (bool, error) {
	replayed, err := stackReplayedOnto(ctx, dir, remote, base, head)
	if err != nil || replayed == "" {
		return false, err
	}
	return gitIsAncestor(ctx, dir, stackRebasePrefix, replayed, pin)
}

func stackPatchSeries(ctx context.Context, dir render.Dir, pin, head string) ([]string, error) {
	span := pin + ".." + head
	merges, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--merges", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-list --merges %s: %w", stackRebasePrefix, span, err)
	}
	if strings.TrimSpace(merges) != "" {
		return nil, nil
	}
	patches, err := render.RunCLI(ctx, dir, "git", []string{"log", "--reverse", "--no-merges", "--format=commit %H", "-p", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git log -p %s: %w", stackRebasePrefix, span, err)
	}
	ids, err := render.RunCLIStdin(ctx, dir, "git", []string{"patch-id", "--stable"}, []byte(patches))
	if err != nil {
		return nil, fmt.Errorf("%s: git patch-id %s: %w", stackRebasePrefix, span, err)
	}
	authored, err := render.RunCLI(ctx, dir, "git", []string{"log", "--no-merges", "-z", "--format=%H%n%an <%ae> %at%n%B", span})
	if err != nil {
		return nil, fmt.Errorf("%s: git log %s: %w", stackRebasePrefix, span, err)
	}
	messages := map[string]string{}
	for entry := range strings.SplitSeq(authored, "\x00") {
		if sha, message, _ := strings.Cut(entry, "\n"); sha != "" {
			messages[sha] = message
		}
	}
	series := []string{}
	for line := range strings.Lines(ids) {
		id, sha, _ := strings.Cut(strings.TrimSpace(line), " ")
		series = append(series, id+"\n"+messages[sha])
	}
	if len(series) != len(messages) {
		return nil, nil
	}
	return series, nil
}

func stackCheckHeld(ctx context.Context, dir render.Dir, trunk string, byName map[string]*stackRebaseBranch) error {
	var held []string
	live := false
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		b := byName[name]
		switch {
		case b.Landed != "":
		case b.Held == "":
			live = true
		default:
			held = append(held, name)
		}
	}
	for _, name := range held {
		b := byName[name]
		ok := b.Parent == b.WasParent
		if ok && b.Parent != trunk {
			parent := byName[b.Parent]
			on, err := gitIsAncestor(ctx, dir, stackRebasePrefix, parent.Head, b.Head)
			if err != nil {
				return err
			}
			ok = parent.Held != "" && on
		}
		if !ok || !live {
			return fmt.Errorf("stack rebase: %s is %s in gt — unfreeze it, or rebase a stack without it", name, b.Held)
		}
	}
	return nil
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
// contains. A branch leaving that parent, landed or named away by --parent, has
// it moved up to where the branch meets trunk when trunk already holds
// everything between. All are read before anything moves, so a push mid-run
// never changes the answer, a squash-landed parent's commits stay behind, and
// a branch already moved off a landed parent onto trunk replays only its own.
func stackOldBase(ctx context.Context, dir render.Dir, trunk, pin string, state gtState, self *stackRebaseBranch, byName map[string]*stackRebaseBranch) (string, error) {
	s := state[self.Name]
	onTrunk, err := stackMergeBase(ctx, dir, self.Head, pin)
	if err != nil || s.Parents[0].Ref == trunk {
		return onTrunk, err
	}
	candidates := []string{state[s.Parents[0].Ref].Head, s.Parents[0].SHA}
	if head := byName[s.Parents[0].Ref]; head != nil {
		candidates = []string{head.Head, head.Local, s.Parents[0].SHA}
	} else {
		receipt, err := stackReadPublication(ctx, dir, s.Parents[0].Ref)
		if err != nil {
			return "", err
		}
		if receipt != nil {
			candidates = append([]string{receipt.Head}, candidates...)
		}
	}
	best := ""
	for _, candidate := range candidates {
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
	if best == "" {
		if best, err = stackMergeBase(ctx, dir, self.Head, candidates[0]); err != nil {
			return "", err
		}
	}
	if self.Parent == s.Parents[0].Ref {
		return best, nil
	}
	behind, err := gitIsAncestor(ctx, dir, stackRebasePrefix, best, onTrunk)
	if err != nil || behind {
		return onTrunk, err
	}
	return best, nil
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
	return fmt.Errorf("stack rebase: %s would replay %d commits but owns %d — the rest are already in %s; name its real parent with ccx vcs stack rebase --parent %s=<branch>",
		b.Name, replayed, own, tr.Name(), b.Name)
}

func stackPlanLines(run *stackRebaseRun) []string {
	lines := make([]string, 0, 1+len(run.Branches))
	lines = append(lines, fmt.Sprintf("plan · trunk %s@%.12s", run.Trunk, run.Pin))
	for _, b := range run.Branches {
		fields := []string{b.Name}
		switch {
		case b.Landed != "":
			fields = append(fields, "drop ("+b.Landed+")")
		case b.Held != "":
			fields = append(fields, "left alone ("+b.Held+")")
		case b.Kept:
			fields = append(fields, fmt.Sprintf("kept at its published head %.12s", b.Head))
		default:
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
	if !run.NoPush {
		var pushes []string
		for _, b := range run.Branches {
			if b.Landed == "" && b.Held == "" && !b.Kept {
				pushes = append(pushes, b.Name)
			}
		}
		lines = append(lines, "pushes "+cmp.Or(strings.Join(pushes, ", "), "nothing"))
	}
	return lines
}

func stackDrive(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed != "" || b.NewHead != "" {
			continue
		}
		if b.Held != "" {
			b.NewHead = b.Head
			continue
		}
		if b.Kept {
			b.NewBase, b.NewHead = b.OldBase, b.Head
			continue
		}
		b.NewBase = run.headOf(b.Parent)
		if b.NewBase == b.OldBase {
			b.NewHead = b.Head
			continue
		}
		head, err := stackReplay(ctx, l.dir(), b)
		if errors.Is(err, errReplayConflict) || errors.Is(err, errReplayMerges) {
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
	if run.Git {
		return stackFinishGit(ctx, cmd, l, commonDir, run)
	}
	return stackFinish(ctx, cmd, l, commonDir, run)
}

// errReplayMerges marks a span git replay refuses to replay: it carries a merge,
// which a rebase in the conflict workspace flattens instead.
var errReplayMerges = errors.New("replay: merge commits")

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
	merges, err := gtRevCount(ctx, stackRebasePrefix, dir, b.OldBase+".."+b.Head, "--merges")
	if err != nil {
		return "", err
	}
	if merges > 0 {
		return "", errReplayMerges
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
	ws, err := mintWorktreePath(ctx, stackRebasePrefix, l.checkout, "conflict-"+strings.ReplaceAll(b.Name, "/", "-"))
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
	if err := stackSaveRun(run); err != nil {
		return err
	}
	if code != 0 {
		unmerged, err := stackUnmerged(ctx, ws)
		if err != nil {
			return err
		}
		if len(unmerged) == 0 {
			return fmt.Errorf("stack rebase: git rebase in %s stopped without a conflict: %s", ws, strings.TrimSpace(stderr))
		}
		if err := stackAdvance(ctx, cmd, run, b); err != nil {
			return err
		}
	}
	if b.NewHead, err = stackRevParse(ctx, render.Dir(ws), "HEAD"); err != nil {
		return err
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return stackCloseWorkspace(ctx, cmd, l, commonDir, run)
}

// stackAdvance drives the workspace's rebase to its end. A stop whose conflicts
// are all declared generated paths regenerates every declared path the stopped
// commit touches and continues; any other stop, or a failing generator, is left
// to the human with its brief.
func stackAdvance(ctx context.Context, cmd *cobra.Command, run *stackRebaseRun, b *stackRebaseBranch) error {
	c := run.Conflict
	ws := render.Dir(c.Workspace)
	for stackRebasing(ctx, ws) {
		unmerged, err := stackUnmerged(ctx, c.Workspace)
		if err != nil {
			return err
		}
		if len(unmerged) > 0 || len(c.Generated) > 0 {
			gens, err := regenStaged(ctx, ws)
			if err != nil {
				return stackStopped(ctx, run, b, unmerged, err.Error())
			}
			var replayed []string
			if len(unmerged) > 0 {
				if replayed, err = regenChanged(ctx, ws, "REBASE_HEAD^", "REBASE_HEAD"); err != nil {
					return err
				}
			}
			declared := regenDeclared(gens, unmerged)
			pending := regenDeclared(gens, slices.Compact(slices.Sorted(slices.Values(slices.Concat(declared, c.Generated, replayed)))))
			if len(declared) < len(unmerged) {
				c.Generated = pending
				return stackStopped(ctx, run, b, unmerged, "")
			}
			if len(pending) == 0 {
				c.Generated = nil
			} else if err := stackRegenerate(ctx, cmd, c.Workspace, gens, pending); err != nil {
				c.Generated = pending
				return stackStopped(ctx, run, b, unmerged, "stack rebase: regenerating "+strings.Join(pending, ", ")+" failed, and nothing was committed: "+err.Error())
			}
			c.Generated = nil
			if err := stackSaveRun(run); err != nil {
				return err
			}
		}
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
	}
	return nil
}

func stackStopped(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch, unmerged []string, lead string) error {
	brief := stackBrief(ctx, run, b, unmerged)
	if lead != "" {
		brief = lead + "\n" + brief
	}
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
	if len(run.Conflict.Generated) > 0 {
		fmt.Fprintf(&s, "generated, rerun from %s by continue: %s\n", regenFile, strings.Join(run.Conflict.Generated, ", "))
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
	fmt.Fprintf(&s, "next: resolve the files in %s and git add them, then run ccx vcs stack continue from this conflict workspace or a branch of this stack; ccx vcs stack abort drops the run.\n", ws)
	if run.NoPush {
		s.WriteString("never git rebase --continue by hand: continue drives it with rerere off, then rebases the rest of the stack.")
	} else {
		s.WriteString("never git rebase --continue by hand: continue drives it with rerere off, then rebases and pushes the rest of the stack.")
	}
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

func runStackContinue(cmd *cobra.Command, stack string) error {
	ctx := cmd.Context()
	l, commonDir, run, err := stackResolveRun(ctx, stack)
	if errors.Is(err, errNoStackRebase) && stack == "" {
		return stackContinueStranded(ctx, cmd)
	}
	if err != nil {
		return err
	}
	if run.Conflict == nil {
		return stackDrive(ctx, cmd, l, commonDir, run)
	}
	return stackResume(ctx, cmd, l, commonDir, run)
}

var errNoStackRebase = errors.New("stack rebase: no stack rebase is in progress in this repository")

// stackContinueStranded finishes a rebase ccx did not start, stopped in this
// working copy — one a gt restack left behind after losing its own operation,
// which gt continue then refuses. It continues with rerere off, and names
// first every file rerere resolved from a recording, which nobody but rerere
// has checked.
func stackContinueStranded(ctx context.Context, cmd *cobra.Command) error {
	ws := render.Dir(workingDir(ctx))
	if !stackRebasing(ctx, ws) {
		return errNoStackRebase
	}
	replays, files, err := stackRerereReplayed(ctx, ws)
	if err != nil {
		return err
	}
	if replays > 0 {
		where := "the files this rebase stopped on"
		if len(files) > 0 {
			where = strings.Join(files, ", ")
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "stack continue: warning: rerere replayed a recorded resolution into %s — check each against both sides of its conflict before submitting\n", where); err != nil {
			return err
		}
	}
	unmerged, err := stackUnmerged(ctx, string(ws))
	if err != nil {
		return err
	}
	if len(unmerged) > 0 {
		return fmt.Errorf("stack continue: %s still has unresolved files: %s — resolve them, git add them, then run ccx vcs stack continue again", ws, strings.Join(unmerged, ", "))
	}
	argv := append(slices.Clone(stackGitNoRerere), "rebase", "--continue")
	_, code, stderr, err := render.RunCLIExitCode(ctx, ws, "git", argv)
	if err != nil {
		return fmt.Errorf("stack continue: git rebase --continue in %s: %w", ws, err)
	}
	if unmerged, err = stackUnmerged(ctx, string(ws)); err != nil {
		return err
	}
	switch {
	case len(unmerged) > 0:
		return fmt.Errorf("stack continue: the rebase in %s stopped on another conflict: %s — resolve them, git add them, then run ccx vcs stack continue again", ws, strings.Join(unmerged, ", "))
	case code != 0:
		return fmt.Errorf("stack continue: git rebase --continue in %s failed: %s", ws, strings.TrimSpace(stderr))
	case stackRebasing(ctx, ws):
		return fmt.Errorf("stack continue: the rebase in %s paused where its todo list asks to — make the change it stopped for, then run ccx vcs stack continue again", ws)
	}
	branch, err := gitCurrentBranch(ctx, ws, "stack continue")
	if err != nil {
		return err
	}
	head, err := stackRevParse(ctx, ws, "HEAD")
	if err != nil {
		return err
	}
	cmd.Println(fmt.Sprintf("finished the rebase of %s at %.12s — ccx vcs stack submit records it with gt and submits it", branch, head))
	return nil
}

// stackRerereReplayed counts the recorded resolutions rerere applied during the
// rebase stopped in ws, and names the files each landed in. rerere writes a
// conflict's thisimage only when it replays a recording, so one written since
// the rebase began is a replay; a file holding its postimage is where it went.
func stackRerereReplayed(ctx context.Context, ws render.Dir) (int, []string, error) {
	onto, err := stackRebaseOnto(ctx, ws)
	if err != nil {
		return 0, nil, err
	}
	began, err := os.Stat(onto)
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: %w", err)
	}
	cache, err := stackGitPath(ctx, ws, "rr-cache")
	if err != nil {
		return 0, nil, err
	}
	conflicts, err := os.ReadDir(cache)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: %w", err)
	}
	var postimages [][]byte
	for _, conflict := range conflicts {
		images, err := filepath.Glob(filepath.Join(cache, conflict.Name(), "thisimage*"))
		if err != nil {
			return 0, nil, fmt.Errorf("stack continue: %w", err)
		}
		for _, image := range images {
			info, err := os.Stat(image)
			if err != nil {
				return 0, nil, fmt.Errorf("stack continue: %w", err)
			}
			if info.ModTime().Before(began.ModTime()) {
				continue
			}
			post, err := os.ReadFile(filepath.Join(filepath.Dir(image), "postimage"+strings.TrimPrefix(filepath.Base(image), "thisimage"))) //nolint:gosec // a postimage beside the thisimage git's own rr-cache listed
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, nil, fmt.Errorf("stack continue: %w", err)
			}
			postimages = append(postimages, post)
		}
	}
	if len(postimages) == 0 {
		return 0, nil, nil
	}
	base, err := os.ReadFile(onto) //nolint:gosec // onto is git's own rebase state file, resolved by git rev-parse --git-path
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: %w", err)
	}
	orig, err := os.ReadFile(filepath.Join(filepath.Dir(onto), "orig-head")) //nolint:gosec // orig-head sits beside onto in git's own rebase state
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: %w", err)
	}
	root, err := render.RunCLI(ctx, ws, "git", []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: git rev-parse --show-toplevel: %w", err)
	}
	span := strings.TrimSpace(string(base)) + "..." + strings.TrimSpace(string(orig))
	rebased, err := render.RunCLI(ctx, ws, "git", []string{"diff", "-z", "--name-only", span})
	if err != nil {
		return 0, nil, fmt.Errorf("stack continue: list the files the rebased commits change: git diff %s: %w", span, err)
	}
	var files []string
	for file := range strings.SplitSeq(strings.TrimRight(rebased, "\x00"), "\x00") {
		path := filepath.Join(strings.TrimSpace(root), file)
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path) //nolint:gosec // path is one git diff listed under the repo root
		if err != nil {
			return 0, nil, fmt.Errorf("stack continue: %w", err)
		}
		if slices.ContainsFunc(postimages, func(post []byte) bool { return bytes.Equal(post, content) }) {
			files = append(files, file)
		}
	}
	return len(postimages), files, nil
}

// stackRebaseOnto is the onto file of the rebase stopped in ws, written once as
// the rebase begins.
func stackRebaseOnto(ctx context.Context, ws render.Dir) (string, error) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path, err := stackGitPath(ctx, ws, dir+"/onto")
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("stack continue: the rebase in %s has no onto file", ws)
}

func stackGitPath(ctx context.Context, ws render.Dir, name string) (string, error) {
	out, err := render.RunCLI(ctx, ws, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", name})
	if err != nil {
		return "", fmt.Errorf("stack continue: git rev-parse --git-path %s: %w", name, err)
	}
	return strings.TrimSpace(out), nil
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
	gens, err := regenStaged(ctx, ws)
	if rest := slices.DeleteFunc(unmerged, func(p string) bool { return regenOwner(gens, p) != nil }); len(rest) > 0 {
		if err != nil {
			return fmt.Errorf("stack rebase: %s still has unresolved files: %s — resolve them and git add them first; nothing regenerates them: %w", c.Workspace, strings.Join(rest, ", "), err)
		}
		return fmt.Errorf("stack rebase: %s still has unresolved files: %s — resolve them and git add them first", c.Workspace, strings.Join(rest, ", "))
	}
	if err := stackAdvance(ctx, cmd, run, b); err != nil {
		return err
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

func stackDropWorkspace(ctx context.Context, l lane, ws string) error {
	listed, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "list", "--porcelain"})
	if err != nil {
		return fmt.Errorf("stack abort: git worktree list: %w", err)
	}
	if !slices.Contains(strings.Split(listed, "\n"), "worktree "+ws) {
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "prune"}); err != nil {
			return fmt.Errorf("stack abort: git worktree prune: %w", err)
		}
		return nil
	}
	if stackRebasing(ctx, render.Dir(ws)) {
		if _, err := render.RunCLI(ctx, render.Dir(ws), "git", []string{"rebase", "--abort"}); err != nil {
			return fmt.Errorf("stack abort: git rebase --abort in %s: %w", ws, err)
		}
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "remove", "--force", ws}); err != nil {
		return fmt.Errorf("stack abort: remove %s: %w", ws, err)
	}
	return nil
}

func runStackAbort(cmd *cobra.Command, stack string) error {
	ctx := cmd.Context()
	l, commonDir, run, err := stackResolveRun(ctx, stack)
	if err != nil {
		return err
	}
	outcome, err := stackSettle(ctx, l, commonDir, run)
	if err != nil {
		return err
	}
	cmd.Println(outcome)
	return nil
}

func stackSettle(ctx context.Context, l lane, commonDir string, run *stackRebaseRun) (string, error) {
	dir := l.dir()
	if err := stackRecoverPublication(ctx, dir, run); err != nil {
		return "", err
	}
	outcome := "aborted · no branch moved"
	switch {
	case run.Receipted:
		return "aborted pending publication metadata · published commits and source checkouts unchanged", stackCompletePublication(ctx, dir, commonDir, run)
	case run.Pushed:
		held, err := stackRemoteMatchesPublication(ctx, dir, "origin", run.PushTargets)
		if err != nil {
			return "", err
		}
		if held {
			return "", errors.New("stack rebase: the stack is pushed, but its publication receipts are not recorded — ccx vcs stack continue records them")
		}
		outcome = "aborted · the remote moved off the pushed stack, so no receipt was recorded · source checkouts unchanged"
	case stackLegacyApplied(run):
		outcome = "aborted · an older ccx rewrote these branches in place, and this ccx cannot resume that run · every branch stays where it is"
	case run.Applied:
		held, err := stackRewriteHeld(ctx, dir, run)
		if err != nil {
			return "", err
		}
		if held {
			return "", errors.New("stack abort: the rewritten stack is already written locally, so there is nothing left to abort — ccx vcs stack continue finishes recording and pushing it")
		}
		outcome = "aborted · the branches no longer hold the rewrite, so every branch stays where it is"
	}
	if c := run.Conflict; c != nil {
		if err := stackDropWorkspace(ctx, l, c.Workspace); err != nil {
			return "", err
		}
	}
	if err := stackDropPublicationPins(ctx, dir, run); err != nil {
		return "", err
	}
	if err := stackDropTempRefs(ctx, dir, run); err != nil {
		return "", err
	}
	if err := stackClearRun(commonDir, run); err != nil {
		return "", fmt.Errorf("stack abort: %w", err)
	}
	return outcome, nil
}

func stackLegacyApplied(run *stackRebaseRun) bool {
	return run.Applied && !run.NoPush
}

func stackRewriteHeld(ctx context.Context, dir render.Dir, run *stackRebaseRun) (bool, error) {
	for _, b := range run.Branches {
		if b.Landed != "" {
			continue
		}
		present, err := gitRefExists(ctx, dir, stackRebasePrefix, gtRestackRef(b.Name))
		if err != nil || !present {
			return false, err
		}
		at, err := stackRevParse(ctx, dir, gtRestackRef(b.Name))
		if err != nil {
			return false, err
		}
		if at != b.NewHead {
			return false, nil
		}
	}
	return true, nil
}

func stackResolveRun(ctx context.Context, stack string) (lane, string, *stackRebaseRun, error) {
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
	run, err := stackRunFor(runs, stack, l.root, current)
	if err != nil {
		return lane{}, "", nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		return lane{}, "", nil, fmt.Errorf("stack rebase: %w", err)
	}
	if run.Pid != os.Getpid() && (run.Host != host || stackPidAlive(run)) {
		return lane{}, "", nil, fmt.Errorf("stack rebase: pid %d on %s is still driving the stack rebase of %s — wait for it to finish", run.Pid, run.Host, strings.Join(run.Roots, ", "))
	}
	run.Pid, run.Started, run.Host = os.Getpid(), stackProcStart(os.Getpid()), host
	if _, err := os.Stat(run.Origin); run.Origin != "" && errors.Is(err, fs.ErrNotExist) {
		run.Origin = l.root
	}
	if err := stackSaveRun(run); err != nil {
		return lane{}, "", nil, err
	}
	if run.Origin != "" && l.root != run.Origin {
		if l, err = resolveLane(ctx, stackRebasePrefix, run.Origin, false); err != nil {
			return lane{}, "", nil, err
		}
	}
	return l, commonDir, run, nil
}

func stackFinish(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	if !run.NoPush {
		return stackFinishPublication(ctx, cmd, l, commonDir, run)
	}
	prefix := stackRebasePrefix
	var moves []restackMove
	var movers, dropped []string
	reparent := map[string]string{}
	revisions := map[string]string{}
	for _, b := range run.Branches {
		if b.Landed != "" {
			dropped = append(dropped, b.Name)
			continue
		}
		if b.Held != "" {
			continue
		}
		revisions[b.Name] = b.NewBase
		if b.Parent != b.WasParent {
			reparent[b.Name] = b.Parent
		}
		if b.NewHead != b.Local {
			movers = append(movers, b.Name)
			moves = append(moves, restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local})
		}
	}
	var realigned []string
	var alignErr error
	if !run.Applied {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if err := stackCheckHolders(ctx, run.Origin, movers, holders); err != nil {
			return err
		}
		if err := gtRestackRefuseClobbers(ctx, prefix, holders, moves); err != nil {
			return err
		}
		run.Applied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
		if err := stackWriteRefs(ctx, l.dir(), run); err != nil {
			run.Applied = false
			return errors.Join(err, stackSaveRun(run))
		}
	} else if err := stackWriteRefs(ctx, l.dir(), run); err != nil {
		return err
	}
	if !run.Aligned {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return err
		}
		for _, m := range moves {
			if holder := holders[m.branch]; holder != "" && holder != run.Origin {
				return fmt.Errorf("stack rebase: %s is now held by %s; checkout untouched", m.branch, holder)
			}
		}
		realigned, alignErr = gtRestackAlign(ctx, prefix, holders, moves)
		if alignErr != nil {
			return alignErr
		}
		run.Aligned = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	if err := errors.Join(
		gtmeta.Reparent(ctx, commonDir, reparent),
		gtmeta.RecordRestacked(ctx, commonDir, revisions),
		gtmeta.Forget(ctx, commonDir, dropped),
		stackDropTempRefs(ctx, l.dir(), run),
	); err != nil {
		return fmt.Errorf("%s: the branches are rewritten, but recording the stack in gt failed — fix the cause and run ccx vcs stack continue: %w", prefix, errors.Join(err, alignErr))
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
	if err := stackClearRun(commonDir, run); err != nil {
		return fmt.Errorf("%s: clear the run state: %w", prefix, err)
	}
	cmd.Println("not pushed (--no-push)")
	return nil
}

func stackLandedSince(ctx context.Context, dir render.Dir, trunk string, live []string) ([]string, error) {
	prs, err := stackPRLookup(ctx, dir, trunk, live)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: reading the stack pull requests before the push failed — run ccx vcs stack continue: %w", err)
	}
	return slices.DeleteFunc(slices.Clone(live), func(name string) bool { return prs[name] == nil || !prs[name].Landed }), nil
}

func stackReplanLanded(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, landed []string) error {
	if err := stackCheckSources(ctx, l.dir(), run); err != nil {
		return err
	}
	var members []string
	vetted := map[string]string{}
	replayed := map[string]stackRebaseBranch{}
	for _, b := range run.Branches {
		if b.Landed == "" {
			members = append(members, b.Name)
			vetted[b.Name] = b.Remote
			replayed[b.Name] = b
		}
	}
	next, err := stackPlan(ctx, l, commonDir, stackRebaseOpts{
		members: members, landed: landed, vetted: vetted, replayed: replayed, draft: run.Draft, noVerify: run.NoVerify, ship: run.Ship,
		tip: run.Tip, tipOnly: run.TipOnly, dropCommits: run.DropCommits,
	})
	if err != nil {
		return err
	}
	if !slices.Equal(run.Roots, next.Roots) {
		return errors.New("stack rebase: stack roots changed during replanning; original recovery state retained")
	}
	next.dir = run.dir
	if err := stackSaveRun(next); err != nil {
		return err
	}
	cmd.Println(fmt.Sprintf("%s landed while the run was stopped%snothing pushed%sreplanning without it", strings.Join(landed, ", "), shipSep, shipSep))
	cmd.Println(strings.Join(stackPlanLines(next), "\n"))
	return stackDrive(ctx, cmd, l, commonDir, next)
}

// stackFinishGit writes a git-lane restack's branch and moves the working copy
// it started from onto the new head, under the same holder rules as stackFinish.
func stackFinishGit(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	b := run.Branches[0]
	move := restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local}
	if !run.Applied {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("restack: %w", err)
		}
		if err := stackCheckHolders(ctx, run.Origin, []string{b.Name}, holders); err != nil {
			return err
		}
		if err := gtRestackRefuseClobbers(ctx, "restack", holders, []restackMove{move}); err != nil {
			return err
		}
		if err := stackWriteRefs(ctx, l.dir(), run); err != nil {
			return err
		}
		run.Applied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	if !run.Aligned {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("restack: %w", err)
		}
		if holder := holders[b.Name]; holder != "" && holder != run.Origin {
			return fmt.Errorf("restack: %s is now held by %s; checkout untouched", b.Name, holder)
		}
		if _, err := gtRestackAlign(ctx, "restack", holders, []restackMove{move}); err != nil {
			return err
		}
		run.Aligned = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	if err := errors.Join(stackDropTempRefs(ctx, l.dir(), run), stackClearRun(commonDir, run)); err != nil {
		return fmt.Errorf("restack: %s is rebased, but clearing the run failed: %w", b.Name, err)
	}
	summary := "fetched" + shipSep + "rebased onto " + run.Trunk
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
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
			at, err := stackRevParse(ctx, dir, gtRestackRef(b.Name))
			if err != nil {
				return err
			}
			if at == b.NewHead {
				fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), b.NewHead)
			} else {
				fmt.Fprintf(&tx, "update %s %s %s\n", gtRestackRef(b.Name), b.NewHead, b.Local)
			}
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

// stackClearRun removes the state directory once its last run is gone: the
// 0.65.x binary claims its lock by creating that directory, so an empty one
// refuses every lane still running it.
func stackClearRun(commonDir string, run *stackRebaseRun) error {
	for _, root := range run.Roots {
		if err := os.RemoveAll(stackRunDir(commonDir, root)); err != nil {
			return err
		}
	}
	err := os.Remove(filepath.Join(commonDir, stackRebaseStateDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func stackRuns(commonDir string) ([]*stackRebaseRun, error) {
	if err := stackRefuseFlatState(filepath.Join(commonDir, stackRebaseStateDir)); err != nil {
		return nil, err
	}
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

func stackRefuseFlatState(dir string) error {
	data, err := os.ReadFile(stackStatePath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	var old stackRebaseRun
	if err := json.Unmarshal(data, &old); err != nil {
		return fmt.Errorf("stack rebase: read %s: %w", stackStatePath(dir), err)
	}
	names := make([]string, 0, len(old.Branches))
	for _, b := range old.Branches {
		names = append(names, b.Name)
	}
	return fmt.Errorf("stack rebase: %s holds a stack rebase of %s started by ccx 0.65.x, which this version cannot drive — finish or abort it with that version, or once nothing drives it: rm -r %s", stackStatePath(dir), strings.Join(names, ", "), dir)
}

func stackRunFor(runs []*stackRebaseRun, stack, root, branch string) (*stackRebaseRun, error) {
	if len(runs) == 0 {
		return nil, errNoStackRebase
	}
	if stack != "" {
		for _, run := range runs {
			if slices.Contains(run.Roots, stack) {
				return run, nil
			}
		}
		return nil, fmt.Errorf("stack rebase: no stack rebase of %s is in progress — %s", stack, stackRunChoices(runs))
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
	return nil, fmt.Errorf("stack rebase: %d stack rebases are in progress — run this from one of the stack's branches or its conflict workspace, or name it: %s", len(runs), stackRunChoices(runs))
}

func stackRunChoices(runs []*stackRebaseRun) string {
	choices := make([]string, 0, len(runs))
	for _, run := range runs {
		choices = append(choices, "--stack "+run.Roots[0])
	}
	return strings.Join(choices, ", ")
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
	prs := map[string]*stackPR{}
	var closes []prQueueClose
	byNumber := map[int]*stackPR{}
	for _, branch := range branches {
		p, found, err := ghNewestPull(ctx, dir, branch)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if p.State == "open" {
			if p, err = ghPullAt(ctx, dir, p.Number); err != nil {
				return nil, err
			}
		}
		landing, err := ghLanding(ctx, dir, p, true)
		if err != nil {
			return nil, err
		}
		pr := &stackPR{
			Number: p.Number, URL: p.HTMLURL, Title: p.Title, Body: p.Body, State: landing.State,
			Base: p.Base.Ref, Head: p.Head.SHA, Mergeable: p.mergeable(), Labels: p.labelNames(),
		}
		switch landing.verdict(true) {
		case prLanded:
			pr.Landed = true
		case prQueueClosed:
			closes = append(closes, prQueueClose{Number: p.Number, Base: trunk})
			byNumber[p.Number] = pr
		}
		prs[branch] = pr
	}
	for number, landed := range resolveQueueLandings(ctx, dir, closes) {
		byNumber[number].Landed = landed
	}
	return prs, nil
}

func stackCheckHolders(ctx context.Context, origin string, movers []string, holders map[string]string) error {
	for _, branch := range movers {
		holder := holders[branch]
		if holder == "" {
			continue
		}
		if holder != origin {
			return fmt.Errorf("stack rebase: %s is checked out in %s; no branches moved — finish or detach that checkout, then retry with ccx", branch, holder)
		}
		status, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"status", "--porcelain", "--untracked-files=normal"})
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("stack rebase: %s has uncommitted work; no branches moved — commit or move that work, then retry with ccx", holder)
		}
	}
	return nil
}
