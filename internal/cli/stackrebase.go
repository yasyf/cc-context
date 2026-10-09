package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	stackRebasePrefix   = "stack rebase"
	stackRebaseStateDir = "ccx-stack-rebase"
	stackRebaseState    = "state.json"
	stackAdoptLock      = "adopt.lock"
	stackClaimOwner     = "owner"
	stackBriefLines     = 25
	stackCulprits       = 10
	stackVerdictTries   = 4
	stackHeadLagWait    = 30 * time.Second
	stackHeadLagRetry   = 5 * time.Second
	stackStaleAfter     = 5 * time.Minute
	stackAbandonedAfter = 2 * time.Hour
	stackReplayMajor    = 2
	stackReplayMinor    = 56
)

var stackRebaseStall = 10 * time.Minute

var stackGitRebaseArgs = []string{"-c", "rerere.enabled=false", "-c", "rebase.updateRefs=false", "-c", "core.editor=true", "-c", "core.hooksPath=/dev/null"}

var stackReplayRebaseArgs = []string{"--no-fork-point", "--no-rebase-merges", "--reapply-cherry-picks", "--keep-empty", "--empty=drop"}

type stackPR struct {
	Number     int      `json:"number"`
	URL        string   `json:"url"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	State      string   `json:"state"`
	Base       string   `json:"base"`
	Head       string   `json:"head"`
	Mergeable  string   `json:"mergeable"`
	Labels     []string `json:"labels,omitempty"`
	Landed     bool     `json:"landed"`
	BaseGone   bool     `json:"base_gone,omitempty"`
	BaseBack   bool     `json:"base_back,omitempty"`
	ParkedFrom string   `json:"parked_from,omitempty"`
}

func (p *stackPR) base() string {
	if p.ParkedFrom != "" {
		return p.ParkedFrom
	}
	return p.Base
}

func stackMarkParked(prs map[string]*stackPR, submitted map[string]gtmeta.Version) {
	for name, pr := range prs {
		if pr.Base == fmt.Sprintf("graphite-base/%d", pr.Number) {
			pr.ParkedFrom = submitted[name].BaseName
		}
	}
}

func (p *stackPR) String() string {
	return fmt.Sprintf("#%d %q", p.Number, p.Title)
}

func (p *stackPR) abandoned() bool {
	return p.State == "CLOSED" && !p.Landed && !p.BaseGone
}

// stackSupersedeClosed forgets the closed pull request of every branch newPRs
// names, so the run keeps the branch and its submit opens a new one in place of
// dropping it as abandoned.
func stackSupersedeClosed(prs map[string]*stackPR, members, newPRs, landed []string) error {
	for _, name := range newPRs {
		pr := prs[name]
		switch {
		case !slices.Contains(members, name):
			return fmt.Errorf("stack rebase: --new-pr named %s, which is not a branch of this run", name)
		case slices.Contains(landed, name):
			return fmt.Errorf("stack rebase: --new-pr and --landed both named %s", name)
		case pr == nil:
			return fmt.Errorf("stack rebase: --new-pr named %s, which has no pull request to replace — submit opens one without it", name)
		case pr.Landed:
			return fmt.Errorf("stack rebase: --new-pr named %s, whose pull request #%d landed", name, pr.Number)
		case pr.State != "CLOSED":
			return fmt.Errorf("stack rebase: --new-pr named %s, whose pull request #%d is still open", name, pr.Number)
		}
		delete(prs, name)
	}
	return nil
}

func (p *stackPR) closedLanding() string {
	return fmt.Sprintf("#%d closed", p.Number)
}

type stackTipClosedError struct {
	branch string
	number int
}

func (e stackTipClosedError) Error() string {
	return fmt.Sprintf("ship: %s's pull request #%d was closed without merging, and ship will not drop the branch it ships — reopen it with gh pr reopen %d, or open a new one with --new-pr %s", e.branch, e.number, e.number, e.branch)
}

type stackRebaseBranch struct {
	Name        string            `json:"name"`
	Parent      string            `json:"parent"`
	WasParent   string            `json:"was_parent"`
	Local       string            `json:"local"`
	Remote      string            `json:"remote,omitempty"`
	Head        string            `json:"head"`
	OldBase     string            `json:"old_base"`
	SourceBase  string            `json:"source_base"`
	Publication *stackPublication `json:"publication,omitempty"`
	Landed      string            `json:"landed,omitempty"`
	Held        string            `json:"held,omitempty"`
	Kept        bool              `json:"kept,omitempty"`
	Stays       bool              `json:"stays,omitempty"`
	Conflicts   []string          `json:"conflicts,omitempty"`
	Pinned      bool              `json:"pinned,omitempty"`
	Resolved    bool              `json:"resolved,omitempty"`
	LocalOnly   bool              `json:"local_only,omitempty"`
	Moved       bool              `json:"moved,omitempty"`
	Bump        bool              `json:"bump,omitempty"`
	PR          *stackPR          `json:"pr,omitempty"`
	NewBase     string            `json:"new_base,omitempty"`
	NewHead     string            `json:"new_head,omitempty"`
}

type stackConflict struct {
	Branch       string               `json:"branch"`
	Workspace    string               `json:"workspace"`
	Registration cleanup.Registration `json:"registration"`
	Brief        string               `json:"brief"`
}

type stackRebaseRun struct {
	Trunk         string   `json:"trunk"`
	Pin           string   `json:"pin"`
	NoPush        bool     `json:"no_push"`
	Git           bool     `json:"git,omitempty"`
	Origin        string   `json:"origin"`
	Draft         *bool    `json:"draft,omitempty"`
	DraftAll      bool     `json:"draft_all,omitempty"`
	NoVerify      bool     `json:"no_verify,omitempty"`
	Tip           string   `json:"tip,omitempty"`
	TipOnly       bool     `json:"tip_only,omitempty"`
	DropCommits   bool     `json:"drop_commits,omitempty"`
	StayClean     bool     `json:"stay_clean,omitempty"`
	Restack       bool     `json:"restack,omitempty"`
	AllLanes      bool     `json:"all_lanes,omitempty"`
	To            string   `json:"to,omitempty"`
	NewPRs        []string `json:"new_prs,omitempty"`
	deferPush     bool
	Ship          *stackShipIntent         `json:"ship,omitempty"`
	Retarget      *restackRetarget         `json:"retarget,omitempty"`
	Aligned       bool                     `json:"aligned,omitempty"`
	Applied       bool                     `json:"applied,omitempty"`
	Publishing    bool                     `json:"publishing,omitempty"`
	PushTargets   []stackPublicationTarget `json:"push_targets,omitempty"`
	Pushed        bool                     `json:"pushed,omitempty"`
	Receipted     bool                     `json:"receipted,omitempty"`
	LocalApplied  bool                     `json:"local_applied,omitempty"`
	LocalAligned  bool                     `json:"local_aligned,omitempty"`
	SourcesMoving bool                     `json:"sources_moving,omitempty"`
	Branches      []stackRebaseBranch      `json:"branches"`
	Conflict      *stackConflict           `json:"conflict,omitempty"`
	Roots         []string                 `json:"roots"`
	Claims        []string                 `json:"claims,omitempty"`
	Pid           int                      `json:"pid"`
	Started       string                   `json:"started"`
	Host          string                   `json:"host"`
	dir           string
	saved         time.Time
	resumed       bool
	left          []stackLeft
	lanePins      []string
	adopted       gtState
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

func (r *stackRebaseRun) leavesLocalRef(b stackRebaseBranch) bool {
	return b.Kept && (b.Pinned || tipOnlyAncestor(r.TipOnly, r.Tip, b.Name))
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
	newPRs      []string
	dryRun      bool
	noPush      bool
	members     []string
	pinned      []string
	draft       *bool
	draftAll    bool
	noVerify    bool
	deferPush   bool
	vetted      map[string]string
	replayed    map[string]stackRebaseBranch
	result      **stackRebaseRun
	ship        *stackShipIntent
	submit      bool
	tip         string
	tipOnly     bool
	dropCommits bool
	restack     bool
	stayClean   bool
	allLanes    bool
	otherLanes  bool
	include     []string
	to          string
	bump        []string
}

const stackDropCommitsUsage = "publish a branch whose local head drops commits its published head carries"

const stackToUsage = "stop at this branch: leave every branch stacked above it out of the run"

const stackNewPRUsage = "open a new pull request for <branch>, whose last one closed without merging, instead of dropping it (repeatable)"

// stackPRQuery reads the pull request of every branch of a stack.
type stackPRQuery func(ctx context.Context, dir render.Dir, trunk string, branches []string) (map[string]*stackPR, error)

type stackPRKey struct{}

// withStackPRs returns ctx answering the stack's pull-request reads with query
// in place of GitHub, for a test pairing its answers with one test rather than
// with the process.
func withStackPRs(ctx context.Context, query stackPRQuery) context.Context {
	return context.WithValue(ctx, stackPRKey{}, query)
}

// stackPRs reads every branch's pull request through the query ctx carries,
// falling back to GitHub.
func stackPRs(ctx context.Context, dir render.Dir, trunk string, branches []string) (map[string]*stackPR, error) {
	if query, ok := ctx.Value(stackPRKey{}).(stackPRQuery); ok {
		return query(ctx, dir, trunk, branches)
	}
	return stackQueryPRs(ctx, dir, trunk, branches)
}

func newStackRebaseCmd() *cobra.Command {
	var o stackRebaseOpts
	cmd := &cobra.Command{
		Use:   "rebase",
		Short: "Rebase the whole stack onto trunk and push it, stopping in a workspace on conflict",
		Long: `Rebase every branch of the stack onto its parent and trunk, then push it.

The stack is this lane: the branch checked out here, the branches below it down
to trunk, and those stacked above it, with the same for each branch --parent,
--linearize, or --landed names. Another lane cut from a shared ancestor is left
out, even when its branches sit on one this run moves. --parent and
--linearize move only the branches they name a parent for and those stacked
above them: each new parent and the branches below it stay at their published
heads, or at their local heads in a run that does not push. --to <branch> stops the
run at <branch>: of the branches stacked above a seed, only those <branch> sits
on, and <branch> itself, join it. Every seed must be <branch> or sit below it;
any other <branch> is refused, as is one a member was last published onto. --all-lanes widens the run to every branch gt
tracks. Without it, a pushing run keeps a branch whose name differs from the
checked-out branch's before the last slash at its published head when this lane
sits on it, never pushing or submitting it, and leaves out the rest of that lane.

Every branch's local and remote head is recorded before anything moves, and each
branch is replayed from the base it was recorded on (--onto <new parent>
<recorded old parent>), so a push partway through never changes what a child is
rebased from. The replay is git replay's, over the recorded commits alone: it
checks nothing out, touches no index, runs no hook, and flattens a merge the
way git rebase --no-rebase-merges does. It needs git 2.56 or newer. A branch
whose pull request landed is dropped and its children move onto what it sat
on, leaving its squashed commits behind. A branch whose pull request was closed
without landing is dropped the same way, and its own commits are never
replayed; one GitHub closed because its base branch was deleted is reopened
onto its new parent before the push instead. Local trunk is left untouched.
Uncommitted work in the invoking checkout, or in a working copy holding a moved
branch, stops publication before any branch moves; a clean working copy holding
a moved branch is moved onto its new head, still on that branch. With
--no-push, a kept branch another working copy holds stays at its local head.
Empty lanes and another lane's branches above the one checked out here are left
where they are and named rather than rebased. Once published, the local
ref of a branch no working copy holds moves onto its published head; a held
branch moves only when that head carries the same commits as the source,
compared without diff context so a clean replay beside upstream edits still
matches, and its working copy is clean and is moved with it; any other branch
keeps its source, with the branches above it, and is named with its kept local
head and its published head.

A conflict stops the run before any ref moves: the conflicted branch alone is
rebased again in a sparse workspace of its own, holding the root files and the
directory of each conflicted file, and left in progress there with both sides'
intent written out; ccx vcs stack continue resumes the rest of the stack from
there (ccx vcs stack abort drops it). rerere is off for every rebase it drives,
and a rebase is killed only once it has made no progress for ten minutes,
with the index.lock it held removed.
A stopped run whose branches have moved since, or that has waited two hours,
is reclaimed by the next rebase or ship that overlaps it, which removes its
workspace and says so.
A conflict in a file .ccx.toml lists under [[generated]] stops like any other;
ccx vcs stack regenerate reruns its generator in the workspace.

After the rewrite, gt's parents are recorded, the stack is force-pushed under
the remote heads recorded at the start, and one verdict line per pull request
names its pushed head, parent, and mergeability; when GitHub cannot be read
for it, the run says so and still moves the local refs and finishes. An open
pull request GitHub bases elsewhere than its recorded parent is retargeted
onto that parent, including one the run submitted that Graphite left on its
graphite-base branch; a refused retarget names the command that finishes it
and fails the run once the local refs have moved.
Labels and draft state are never touched: a draft pull request stays a draft.

stack rebase never opens a pull request. A stack none of whose branches has one
is rebased locally as if --no-push were given. In a stack mixing the two, a
branch without one that only branches without one sit on is rebased locally
onto its parent's published head and not pushed, while the rest publish; one a
branch with a pull request sits on is refused before anything moves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return stackSettleTracking(cmd, func() error { return runStackRebase(cmd, o) })
		},
	}
	cmd.Flags().StringArrayVar(&o.parents, "parent", nil, "restack <branch>=<parent> onto a new parent, moving only <branch> and the branches above it (repeatable)")
	cmd.Flags().StringSliceVar(&o.linearize, "linearize", nil, "chain these branches in this order, each onto the one before it")
	cmd.Flags().StringArrayVar(&o.landed, "landed", nil, "treat <branch> as landed and drop it (repeatable)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan and move nothing")
	cmd.Flags().BoolVar(&o.noPush, "no-push", false, "rewrite the local stack and gt's record, but push nothing")
	cmd.Flags().BoolVar(&o.dropCommits, "drop-commits", false, stackDropCommitsUsage)
	cmd.Flags().BoolVar(&o.allLanes, "all-lanes", false, "rebase every branch gt tracks, not only this lane's")
	cmd.Flags().StringVar(&o.to, "to", "", stackToUsage)
	cmd.MarkFlagsMutuallyExclusive("to", "all-lanes")
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
stale recording silently drops a branch's own changes.

Continue never opens a pull request the run carries no title and body for. A
branch with no open pull request and no --pr-title and --pr-body-file from the
command that started the run is pushed, not submitted, and named with the
ccx vcs ship command that opens it, unless that command named it with --new-pr.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return stackSettleTracking(cmd, func() error { return runStackContinue(cmd, stack) })
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "the run to resume, named by its stack's bottom branch or a branch it writes")
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
	cmd.Flags().StringVar(&stack, "stack", "", "the run to drop, named by its stack's bottom branch or a branch it writes")
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
	if err := stackRequireGit(ctx, l.dir(), stackRebasePrefix); err != nil {
		return err
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
		if (other.Conflict != nil && other.Conflict.Workspace == l.root) || (current != "" && slices.Contains(stackOwned(other), current)) {
			if _, err := stackGate(ctx, cmd, l, commonDir, other, o.dryRun); err != nil {
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
	if err := stackRefusePins(ctx, l.dir(), run); err != nil {
		return err
	}
	if o.tip != "" && !o.restack {
		if err := stackRefuseGreenRestack(ctx, l, run); err != nil {
			return err
		}
	}
	if !o.submit && !o.noPush {
		if run, err = stackKeepLocal(ctx, cmd, l, commonDir, o, run); err != nil {
			return err
		}
	}
	if o.result != nil {
		*o.result = run
	}
	finished, err := stackAdmit(ctx, cmd, l, commonDir, others, run, o.dryRun)
	if err != nil {
		return err
	}
	if finished {
		runs, err := stackRuns(commonDir)
		if err != nil {
			return err
		}
		return stackBegin(ctx, cmd, l, commonDir, runs, o)
	}
	if err := stackShipCovers(run, o.ship); err != nil {
		return err
	}
	if err := stackAnnounceLeft(cmd, run.left); err != nil {
		return err
	}
	if pins := slices.DeleteFunc(slices.Clone(run.lanePins), func(name string) bool { return run.branch(name) == nil || !run.branch(name).Pinned }); len(pins) > 0 {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "stack rebase: not pushing %s — another lane's, kept at their published heads; --all-lanes pushes them\n", strings.Join(pins, ", ")); err != nil {
			return err
		}
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
	if err := stackTrackAdopted(ctx, commonDir, run); err != nil {
		return err
	}
	if err := stackClaim(commonDir, run); err != nil {
		return err
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return stackDrive(ctx, cmd, l, commonDir, run)
}

// stackKeepLocal keeps stack rebase from opening a pull request, which a
// pushing run's submit does for every branch that has none: a stack none of
// whose branches has one is replanned to rebase locally, a branch with none
// stacked only under others without one is rebased locally while the rest
// publish, and one a branch with a pull request sits on is refused before
// anything moves.
func stackKeepLocal(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, o stackRebaseOpts, run *stackRebaseRun) (*stackRebaseRun, error) {
	var live, bare []string
	for _, b := range run.Branches {
		if b.Landed != "" || b.Held != "" || b.Kept {
			continue
		}
		live = append(live, b.Name)
		if b.PR == nil || (b.PR.State != "OPEN" && !b.reopens()) {
			bare = append(bare, b.Name)
		}
	}
	switch {
	case len(bare) == 0:
		return run, nil
	case len(bare) < len(live):
		verb, them := "have", "them"
		if below := stackBareBelow(run, live, bare); len(below) > 0 {
			if len(below) == 1 {
				verb, them = "has", "it"
			}
			return nil, fmt.Errorf("stack rebase: %s %s no pull request, and stack rebase never opens one — rebase with --no-push to keep the stack local, or open %s first with ccx vcs ship --pr-body-file", strings.Join(below, ", "), verb, them)
		}
		for i := range run.Branches {
			run.Branches[i].LocalOnly = slices.Contains(bare, run.Branches[i].Name)
		}
		if len(bare) == 1 {
			verb = "has"
		}
		cmd.Printf("%s %s no pull request, and stack rebase never opens one, so it rebases locally without pushing\n", strings.Join(bare, ", "), verb)
		return run, nil
	}
	cmd.Println("no branch of this stack has a pull request, and stack rebase never opens one, so it rebases locally without pushing")
	o.noPush = true
	return stackPlan(ctx, l, commonDir, o)
}

func stackBareBelow(run *stackRebaseRun, live, bare []string) []string {
	var below []string
	for _, name := range live {
		if slices.Contains(bare, name) {
			continue
		}
		for b := run.branch(run.branch(name).Parent); b != nil; b = run.branch(b.Parent) {
			if slices.Contains(bare, b.Name) && !slices.Contains(below, b.Name) {
				below = append(below, b.Name)
			}
		}
	}
	slices.Sort(below)
	return below
}

// stackGate refuses a rebase over a run in progress, finishes a dead run whose
// push already matches origin, or reclaims the run when it is stale.
func stackGate(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, other *stackRebaseRun, dryRun bool) (bool, error) {
	done, err := stackGateRun(ctx, cmd, l, commonDir, other, dryRun)
	if errors.Is(err, errStackRunGone) {
		cmd.Println(fmt.Sprintf("the stack rebase of %s ended meanwhile", strings.Join(other.Roots, ", ")))
		return true, nil
	}
	return done, err
}

func stackGateRun(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, other *stackRebaseRun, dryRun bool) (bool, error) {
	pushed, err := stackDeadPushed(ctx, l.dir(), other)
	if err != nil {
		return false, err
	}
	if pushed {
		return !dryRun, stackFinishDead(ctx, cmd, l, commonDir, other, dryRun)
	}
	if !stackStale(other) {
		settledRuns, err := stackSettled(ctx, l.dir(), []*stackRebaseRun{other})
		if err != nil {
			return false, err
		}
		settled := settledRuns[other]
		if settled != "" && other.Conflict == nil {
			return false, stackDiscard(ctx, cmd, l, commonDir, other, settled, dryRun)
		}
		why, err := stackAbandoned(ctx, l.dir(), other)
		if err != nil {
			return false, err
		}
		why = cmp.Or(settled, why)
		if why == "" || other.Conflict.Workspace == l.root {
			return false, stackInProgress(other)
		}
		return false, stackReclaimConflict(ctx, cmd, l, commonDir, other, why, dryRun)
	}
	if dryRun {
		cmd.Println(fmt.Sprintf("would reclaim the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
		return false, nil
	}
	if err := stackReclaim(ctx, l, commonDir, other); err != nil {
		return false, err
	}
	cmd.Println(fmt.Sprintf("reclaimed the stale stack rebase of %s (%s)", strings.Join(other.Roots, ", "), stackHolder(other)))
	return false, nil
}

func stackDeadPushed(ctx context.Context, dir render.Dir, run *stackRebaseRun) (bool, error) {
	host, _ := os.Hostname()
	if run.Host != host || run.Conflict != nil || !run.Publishing || len(run.PushTargets) == 0 || stackPidAlive(run) {
		return false, nil
	}
	return stackRemoteMatchesPublication(ctx, dir, "origin", run.PushTargets)
}

func stackFinishDead(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, dryRun bool) error {
	line := fmt.Sprintf("the stack rebase of %s (%s), whose push already matches origin", strings.Join(run.Roots, ", "), stackHolder(run))
	if dryRun {
		cmd.Println("would finish " + line)
		return nil
	}
	cmd.Println("finishing " + line)
	if err := stackAdopt(run); err != nil {
		return err
	}
	run.resumed = true
	owner, err := stackTakeOver(ctx, l, run)
	if err != nil {
		return err
	}
	if err := stackDrive(ctx, cmd, owner, commonDir, run); err != nil {
		return fmt.Errorf("stack rebase: finish the stack rebase of %s: %w", strings.Join(run.Roots, ", "), err)
	}
	return nil
}

func stackAdopt(run *stackRebaseRun) (err error) {
	roots := strings.Join(run.Roots, ", ")
	lock := filepath.Join(run.dir, stackAdoptLock)
	if err := os.Mkdir(lock, 0o700); errors.Is(err, fs.ErrNotExist) {
		return errStackRunGone
	} else if err != nil {
		return fmt.Errorf("stack rebase: adopt the run of %s (another caller may be adopting it — re-run): %w", roots, err)
	}
	defer func() { err = errors.Join(err, os.Remove(lock)) }()
	info, err := os.Stat(stackStatePath(run.dir))
	if errors.Is(err, fs.ErrNotExist) {
		return errStackRunGone
	}
	if err != nil || !info.ModTime().Equal(run.saved) {
		return fmt.Errorf("stack rebase: the run of %s saved again while it was being adopted — re-run", roots)
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	run.Pid, run.Started, run.Host = os.Getpid(), stackProcStart(os.Getpid()), host
	return stackSaveRun(run)
}

func stackAbandoned(ctx context.Context, dir render.Dir, run *stackRebaseRun) (string, error) {
	host, _ := os.Hostname()
	if run.Conflict == nil || run.Host != host || run.Applied || run.Publishing || stackPidAlive(run) || time.Since(run.saved) < stackStaleAfter {
		return "", nil
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("stack rebase: %w", err)
	}
	for _, b := range run.Branches {
		if b.Landed != "" {
			continue
		}
		for ref, want := range map[string]string{gtRestackRef(b.Name): b.Local, "refs/remotes/" + remote + "/" + b.Name: b.Remote} {
			if want == "" {
				continue
			}
			at, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"rev-parse", "--verify", "--quiet", ref})
			if err == nil && code > 1 {
				err = errors.New(strings.TrimSpace(stderr))
			}
			if err != nil {
				return "", fmt.Errorf("stack rebase: git rev-parse %s: %w", ref, err)
			}
			if strings.TrimSpace(at) != want {
				return "since its branches moved", nil
			}
		}
	}
	if time.Since(run.saved) >= stackAbandonedAfter {
		return "past the " + stackAge(stackAbandonedAfter) + " limit", nil
	}
	return "", nil
}

func stackReclaimConflict(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, why string, dryRun bool) error {
	ws := run.Conflict.Workspace
	line := fmt.Sprintf("the stack rebase of %s stopped on a conflict %s ago, %s", strings.Join(run.Roots, ", "), stackAge(time.Since(run.saved)), why)
	if dryRun {
		cmd.Println("would reclaim " + line + " — and remove " + ws)
		return nil
	}
	state, err := stackWorkspaceAt(stackRebasePrefix, run.Conflict)
	if err != nil {
		return err
	}
	var status string
	if state == stackWorkspaceOwned {
		if status, err = render.RunCLI(ctx, render.Dir(ws), "git", []string{"status", "--porcelain", "--untracked-files=normal"}); err != nil {
			return fmt.Errorf("stack rebase: read %s before reclaiming its run: %w", ws, err)
		}
	}
	if err := stackReclaim(ctx, l, commonDir, run); err != nil {
		return err
	}
	note, err := stackReleaseWorkspace(ctx, l, commonDir, run.Conflict, true)
	if err != nil {
		return fmt.Errorf("%w — the stale run of %s is reclaimed", err, strings.Join(run.Roots, ", "))
	}
	line = "reclaimed " + line + " — " + cmp.Or(note, "removed "+ws)
	if strings.TrimSpace(status) != "" {
		line += ", which held uncommitted changes"
	}
	cmd.Println(line)
	return nil
}

func stackAge(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func stackAdmit(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, runs []*stackRebaseRun, run *stackRebaseRun, dryRun bool) (bool, error) {
	run.Claims = stackWrites(run)
	if len(run.Claims) == 0 {
		run.Claims = run.Roots
	}
	var apart []*stackRebaseRun
	for _, other := range runs {
		if other.Conflict == nil && !stackOverlaps(run, other) {
			apart = append(apart, other)
		}
	}
	settled, err := stackSettled(ctx, l.dir(), apart)
	if err != nil {
		return false, err
	}
	for _, other := range apart {
		if why := settled[other]; why != "" {
			if err := stackDiscard(ctx, cmd, l, commonDir, other, why, dryRun); err != nil && !errors.Is(err, errStackRunGone) {
				return false, err
			}
		}
	}
	finished := false
	for _, other := range runs {
		if !stackOverlaps(run, other) {
			continue
		}
		done, err := stackGate(ctx, cmd, l, commonDir, other, dryRun)
		if err != nil {
			return false, err
		}
		finished = finished || done
	}
	return finished, nil
}

func stackInProgress(run *stackRebaseRun) error {
	if c := run.Conflict; c != nil {
		return fmt.Errorf("stack rebase: a stack rebase of %s is stopped on a conflict in %s since %s (on %s, %s ago) — resolve it there and run ccx vcs stack continue, or drop it with ccx vcs stack abort --stack %s", strings.Join(run.Roots, ", "), c.Workspace, run.saved.UTC().Format("15:04Z"), c.Branch, stackAge(time.Since(run.saved)), run.locks()[0])
	}
	return fmt.Errorf("stack rebase: a stack rebase of %s is already in progress (%s) writing %s — finish it with ccx vcs stack continue --stack %s, or drop it with ccx vcs stack abort --stack %s", strings.Join(run.Roots, ", "), stackHolder(run), strings.Join(stackOwned(run), ", "), run.locks()[0], run.locks()[0])
}

func stackOverlaps(run, other *stackRebaseRun) bool {
	return slices.ContainsFunc(stackOwned(run), func(name string) bool { return slices.Contains(stackOwned(other), name) })
}

func stackWrites(run *stackRebaseRun) []string {
	var names []string
	for _, b := range run.Branches {
		if b.Landed == "" && b.Held == "" && !run.leavesLocalRef(b) && (!b.Kept || run.NoPush || b.Head != b.Local) {
			names = append(names, b.Name)
		}
	}
	return names
}

func stackOwned(run *stackRebaseRun) []string {
	owned := slices.Clone(run.locks())
	for _, name := range stackWrites(run) {
		if !slices.Contains(owned, name) {
			owned = append(owned, name)
		}
	}
	return owned
}

func (r *stackRebaseRun) locks() []string {
	if len(r.Claims) > 0 {
		return r.Claims
	}
	return r.Roots
}

func stackSettled(ctx context.Context, dir render.Dir, runs []*stackRebaseRun) (map[*stackRebaseRun]string, error) {
	host, _ := os.Hostname()
	open := map[string][]string{}
	var dead []*stackRebaseRun
	for _, run := range runs {
		if run.Host != host || stackPidAlive(run) {
			continue
		}
		dead = append(dead, run)
		for _, name := range stackOwned(run) {
			if b := run.branch(name); (b == nil || b.Landed == "") && !slices.Contains(open[run.Trunk], name) {
				open[run.Trunk] = append(open[run.Trunk], name)
			}
		}
	}
	landed := map[string]*stackPR{}
	for trunk, names := range open {
		prs, err := stackPRs(ctx, dir, trunk, names)
		if err != nil {
			return nil, fmt.Errorf("stack rebase: read the pull requests of exited stack rebases: %w", err)
		}
		for name, pr := range prs {
			if pr != nil && pr.Landed {
				landed[name] = pr
			}
		}
	}
	settled := map[*stackRebaseRun]string{}
	for _, run := range dead {
		var gone []string
		for _, name := range stackOwned(run) {
			b := run.branch(name)
			if b != nil && b.Landed != "" {
				gone = append(gone, name+" landed")
				continue
			}
			pr := landed[name]
			if b != nil && !run.Pushed && b.NewHead != "" && b.NewHead != b.Head && (pr == nil || pr.Head != b.NewHead) {
				gone = nil
				break
			}
			if pr != nil {
				gone = append(gone, fmt.Sprintf("%s landed as #%d", name, pr.Number))
				continue
			}
			_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"rev-parse", "--verify", "--quiet", gtRestackRef(name)})
			if err == nil && code > 1 {
				err = errors.New(strings.TrimSpace(stderr))
			}
			if err != nil {
				return nil, fmt.Errorf("stack rebase: git rev-parse %s: %w", gtRestackRef(name), err)
			}
			if code == 0 {
				gone = nil
				break
			}
			gone = append(gone, name+" deleted")
		}
		if len(gone) > 0 {
			settled[run] = "since every branch it writes is gone (" + strings.Join(gone, ", ") + ")"
		}
	}
	return settled, nil
}

func stackDiscard(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, why string, dryRun bool) error {
	line := fmt.Sprintf("the stack rebase of %s (%s), %s", strings.Join(run.Roots, ", "), stackHolder(run), why)
	if dryRun {
		cmd.Println("would discard " + line)
		return nil
	}
	if err := stackReclaim(ctx, l, commonDir, run); err != nil {
		return err
	}
	cmd.Println("discarded " + line)
	return nil
}

// stackReclaim takes a stale run's state aside with one rename, so of two
// callers reclaiming the same run only one proceeds, and a run that saved
// after the caller judged it stale is put back rather than dropped.
func stackReclaim(ctx context.Context, l lane, commonDir string, run *stackRebaseRun) error {
	roots := strings.Join(run.Roots, ", ")
	tomb := fmt.Sprintf("%s.reclaim-%d", run.dir, os.Getpid())
	if err := os.Rename(run.dir, tomb); errors.Is(err, fs.ErrNotExist) {
		return errStackRunGone
	} else if err != nil {
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
	for _, name := range run.locks()[1:] {
		if err := os.RemoveAll(stackRunDir(commonDir, name)); err != nil {
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
	state, err := gtStateAtFocused(ctx, commonDir, prefix, o.tip, "")
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
	tr, err := gtTrunkRef(ctx, l.dir(), prefix, trunk)
	if err != nil {
		return nil, err
	}
	parents, err := stackAdoptUntrackedParents(ctx, l.dir(), commonDir, tr, state, overrides)
	if err != nil {
		return nil, err
	}
	adopted, err := stackAdoptUntracked(ctx, l.dir(), tr, state, overrides)
	if err != nil {
		return nil, err
	}
	maps.Copy(adopted, parents)
	retargeted := stackRetargeted(state, overrides)
	seeds := append(slices.Sorted(maps.Keys(overrides)), o.landed...)
	if current != "" && current != trunk {
		seeds = append([]string{current}, seeds...)
	}
	if o.allLanes {
		seeds = append(seeds, slices.DeleteFunc(slices.Sorted(maps.Keys(retargeted)), func(name string) bool { return name == trunk })...)
	}
	if len(seeds) == 0 {
		return nil, errors.New("stack rebase: HEAD is not on a stack branch — run it from a working copy holding one, or name the branches with --parent/--linearize")
	}
	var members, roots, upTo []string
	if o.to == "" {
		members, roots, err = stackMembers(retargeted, trunk, seeds)
	} else if upTo, err = stackUpTo(prefix, retargeted, trunk, seeds, o.to); err == nil {
		members, roots = gtBottomUp(upTo), upTo[len(upTo)-1:]
	}
	if err != nil {
		return nil, err
	}
	if o.members != nil {
		members = o.members
	}
	submitted, err := gtmeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}
	if !o.noPush {
		planned := members
		if members, roots, err = stackWithPublishedParents(ctx, l.dir(), retargeted, submitted, trunk, members, roots, overrides); err != nil {
			return nil, err
		}
		if o.submit {
			if o.pinned, err = stackPinHeldParents(ctx, l, planned, members, o.pinned); err != nil {
				return nil, err
			}
		}
	}
	var otherLanes []stackLeft
	var lanePins []string
	if !o.noPush && !o.allLanes && !o.otherLanes && o.replayed == nil && current != "" && current != trunk {
		members, lanePins, otherLanes = stackPinOtherLanes(retargeted, current, members, o.include)
		o.pinned = append(o.pinned, slices.DeleteFunc(slices.Clone(lanePins), func(name string) bool { return slices.Contains(o.pinned, name) })...)
	}
	if above := slices.DeleteFunc(slices.Clone(members), func(name string) bool { return upTo == nil || slices.Contains(upTo, name) }); len(above) > 0 {
		return nil, fmt.Errorf("stack rebase: --to %s cannot stop there: %s, above it, is where a branch of the run was last published", o.to, strings.Join(above, ", "))
	}

	prs, err := stackPRs(ctx, l.dir(), trunk, members)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read the stack's pull requests: %w", err)
	}
	if err := stackSupersedeClosed(prs, members, o.newPRs, o.landed); err != nil {
		return nil, err
	}
	locals := make(map[string]string, len(members))
	for _, name := range members {
		locals[name] = retargeted[name].Head
	}
	if err := stackRequireHistory(ctx, l.dir(), prefix, tr.Remote(), trunk, locals); err != nil {
		return nil, err
	}
	if err := stackMarkBaseGone(ctx, l.dir(), tr, prs); err != nil {
		return nil, err
	}
	stackMarkParked(prs, submitted)
	members, left, err := stackKept(ctx, l.dir(), state, tr, current, members, prs, overrides, o.landed)
	if err != nil {
		return nil, err
	}
	left = slices.Concat(otherLanes, left)
	pin, err := gtTrunkHead(ctx, l.dir(), prefix, tr)
	if err != nil {
		return nil, err
	}
	remotes, err := stackRemoteHeads(ctx, l.dir(), stackRebasePrefix, tr.Remote(), members, pin)
	if err != nil {
		return nil, err
	}
	if err := stackRequireHistory(ctx, l.dir(), prefix, tr.Remote(), trunk, remotes); err != nil {
		return nil, err
	}

	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("stack rebase: %w", err)
	}
	run := &stackRebaseRun{Trunk: trunk, Pin: pin, NoPush: o.noPush, Origin: l.checkout.Root, Draft: o.draft, DraftAll: o.draftAll, NoVerify: o.noVerify, Tip: o.tip, TipOnly: o.tipOnly, DropCommits: o.dropCommits, StayClean: o.stayClean, Restack: o.restack, AllLanes: o.allLanes, To: o.to, NewPRs: o.newPRs, deferPush: o.deferPush, Ship: o.ship, Roots: roots, Pid: os.Getpid(), Started: stackProcStart(os.Getpid()), Host: host, left: left, lanePins: lanePins, adopted: adopted}
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
		superseded, retracked, publishedOn := false, false, ""
		if !o.noPush {
			receipt, err = stackReadPublication(ctx, l.dir(), name)
			if err != nil {
				return nil, err
			}
			retracked = stackRetracked(receipt, source)
			if receipt != nil && !retracked && remotes[name] != "" && remotes[name] != receipt.Head {
				switch remotes[name] {
				case submitted[name].HeadSha:
					superseded, publishedOn = true, stackPublishedParent(receipt, remotes[name], submitted[name])
				case source.Head:
					superseded, publishedOn = true, receipt.Parent
				default:
					followed, base, err := thinFollowAdopted(ctx, l, name, source.Head, remotes[name], trunk, pin, !o.dryRun)
					if err != nil {
						return nil, err
					}
					if followed {
						source.Head = remotes[name]
						source.Parents = []gtRef{{Ref: source.Parents[0].Ref, SHA: base}}
						state[name], effective = source, source
						superseded, publishedOn = true, receipt.Parent
						break
					}
					if superseded, err = stackOwnRemote(ctx, l.dir(), name, source.Head, remotes[name], pin); err != nil {
						return nil, err
					}
				}
			}
			if receipt != nil && !superseded && !retracked && receipt.Source == source.Head {
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
		published := stackReadsPublished(o, name)
		b, err := stackSnapshot(ctx, l.dir(), tr, effective, name, remotes[name], ours, prs[name], slices.Contains(o.landed, name), pin, o.dropCommits, published)
		if err != nil && len(own) > 0 && !own[name] {
			outside[name] = true
			run.left = append(run.left, stackLeft{branch: name, why: strings.TrimPrefix(err.Error(), stackRebasePrefix+": ")})
			continue
		}
		if err != nil {
			return nil, stackTipOnlyHint(o, name, err)
		}
		if name == o.tip && b.PR != nil && b.Landed == b.PR.closedLanding() {
			return nil, stackTipClosedError{branch: name, number: b.PR.Number}
		}
		b.Local = source.Head
		if !o.noPush {
			if superseded {
				b.Publication = receipt
				b.WasParent = cmp.Or(publishedOn, b.WasParent)
			} else if retracked {
				b.Publication = receipt
			} else if err := stackUsePublication(ctx, l.dir(), &b, receipt, pin, published, own[name] && name != current); err != nil {
				return nil, err
			} else if receipt != nil && b.OldBase == receipt.Base && b.Head == receipt.Head {
				stale, err := stackBaseInTrunk(ctx, l.dir(), receipt.Base, receipt.Head, pin)
				if err != nil {
					return nil, err
				}
				if stale {
					b.OldBase, b.SourceBase, b.WasParent = "", "", state[name].Parents[0].Ref
				}
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
	for name, b := range byName {
		if b.reopens() && !o.noPush && b.Parent != trunk && remotes[b.Parent] == "" {
			return nil, fmt.Errorf("stack rebase: %s's pull request #%d closed when its base %s was deleted, and its new parent %s is not on the remote to reopen it onto — publish %s first with ccx vcs stack submit --to %s from its checkout, then re-run, or pass --landed %s if it did land", name, b.PR.Number, b.PR.Base, b.Parent, b.Parent, b.Parent, name)
		}
	}
	if err := stackCheckHeld(ctx, l.dir(), trunk, byName); err != nil {
		return nil, err
	}
	order, err := stackOrder(trunk, byName)
	if err != nil {
		return nil, err
	}
	queue, err := stackQueueStates(ctx, l, o.noPush, byName)
	if err != nil {
		return nil, err
	}
	moving, err := stackMovingBranches(retargeted, o, overrides)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{trunk: true}
	inherits := map[string]bool{}
	var unpushed []string
	for _, name := range order {
		b := byName[name]
		if b.Landed != "" || b.Held != "" {
			continue
		}
		if inherits[name], err = stackInheritsTrunk(ctx, l.dir(), trunk, pin, b, inherits); err != nil {
			return nil, err
		}
		_, named := overrides[name]
		switch {
		case queue[name] == prQueueQueued && (named || (name == o.tip && b.Local != b.Remote)):
			return nil, fmt.Errorf("stack rebase: %s is in the merge queue as %s, and moving it would evict it — take it out of the queue first", name, b.PR)
		case slices.Contains(o.pinned, name):
			if !stackPinPublished(b) {
				unpushed = append(unpushed, name)
			}
			b.Kept, b.Pinned = true, true
		case queue[name] == prQueueQueued || (moving != nil && !moving[name] && !inherits[name]):
			b.Kept = o.noPush || stackPinPublished(b)
		case o.tip != "" && name != o.tip && !o.restack && (o.tipOnly || !inherits[name]):
			if b.Kept, err = stackKeepsAncestor(ctx, l.dir(), pin, b, kept[b.Parent], o.tipOnly); err != nil {
				return nil, err
			}
		}
		kept[name] = b.Kept
	}
	if len(unpushed) > 0 {
		return nil, stackRefuseUnpushedPins(unpushed)
	}
	for _, name := range order {
		b := byName[name]
		if b.Landed == "" && b.Held == "" {
			if b.OldBase == "" {
				if b.OldBase, err = stackOldBase(ctx, l.dir(), tr, pin, state, b, byName); err != nil {
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
			b.Bump = slices.Contains(o.bump, name)
			if o.stayClean && !b.Bump && !inherits[name] && b.Parent == trunk && b.WasParent == trunk && b.Remote != "" && b.OldBase != pin && queue[name] != prQueueEvicted && (b.PR == nil || b.PR.Mergeable != "CONFLICTING") {
				if b.Stays, err = stackMergesClean(ctx, l.dir(), pin, stackUpstack(order, byName, name)); err != nil {
					return nil, err
				}
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
		return slices.Contains(landed, name) || (prs[name] != nil && (prs[name].Landed || prs[name].abandoned()))
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
	if pr != nil && pr.State == "OPEN" && pr.Base != "" && pr.base() != parent && pr.base() != effective {
		if pr.Base == fmt.Sprintf("graphite-base/%d", pr.Number) && pr.ParkedFrom == "" {
			return fmt.Sprintf("gt records its parent as %s, but its pull request #%d sits on Graphite's %s and this clone never submitted it — name its real parent with ccx vcs stack rebase --parent %s=<branch>", parent, pr.Number, pr.Base, branch), nil
		}
		return fmt.Sprintf("gt records its parent as %s, but its pull request #%d is based on %s — re-record it with gt track --parent %s %s", parent, pr.Number, pr.base(), pr.base(), branch), nil
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
	none, fromEmpty, err = stackCarriesNoneOf(ctx, dir, state, tr, branch, child, parent, gtRestackRef(parent))
	if err != nil || !none {
		return none, fromEmpty, err
	}
	pushed := "refs/remotes/" + tr.Remote() + "/" + parent
	if present, err := gitRefExists(ctx, dir, stackRebasePrefix, pushed); err != nil || !present {
		return none, fromEmpty, err
	}
	return stackCarriesNoneOf(ctx, dir, state, tr, branch, child, parent, pushed)
}

// stackCarriesNoneOf is stackCarriesNone against one head of parent: its
// local branch, or the head it was last pushed at, which a branch cut from
// it still carries after the parent's lane rewrites it locally.
func stackCarriesNoneOf(ctx context.Context, dir render.Dir, state gtState, tr vcs.Trunk, branch, child, parent, parentRef string) (none, fromEmpty bool, err error) {
	below := state[parent].Parents[0].Ref
	outside := []string{"^" + string(tr.Ref())}
	if below != tr.Name() {
		outside = append(outside, "^"+gtRestackRef(below))
	}
	parentOwn, err := gtRevCount(ctx, stackRebasePrefix, dir, parentRef, outside...)
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
	missing, err := gtRevCount(ctx, stackRebasePrefix, dir, parentRef+"..."+gtRestackRef(branch), append([]string{"--left-only", "--cherry-pick"}, outside...)...)
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
		disowned := pr != nil && pr.State == "OPEN" && pr.Base != "" && !slices.Contains(down[carrier+1:i+1], pr.base())
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

func stackAdoptUntracked(ctx context.Context, dir render.Dir, tr vcs.Trunk, state gtState, overrides map[string]string) (gtState, error) {
	adopted := gtState{}
	for _, child := range slices.Sorted(maps.Keys(overrides)) {
		if s, tracked := state[child]; s.Trunk || (tracked && len(s.Parents) > 0) {
			continue
		}
		head, err := gtRestackHead(ctx, stackRebasePrefix, dir, child)
		if err != nil {
			return nil, err
		}
		base, err := stackMergeBase(ctx, dir, head, string(tr.Ref()))
		if err != nil {
			return nil, err
		}
		adopted[child] = gtBranchState{Head: head, Parents: []gtRef{{Ref: overrides[child], SHA: base}}}
		state[child] = adopted[child]
	}
	return adopted, nil
}

func stackAdoptUntrackedParents(ctx context.Context, dir render.Dir, commonDir string, tr vcs.Trunk, state gtState, overrides map[string]string) (gtState, error) {
	adopted := gtState{}
	for _, child := range slices.Sorted(maps.Keys(overrides)) {
		parent := overrides[child]
		if _, tracked := state[parent]; tracked || parent == tr.Name() || overrides[parent] != "" {
			continue
		}
		present, err := gitRefExists(ctx, dir, stackRebasePrefix, gtRestackRef(parent))
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("stack rebase: --parent %s=%s names a branch with no local copy: git fetch %s refs/heads/%s:refs/heads/%s, then re-run", child, parent, tr.Remote(), parent, parent)
		}
		below, err := gtInferParent(ctx, &gtCache{dir: dir, prefix: stackRebasePrefix, commonDir: commonDir, state: state}, parent)
		if err != nil {
			return nil, err
		}
		head, err := gtRestackHead(ctx, stackRebasePrefix, dir, parent)
		if err != nil {
			return nil, err
		}
		floor := string(tr.Ref())
		if below != tr.Name() {
			floor = gtRestackRef(below)
		}
		base, err := stackMergeBase(ctx, dir, head, floor)
		if err != nil {
			return nil, err
		}
		adopted[parent] = gtBranchState{Head: head, Parents: []gtRef{{Ref: below, SHA: base}}}
		state[parent] = adopted[parent]
	}
	return adopted, nil
}

func stackTrackAdopted(ctx context.Context, commonDir string, run *stackRebaseRun) error {
	moves := map[string]string{}
	for _, child := range slices.Sorted(maps.Keys(run.adopted)) {
		s := run.adopted[child]
		if err := gtmeta.AdoptRoot(ctx, commonDir, child, run.Trunk, s.Parents[0].SHA, s.Head); err != nil {
			return fmt.Errorf("stack rebase: %w", err)
		}
		if parent := s.Parents[0].Ref; parent != run.Trunk {
			moves[child] = parent
		}
	}
	if err := gtmeta.Reparent(ctx, commonDir, moves); err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	return nil
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

func branchLane(branch string) string {
	return branch[:max(strings.LastIndexByte(branch, '/'), 0)]
}

// stackPinOtherLanes pins at its published head each branch of another lane,
// one whose name differs from the checked-out branch's before the last slash,
// that this lane sits on, and leaves out the rest of the other lanes.
func stackPinOtherLanes(state gtState, current string, members, include []string) (kept, pinned []string, left []stackLeft) {
	lane := branchLane(current)
	other := func(name string) bool { return branchLane(name) != lane && !slices.Contains(include, name) }
	carried := map[string]bool{}
	for _, name := range members {
		if other(name) {
			continue
		}
		for parent := state[name].Parents[0].Ref; other(parent) && slices.Contains(members, parent) && !carried[parent]; parent = state[parent].Parents[0].Ref {
			carried[parent] = true
		}
	}
	for _, name := range members {
		switch {
		case !other(name):
			kept = append(kept, name)
		case carried[name]:
			kept = append(kept, name)
			pinned = append(pinned, name)
		default:
			left = append(left, stackLeft{branch: name, why: "it is lane " + branchLane(name) + "'s, not " + lane + "'s; --all-lanes takes it"})
		}
	}
	return kept, pinned, left
}

// stackPinHeldParents pins a published parent stackWithPublishedParents
// brought back when a working copy of another lane holds it, so a submit keeps
// it at its published head like any held ancestor gt still records.
func stackPinHeldParents(ctx context.Context, l lane, planned, members, pinned []string) ([]string, error) {
	holders, err := vcs.ForeignBranchHolders(ctx, l.checkout)
	if err != nil {
		return nil, fmt.Errorf("stack submit: %w", err)
	}
	for _, name := range members {
		if holders[name] != "" && !slices.Contains(planned, name) && !slices.Contains(pinned, name) {
			pinned = append(pinned, name)
		}
	}
	return pinned, nil
}

// stackWithPublishedParents adds back the parent a member was last published
// onto when the run left it out, by --parent or because gt records another
// parent: gt state keeps the old parent until the source checkout moves, and
// the publication is what gets replayed.
func stackWithPublishedParents(ctx context.Context, dir render.Dir, state gtState, submitted map[string]gtmeta.Version, trunk string, members, roots []string, overrides map[string]string) ([]string, []string, error) {
	for {
		receipts := map[string]*stackPublication{}
		var outran []string
		for _, name := range members {
			if _, named := overrides[name]; named {
				continue
			}
			receipt, err := stackReadPublication(ctx, dir, name)
			if err != nil {
				return nil, nil, err
			}
			if receipt == nil || stackRetracked(receipt, state[name]) {
				continue
			}
			receipts[name] = receipt
			if last := submitted[name]; last.HeadSha != "" && last.HeadSha != receipt.Head {
				outran = append(outran, name)
			}
		}
		remotes := map[string]string{}
		if len(outran) > 0 {
			var err error
			if remotes, err = stackRemoteHeads(ctx, dir, stackRebasePrefix, "origin", outran, ""); err != nil {
				return nil, nil, err
			}
		}
		var missing []string
		for _, name := range slices.Sorted(maps.Keys(receipts)) {
			parent := stackPublishedParent(receipts[name], remotes[name], submitted[name])
			if parent == trunk || slices.Contains(members, parent) || slices.Contains(missing, parent) {
				continue
			}
			if _, tracked := state[parent]; tracked {
				missing = append(missing, parent)
			}
		}
		if len(missing) == 0 {
			return members, roots, nil
		}
		more, moreRoots, err := stackAncestry(state, trunk, missing)
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

// stackMembers is the lane of every seed: the branches below it down to trunk,
// then those stacked above it, parents first. A sibling lane cut from a shared
// ancestor stays out.
func stackMembers(state gtState, trunk string, seeds []string) ([]string, []string, error) {
	members, roots, err := stackAncestry(state, trunk, seeds)
	if err != nil {
		return nil, nil, err
	}
	for _, seed := range seeds {
		up, err := gtUpstack(stackRebasePrefix, state, seed)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range up {
			if !slices.Contains(members, name) {
				members = append(members, name)
			}
		}
	}
	return members, roots, nil
}

// stackUpTo is to's downstack, the branches a run stopped at to holds, refused
// unless every seed is to or sits below it.
func stackUpTo(prefix string, state gtState, trunk string, seeds []string, to string) ([]string, error) {
	if to == trunk {
		return nil, fmt.Errorf("%s: --to %s is trunk, not a stack branch", prefix, to)
	}
	if _, tracked := state[to]; !tracked {
		return nil, fmt.Errorf("%s: --to %s names a branch gt does not track", prefix, to)
	}
	down, err := gtDownstack(prefix, state, to, trunk)
	if err != nil {
		return nil, err
	}
	for _, seed := range seeds {
		if !slices.Contains(down, seed) {
			return nil, fmt.Errorf("%s: --to %s is neither %s nor stacked above it, so the run cannot stop there", prefix, to, seed)
		}
	}
	return down, nil
}

func stackAncestry(state gtState, trunk string, seeds []string) ([]string, []string, error) {
	var members, roots []string
	for _, seed := range seeds {
		if seed == trunk {
			return nil, nil, fmt.Errorf("stack rebase: %s is trunk, not a stack branch", seed)
		}
		down, err := gtDownstack(stackRebasePrefix, state, seed, trunk)
		if err != nil {
			return nil, nil, err
		}
		if bottom := down[len(down)-1]; !slices.Contains(roots, bottom) {
			roots = append(roots, bottom)
		}
		for _, name := range gtBottomUp(down) {
			if !slices.Contains(members, name) {
				members = append(members, name)
			}
		}
	}
	return members, roots, nil
}

// stackRetracked reports a receipt whose parent gt has since re-recorded: gt
// names another parent at another revision than the one the branch was
// published from. A --parent publication leaves gt's record as it was, so there
// the receipt still names the parent.
func stackRetracked(receipt *stackPublication, s gtBranchState) bool {
	return receipt != nil && receipt.Parent != s.Parents[0].Ref && receipt.SourceBase != s.Parents[0].SHA
}

// stackPublishedParent names the parent a branch's remote head was published
// onto: Graphite's record when the remote is the version Graphite last submitted
// and the receipt names an older one, the receipt's otherwise.
func stackPublishedParent(receipt *stackPublication, remote string, submitted gtmeta.Version) string {
	if remote != "" && remote != receipt.Head && remote == submitted.HeadSha {
		return submitted.BaseName
	}
	return receipt.Parent
}

func stackRemoteHeads(ctx context.Context, dir render.Dir, prefix, remote string, branches []string, negotiationTip string) (map[string]string, error) {
	heads, err := stackListRemoteHeads(ctx, dir, prefix, remote, branches)
	if err != nil {
		return nil, err
	}
	err = stackFetchRemoteHeads(ctx, dir, prefix, remote, heads, negotiationTip)
	if err != nil && strings.Contains(err.Error(), "couldn't find remote ref") {
		if heads, err = stackListRemoteHeads(ctx, dir, prefix, remote, branches); err != nil {
			return nil, err
		}
		err = stackFetchRemoteHeads(ctx, dir, prefix, remote, heads, negotiationTip)
	}
	if err != nil {
		return nil, err
	}
	return heads, nil
}

func stackListRemoteHeads(ctx context.Context, dir render.Dir, prefix, remote string, branches []string) (map[string]string, error) {
	argv := make([]string, 0, 2+len(branches))
	argv = append(argv, "ls-remote", remote)
	wanted := make(map[string]string, len(branches))
	for _, b := range branches {
		wanted[gtRestackRef(b)] = b
		argv = append(argv, gtRestackRef(b))
	}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return nil, fmt.Errorf("%s: git ls-remote %s: %w", prefix, remote, err)
	}
	heads := map[string]string{}
	for line := range strings.Lines(out) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if name, want := wanted[ref]; ok && want {
			heads[name] = sha
		}
	}
	return heads, nil
}

func stackFetchRemoteHeads(ctx context.Context, dir render.Dir, prefix, remote string, heads map[string]string, negotiationTip string) error {
	if len(heads) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(heads))
	refs := make([]string, 0, len(names))
	for _, name := range names {
		refs = append(refs, "refs/remotes/"+remote+"/"+name)
	}
	localOut, err := render.RunCLIStdin(ctx, dir, "git", []string{"for-each-ref", "--format=%(refname) %(objectname)", "--stdin"}, []byte(strings.Join(refs, "\n")+"\n"))
	if err != nil {
		return fmt.Errorf("%s: git for-each-ref: %w", prefix, err)
	}
	local := map[string]string{}
	for line := range strings.Lines(localOut) {
		ref, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			local[ref] = sha
		}
	}
	fetch := []string{"--quiet", "--no-tags", "--no-write-fetch-head"}
	if negotiationTip != "" {
		fetch = append(fetch, "--negotiation-tip="+negotiationTip)
	}
	fetch = append(fetch, remote)
	baseLen := len(fetch)
	for _, name := range names {
		ref := "refs/remotes/" + remote + "/" + name
		if local[ref] != heads[name] {
			fetch = append(fetch, "+refs/heads/"+name+":"+ref)
		}
	}
	if len(fetch) > baseLen {
		if err := gitFetch(ctx, dir, fetch...); err != nil {
			return fmt.Errorf("%s: git fetch %s: %w", prefix, remote, err)
		}
	}
	return nil
}

// stackSnapshot takes a remote that equals the branch's last submitted head
// as ours even when local no longer contains it: every rewrite a rebase or
// restack makes diverges from the head it last pushed. A branch the run reads
// at its published head is taken at that head, its local one never compared.
func stackSnapshot(ctx context.Context, dir render.Dir, tr vcs.Trunk, s gtBranchState, name, remote, submitted string, pr *stackPR, declared bool, pin string, dropCommits, published bool) (stackRebaseBranch, error) {
	b := stackRebaseBranch{
		Name: name, WasParent: s.Parents[0].Ref, Local: s.Head, Remote: remote,
		Head: s.Head, PR: pr, Held: s.State,
	}
	if published && remote != "" {
		b.Head = remote
	} else if b.Held == "" && remote != "" && remote != s.Head && (pr == nil || !pr.abandoned()) {
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
			b.Head = remote
		case !ahead && remote != submitted:
			if s.Head == submitted {
				restacked, err := stackQueueRestackBase(ctx, dir, pin, remote, submitted)
				if err != nil {
					return b, err
				}
				if restacked != "" {
					b.Head = remote
					break
				}
			}
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
			if !replay {
				if replay, err = gitOnlyCopies(ctx, dir, stackRebasePrefix, remote, s.Head, pin); err != nil {
					return b, err
				}
			}
			if replay {
				break
			}
			return b, stackDivergedError{fmt.Errorf("stack rebase: %s has diverged from %s/%s (local %.12s, remote %.12s) — someone pushed to it; reconcile the two by hand, then re-run", name, tr.Remote(), name, s.Head, remote)}
		}
		if !ahead && !behind && !dropCommits {
			if err := stackRefuseDroppedCommits(ctx, dir, tr, name, s.Head, remote, pin); err != nil {
				return b, stackDivergedError{err}
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
				past, err := gtRevCount(ctx, stackRebasePrefix, dir, pr.Head+"..."+b.Head, "--right-only", "--cherry-pick", "--no-merges")
				if err != nil {
					return b, err
				}
				within = past == 0
			}
			if !within {
				return b, fmt.Errorf("stack rebase: %s's pull request #%d landed at %.12s, but the branch holds commits past it (%.12s) — move them to a new branch, or pass --landed %s to drop them too", name, pr.Number, pr.Head, b.Head, name)
			}
		}
		b.Landed = fmt.Sprintf("#%d landed", pr.Number)
	case contained:
		b.Landed = "already in " + tr.Name()
	case pr != nil && pr.abandoned():
		b.Landed = pr.closedLanding()
	}
	return b, nil
}

// reopens reports a branch whose pull request GitHub closed when its base was
// deleted: the run reopens it onto the branch's new parent before pushing.
func (b *stackRebaseBranch) reopens() bool {
	return b.Landed == "" && b.Held == "" && b.PR != nil && b.PR.State == "CLOSED" && b.PR.BaseGone
}

// stackRefuseDroppedCommits refuses a local head that only loses work its
// published head carries: one sharing none of its commits, or one holding a
// strict subset of them, matched by subject so a rebase, an amend, or a rewrite
// of a commit still passes. A hard reset onto the wrong commit would otherwise
// force-push over the pull request's work. A published commit trunk already
// landed as a squash, as the merge queue's replay of a landed parent leaves
// one, is no work to lose.
func stackRefuseDroppedCommits(ctx context.Context, dir render.Dir, tr vcs.Trunk, name, local, remote, pin string) error {
	commits := func(head string) ([]string, []string, error) {
		out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--no-merges", "--format=%H %s", head, "^" + pin})
		if err != nil {
			return nil, nil, fmt.Errorf("%s: git log %.12s: %w", stackRebasePrefix, head, err)
		}
		var shas, subjects []string
		for line := range strings.Lines(out) {
			sha, subject, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
			if subject == "" {
				continue
			}
			shas = append(shas, sha)
			subjects = append(subjects, subject)
		}
		return shas, subjects, nil
	}
	_, kept, err := commits(local)
	if err != nil {
		return err
	}
	shas, published, err := commits(remote)
	if err != nil {
		return err
	}
	var missing []string
	for i, subject := range published {
		if !slices.Contains(kept, subject) {
			missing = append(missing, shas[i])
		}
	}
	shared := len(published) - len(missing)
	var landed []string
	if len(missing) > 0 {
		if landed, err = gitLandedReplays(ctx, dir, stackRebasePrefix, pin, missing); err != nil {
			return err
		}
	}
	var dropped []string
	for i, sha := range shas {
		if slices.Contains(missing, sha) && !slices.Contains(landed, sha) {
			dropped = append(dropped, fmt.Sprintf("%q", published[i]))
		}
	}
	added := slices.ContainsFunc(kept, func(subject string) bool { return !slices.Contains(published, subject) })
	if len(dropped) == 0 || (shared > 0 && added) {
		return nil
	}
	return fmt.Errorf("stack rebase: %s's local head %.12s drops %d commit(s) its published head %s/%s (%.12s) carries: %s — restore them, or pass --drop-commits to publish the local head anyway",
		name, local, len(dropped), tr.Remote(), name, remote, strings.Join(dropped, ", "))
}

type stackDivergedError struct{ error }

func stackTipOnlyHint(o stackRebaseOpts, name string, err error) error {
	var diverged stackDivergedError
	if o.tip == "" || o.tipOnly || name == o.tip || !errors.As(err, &diverged) {
		return err
	}
	return fmt.Errorf("%w; to ship %s alone onto %s's published head, run ccx vcs ship --tip-only", err, o.tip, name)
}

// stackReadsPublished is whether the run takes name at its published head
// without reading its local one: every ancestor of a --tip-only ship, and every
// branch pinned as another lane's.
func stackReadsPublished(o stackRebaseOpts, name string) bool {
	return tipOnlyAncestor(o.tipOnly, o.tip, name) || slices.Contains(o.pinned, name)
}

// tipOnlyAncestor is a branch a --tip-only ship of tip neither moves nor
// pushes, the one rule its dry run and its run both read.
func tipOnlyAncestor(tipOnly bool, tip, name string) bool {
	return tipOnly && name != tip
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
			return false, fmt.Errorf("stack rebase: --tip-only ships onto %s's published head, and the remote has none — push it first, or pass --landed %s if it just landed", b.Name, b.Name)
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
	b.Head = b.Remote
	return true, nil
}

// stackPinPublished keeps a branch the run must not push at the head its
// pull request already shows; one never pushed has nothing to keep.
// stackRefuseUnpushedPins refuses a run kept at the published heads of branches
// another lane holds and never pushed: there is nothing to stack on.
func stackRefuseUnpushedPins(names []string) error {
	flags := make([]string, len(names))
	for i, name := range names {
		flags[i] = "--include " + name
	}
	if len(names) == 1 {
		return fmt.Errorf("stack submit: %s is another lane's and has never been pushed, so there is no published head to stack on — submit it from its own working copy first, or take it into this run with %s or --all-lanes", names[0], flags[0])
	}
	return fmt.Errorf("stack submit: %s are other lanes' and have never been pushed, so there are no published heads to stack on — submit each from its own working copy first, or take them into this run with %s or --all-lanes", strings.Join(names, ", "), strings.Join(flags, " "))
}

func stackPinPublished(b *stackRebaseBranch) bool {
	if b.Remote == "" {
		return false
	}
	b.Head = b.Remote
	return true
}

// stackMovingBranches is what a --parent or --linearize run may move: the
// branches it names a parent for and everything stacked on them. The parents
// it names, and the branches below them, stay where they are: at their
// published heads when the run pushes, at their local heads when it does not,
// since rewriting another lane's branch is not what the run asked for. nil
// lets every branch move.
func stackMovingBranches(state gtState, o stackRebaseOpts, overrides map[string]string) (map[string]bool, error) {
	if o.tip != "" || len(overrides) == 0 {
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

// stackQueueStates reads where every branch with an open pull request a
// pushing run could move stands against Graphite's merge queue: a push to a
// queued pull request evicts it. A queued mergeability status settles it, as
// in failure handling, when Graphite drops the flag. Graphite keeps a pull
// request flagged after the queue lets it go, so each one flagged or carrying a
// queue label is settled by its merge activity, read the way ccx vcs pr status
// reads it.
func stackQueueStates(ctx context.Context, l lane, noPush bool, byName map[string]*stackRebaseBranch) (map[string]prQueueState, error) {
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
	infos, err := gtAPI(ctx).PullRequestInfo(ctx, gtapi.PullRequestInfoRequest{RepoOwner: owner, RepoName: name, PRHeadRefNames: heads, Consistent: true, Callsite: "ccx"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read the merge queue before pushing: %w", err)
	}
	infos = slices.DeleteFunc(infos, func(info gtapi.PullRequestInfo) bool {
		return info.State != gtapi.PROpen || !slices.Contains(heads, info.HeadRefName)
	})
	numbers := make([]int, 0, len(infos))
	for _, info := range infos {
		numbers = append(numbers, info.PRNumber)
	}
	mergeability, err := gtAPI(ctx).MergeabilityStatuses(ctx, owner, name, numbers)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: read the merge queue before pushing: %w", err)
	}
	var watched []int
	for _, info := range infos {
		if gtapi.InMergeQueue(&info, mergeability[info.PRNumber]) || (prstate.PR{Labels: byName[info.HeadRefName].PR.Labels}).QueueLabelled() {
			watched = append(watched, info.PRNumber)
		}
	}
	var st prstate.State
	if len(watched) > 0 {
		if st, _, err = readPRQueue(ctx, owner+"/"+name, watched, io.Discard, ghRateLimitWait); err != nil {
			return nil, fmt.Errorf("stack rebase: read the merge activity before pushing: %w", err)
		}
	}
	states := map[string]prQueueState{}
	for _, info := range infos {
		pr := st.PRs[info.PRNumber]
		pr.Mergeability = mergeability[info.PRNumber]
		states[info.HeadRefName] = prQueueOf(info, pr, "").Queue
	}
	return states, nil
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

// stackQueueRestackBase returns the trunk commit remote sits on when remote is
// ours restacked onto another trunk commit with every commit keeping its
// author, author date and message, as the merge queue restacks a pull request
// even where resolving a conflict changed a patch; it returns "" otherwise.
func stackQueueRestackBase(ctx context.Context, dir render.Dir, pin, remote, ours string) (string, error) {
	commits := func(head string) (string, int, error) {
		span := pin + ".." + head
		merges, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--merges", span})
		if err != nil || strings.TrimSpace(merges) != "" {
			return "", 0, err
		}
		out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--reverse", "-z", "--format=%an <%ae> %at%n%B", span})
		if err != nil {
			return "", 0, fmt.Errorf("%s: git log %s: %w", stackRebasePrefix, span, err)
		}
		return out, strings.Count(out, "\x00"), nil
	}
	theirs, n, err := commits(remote)
	if err != nil {
		return "", err
	}
	mine, _, err := commits(ours)
	if err != nil || mine == "" || theirs != mine {
		return "", err
	}
	base, err := stackRevParse(ctx, dir, fmt.Sprintf("%s~%d", remote, n))
	if err != nil {
		return "", err
	}
	was, err := stackRevParse(ctx, dir, fmt.Sprintf("%s~%d", ours, n))
	if err != nil || base == was {
		return "", err
	}
	onTrunk, err := gitIsAncestor(ctx, dir, stackRebasePrefix, base, pin)
	if err != nil || !onTrunk {
		return "", err
	}
	return base, nil
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
	commits, err := stackPatches(ctx, dir, span)
	if err != nil {
		return nil, err
	}
	series := []string{}
	for _, c := range commits {
		if c.patch == "" {
			return nil, nil
		}
		series = append(series, c.patch+"\n"+c.authored)
	}
	return series, nil
}

type stackPatch struct{ sha, patch, authored string }

func stackPatches(ctx context.Context, dir render.Dir, revs ...string) ([]stackPatch, error) {
	span := strings.Join(revs, " ")
	patches, err := render.RunCLI(ctx, dir, "git", append([]string{"log", "--no-merges", "--format=commit %H", "-p", "-U0"}, revs...))
	if err != nil {
		return nil, fmt.Errorf("%s: git log -p %s: %w", stackRebasePrefix, span, err)
	}
	ids, err := render.RunCLIStdin(ctx, dir, "git", []string{"patch-id", "--stable"}, []byte(patches))
	if err != nil {
		return nil, fmt.Errorf("%s: git patch-id %s: %w", stackRebasePrefix, span, err)
	}
	patchOf := map[string]string{}
	for line := range strings.Lines(ids) {
		id, sha, _ := strings.Cut(strings.TrimSpace(line), " ")
		patchOf[sha] = id
	}
	authored, err := render.RunCLI(ctx, dir, "git", append([]string{"log", "--topo-order", "--reverse", "--no-merges", "-z", "--format=%H%n%an <%ae> %at%n%B"}, revs...))
	if err != nil {
		return nil, fmt.Errorf("%s: git log %s: %w", stackRebasePrefix, span, err)
	}
	var commits []stackPatch
	for entry := range strings.SplitSeq(authored, "\x00") {
		if sha, message, _ := strings.Cut(entry, "\n"); sha != "" {
			commits = append(commits, stackPatch{sha: sha, patch: patchOf[sha], authored: message})
		}
	}
	return commits, nil
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

// stackOldBase is where a branch's own work starts: the furthest recorded parent
// head it contains, else the furthest fork point in the parent's local and
// remote-tracking reflogs. A branch staying on the parent is refused past that;
// one leaving it takes its merge base, or trunk's when trunk holds the rest.
// Either way it starts past the furthest head of a branch below it that it
// holds. Unless that is its parent's, it also starts past each leading commit
// copying one of theirs by patch or by author, date and message, as a branch
// cut from a rewritten parent carries.
func stackOldBase(ctx context.Context, dir render.Dir, tr vcs.Trunk, pin string, state gtState, self *stackRebaseBranch, byName map[string]*stackRebaseBranch) (string, error) {
	s := state[self.Name]
	onTrunk, err := stackMergeBase(ctx, dir, self.Head, pin)
	if err != nil {
		return "", err
	}
	parent := s.Parents[0].Ref
	if parent == tr.Name() {
		fork, err := stackPastFork(ctx, dir, s.Parents[0].SHA, onTrunk, self.Head)
		if err != nil {
			return "", err
		}
		return stackPastChain(ctx, dir, pin, self, byName, fork)
	}
	candidates := []string{state[parent].Head, s.Parents[0].SHA}
	if head := byName[parent]; head != nil {
		candidates = []string{head.Head, head.Local, s.Parents[0].SHA}
	} else {
		receipt, err := stackReadPublication(ctx, dir, parent)
		if err != nil {
			return "", err
		}
		if receipt != nil {
			candidates = append([]string{receipt.Head}, candidates...)
		}
	}
	best, err := stackFurthest(ctx, dir, self.Head, candidates)
	if err != nil {
		return "", err
	}
	if best == "" {
		pushed := "refs/remotes/" + tr.Remote() + "/" + parent
		published, err := gitRefExists(ctx, dir, stackRebasePrefix, pushed)
		if err != nil {
			return "", err
		}
		refs := []string{gtRestackRef(parent)}
		if published {
			refs = append(refs, pushed)
		}
		var forks []string
		for _, ref := range refs {
			fork, err := stackForkPoint(ctx, dir, ref, self.Head)
			if err != nil {
				return "", err
			}
			forks = append(forks, fork)
		}
		if best, err = stackFurthest(ctx, dir, self.Head, forks); err != nil {
			return "", err
		}
		if best == "" && self.Parent == parent {
			return "", stackNoOldBase(ctx, dir, tr, state, self.Name, published)
		}
		if best == "" {
			if best, err = stackMergeBase(ctx, dir, self.Head, candidates[0]); err != nil {
				return "", err
			}
		}
	}
	if self.Parent != parent {
		behind, err := gitIsAncestor(ctx, dir, stackRebasePrefix, best, onTrunk)
		if err != nil {
			return "", err
		}
		if behind {
			best = onTrunk
		}
	}
	return stackPastChain(ctx, dir, pin, self, byName, best)
}

func stackPastChain(ctx context.Context, dir render.Dir, pin string, self *stackRebaseBranch, byName map[string]*stackRebaseBranch, base string) (string, error) {
	var heads, parentHeads []string
	for name := self.Parent; byName[name] != nil; name = byName[name].Parent {
		below := byName[name]
		held := []string{below.Head, below.Local, below.Remote}
		if below.Publication != nil {
			held = append(held, below.Publication.Head)
		}
		if name == self.Parent {
			parentHeads = held
		}
		heads = append(heads, held...)
	}
	heads = slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(heads))), func(head string) bool { return head == "" })
	if len(heads) == 0 {
		return base, nil
	}
	furthest, err := stackFurthest(ctx, dir, self.Head, append([]string{base}, heads...))
	if err != nil || slices.Contains(parentHeads, furthest) {
		return furthest, err
	}
	if furthest == base {
		inTrunk, err := gitIsAncestor(ctx, dir, stackRebasePrefix, base, pin)
		if err != nil || !inTrunk {
			return base, err
		}
	}
	base = furthest
	span := base + ".." + self.Head
	merges, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", "--merges", span})
	if err != nil {
		return "", fmt.Errorf("%s: git rev-list --merges %s: %w", stackRebasePrefix, span, err)
	}
	if strings.TrimSpace(merges) != "" {
		return base, nil
	}
	ours, err := stackPatches(ctx, dir, span)
	if err != nil || len(ours) == 0 {
		return base, err
	}
	theirs, err := stackPatches(ctx, dir, append(heads, "^"+pin)...)
	if err != nil {
		return "", err
	}
	patches, authored := map[string]bool{}, map[string]bool{}
	for _, c := range theirs {
		patches[c.patch], authored[c.authored] = true, true
	}
	delete(patches, "")
	for _, c := range ours {
		if !patches[c.patch] && !authored[c.authored] {
			break
		}
		base = c.sha
	}
	return base, nil
}

// stackFurthest is the candidate head contains that every other contained
// candidate is an ancestor of, or empty when head contains none of them.
func stackFurthest(ctx context.Context, dir render.Dir, head string, candidates []string) (string, error) {
	best := ""
	for _, candidate := range candidates {
		if candidate == "" || candidate == best {
			continue
		}
		ok, err := gitIsAncestor(ctx, dir, stackRebasePrefix, candidate, head)
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
	return best, nil
}

func stackNoOldBase(ctx context.Context, dir render.Dir, tr vcs.Trunk, state gtState, name string, published bool) error {
	parent := state[name].Parents[0].Ref
	pushed := "never pushed"
	if published {
		head, err := gitRevParse(ctx, dir, stackRebasePrefix, "refs/remotes/"+tr.Remote()+"/"+parent)
		if err != nil {
			return err
		}
		pushed = "at " + shortSHA(head) + " on " + tr.Remote()
	}
	return fmt.Errorf("stack rebase: %s carries no head of %s that gt or a reflog remembers — gt records it on %s, and %s is at %s here and %s — so its own commits cannot be told from %s's; move them onto %s with git rebase --onto %s <%s's last commit in %s> %s, then gt track --parent %s %s",
		name, parent, shortSHA(state[name].Parents[0].SHA), parent, shortSHA(state[parent].Head), pushed, parent, parent, parent, parent, name, name, parent, name)
}

// stackBaseInTrunk reports whether base..head replays commits trunk already
// holds: a receipt recorded after the branch took newer trunk names a base
// below that trunk, and its own commits start at the merge base instead.
func stackBaseInTrunk(ctx context.Context, dir render.Dir, base, head, pin string) (bool, error) {
	span := base + ".." + head
	replayed, err := gtRevCount(ctx, stackRebasePrefix, dir, span)
	if err != nil {
		return false, err
	}
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, span, "--not", pin)
	if err != nil {
		return false, err
	}
	return own != replayed, nil
}

// stackPastFork is where a trunk-parented branch's own commits start: its
// recorded parent revision when that lies past the trunk fork point, as a
// squash-landed parent's head does, and the fork point otherwise.
func stackPastFork(ctx context.Context, dir render.Dir, recorded, fork, head string) (string, error) {
	if recorded == "" || recorded == fork {
		return fork, nil
	}
	past, err := gitIsAncestor(ctx, dir, stackRebasePrefix, fork, recorded)
	if err != nil || !past {
		return fork, err
	}
	held, err := gitIsAncestor(ctx, dir, stackRebasePrefix, recorded, head)
	if err != nil || !held {
		return fork, err
	}
	return recorded, nil
}

func stackMergesClean(ctx context.Context, dir render.Dir, pin string, branches []*stackRebaseBranch) (bool, error) {
	clean := true
	for _, b := range branches {
		merges, conflicts, err := stackTrunkConflicts(ctx, dir, pin, b.Head)
		if err != nil {
			return false, err
		}
		b.Conflicts = conflicts
		clean = clean && merges
	}
	return clean, nil
}

// Merge attributes come from pin's tree: a working copy's .gitattributes can
// predate trunk's merge=binary rules and pass a conflicting text merge.
// Never --quiet: git 2.56 exits 0 under it when a conflicted path sorts before
// a clean content merge in a sibling directory.
func stackTrunkConflicts(ctx context.Context, dir render.Dir, pin, head string) (bool, []string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"--attr-source=" + pin, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", pin, head})
	if err != nil {
		return false, nil, fmt.Errorf("stack rebase: git merge-tree %.12s %.12s: %w", pin, head, err)
	}
	switch code {
	case 0:
		return true, nil, nil
	case 1:
		return false, strings.Split(strings.TrimRight(out, "\x00"), "\x00")[1:], nil
	default:
		return false, nil, fmt.Errorf("stack rebase: git merge-tree %.12s %.12s: exit %d: %s", pin, head, code, strings.TrimSpace(stderr))
	}
}

func stackUpstack(order []string, byName map[string]*stackRebaseBranch, name string) []*stackRebaseBranch {
	above := map[string]bool{name: true}
	branches := []*stackRebaseBranch{byName[name]}
	for _, n := range order {
		b := byName[n]
		if above[b.Parent] && b.Landed == "" && b.Held == "" {
			above[n] = true
			branches = append(branches, b)
		}
	}
	return branches
}

func stackMergeBase(ctx context.Context, dir render.Dir, a, b string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", a, b})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git merge-base %.12s %.12s: %w", a, b, err)
	}
	return strings.TrimSpace(out), nil
}

func stackForkPoint(ctx context.Context, dir render.Dir, ref, head string) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"merge-base", "--fork-point", ref, head})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git merge-base --fork-point %s %.12s: %w", ref, head, err)
	}
	switch code {
	case 0:
		return strings.TrimSpace(out), nil
	case 1:
		return "", nil
	default:
		return "", fmt.Errorf("stack rebase: git merge-base --fork-point %s %.12s: exit %d: %s", ref, head, code, strings.TrimSpace(stderr))
	}
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

func stackInheritsTrunk(ctx context.Context, dir render.Dir, trunk, pin string, b *stackRebaseBranch, inherits map[string]bool) (bool, error) {
	if b.Parent != trunk {
		return inherits[b.Parent], nil
	}
	for _, head := range slices.Compact([]string{b.Head, b.Remote}) {
		if head == "" {
			continue
		}
		base, err := stackMergeBase(ctx, dir, head, pin)
		if err != nil {
			return false, err
		}
		copies, err := gtCherryCopies(ctx, stackRebasePrefix, dir, pin, head, base)
		if err != nil || len(copies) > 0 {
			return len(copies) > 0, err
		}
	}
	return false, nil
}

func stackPlanLines(run *stackRebaseRun) []string {
	lines := make([]string, 0, 1+len(run.Branches))
	lines = append(lines, fmt.Sprintf("plan · trunk %s@%.12s", run.Trunk, run.Pin))
	var stayed []string
	for _, b := range run.Branches {
		fields := []string{b.Name}
		switch {
		case b.Landed != "":
			fields = append(fields, "drop ("+b.Landed+")")
		case b.Held != "":
			fields = append(fields, "left alone ("+b.Held+")")
		case b.Kept && run.NoPush:
			fields = append(fields, fmt.Sprintf("kept at its local head %.12s", b.Head))
		case b.Kept:
			fields = append(fields, fmt.Sprintf("kept at its published head %.12s", b.Head))
		case b.Stays:
			stayed = append(stayed, b.Name)
			fields = append(fields, fmt.Sprintf("stays on %.12s", b.OldBase), fmt.Sprintf("merges cleanly onto %s@%.12s", run.Trunk, run.Pin))
		default:
			parent := "onto " + b.Parent
			if b.Parent != b.WasParent {
				parent += " (was " + b.WasParent + ")"
			}
			fields = append(fields, parent, fmt.Sprintf("from %.12s", b.OldBase))
			if len(b.Conflicts) > 0 {
				fields = append(fields, fmt.Sprintf("conflicts with %s@%.12s in %s", run.Trunk, run.Pin, strings.Join(b.Conflicts, ", ")))
			}
			if b.Head != b.Local {
				fields = append(fields, fmt.Sprintf("taking the remote head %.12s", b.Head))
			}
			if b.reopens() && !run.NoPush {
				fields = append(fields, "reopen (base "+b.PR.Base+" deleted)")
			}
		}
		if b.PR != nil {
			fields = append(fields, b.PR.String())
		}
		lines = append(lines, strings.Join(fields, shipSep))
	}
	if len(stayed) > 0 {
		lines = append(lines, fmt.Sprintf("old base kept%s%s%slacks %s@%.12s%s--restack replays the stack onto it", shipSep, strings.Join(stayed, ", "), shipSep, run.Trunk, run.Pin, shipSep))
	}
	if !run.NoPush {
		var pushes []string
		for _, b := range run.Branches {
			if b.Landed == "" && b.Held == "" && !b.Kept && !b.LocalOnly && (!b.Stays || b.Head != b.Remote) {
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
		if b.Kept || b.Stays {
			b.NewBase, b.NewHead = b.OldBase, b.Head
			continue
		}
		b.NewBase = run.headOf(b.Parent)
		head := b.Head
		if b.NewBase != b.OldBase {
			var err error
			head, err = stackReplay(ctx, l.dir(), run, b)
			if errors.Is(err, errReplayConflict) {
				return stackOpenConflict(ctx, cmd, l, commonDir, run, b)
			}
			if err != nil {
				return err
			}
		}
		if b.Bump && head == b.Head {
			var err error
			if head, err = stackBumpHead(ctx, l.dir(), run, b); err != nil {
				return err
			}
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

func stackReplay(ctx context.Context, dir render.Dir, run *stackRebaseRun, b *stackRebaseBranch) (string, error) {
	pin := stackPublicationPin(run, b.Name)
	was, err := stackPinned(ctx, dir, pin, len(b.Head))
	if err != nil {
		return "", err
	}
	span := b.OldBase + ".." + b.Head
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"--attr-source=" + b.NewBase, "replay", "--ref-action=print", "--linearize", "--ref=" + pin, "--onto=" + b.NewBase, span})
	if err != nil {
		return "", fmt.Errorf("stack rebase: git replay: %w", err)
	}
	switch code {
	case 0:
	case 1:
		return "", errReplayConflict
	default:
		return "", fmt.Errorf("stack rebase: git replay --onto=%.12s %.12s..%.12s exited %d: %s", b.NewBase, b.OldBase, b.Head, code, strings.TrimSpace(stderr))
	}
	head, ok := replayUpdate(out, pin, was)
	if !ok {
		return "", fmt.Errorf("stack rebase: git replay of %s printed %q, want exactly one update of %s from %s", b.Name, strings.TrimSpace(out), pin, was)
	}
	return head, stackPinHead(ctx, dir, pin, head, was)
}

func stackPinned(ctx context.Context, dir render.Dir, pin string, oidLen int) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"for-each-ref", "--format=%(objectname)", pin})
	if err != nil {
		return "", fmt.Errorf("stack rebase: read %s: %w", pin, err)
	}
	return cmp.Or(strings.TrimSpace(out), strings.Repeat("0", oidLen)), nil
}

func stackPinHead(ctx context.Context, dir render.Dir, pin, head, was string) error {
	if _, err := render.RunCLI(ctx, dir, "git", []string{"update-ref", pin, head, was}); err != nil {
		return fmt.Errorf("stack rebase: pin the rebased head %.12s under %s: %w", head, pin, err)
	}
	return nil
}

func stackPinResolved(ctx context.Context, dir render.Dir, run *stackRebaseRun, b *stackRebaseBranch, head string) error {
	pin := stackPublicationPin(run, b.Name)
	was, err := stackPinned(ctx, dir, pin, len(head))
	if err != nil {
		return err
	}
	if err := stackPinHead(ctx, dir, pin, head, was); err != nil {
		return err
	}
	b.NewHead, b.Resolved = head, true
	return stackSaveRun(run)
}

func stackRequireGit(ctx context.Context, dir render.Dir, prefix string) error {
	out, err := render.RunCLI(ctx, dir, "git", []string{"version"})
	if err != nil {
		return fmt.Errorf("%s: git version: %w", prefix, err)
	}
	version, _ := strings.CutPrefix(strings.TrimSpace(out), "git version ")
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("%s: read the git version from %q", prefix, strings.TrimSpace(out))
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if err := errors.Join(errMajor, errMinor); err != nil {
		return fmt.Errorf("%s: read the git version from %q: %w", prefix, strings.TrimSpace(out), err)
	}
	if major < stackReplayMajor || (major == stackReplayMajor && minor < stackReplayMinor) {
		return fmt.Errorf("%s: needs git %d.%d or newer for git replay --ref and --linearize, and found git %s — upgrade git, then re-run", prefix, stackReplayMajor, stackReplayMinor, strings.Fields(version)[0])
	}
	return nil
}

func stackFreeWorkspace(ctx context.Context, l lane, name string) (string, error) {
	worktrees, err := vcs.Worktrees(ctx, l.checkout)
	if err != nil {
		return "", fmt.Errorf("stack rebase: %w", err)
	}
	for n := 1; ; n++ {
		candidate := name
		if n > 1 {
			candidate = name + "-" + strconv.Itoa(n)
		}
		ws, err := mintWorktreePath(ctx, stackRebasePrefix, l.checkout, candidate)
		if err != nil {
			return "", err
		}
		_, err = os.Lstat(ws)
		switch {
		case err == nil, slices.ContainsFunc(worktrees, func(w vcs.Worktree) bool { return w.Path == ws }):
		case errors.Is(err, fs.ErrNotExist):
			return ws, nil
		default:
			return "", fmt.Errorf("stack rebase: %w", err)
		}
	}
}

func stackOpenConflict(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, b *stackRebaseBranch) error {
	ws, err := stackFreeWorkspace(ctx, l, "conflict-"+strings.ReplaceAll(b.Name, "/", "-"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ws), 0o750); err != nil {
		return fmt.Errorf("stack rebase: mint the conflict workspace: %w", err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", "--no-checkout", ws, b.Head}); err != nil {
		return fmt.Errorf("stack rebase: git worktree add %s: %w", ws, err)
	}
	registration, err := cleanupObserveWorkspace(ws)
	if err != nil {
		return fmt.Errorf("stack rebase: %w", err)
	}
	run.Conflict = &stackConflict{Branch: b.Name, Workspace: ws, Registration: registration, Brief: stackBriefPath(run.dir, b.Name)}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	for _, argv := range [][]string{
		{"-c", "core.hooksPath=/dev/null", "sparse-checkout", "set", "--cone", "--no-sparse-index"},
		{"config", "--worktree", "core.hooksPath", "/dev/null"},
	} {
		if _, err := render.RunCLI(ctx, render.Dir(ws), "git", argv); err != nil {
			return fmt.Errorf("stack rebase: prepare %s: %w", ws, err)
		}
	}
	if err := stackPopulateSparse(ctx, render.Dir(ws), stackRebasePrefix, b.Head); err != nil {
		return err
	}
	argv := slices.Concat(stackGitRebaseArgs, []string{"rebase"}, stackReplayRebaseArgs, []string{"--onto", b.NewBase, b.OldBase})
	code, stderr, err := stackRunRebase(ctx, render.Dir(ws), argv, true)
	if err != nil {
		return fmt.Errorf("stack rebase: git rebase in %s: %w", ws, err)
	}
	if code != 0 {
		unmerged, err := stackUnmerged(ctx, ws)
		if err != nil {
			return err
		}
		if len(unmerged) == 0 {
			return fmt.Errorf("stack rebase: git rebase in %s stopped without a conflict: %s", ws, strings.TrimSpace(stderr))
		}
		return stackStopped(ctx, run, b, unmerged)
	}
	head, err := stackRevParse(ctx, render.Dir(ws), "HEAD")
	if err != nil {
		return err
	}
	if err := stackPinResolved(ctx, l.dir(), run, b, head); err != nil {
		return err
	}
	return stackCloseWorkspace(ctx, cmd, l, commonDir, run)
}

func stackAdvance(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch) error {
	ws := render.Dir(run.Conflict.Workspace)
	for stackRebasing(ctx, ws) {
		argv := append(slices.Clone(stackGitRebaseArgs), "rebase", "--continue")
		code, stderr, err := stackRunRebase(ctx, ws, argv, true)
		if err != nil {
			return fmt.Errorf("stack rebase: git rebase --continue in %s: %w", ws, err)
		}
		if code == 0 {
			continue
		}
		unmerged, err := stackUnmerged(ctx, run.Conflict.Workspace)
		if err != nil {
			return err
		}
		if len(unmerged) == 0 {
			return fmt.Errorf("stack rebase: git rebase --continue in %s failed: %s", ws, strings.TrimSpace(stderr))
		}
		return stackStopped(ctx, run, b, unmerged)
	}
	return nil
}

func stackStopped(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch, unmerged []string) error {
	sparse, err := stackWidenCone(ctx, run.Conflict.Workspace, unmerged)
	if err != nil {
		return err
	}
	brief := stackBrief(ctx, run, b, unmerged, sparse)
	if err := os.WriteFile(run.Conflict.Brief, []byte(brief), 0o600); err != nil {
		return fmt.Errorf("stack rebase: write the conflict brief: %w", err)
	}
	if err := stackSaveRun(run); err != nil {
		return err
	}
	return errors.New(brief)
}

func stackWidenCone(ctx context.Context, ws string, paths []string) (bool, error) {
	sparse, err := stackConfigBool(ctx, render.Dir(ws), "core.sparseCheckout", false)
	if err != nil || !sparse {
		return false, err
	}
	var dirs []string
	for _, p := range paths {
		if dir := path.Dir(p); dir != "." {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return true, nil
	}
	slices.Sort(dirs)
	return true, stackAddCone(ctx, stackRebasePrefix, render.Dir(ws), slices.Compact(dirs))
}

func stackAddCone(ctx context.Context, prefix string, ws render.Dir, dirs []string) error {
	argv := make([]string, 0, 4+len(dirs))
	argv = append(argv, "sparse-checkout", "add", "--skip-checks", "--")
	for _, dir := range dirs {
		argv = append(argv, "./"+path.Clean(dir)+"/")
	}
	if _, err := render.RunCLI(ctx, ws, "git", argv); err != nil {
		return fmt.Errorf("%s: check out %s in %s: %w", prefix, strings.Join(dirs, ", "), ws, err)
	}
	return nil
}

func stackRunRebase(ctx context.Context, ws render.Dir, argv []string, conflictWorkspace bool) (int, string, error) {
	out, err := render.RunCLI(ctx, ws, "git", []string{"rev-parse", "--absolute-git-dir"})
	if err != nil {
		return 0, "", fmt.Errorf("git rev-parse --absolute-git-dir: %w", err)
	}
	gitDir := strings.TrimSpace(out)
	_, code, stderr, err := render.RunCLIExitCode(render.WithProgress(ctx, stackRebaseProgress(gitDir), stackRebaseStall), ws, "git", argv)
	if !errors.Is(err, render.ErrStalled) {
		return code, stderr, err
	}
	lock := filepath.Join(gitDir, "index.lock")
	info, statErr := os.Stat(lock)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return code, stderr, err
	case statErr != nil:
		return code, stderr, errors.Join(err, statErr)
	case !conflictWorkspace:
		return code, stderr, fmt.Errorf("%w — left %s: %s is your checkout, not a ccx conflict workspace", err, lock, ws)
	case time.Since(info.ModTime()) < stackRebaseStall:
		return code, stderr, fmt.Errorf("%w — left %s, which another git process took after the kill", err, lock)
	}
	if rmErr := os.Remove(lock); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return code, stderr, errors.Join(err, rmErr)
	}
	return code, stderr, fmt.Errorf("%w — the killed git exited, so removed the index.lock it left in ccx's conflict workspace %s", err, ws)
}

func stackRebaseProgress(gitDir string) func() string {
	return func() string {
		var state strings.Builder
		for _, name := range []string{"rebase-merge/msgnum", "rebase-merge/done", "HEAD", "index", "index.lock"} {
			info, err := os.Stat(filepath.Join(gitDir, name))
			if err != nil {
				fmt.Fprintf(&state, "%s absent;", name)
				continue
			}
			fmt.Fprintf(&state, "%s %d %d;", name, info.Size(), info.ModTime().UnixNano())
		}
		return state.String()
	}
}

func stackUnmerged(ctx context.Context, ws string) ([]string, error) {
	out, err := render.RunCLI(ctx, render.Dir(ws), "git", []string{"diff", "--name-only", "-z", "--diff-filter=U"})
	if err != nil {
		return nil, fmt.Errorf("stack rebase: list the conflicted files in %s: %w", ws, err)
	}
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(p string) bool { return p == "" }), nil
}

func stackBrief(ctx context.Context, run *stackRebaseRun, b *stackRebaseBranch, unmerged []string, sparse bool) string {
	ws := render.Dir(run.Conflict.Workspace)
	state, checkedOut := "detached, rebase in progress, rerere off", "every file"
	if sparse {
		state = "detached, sparse, rebase in progress, rerere off"
		checkedOut = fmt.Sprintf("the root files and each conflicted file's directory — git sparse-checkout add <dir> in %s checks out more", ws)
	}
	var s strings.Builder
	fmt.Fprintf(&s, "stack rebase: conflict — %s does not rebase onto %s cleanly; nothing has moved\n", b.Name, b.Parent)
	fmt.Fprintf(&s, "workspace: %s (%s)\n", ws, state)
	if stopped, err := render.RunCLI(ctx, ws, "git", []string{"log", "-1", "--format=%h %s", "REBASE_HEAD"}); err == nil {
		fmt.Fprintf(&s, "stopped at: %s\n", strings.TrimSpace(stopped))
	}
	s.WriteString("conflicted files:\n")
	for _, f := range unmerged {
		fmt.Fprintf(&s, "  %s\n", f)
	}
	s.WriteString(regenHint(ctx, ws, unmerged, sparse))
	stackBriefIntent(&s, "this branch", b.Name, b.PR)
	if parent := run.branch(b.Parent); parent != nil {
		stackBriefIntent(&s, "the side it lands on", parent.Name, parent.PR)
	}
	argv := append([]string{"--literal-pathspecs", "log", "--format=%h %s", fmt.Sprintf("-%d", stackCulprits), b.OldBase + ".." + b.NewBase, "--"}, unmerged...)
	if culprits, err := render.RunCLI(ctx, ws, "git", argv); err == nil && strings.TrimSpace(culprits) != "" {
		fmt.Fprintf(&s, "upstream commits touching these files (%.12s..%.12s):\n", b.OldBase, b.NewBase)
		for line := range strings.Lines(strings.TrimSpace(culprits)) {
			fmt.Fprintf(&s, "  %s\n", strings.TrimRight(line, "\n"))
		}
	}
	fmt.Fprintf(&s, "checked out: %s\n", checkedOut)
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
	run.resumed = true
	if run.Conflict == nil {
		return stackDrive(ctx, cmd, l, commonDir, run)
	}
	return stackResume(ctx, cmd, l, commonDir, run)
}

var (
	errNoStackRebase = errors.New("stack rebase: no stack rebase is in progress in this repository")
	errStackRunGone  = errors.New("stack rebase: the run ended while this caller was taking it over")
)

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
	argv := append(slices.Clone(stackGitRebaseArgs), "rebase", "--continue")
	code, stderr, err := stackRunRebase(ctx, ws, argv, false)
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
	if err := stackRequireWorkspace(stackRebasePrefix, c); err != nil {
		return err
	}
	unmerged, err := stackUnmerged(ctx, c.Workspace)
	if err != nil {
		return err
	}
	if len(unmerged) > 0 {
		sparse, err := stackConfigBool(ctx, ws, "core.sparseCheckout", false)
		if err != nil {
			return err
		}
		return errors.New(strings.TrimRight(fmt.Sprintf("stack rebase: %s still has unresolved files: %s — resolve them and git add them first\n%s", c.Workspace, strings.Join(unmerged, ", "), regenHint(ctx, ws, unmerged, sparse)), "\n"))
	}
	if err := stackAdvance(ctx, run, b); err != nil {
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
	if err := stackPinResolved(ctx, l.dir(), run, b, head); err != nil {
		return err
	}
	cmd.Println(fmt.Sprintf("resolved %s at %.12s", b.Name, head))
	return stackCloseWorkspace(ctx, cmd, l, commonDir, run)
}

func stackCloseWorkspace(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	c := run.Conflict
	state, err := stackWorkspaceAt(stackRebasePrefix, c)
	if err != nil {
		return fmt.Errorf("%w; the run is kept, so continue can run again", err)
	}
	if state == stackWorkspaceOwned {
		changed, err := render.RunCLI(ctx, render.Dir(c.Workspace), "git", []string{"status", "--porcelain", "--untracked-files=no"})
		if err != nil {
			return fmt.Errorf("stack rebase: read the conflict workspace %s: %w", c.Workspace, err)
		}
		if strings.TrimSpace(changed) != "" {
			return fmt.Errorf("stack rebase: the conflict workspace %s holds uncommitted changes to tracked files (clear what it holds, then continue again): %s", c.Workspace, strings.TrimSpace(changed))
		}
	}
	note, err := stackReleaseWorkspace(ctx, l, commonDir, c, false)
	if err != nil {
		return fmt.Errorf("%w; the run is kept, so continue can run again", err)
	}
	if note != "" {
		cmd.Println(note)
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

const stackCleanupOwner = "ccx stack rebase"

type stackWorkspaceState int

const (
	stackWorkspaceGone stackWorkspaceState = iota
	stackWorkspaceOwned
	stackWorkspaceMismatched
)

func stackWorkspaceAt(prefix string, c *stackConflict) (stackWorkspaceState, error) {
	if _, err := os.Lstat(c.Workspace); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return stackWorkspaceGone, nil
		}
		return 0, fmt.Errorf("%s: %w", prefix, err)
	}
	if c.Registration.Validate() != nil {
		return 0, stackUnregistered(prefix, c.Workspace)
	}
	observed, err := cleanupObserveWorkspace(c.Workspace)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", prefix, err)
	}
	if observed != c.Registration {
		return stackWorkspaceMismatched, nil
	}
	return stackWorkspaceOwned, nil
}

func stackRequireWorkspace(prefix string, c *stackConflict) error {
	state, err := stackWorkspaceAt(prefix, c)
	if err != nil {
		return err
	}
	switch state {
	case stackWorkspaceGone:
		return fmt.Errorf("%s: the conflict workspace %s is gone, so its rebase cannot go on — ccx vcs stack abort, then re-run", prefix, c.Workspace)
	case stackWorkspaceMismatched:
		return fmt.Errorf("%s: %s — ccx vcs stack abort, then re-run", prefix, stackMismatchLine(c.Workspace))
	}
	return nil
}

func stackMismatchLine(ws string) string {
	return ws + " no longer matches the registration this run saved when it opened it, so it was left untouched"
}

func stackUnregistered(prefix, ws string) error {
	return fmt.Errorf("%s: %s was opened by an older ccx that saved no registration for it, so this run cannot go on — ccx vcs stack abort drops the run and removes the workspace git registers there, or remove it yourself with ccx vcs worktree rm --path %s, then run ccx vcs stack abort", prefix, ws, ws)
}

func stackUnidentified(ws string) error {
	return fmt.Errorf("%s: %s was opened by an older ccx that saved no registration for it, and git does not register a detached worktree of this repository named for it there, so it is left alone — remove it yourself with ccx vcs worktree rm --path %s, then run ccx vcs stack abort", stackRebasePrefix, ws, ws)
}

// stackAdoptLegacyWorkspace recovers the registration a pre-0.66.1 run never
// saved, from the worktree git registers at the path: its admin directory
// must be this repository's and named for the workspace, and its HEAD
// detached, as every conflict workspace ccx opens is.
func stackAdoptLegacyWorkspace(ctx context.Context, commonDir string, c *stackConflict) (cleanup.Registration, error) {
	observed, err := cleanupObserveWorkspace(c.Workspace)
	if err != nil {
		return cleanup.Registration{}, fmt.Errorf("%s: %w", stackRebasePrefix, err)
	}
	common, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return cleanup.Registration{}, fmt.Errorf("%s: %w", stackRebasePrefix, err)
	}
	if filepath.Dir(observed.AdminDir) != filepath.Join(common, "worktrees") || filepath.Base(observed.AdminDir) != filepath.Base(c.Workspace) {
		return cleanup.Registration{}, stackUnidentified(c.Workspace)
	}
	if _, err := render.RunCLI(ctx, render.Dir(c.Workspace), "git", []string{"symbolic-ref", "-q", "HEAD"}); err == nil {
		return cleanup.Registration{}, stackUnidentified(c.Workspace)
	}
	return observed, nil
}

func stackReleaseWorkspace(ctx context.Context, l lane, commonDir string, c *stackConflict, force bool) (string, error) {
	ws := c.Workspace
	_, err := os.Lstat(ws)
	absent := errors.Is(err, fs.ErrNotExist)
	if cleanupDaemonized && absent {
		return ws + " was already gone", nil
	}
	if c.Registration.Validate() != nil {
		if absent {
			return stackReleaseInline(ctx, l, c)
		}
		if c.Registration, err = stackAdoptLegacyWorkspace(ctx, commonDir, c); err != nil {
			return "", err
		}
	}
	receipt, err := cleanupDeferWorkspace(ctx, commonDir, ws, stackCleanupOwner, c.Registration, force)
	switch {
	case errors.Is(err, cleanup.ErrUnsupported):
		return stackReleaseInline(ctx, l, c)
	case errors.Is(err, cleanup.ErrPaused):
		return stackPausedLine(ws), nil
	case err != nil:
		var refused *cleanup.RefusedError
		switch {
		case errors.As(err, &refused) && refused.Reason == "replaced":
			return stackMismatchLine(ws), nil
		case errors.As(err, &refused):
			return "", fmt.Errorf("stack rebase: %w — nothing was removed, and %s is left where it is", err, ws)
		}
		return "", fmt.Errorf("stack rebase: %w — the cleanup daemon may already hold %s", err, ws)
	}
	line := fmt.Sprintf("handed %s to cleanup job %s, %s", ws, receipt.JobID, receipt.State)
	if receipt.Detail != "" {
		line += ": " + receipt.Detail
	}
	return line, nil
}

func stackReleaseInline(ctx context.Context, l lane, c *stackConflict) (string, error) {
	state, err := stackWorkspaceAt(stackRebasePrefix, c)
	if err != nil {
		return "", err
	}
	switch state {
	case stackWorkspaceMismatched:
		return stackMismatchLine(c.Workspace), nil
	case stackWorkspaceGone:
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "prune"}); err != nil {
			return "", fmt.Errorf("stack rebase: git worktree prune: %w", err)
		}
		return "", nil
	}
	occupied, err := stackOccupied(ctx, c.Workspace)
	switch {
	case err != nil:
		return "", err
	case occupied:
		return stackKeptLine(c.Workspace), nil
	}
	return "", stackDiscardWorkspace(ctx, l, c.Workspace)
}

func stackOccupied(ctx context.Context, ws string) (bool, error) {
	here, err := filepath.EvalSymlinks(workingDir(ctx))
	if err != nil {
		return false, fmt.Errorf("stack rebase: %w", err)
	}
	root, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return false, fmt.Errorf("stack rebase: %w", err)
	}
	rel, err := filepath.Rel(root, here)
	if err != nil {
		return false, fmt.Errorf("stack rebase: %w", err)
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func stackPausedLine(ws string) string {
	return "left " + ws + " in place, since the cleanup queue is paused — remove it with ccx vcs worktree rm --path " + ws + " after ccx vcs cleanup resume"
}

func stackKeptLine(ws string) string {
	return "left " + ws + " in place, since this command runs inside it — remove it with ccx vcs worktree rm --path " + ws + " once you leave it"
}

// stackDiscardWorkspace moves ws aside, prunes its registration, and deletes
// it in a detached rm: removing a full checkout in place can take longer than
// the command waiting on it is allowed to run.
func stackDiscardWorkspace(ctx context.Context, l lane, ws string) error {
	aside := filepath.Join(filepath.Dir(ws), "."+filepath.Base(ws)+".discarded-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.Rename(ws, aside); err != nil {
		return fmt.Errorf("stack rebase: move the conflict workspace aside: %w", err)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "prune"}); err != nil {
		return fmt.Errorf("stack rebase: git worktree prune: %w", err)
	}
	program, args := "rm", []string{"-rf", aside}
	if runtime.GOOS == "darwin" {
		program = "/usr/sbin/taskpolicy"
		args = append([]string{"-d", "throttle", "-c", "background", "-b", "/usr/bin/nice", "-n", "20", "rm"}, args...)
	}
	if err := render.StartDetached(ctx, render.Dir(filepath.Dir(aside)), program, args); err != nil {
		return fmt.Errorf("stack rebase: delete %s: %w", aside, err)
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
	case run.LocalApplied:
		return "", errors.New("stack abort: the stack is published and its branches without a pull request are already rebased locally, so there is nothing left to abort — ccx vcs stack continue finishes recording them")
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
		note, err := stackReleaseWorkspace(ctx, l, commonDir, c, true)
		if err != nil {
			return "", fmt.Errorf("%w; the run is kept, so abort can run again", err)
		}
		if note != "" {
			outcome += shipSep + note
		}
	}
	if err := stackDropPublicationPins(ctx, dir, run); err != nil {
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
	if l, err = stackTakeOver(ctx, l, run); err != nil {
		return lane{}, "", nil, err
	}
	return l, commonDir, run, nil
}

func stackTakeOver(ctx context.Context, l lane, run *stackRebaseRun) (lane, error) {
	if _, err := os.Stat(run.Origin); run.Origin != "" && errors.Is(err, fs.ErrNotExist) {
		run.Origin = l.root
	}
	if err := stackSaveRun(run); err != nil {
		return lane{}, err
	}
	if run.Origin == "" || l.root == run.Origin {
		return l, nil
	}
	return resolveLane(ctx, stackRebasePrefix, run.Origin, false)
}

func stackFinish(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	if !run.NoPush {
		return stackFinishPublication(ctx, cmd, l, commonDir, run)
	}
	prefix := stackRebasePrefix
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	left := stackKeptElsewhere(run, holders)
	var moves []restackMove
	var movers, dropped, reasons []string
	reparent := map[string]string{}
	revisions := map[string]string{}
	for _, b := range run.Branches {
		if b.Landed != "" {
			dropped = append(dropped, b.Name)
			reasons = append(reasons, b.Name+" ("+b.Landed+")")
			continue
		}
		if b.Held != "" {
			continue
		}
		revisions[b.Name] = b.NewBase
		if b.Parent != b.WasParent {
			reparent[b.Name] = b.Parent
		}
		if b.NewHead != b.Local && left[b.Name] == "" {
			movers = append(movers, b.Name)
			moves = append(moves, restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local})
		}
	}
	var realigned []string
	var alignErr error
	if !run.Applied {
		if err := stackCheckClean(ctx, movers, holders, stackResumeAdvice); err != nil {
			return err
		}
		if err := gtRestackRefuseClobbers(ctx, prefix, holders, moves); err != nil {
			return err
		}
		run.Applied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
		if err := stackWriteRefs(ctx, l.dir(), run, left); err != nil {
			run.Applied = false
			return errors.Join(err, stackSaveRun(run))
		}
	} else if err := stackWriteRefs(ctx, l.dir(), run, left); err != nil {
		return err
	}
	if !run.Aligned {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return err
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
	forget, err := stackForgettable(ctx, commonDir, run, dropped)
	if err != nil {
		return err
	}
	if err := errors.Join(
		gtmeta.Reparent(ctx, commonDir, reparent),
		gtmeta.RecordRestacked(ctx, commonDir, revisions),
		gtmeta.Forget(ctx, commonDir, forget),
		stackDropPublicationPins(ctx, l.dir(), run),
	); err != nil {
		return fmt.Errorf("%s: the branches are rewritten, but recording the stack in gt failed — fix the cause and run ccx vcs stack continue: %w", prefix, errors.Join(err, alignErr))
	}

	summary := []string{fmt.Sprintf("rebased %s onto %s@%.12s", gtBranchCount(len(movers)), run.Trunk, run.Pin)}
	if len(dropped) > 0 {
		summary = append(summary, "dropped "+strings.Join(reasons, ", "))
	}
	if len(realigned) > 0 {
		summary = append(summary, "reset "+strings.Join(realigned, ", "))
	}
	for _, b := range run.Branches {
		if holder := left[b.Name]; holder != "" {
			summary = append(summary, fmt.Sprintf("left %s at %.12s, not %.12s — checked out in %s", b.Name, b.Local, b.NewHead, holder))
		}
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

// stackKeptElsewhere is every branch the run keeps whose local ref is behind
// the head it kept and another working copy holds, by holder: that lane owns
// the ref and its checkout, so the run leaves both where they are.
func stackKeptElsewhere(run *stackRebaseRun, holders map[string]string) map[string]string {
	left := map[string]string{}
	for _, b := range run.Branches {
		if holder := holders[b.Name]; b.Kept && b.NewHead != b.Local && holder != "" && holder != run.Origin {
			left[b.Name] = holder
		}
	}
	return left
}

// stackForgettable is every dropped branch no branch outside the run still sits
// on: forgetting one another lane's branch sits on leaves that lane's parent
// unresolvable, and its own rebase drops it instead.
func stackForgettable(ctx context.Context, commonDir string, run *stackRebaseRun, dropped []string) ([]string, error) {
	state, err := gtStateAt(ctx, commonDir, stackRebasePrefix)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(dropped), func(name string) bool {
		for child, s := range state {
			if b := run.branch(child); len(s.Parents) > 0 && s.Parents[0].Ref == name && (b == nil || b.Held != "") {
				return true
			}
		}
		return false
	}), nil
}

func stackLandedSince(ctx context.Context, dir render.Dir, run *stackRebaseRun, live []string) ([]string, error) {
	prs, err := stackPRs(ctx, dir, run.Trunk, live)
	if err != nil {
		return nil, fmt.Errorf("stack rebase: reading the stack pull requests before the push failed — run ccx vcs stack continue: %w", err)
	}
	for _, name := range live {
		if pr := prs[name]; pr != nil && pr.State == "CLOSED" && !pr.Landed && !run.branch(name).reopens() && !slices.Contains(run.NewPRs, name) {
			return nil, fmt.Errorf("stack rebase: %s's pull request #%d closed without landing while the run was stopped, and publishing would open a new one — reopen it and run ccx vcs stack continue, or drop the run with ccx vcs stack abort", name, pr.Number)
		}
	}
	return slices.DeleteFunc(slices.Clone(live), func(name string) bool { return prs[name] == nil || !prs[name].Landed }), nil
}

func stackReplanLanded(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun, landed []string) error {
	if err := stackCheckSources(ctx, l.dir(), run); err != nil {
		return err
	}
	var members, pinned []string
	vetted := map[string]string{}
	replayed := map[string]stackRebaseBranch{}
	for _, b := range run.Branches {
		if b.Landed == "" {
			members = append(members, b.Name)
			vetted[b.Name] = b.Remote
			replayed[b.Name] = b
		}
		if b.Pinned {
			pinned = append(pinned, b.Name)
		}
	}
	next, err := stackPlan(ctx, l, commonDir, stackRebaseOpts{
		members: members, pinned: pinned, landed: landed, vetted: vetted, replayed: replayed, draft: run.Draft, draftAll: run.DraftAll, noVerify: run.NoVerify, ship: run.Ship,
		tip: run.Tip, tipOnly: run.TipOnly, dropCommits: run.DropCommits, stayClean: run.StayClean, restack: run.Restack, allLanes: run.AllLanes, to: run.To, newPRs: run.NewPRs,
	})
	if err != nil {
		return err
	}
	if !slices.Equal(run.Roots, next.Roots) {
		return errors.New("stack rebase: stack roots changed during replanning; original recovery state retained")
	}
	for i := range next.Branches {
		if b := run.branch(next.Branches[i].Name); b != nil {
			next.Branches[i].LocalOnly, next.Branches[i].Resolved = b.LocalOnly, b.Resolved
		}
	}
	next.dir, next.Claims, next.resumed = run.dir, run.Claims, run.resumed
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
		if err := stackCheckClean(ctx, []string{b.Name}, holders, stackResumeAdvice); err != nil {
			return err
		}
		if err := gtRestackRefuseClobbers(ctx, "restack", holders, []restackMove{move}); err != nil {
			return err
		}
		if err := stackWriteRefs(ctx, l.dir(), run, nil); err != nil {
			return err
		}
		run.Applied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	} else if err := stackWriteRefs(ctx, l.dir(), run, nil); err != nil {
		return err
	}
	if !run.Aligned {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("restack: %w", err)
		}
		if _, err := gtRestackAlign(ctx, "restack", holders, []restackMove{move}); err != nil {
			return err
		}
		run.Aligned = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	summary := "fetched" + shipSep + "rebased onto " + run.Trunk
	if b.NewHead == b.Local {
		summary = "fetched" + shipSep + "already on " + run.Trunk
	}
	if run.Retarget != nil {
		published, err := restackGitPublish(ctx, l.dir(), b.Name, b.NewHead, run.Trunk, run.Retarget, func() error { return stackSaveRun(run) })
		if err != nil {
			return errors.Join(err, stackDropPublicationPins(ctx, l.dir(), run), stackClearRun(commonDir, run))
		}
		if published != "" {
			summary += shipSep + published
		}
	}
	if err := errors.Join(stackDropPublicationPins(ctx, l.dir(), run), stackClearRun(commonDir, run)); err != nil {
		return fmt.Errorf("restack: %s is rebased, but clearing the run failed: %w", b.Name, err)
	}
	if l.note != "" {
		summary = fmt.Sprintf("lane %s (%s)%s%s", kindLabel(l.kind), l.note, shipSep, summary)
	}
	cmd.Println(summary)
	return nil
}

// stackWriteRefs moves every rewritten branch but those left where they are in
// one transaction and verifies every unmoved one, so a branch that changed
// locally while the run was stopped fails the whole write rather than leaving a
// child on a parent it never saw.
func stackWriteRefs(ctx context.Context, dir render.Dir, run *stackRebaseRun, left map[string]string) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		switch {
		case b.Landed != "":
		case b.NewHead != b.Local && left[b.Name] == "":
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

// stackVerdict reports each live branch's pull request after a publication and
// moves one GitHub bases off its parent back onto it, returning those it could
// not move.
func stackVerdict(ctx context.Context, cmd *cobra.Command, l lane, run *stackRebaseRun, live, published []string) ([]string, error) {
	dir := l.dir()
	var strays []string
	var prs map[string]*stackPR
	var err error
	lagUntil := time.Now().Add(stackHeadLagWait)
	overwritten := map[string]stackOverwrite{}
	for try := 0; ; try++ {
		if prs, err = stackPRs(ctx, dir, run.Trunk, live); err != nil {
			_, werr := fmt.Fprintf(cmd.ErrOrStderr(), "stack rebase: pushed, but the verdict could not read the pull requests: %v\n", err)
			return nil, werr
		}
		if err := stackReadOverwrites(ctx, dir, run, prs, overwritten); err != nil {
			_, werr := fmt.Fprintf(cmd.ErrOrStderr(), "stack rebase: pushed, but the verdict could not read who moved a pull request's head: %v\n", err)
			return nil, werr
		}
		var wait time.Duration
		switch {
		case stackAnyLagging(run, prs, overwritten) && time.Now().Add(stackHeadLagRetry).Before(lagUntil):
			wait = stackHeadLagRetry
		case stackAnyUnknown(prs) && try < stackVerdictTries-1:
			wait = statusMergeableRetry
		}
		if wait == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	untracked, trackingErr := stackReadTracking(ctx, l, run, live, prs)
	recordStackTracking(ctx, untracked, trackingErr)
	for _, name := range live {
		b := run.branch(name)
		parent, err := stackSubmittedParent(ctx, dir, run, b)
		if err != nil {
			return nil, err
		}
		fields := []string{name}
		pr := prs[name]
		if pr == nil {
			fields = append(fields, "no pull request", fmt.Sprintf("head %.12s", b.NewHead), "parent "+parent)
			cmd.Println(strings.Join(fields, shipSep))
			continue
		}
		mergeable := strings.ToLower(pr.Mergeable)
		switch {
		case trackingErr != nil:
			mergeable = "graphite tracking unread"
		case slices.ContainsFunc(untracked, func(u stackUntracked) bool { return u.Branch == name }):
			mergeable = "untracked by graphite"
		}
		fields = append(fields, fmt.Sprintf("#%d", pr.Number), fmt.Sprintf("head %.12s", pr.Head), "parent "+parent, mergeable)
		switch pusher := overwritten[name].by(pr); {
		case stackHeadLags(pr, b) && pusher != "":
			fields = append(fields, fmt.Sprintf("overwritten: %s force-pushed %.12s over the push of %.12s — re-run ccx vcs stack submit to publish it again", pusher, pr.Head, b.NewHead))
		case stackHeadLags(pr, b):
			fields = append(fields, fmt.Sprintf("stale read: GitHub still shows %.12s %s after the push of %.12s — re-run ccx vcs stack submit if it stays", pr.Head, stackHeadLagWait, b.NewHead))
		}
		if stackBaseStrays(pr, parent, slices.Contains(published, name)) {
			field, moved := stackRetargetToParent(ctx, dir, pr, parent)
			fields = append(fields, field)
			if !moved {
				strays = append(strays, fmt.Sprintf("#%d", pr.Number))
			}
		}
		if len(pr.Labels) > 0 {
			fields = append(fields, "labels "+strings.Join(pr.Labels, ","))
		}
		cmd.Println(strings.Join(fields, shipSep))
	}
	return strays, nil
}

// stackSubmittedParent is the base the submit gave b's pull request: b's
// parent, or trunk when the remote trunk already holds that parent's replayed
// head, since the submit drops such a parent and anchors its child on trunk.
func stackSubmittedParent(ctx context.Context, dir render.Dir, run *stackRebaseRun, b *stackRebaseBranch) (string, error) {
	parent := run.branch(b.Parent)
	if parent == nil || parent.Landed != "" || parent.Held != "" || parent.LocalOnly {
		return b.Parent, nil
	}
	contained, err := gitIsAncestor(ctx, dir, stackRebasePrefix, parent.NewHead, run.Pin)
	if err != nil || !contained {
		return b.Parent, err
	}
	return run.Trunk, nil
}

// stackBaseStrays is an open pull request GitHub bases somewhere other than the
// parent the submit gave it. One parked on its graphite-base branch strays only
// once this run submitted it, since that submit moves it back; one the run
// left out is the merge queue's to move.
func stackBaseStrays(pr *stackPR, parent string, submitted bool) bool {
	return pr.State == "OPEN" && pr.Base != "" && pr.Base != parent && (submitted || pr.Base != fmt.Sprintf("graphite-base/%d", pr.Number))
}

// stackRetargetToParent moves a pull request GitHub still bases on another
// branch onto the parent the stack records, the base Graphite submitted, and
// names the command that finishes the move when GitHub refuses it.
func stackRetargetToParent(ctx context.Context, dir render.Dir, pr *stackPR, parent string) (string, bool) {
	repo, err := vcs.LookupRepo(ctx, dir, false)
	if err != nil {
		return fmt.Sprintf("base %s ≠ parent %s — the repository could not be read to retarget it: %v", pr.Base, parent, err), false
	}
	retarget := ghPatchPullArgv(repo.NameWithOwner, pr.Number, "-f", "base="+parent)
	if _, err := render.RunCLI(ctx, render.Ambient, "gh", retarget); err != nil {
		return fmt.Sprintf("base %s ≠ parent %s — retargeting failed, finish it with %s: %v", pr.Base, parent, ghCommand(retarget), err), false
	}
	pr.Mergeable = statusUnknown
	return fmt.Sprintf("retargeted onto %s from %s", parent, pr.Base), true
}

// stackHeadLags is a pull request GitHub still reads at another head than the
// one just pushed; its REST view has trailed a push by minutes.
func stackHeadLags(pr *stackPR, b *stackRebaseBranch) bool {
	return pr.State == "OPEN" && pr.Head != "" && pr.Head != b.NewHead
}

// stackOverwrite is who force-pushed head over the run's push.
type stackOverwrite struct{ head, pusher string }

func (o stackOverwrite) by(pr *stackPR) string {
	if o.head != pr.Head {
		return ""
	}
	return o.pusher
}

// stackReadOverwrites records who force-pushed each lagging pull request's
// head after the run's push, once per head: a head someone pushed over the
// run's is no lag to wait out.
func stackReadOverwrites(ctx context.Context, dir render.Dir, run *stackRebaseRun, prs map[string]*stackPR, overwritten map[string]stackOverwrite) error {
	for name, pr := range prs {
		b := run.branch(name)
		if pr == nil || !stackHeadLags(pr, b) || overwritten[name].head == pr.Head {
			continue
		}
		pusher, err := ghHeadPusher(ctx, dir, pr.Number, b.NewHead, pr.Head)
		if err != nil {
			return err
		}
		overwritten[name] = stackOverwrite{head: pr.Head, pusher: pusher}
	}
	return nil
}

func stackAnyLagging(run *stackRebaseRun, prs map[string]*stackPR, overwritten map[string]stackOverwrite) bool {
	for name, pr := range prs {
		if pr != nil && overwritten[name].by(pr) == "" && stackHeadLags(pr, run.branch(name)) {
			return true
		}
	}
	return false
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
	locks := run.locks()
	for i, name := range locks {
		dir := stackRunDir(commonDir, name)
		if err := stackReclaimAbandoned(commonDir, dir); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o750); err != nil {
			for _, claimed := range locks[:i] {
				_ = os.Remove(stackRunDir(commonDir, claimed))
			}
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("stack rebase: another stack rebase claimed %s meanwhile — re-run to see which, or drop it with ccx vcs stack abort --stack %s", name, name)
			}
			return fmt.Errorf("stack rebase: %w", err)
		}
		if i > 0 {
			if err := os.WriteFile(filepath.Join(dir, stackClaimOwner), []byte(locks[0]), 0o600); err != nil {
				return fmt.Errorf("stack rebase: %w", err)
			}
		}
	}
	run.dir = stackRunDir(commonDir, locks[0])
	return nil
}

func stackReclaimAbandoned(commonDir, dir string) error {
	info, err := os.Stat(dir)
	if err != nil || time.Since(info.ModTime()) < stackStaleAfter {
		return nil
	}
	if _, err := os.Stat(stackStatePath(dir)); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if owner, err := os.ReadFile(filepath.Join(dir, stackClaimOwner)); err == nil { //nolint:gosec // a claim directory ccx created under the git common dir
		if _, err := os.Stat(stackStatePath(stackRunDir(commonDir, string(owner)))); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
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
	for _, name := range run.locks() {
		if err := os.RemoveAll(stackRunDir(commonDir, name)); err != nil {
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
		named := slices.DeleteFunc(slices.Clone(runs), func(run *stackRebaseRun) bool {
			return !slices.Contains(run.Roots, stack) && !slices.Contains(run.locks(), stack)
		})
		switch {
		case len(named) == 0:
			return nil, fmt.Errorf("stack rebase: no stack rebase of %s is in progress — %s", stack, stackRunChoices(runs))
		case len(named) > 1:
			if i := slices.IndexFunc(named, func(run *stackRebaseRun) bool { return slices.Contains(run.locks(), stack) }); i >= 0 {
				return named[i], nil
			}
			return nil, fmt.Errorf("stack rebase: %d stack rebases of %s are in progress — name one: %s", len(named), stack, stackRunChoices(named))
		}
		return named[0], nil
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
		choices = append(choices, "--stack "+run.locks()[0])
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

func stackMarkBaseGone(ctx context.Context, dir render.Dir, tr vcs.Trunk, prs map[string]*stackPR) error {
	argv := []string{"ls-remote", "--heads", tr.Remote()}
	var closed []*stackPR
	for _, pr := range prs {
		if pr.State == "CLOSED" && !pr.Landed && pr.Base != "" && pr.Base != tr.Name() {
			closed = append(closed, pr)
			argv = append(argv, gtRestackRef(pr.Base))
		}
	}
	if len(closed) == 0 {
		return nil
	}
	out, err := render.RunCLI(ctx, dir, "git", argv)
	if err != nil {
		return fmt.Errorf("stack rebase: git ls-remote --heads %s: %w", tr.Remote(), err)
	}
	live := map[string]bool{}
	for line := range strings.Lines(out) {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			live[strings.TrimPrefix(ref, "refs/heads/")] = true
		}
	}
	for _, pr := range closed {
		if pr.BaseGone = !live[pr.Base]; pr.BaseGone {
			continue
		}
		back, err := ghClosedByBaseDeletion(ctx, dir, pr.Number)
		if err != nil {
			return fmt.Errorf("stack rebase: %w", err)
		}
		pr.BaseGone, pr.BaseBack = back, back
	}
	return nil
}

const (
	stackRetryAdvice  = "retry with ccx"
	stackResumeAdvice = "resume the run with ccx vcs stack continue, or drop it with ccx vcs stack abort"
)

// stackCheckClean refuses a move under a working copy with uncommitted work,
// which realigning that copy onto the moved branch would overwrite.
func stackCheckClean(ctx context.Context, movers []string, holders map[string]string, advice string) error {
	for _, branch := range movers {
		holder := holders[branch]
		if holder == "" {
			continue
		}
		status, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"status", "--porcelain", "--untracked-files=normal"})
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("stack rebase: %s has uncommitted work; no branches moved — commit or move that work, then %s", holder, advice)
		}
	}
	return nil
}
