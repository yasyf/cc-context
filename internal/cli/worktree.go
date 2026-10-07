package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/yasyf/cc-context/internal/cleanup"
	"github.com/yasyf/cc-context/internal/cleanup/native"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// jjColocateRefusal is jj's own answer to `jj git init --colocate` inside a git
// worktree.
const jjColocateRefusal = "Error: Cannot create a colocated jj repo inside a Git worktree.\n" +
	"Hint: Run `jj git init` in the main Git repository instead, or use `jj workspace add` to create additional jj workspaces."

const orphanShape = "orphaned checkout"

const (
	jjModeNone      = "none"
	jjModeWorkspace = "workspace"
	jjModeColocate  = "colocate"
)

// worktreeEntry is one working copy registered against the repository. Defect
// carries whatever is wrong with it — a gitdir pointer resolving to nothing, a
// registration whose tree is gone — because a broken worktree is what someone
// runs the listing to find, never a reason to withhold the rest of it.
type worktreeEntry struct {
	Path     string `json:"path"`
	Shape    string `json:"shape,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	Bare     bool   `json:"bare,omitempty"`
	Locked   string `json:"locked,omitempty"`
	Prunable string `json:"prunable,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Defect   string `json:"defect,omitempty"`
}

// worktreeReport is the repository's working copies as ccx vcs worktree list
// reports them. CheckoutError follows vcsinfo's precedent: a working copy whose
// own pointer resolves to nothing is the report's answer, not its failure.
type worktreeReport struct {
	Root          string          `json:"root"`
	RepoKey       string          `json:"repo_key,omitempty"`
	Worktrees     []worktreeEntry `json:"worktrees,omitempty"`
	CheckoutError string          `json:"checkout_error,omitempty"`
}

func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "List, create, remove, and repair this repository's working copies",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(
		newWorktreeListCmd(),
		newWorktreeAddCmd(),
		newWorktreeRmCmd(),
		newWorktreeParkCmd(),
		newWorktreeRepairCmd(),
	)
	return cmd
}

func newWorktreeListCmd() *cobra.Command {
	var (
		asJSON bool
		budget int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every working copy registered against this repository",
		Long: `List every working copy registered against this repository.

Each line is "<path> · <shape> · <branch>", with a leading "*" on the checkout
you are in and any defect — a dangling gitdir pointer, a prunable registration,
a lock — appended inline. Those defects are the answer, so the listing exits 0
carrying them. It reads git's worktree registry: a jj workspace is attached
through .jj alone and appears in "jj workspace list" instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorktreeList(cmd, asJSON, budget)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the listing as JSON")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for the output (0 = uncapped)")
	return cmd
}

func newWorktreeAddCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a working copy named <name> under the repository's pool",
		Long: `Create a working copy named <name> under the repository's pool.

The path is minted at "$HOME/.claude/worktrees/<main-basename>/<name>",
outside every repository tree so a worktree is never mistaken for repo content.
A branch-shaped name such as user/slug keeps its full branch name and mints the
directory user-slug.
--jj picks how the new copy attaches: "none" is a git worktree, "workspace" is a
jj workspace, and "colocate" is impossible — jj refuses to create a colocated
repo inside a git worktree. Without --jj, a jj workspace mints another workspace
and everything else mints a git worktree.

A git worktree checks out an existing local branch <name> as it stands. If only
the remote holds <name>, ccx fetches it and checks it out at the remote head
without setting an upstream. This lets a removed lane resume at its published
head. Otherwise, ccx cuts <name> from the freshly fetched remote trunk, or from
HEAD when the repository has no remote. An existing branch on the gt stack of
the branch checked out here joins this working copy's lane, so stack submit
from either submits it. A jj workspace starts on trunk() after jj git fetch, or
on the caller's parents with no remote. The summary names the base commit.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorktreeAdd(cmd, args[0], mode)
		},
	}
	cmd.Flags().StringVar(&mode, "jj", "", "how the new working copy attaches: none|workspace|colocate")
	return cmd
}

type worktreeRmOptions struct {
	path   string
	force  bool
	wait   bool
	dryRun bool
}

func newWorktreeRmCmd() *cobra.Command {
	var opts worktreeRmOptions
	cmd := &cobra.Command{
		Use:   "rm (<name> | --path <absolute-path>)",
		Short: "Remove the working copy named <name>, or the linked worktree at --path",
		Long: `Remove the working copy named <name>, or the linked worktree at --path.

<name> is the working copy "add" minted under the repository's pool — rm
removes only what add created, so a worktree ccx never minted by name is
refused and left to --path. --path names any linked worktree this repository
registers, by its absolute path. Removing the checkout
that holds trunk is refused, since the branch it pins is the one every restack
rebases onto, and so is the repository's own working copy. A tree holding
uncommitted changes is refused unless --force discards them; --force overrides
nothing else. --dry-run removes nothing and reports what rm would remove.

On macOS a git worktree is handed to the per-user cleanup daemon: rm returns
once the tree has left its path and git's registry, with its committed head
pinned under refs/ccx/cleanup/, and the daemon deletes the files afterward.
A tree a live process is working in, holding open, or was started on is
refused. --wait then polls that job until its tree is deleted, as
"ccx vcs cleanup wait" does; "ccx vcs cleanup status" reports the queue.
--dry-run runs the daemon's whole preflight in process, so it refuses whatever
rm would.

Linux removes a git worktree inline with git worktree remove. There --dry-run
checks only the trunk and main-working-copy refusals: git's own dirty and
locked refusals surface on the real removal alone.

A jj workspace is forgotten and its directory deleted — "jj workspace forget"
leaves the tree on disk with a live-looking pointer otherwise.

An orphaned worktree, one whose .git file names an admin dir that no longer
exists, is out of git's reach. --path moves it to ~/.Trash/<name>-<timestamp>
from any directory, since git can no longer tell whether it held unpushed
work. It must sit at $HOME/.claude/worktrees/<repo>/<name>, its .git must be a
regular file, and no live process may hold it. Off macOS, where that process
check is missing, --force stands in for it. A tree on another volume than the
Trash is refused on the move itself, so --dry-run, which runs every other
check, does not report it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			byPath := cmd.Flags().Changed("path")
			switch {
			case len(args) == 1 && byPath:
				return errors.New("worktree rm: name a working copy or pass --path, not both")
			case len(args) == 0 && !byPath:
				return errors.New("worktree rm: name a working copy, or pass --path <absolute-path>")
			case byPath:
				return runWorktreeRmPath(cmd, opts)
			}
			return runWorktreeRm(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.path, "path", "", "absolute path of a linked worktree this repository registers")
	cmd.Flags().BoolVar(&opts.force, "force", false, "remove a worktree with uncommitted changes")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "on macOS, also wait until the daemon has deleted the tree")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "report what would be removed, removing nothing (on Linux a git worktree gets only the trunk and main-working-copy refusals)")
	return cmd
}

func newWorktreeParkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "park",
		Short: "Free the trunk ref by detaching the working copy that holds it",
		Long: `Free the trunk ref by detaching the working copy that holds it.

A checkout sitting on trunk pins the ref every restack rebases onto: git refuses
to fetch into a branch checked out elsewhere, so no sibling working copy can
advance it and the whole pool restacks onto a trunk that is weeks old. A shared
checkout that exists only to host worktrees has no reason to hold it.

Park detaches that checkout's HEAD at the commit it already stands on — no file
in it changes — and fast-forwards the freed ref to the remote-tracking trunk
already fetched, without touching the network. It is deliberately a command you
run rather than something ccx does when it cuts a worktree: the checkout may be
somebody's, and HEAD is theirs to move.

It refuses on any uncommitted change there, which is work park must never touch,
and on a local trunk carrying commits the remote does not, which the ref advance
would discard.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorktreePark(cmd)
		},
	}
	return cmd
}

func runWorktreePark(cmd *cobra.Command) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "worktree park", workingDir(ctx), true)
	if err != nil {
		return err
	}
	branch, err := gitCurrentBranch(ctx, l.dir(), "worktree park")
	if err != nil {
		return err
	}
	remote, err := vcs.GitRemoteFor(ctx, l.dir(), branch)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	trunk, err := vcs.ResolveTrunk(ctx, l.dir(), remote)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	holders, err := vcs.TrunkHolders(ctx, l.checkout, trunk)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	if len(holders) == 0 {
		cmd.Println(strings.Join([]string{"already free", trunk.Name() + " is checked out in no working copy"}, shipSep))
		return nil
	}
	if len(holders) > 1 {
		return fmt.Errorf("worktree park: %d working copies hold %s (%s) — park frees one ref and would move HEAD under the rest; detach all but one by hand first",
			len(holders), trunk.Name(), strings.Join(holders, ", "))
	}
	holder := holders[0]
	local, target := vcs.LocalBranchRef(trunk.Name()), trunk.Ref()
	// The ref is read before anything is measured and handed to update-ref as
	// its expected old value, so a concurrent commit onto trunk fails the
	// advance rather than being overwritten by it.
	was, err := vcs.ResolveRef(ctx, l.dir(), local)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	onto, err := vcs.ResolveRef(ctx, l.dir(), target)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	state, err := vcs.ReadTrunkState(ctx, l.dir(), trunk, holder)
	if err != nil {
		return fmt.Errorf("worktree park: %w", err)
	}
	if err := parkRefusals(state); err != nil {
		return err
	}
	if _, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"checkout", "--detach"}); err != nil {
		return fmt.Errorf("worktree park: git checkout --detach in %s: %w", holder, err)
	}
	if state.Behind > 0 {
		if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"update-ref", string(local), onto, was}); err != nil {
			return fmt.Errorf("worktree park: git update-ref %s %s %s: %w — %s is detached and %s was left where it stood",
				local, onto, was, err, holder, trunk.Name())
		}
	}
	cmd.Println(strings.Join([]string{"parked " + holder, "freed " + trunk.Name(), parkAdvanced(state, remote)}, shipSep))
	return nil
}

// parkRefusals is the pair of states park must not act on: work it would have
// to touch, and commits the fast-forward would discard.
func parkRefusals(state vcs.TrunkState) error {
	if state.Stale {
		return fmt.Errorf("worktree park: %s holds %s but its tree is gone — run git worktree prune, then park again",
			state.Holder, state.Trunk)
	}
	if state.Dirty > 0 {
		return fmt.Errorf("worktree park: %s holds %s with %d uncommitted %s — commit or move that work first; park never touches it",
			state.Holder, state.Trunk, state.Dirty, plural(state.Dirty, "file", "files"))
	}
	if state.Contaminated() {
		return fmt.Errorf("worktree park: local %s carries %d %s %s/%s does not (%s) — land or move them first; the fast-forward park runs would drop them",
			state.Trunk, len(state.Foreign), plural(len(state.Foreign), "commit", "commits"), state.Remote, state.Trunk, parkForeign(state))
	}
	return nil
}

func parkForeign(state vcs.TrunkState) string {
	segs := make([]string, 0, len(state.Foreign))
	for _, c := range state.Foreign {
		segs = append(segs, c.SHA+" "+c.Subject)
	}
	return strings.Join(segs, "; ")
}

func parkAdvanced(state vcs.TrunkState, remote string) string {
	if state.Behind == 0 {
		return "already at " + remote + "/" + state.Trunk
	}
	return fmt.Sprintf("advanced %d to %s/%s", state.Behind, remote, state.Trunk)
}

func newWorktreeRepairCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Re-point gitdir pointers that resolve to nothing",
		Long: `Re-point gitdir pointers that resolve to nothing.

Run from a healthy checkout, this repairs every working copy the repository
registers — the recovery for a repository or a worktree that moved on disk. Run
from a broken one, it repairs that checkout from the repository its own dangling
pointer names, which fails honestly when the admin dir behind the pointer is
gone rather than merely misplaced.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorktreeRepair(cmd, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the git invocation instead of running it")
	return cmd
}

func runWorktreeList(cmd *cobra.Command, asJSON bool, budget int) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "worktree list", workingDir(ctx), true, false)
	if err != nil {
		return err
	}
	report := worktreeReport{Root: l.root}
	if l.broken != nil {
		report.CheckoutError = l.broken.Error()
	} else {
		report.RepoKey = l.checkout.RepoKey()
		report.Worktrees, err = collectWorktrees(ctx, "worktree list", l.checkout)
		if err != nil {
			return err
		}
	}
	if asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("worktree list: marshal report: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Print(render.Cap(renderWorktreeList(report), budget))
	return nil
}

// collectWorktrees pairs git's registry with a filesystem re-resolution of each
// registered path, which is what turns "git lists it" into a shape, or into the
// diagnosis of a pointer git itself never follows.
func collectWorktrees(ctx context.Context, prefix string, c vcs.Checkout) ([]worktreeEntry, error) {
	if c.CommonDir == "" {
		return nil, fmt.Errorf("%s: %q has no git repository behind it — worktrees are git's registry", prefix, c.Root)
	}
	list, err := vcs.Worktrees(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	entries := make([]worktreeEntry, 0, len(list))
	for _, wt := range list {
		entries = append(entries, worktreeEntryOf(wt, c.Root))
	}
	return entries, nil
}

func worktreeEntryOf(wt vcs.Worktree, root string) worktreeEntry {
	e := worktreeEntry{
		Path:     wt.Path,
		Branch:   wt.Branch,
		Detached: wt.Detached,
		Bare:     wt.Bare,
		Locked:   wt.Locked,
		Prunable: wt.Prunable,
		Current:  wt.Path == root,
	}
	if wt.Prunable != "" || wt.Bare {
		return e
	}
	ck, err := vcs.ResolveCheckout(wt.Path)
	if err != nil {
		e.Defect = err.Error()
		return e
	}
	e.Shape = infoShape(ck.Shape)
	return e
}

func renderWorktreeList(r worktreeReport) string {
	var b strings.Builder
	if r.CheckoutError != "" {
		fmt.Fprintf(&b, "%-*s%s\n", infoLabelWidth, "checkout", r.CheckoutError)
		return b.String()
	}
	for _, e := range r.Worktrees {
		gutter := "  "
		if e.Current {
			gutter = "* "
		}
		b.WriteString(gutter + strings.Join(worktreeSegments(e), shipSep) + "\n")
	}
	return b.String()
}

func worktreeSegments(e worktreeEntry) []string {
	segs := []string{e.Path}
	if e.Shape != "" {
		segs = append(segs, e.Shape)
	}
	switch {
	case e.Bare:
		segs = append(segs, "(bare)")
	case e.Detached:
		segs = append(segs, "(detached)")
	case e.Branch != "":
		segs = append(segs, e.Branch)
	}
	if e.Locked != "" {
		segs = append(segs, "locked: "+e.Locked)
	}
	if e.Prunable != "" {
		segs = append(segs, "prunable: "+e.Prunable)
	}
	if e.Defect != "" {
		segs = append(segs, e.Defect)
	}
	return segs
}

func runWorktreeAdd(cmd *cobra.Command, name, requested string) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "worktree add", workingDir(ctx), true)
	if err != nil {
		return err
	}
	mode, err := worktreeMode(requested, l.checkout)
	if err != nil {
		return err
	}
	path, err := mintWorktreePath(ctx, "worktree add", l.checkout, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("worktree add: mint pool for %q: %w", name, err)
	}
	var base worktreeBase
	switch mode {
	case jjModeWorkspace:
		if base, err = worktreeJJBase(ctx, l.dir()); err != nil {
			return err
		}
		if _, err := render.RunCLI(ctx, l.dir(), "jj", []string{"workspace", "add", "--name", name, "-r", base.rev, path}); err != nil {
			return fmt.Errorf("worktree add: jj workspace add %s: %w", name, err)
		}
	default:
		if base, err = worktreeGitBase(ctx, l.dir(), name); err != nil {
			return err
		}
		args := []string{"worktree", "add", path, name}
		if base.rev != "" {
			args = []string{"worktree", "add", "-b", name, path, base.rev}
		}
		if _, err := render.RunCLI(ctx, l.dir(), "git", args); err != nil {
			return fmt.Errorf("worktree add: git worktree add %s: %w", path, err)
		}
		if base.rev == "" || base.existing {
			if err := worktreeJoinLane(ctx, l, name, path); err != nil {
				return err
			}
		}
	}
	cmd.Println(strings.Join([]string{"added " + name, worktreeShapeOf(mode), base.segment(), path}, shipSep))
	return nil
}

// worktreeJoinLane puts the working copy checked out on branch at path into
// l's lane when branch shares the gt stack of the branch l has checked out.
func worktreeJoinLane(ctx context.Context, l lane, branch, path string) error {
	graphite, err := vcs.GraphiteRepo(l.checkout)
	if err != nil {
		return fmt.Errorf("worktree add: %w", err)
	}
	if !graphite {
		return nil
	}
	current, err := gitCurrentBranch(ctx, l.dir(), "worktree add")
	if err != nil || current == "" {
		return err
	}
	state, err := gtStateQuery(ctx, l.dir(), "worktree add")
	if err != nil {
		return err
	}
	trunk, err := gtTrunkBranch("worktree add", state)
	if err != nil {
		return err
	}
	if current == trunk || branch == trunk || !gtSameStack(state, trunk, branch, current) {
		return nil
	}
	return laneJoin("worktree add", l, path)
}

type worktreeBase struct {
	rev      string
	label    string
	shas     []string
	existing bool
}

func (b worktreeBase) segment() string {
	short := make([]string, len(b.shas))
	for i, sha := range b.shas {
		short[i] = shortOID(sha)
	}
	if b.rev == "" || b.existing {
		return "existing " + b.label + " at " + strings.Join(short, "+")
	}
	return "from " + b.label + " " + strings.Join(short, "+")
}

func worktreeGitBase(ctx context.Context, dir render.Dir, name string) (worktreeBase, error) {
	existing, err := gitRefExists(ctx, dir, "worktree add", "refs/heads/"+name)
	if err != nil {
		return worktreeBase{}, err
	}
	if existing {
		sha, err := worktreeRevParse(ctx, dir, "refs/heads/"+name)
		return worktreeBase{label: name, shas: []string{sha}}, err
	}
	branch, err := gitCurrentBranch(ctx, dir, "worktree add")
	if err != nil {
		return worktreeBase{}, err
	}
	remote, err := vcs.GitRemoteFor(ctx, dir, cmp.Or(branch, "HEAD"))
	if err != nil {
		return worktreeBase{}, fmt.Errorf("worktree add: %w", err)
	}
	key := "remote." + remote + ".url"
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", key})
	switch {
	case err != nil:
		return worktreeBase{}, fmt.Errorf("worktree add: git config %s: %w", key, err)
	case code == 1:
		sha, err := worktreeRevParse(ctx, dir, "HEAD")
		return worktreeBase{rev: sha, label: "HEAD", shas: []string{sha}}, err
	case code != 0:
		return worktreeBase{}, fmt.Errorf("worktree add: git config %s: exit %d: %s", key, code, strings.TrimSpace(stderr))
	}
	trunk, _, err := gitRemoteHead(ctx, dir, "worktree add", remote)
	if err != nil {
		return worktreeBase{}, err
	}
	if trunk == "" {
		return worktreeBase{}, fmt.Errorf("worktree add: %s names no default branch to cut %s from — run git remote set-head %s -a", remote, name, remote)
	}
	published, err := worktreeRemoteHas(ctx, dir, remote, name)
	if err != nil {
		return worktreeBase{}, err
	}
	from := cmp.Or(published, trunk)
	tracking := string(vcs.RemoteBranchRef(remote, from))
	if err := gitFetch(ctx, dir, "--no-tags", "--no-write-fetch-head", remote, "+refs/heads/"+from+":"+tracking); err != nil {
		return worktreeBase{}, fmt.Errorf("worktree add: git fetch %s %s: %w", remote, from, err)
	}
	sha, err := worktreeRevParse(ctx, dir, tracking)
	if err != nil {
		return worktreeBase{}, err
	}
	return worktreeBase{rev: sha, label: remote + "/" + from, shas: []string{sha}, existing: published != ""}, nil
}

// worktreeRemoteHas returns name when remote holds branch name, asking the
// remote itself: a clone whose fetch refspec maps only trunk has no
// remote-tracking ref to consult.
func worktreeRemoteHas(ctx context.Context, dir render.Dir, remote, name string) (string, error) {
	ref := "refs/heads/" + name
	out, err := render.RunCLI(ctx, dir, "git", []string{"ls-remote", "--heads", remote, ref})
	if err != nil {
		return "", fmt.Errorf("worktree add: git ls-remote %s %s: %w", remote, ref, err)
	}
	for line := range strings.Lines(out) {
		if _, got, _ := strings.Cut(strings.TrimSpace(line), "\t"); got == ref {
			return name, nil
		}
	}
	return "", nil
}

func worktreeJJBase(ctx context.Context, dir render.Dir) (worktreeBase, error) {
	remotes, err := render.RunCLI(ctx, dir, "jj", []string{"git", "remote", "list"})
	if err != nil {
		return worktreeBase{}, fmt.Errorf("worktree add: jj git remote list: %w", err)
	}
	base := worktreeBase{rev: "@-", label: "@-"}
	if strings.TrimSpace(remotes) != "" {
		if err := jjGitFetch(ctx, dir); err != nil {
			return worktreeBase{}, fmt.Errorf("worktree add: jj git fetch: %w", err)
		}
		names, err := jjTrunkBookmarkNames(ctx, dir, "worktree add")
		if err != nil {
			return worktreeBase{}, err
		}
		if len(names) != 1 {
			return worktreeBase{}, fmt.Errorf("worktree add: cannot resolve the trunk bookmark from %q — configure trunk() to resolve one tracked bookmark", names)
		}
		base = worktreeBase{rev: "trunk()", label: names[0]}
	}
	out, err := render.RunCLI(ctx, dir, "jj", []string{"--ignore-working-copy", "log", "--no-graph", "-r", base.rev, "-T", `commit_id ++ "\n"`})
	if err != nil {
		return worktreeBase{}, fmt.Errorf("worktree add: jj log %s: %w", base.rev, err)
	}
	base.shas = strings.Fields(out)
	return base, nil
}

func worktreeRevParse(ctx context.Context, dir render.Dir, rev string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", rev})
	if err != nil {
		return "", fmt.Errorf("worktree add: git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// worktreeMode resolves --jj against the shape of the checkout it was asked
// from. A jj workspace carries no .git at all, so git cannot cut a sibling off
// it; a colocated linked worktree is a shape neither tool creates, so the mode
// naming it is refused with jj's own words rather than attempted.
func worktreeMode(requested string, c vcs.Checkout) (string, error) {
	switch requested {
	case jjModeColocate:
		return "", fmt.Errorf("worktree add: --jj colocate is a shape jj refuses to create:\n%s\n"+
			"use --jj workspace for a jj-native working copy, or --jj none for a plain git worktree", jjColocateRefusal)
	case jjModeWorkspace:
		if c.Kind != vcs.JJ {
			return "", errors.New("worktree add: --jj workspace needs a jj repository; this is git — use --jj none")
		}
		return jjModeWorkspace, nil
	case jjModeNone:
		if c.Shape == vcs.ShapeJJWorkspace {
			return "", errors.New("worktree add: --jj none needs a git working copy; this checkout is a jj workspace with no .git — use --jj workspace")
		}
		return jjModeNone, nil
	case "":
		if c.Shape == vcs.ShapeJJWorkspace {
			return jjModeWorkspace, nil
		}
		return jjModeNone, nil
	default:
		return "", fmt.Errorf("worktree add: unknown --jj mode %q — one of %s, %s, %s", requested, jjModeNone, jjModeWorkspace, jjModeColocate)
	}
}

func worktreeShapeOf(mode string) string {
	if mode == jjModeWorkspace {
		return infoShape(vcs.ShapeJJWorkspace)
	}
	return infoShape(vcs.ShapeGitWorktree)
}

// mintWorktreePath places name's working copy under
// $HOME/.claude/worktrees/<main-basename>, where every other tool that cuts a
// lane on this machine already puts one, so every sibling worktree of one
// repository mints into the same directory however far apart their roots sit.
// Two repositories sharing a basename share a pool, which is the price of a
// path a person can read and a sweep can find. The home prefix is resolved
// symlink-free — the spelling git canonicalizes every registered path to — so a
// minted path equals its registry entry byte for byte.
func mintWorktreePath(ctx context.Context, prefix string, c vcs.Checkout, name string) (string, error) {
	elements := strings.Split(name, "/")
	if slices.ContainsFunc(elements, func(e string) bool { return e == "" || e == "." || e == ".." }) {
		return "", fmt.Errorf("%s: %q is not a worktree name — a name has no empty, \".\", or \"..\" /-separated element", prefix, name)
	}
	home, err := canonicalHome(ctx, prefix)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreePool(home), filepath.Base(c.MainRoot), strings.Join(elements, "-")), nil
}

func canonicalHome(ctx context.Context, prefix string) (string, error) {
	home, err := render.Home(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: resolve home directory: %w", prefix, err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return "", fmt.Errorf("%s: canonicalize home directory: %w", prefix, err)
	}
	return home, nil
}

func worktreePool(home string) string {
	return filepath.Join(home, ".claude", "worktrees")
}

func runWorktreeRm(cmd *cobra.Command, name string, opts worktreeRmOptions) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "worktree rm", workingDir(ctx), true)
	if err != nil {
		return err
	}
	minted, err := mintWorktreePath(ctx, "worktree rm", l.checkout, name)
	if err != nil {
		return err
	}
	if l.checkout.CommonDir != "" {
		list, err := vcs.Worktrees(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("worktree rm: %w", err)
		}
		target, err := matchPoolWorktree(list, name, minted)
		if err != nil {
			return err
		}
		if target != nil {
			return removeGitWorktree(ctx, cmd, l, *target, opts)
		}
		store, ok, err := worktreeThinStore(ctx, l)
		if err != nil {
			return err
		}
		if ok {
			list, err := vcs.Worktrees(ctx, store.checkout)
			if err != nil {
				return fmt.Errorf("worktree rm: %w", err)
			}
			if target, err = matchPoolWorktree(list, name, minted); err != nil || target != nil {
				if err != nil {
					return err
				}
				return removeGitWorktree(ctx, cmd, store, *target, opts)
			}
		}
	}
	workspace, err := jjWorkspaceOf(minted, l.checkout)
	if err != nil {
		return err
	}
	if workspace {
		return removeJJWorkspace(ctx, cmd, l, name, minted, opts)
	}
	return fmt.Errorf("worktree rm: no working copy named %q in this repository: %w", name, ErrNotFound)
}

func runWorktreeRmPath(cmd *cobra.Command, opts worktreeRmOptions) error {
	ctx := cmd.Context()
	if !filepath.IsAbs(opts.path) {
		return fmt.Errorf("worktree rm: --path %q is not an absolute path", opts.path)
	}
	path, err := filepath.EvalSymlinks(opts.path)
	if errors.Is(err, fs.ErrNotExist) {
		return missingWorktreeRm(ctx, filepath.Clean(opts.path))
	}
	if err != nil {
		return fmt.Errorf("worktree rm: --path: %w", err)
	}
	target, err := vcs.ResolveCheckout(path)
	var broken *vcs.BrokenCheckout
	if errors.As(err, &broken) && broken.Orphaned && broken.Root == path {
		return removeOrphanWorktree(ctx, cmd, path, opts)
	}
	if err != nil {
		return fmt.Errorf("worktree rm: --path %s: %w", path, err)
	}
	l, err := resolveWorktreeRmLane(ctx)
	if err != nil {
		return err
	}
	switch {
	case target.Kind == vcs.None:
		return fmt.Errorf("worktree rm: this repository registers no worktree at %s: %w", path, ErrNotFound)
	case target.Root != path:
		return fmt.Errorf("worktree rm: %s is inside the working copy %s, not its root", path, target.Root)
	case target.CommonDir != l.checkout.CommonDir:
		store, ok, err := worktreeThinStore(ctx, l)
		if err != nil {
			return err
		}
		if !ok || target.CommonDir != store.checkout.CommonDir {
			return fmt.Errorf("worktree rm: %s is a working copy of %s, not of this repository", path, target.CommonDir)
		}
		l = store
	}
	list, err := vcs.Worktrees(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	for i, wt := range list {
		if wt.Path == path {
			return removeGitWorktree(ctx, cmd, l, list[i], opts)
		}
	}
	return fmt.Errorf("worktree rm: this repository registers no worktree at %s: %w", path, ErrNotFound)
}

func resolveWorktreeRmLane(ctx context.Context) (lane, error) {
	l, err := resolveLane(ctx, "worktree rm", workingDir(ctx), true)
	if err != nil {
		return lane{}, err
	}
	if l.checkout.CommonDir == "" {
		return lane{}, fmt.Errorf("worktree rm: %q has no git repository behind it — --path names a git linked worktree", l.checkout.Root)
	}
	return l, nil
}

func missingWorktreeRm(ctx context.Context, path string) error {
	l, err := resolveWorktreeRmLane(ctx)
	if err != nil {
		return err
	}
	err = missingWorktreePath(ctx, l, path)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	store, ok, storeErr := worktreeThinStore(ctx, l)
	if storeErr != nil || !ok {
		return errors.Join(err, storeErr)
	}
	return missingWorktreePath(ctx, store, path)
}

func removeOrphanWorktree(ctx context.Context, cmd *cobra.Command, path string, opts worktreeRmOptions) error {
	home, err := canonicalHome(ctx, "worktree rm")
	if err != nil {
		return err
	}
	parent, err := checkOrphanWorktree(home, path)
	if err != nil {
		return err
	}
	if err := guardOrphanWorktree(ctx, path, opts.force); err != nil {
		return err
	}
	name := filepath.Base(path)
	if opts.dryRun {
		cmd.Println(strings.Join([]string{"would remove " + name, orphanShape, path}, shipSep))
		return nil
	}
	rechecked, err := checkOrphanWorktree(home, path)
	if err != nil {
		return err
	}
	if !os.SameFile(parent, rechecked) {
		return fmt.Errorf("worktree rm: %s changed while rm checked %s for live processes — run rm again", filepath.Dir(path), path)
	}
	trash := filepath.Join(home, ".Trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	dest := name + "-" + time.Now().Format("20060102-150405")
	if err := moveOrphanToTrash(path, rechecked, trash, dest); err != nil {
		return err
	}
	cmd.Println(strings.Join([]string{"removed " + name, orphanShape, "moved to Trash " + filepath.Join(trash, dest)}, shipSep))
	return nil
}

func checkOrphanWorktree(home, path string) (os.FileInfo, error) {
	if pool := worktreePool(home); filepath.Dir(filepath.Dir(path)) != pool {
		return nil, fmt.Errorf("worktree rm: %s is an orphaned checkout outside the worktree pool %s — rm moves only <pool>/<repo>/<name> orphans to the Trash", path, pool)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	if resolved != path {
		return nil, fmt.Errorf("worktree rm: %s now resolves to %s — run rm again", path, resolved)
	}
	pointer := filepath.Join(path, ".git")
	info, err := os.Lstat(pointer)
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("worktree rm: %s is not a regular gitdir file — rm moves only an orphaned linked worktree to the Trash", pointer)
	}
	_, err = vcs.ResolveCheckout(path)
	var broken *vcs.BrokenCheckout
	if !errors.As(err, &broken) || !broken.Orphaned || broken.Root != path {
		return nil, fmt.Errorf("worktree rm: %s is no longer an orphaned checkout — run rm again", path)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	return parent, nil
}

func moveOrphanToTrash(path string, parent os.FileInfo, trash, dest string) error {
	from, err := openDirNoFollow(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	held, err := from.Stat()
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	if !os.SameFile(parent, held) {
		return fmt.Errorf("worktree rm: %s changed between its check and the move — run rm again", filepath.Dir(path))
	}
	to, err := openDirNoFollow(trash)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	err = unix.Renameat(int(from.Fd()), filepath.Base(path), int(to.Fd()), dest)
	if errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("worktree rm: %s sits on another volume than the Trash %s, so rm cannot move it there and will not delete it outright — move or delete it by hand", path, trash)
	}
	if err != nil {
		return fmt.Errorf("worktree rm: move %s to the Trash: %w", path, err)
	}
	return nil
}

func openDirNoFollow(dir string) (*os.File, error) {
	file, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0) //nolint:gosec // O_NOFOLLOW|O_DIRECTORY open of a path the caller already resolved
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	return file, nil
}

func guardOrphanWorktree(ctx context.Context, path string, force bool) error {
	err := native.Guard(ctx, path)
	switch {
	case errors.Is(err, cleanup.ErrUnsupported) && force:
		return nil
	case errors.Is(err, cleanup.ErrUnsupported):
		return fmt.Errorf("worktree rm: this platform has no live-process check, so nothing proves %s is unused — --force moves it to the Trash unchecked", path)
	case err != nil:
		return fmt.Errorf("worktree rm: %w", err)
	}
	return nil
}

func worktreeThinStore(ctx context.Context, l lane) (lane, bool, error) {
	if l.checkout.Kind != vcs.Git || l.checkout.MainRoot == "" {
		return lane{}, false, nil
	}
	inStore, err := thinIsStore(ctx, l.checkout)
	if err != nil || inStore {
		return lane{}, false, err
	}
	return thinStoreOf(ctx, "worktree rm", l)
}

func missingWorktreePath(ctx context.Context, l lane, path string) error {
	list, err := vcs.Worktrees(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	for _, wt := range list {
		if wt.Path == path {
			return fmt.Errorf(`worktree rm: this repository still registers %s, but nothing exists there — "git worktree prune" drops the stale registration`, path)
		}
	}
	return fmt.Errorf("worktree rm: nothing exists at %s: %w", path, ErrNotFound)
}

// matchPoolWorktree finds the registered worktree at minted, the pool path add
// mints for name: rm removes only what add created. A worktree merely named
// name elsewhere is somebody else's checkout, refused by the path it lives at
// rather than resolved by basename — resolving it would hand rm a tree the
// user never pointed ccx at, and removing the wrong one is not undoable.
func matchPoolWorktree(list []vcs.Worktree, name, minted string) (*vcs.Worktree, error) {
	var foreign []string
	for i, wt := range list {
		if wt.Path == minted {
			return &list[i], nil
		}
		if filepath.Base(wt.Path) == filepath.Base(minted) {
			foreign = append(foreign, wt.Path)
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("worktree rm: %q is not in this repository's pool — ccx never minted %s; use ccx vcs worktree rm --path <absolute-path> for working copies ccx does not manage",
			name, strings.Join(foreign, ", "))
	}
	return nil, nil
}

func removeGitWorktree(ctx context.Context, cmd *cobra.Command, l lane, wt vcs.Worktree, opts worktreeRmOptions) error {
	if wt.Path == l.checkout.MainRoot {
		return fmt.Errorf("worktree rm: %q is the repository's own working copy, not a linked worktree", wt.Path)
	}
	if err := guardTrunkHolder(ctx, l, wt); err != nil {
		return err
	}
	name, shape := filepath.Base(wt.Path), infoShape(vcs.ShapeGitWorktree)
	if opts.dryRun {
		if cleanupDaemonized {
			if err := previewGitWorktreeRemoval(ctx, wt.Path, opts.force); err != nil {
				return err
			}
		}
		cmd.Println(strings.Join([]string{"would remove " + name, shape, wt.Path}, shipSep))
		return nil
	}
	segs := []string{"removed " + name, shape, wt.Path}
	if cleanupDaemonized {
		queued, err := queueGitWorktreeRemoval(ctx, wt.Path, opts)
		if err != nil {
			return err
		}
		cmd.Println(strings.Join(append(segs, queued...), shipSep))
		return nil
	}
	argv := []string{"worktree", "remove"}
	if opts.force {
		argv = append(argv, "--force")
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", append(argv, wt.Path)); err != nil {
		return fmt.Errorf("worktree rm: git worktree remove %s: %w", wt.Path, err)
	}
	cmd.Println(strings.Join(segs, shipSep))
	return nil
}

func previewGitWorktreeRemoval(ctx context.Context, path string, force bool) error {
	git, err := cleanupGit(ctx, "worktree rm")
	if err != nil {
		return err
	}
	if _, err := cleanupPreview(ctx)(ctx, cleanup.Request{Worktree: path, Force: force, Git: git}); err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	return nil
}

func queueGitWorktreeRemoval(ctx context.Context, path string, opts worktreeRmOptions) ([]string, error) {
	git, err := cleanupGit(ctx, "worktree rm")
	if err != nil {
		return nil, err
	}
	svc, err := cleanupService(ctx)
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	receipt, err := cleanupHandoff(ctx, func(ctx context.Context) (cleanup.Receipt, error) {
		return svc.Remove(ctx, cleanup.Request{Worktree: path, Force: opts.force, Git: git})
	})
	if err != nil {
		receipt, err = followCleanupRetry(ctx, svc, err)
	}
	if err != nil {
		return nil, fmt.Errorf("worktree rm: %w", err)
	}
	queued := "deletion queued " + receipt.JobID
	if !opts.wait {
		return []string{queued}, nil
	}
	if err := waitCleanupReceipt(ctx, receipt); err != nil {
		return nil, fmt.Errorf("worktree rm: removed %s, deletion queued %s; wait: %w", path, receipt.JobID, err)
	}
	return []string{queued, "deleted"}, nil
}

// removeJJWorkspace forgets the workspace and deletes its tree: forget alone
// drops the entry while leaving the directory on disk carrying a live-looking
// .jj/repo pointer, which reads as a working copy until jj is asked about it.
// jj has no counterpart to git worktree remove's dirty-tree refusal — forget
// happily drops a workspace whose files were never snapshotted — so rm runs
// its own: jj diff snapshots the working copy first (--ignore-working-copy
// would suppress exactly that snapshot), and a non-empty answer refuses unless
// --force says to discard it, the git path's semantics.
func removeJJWorkspace(ctx context.Context, cmd *cobra.Command, l lane, name, path string, opts worktreeRmOptions) error {
	if !opts.force {
		summary, err := render.RunCLI(ctx, render.Dir(path), "jj", []string{"diff", "--summary"})
		if err != nil {
			return fmt.Errorf("worktree rm: jj diff --summary in %s: %w", path, err)
		}
		if changes := strings.TrimSpace(summary); changes != "" {
			return fmt.Errorf("worktree rm: %s holds uncommitted changes (%s) — commit them there, or --force discards them",
				path, strings.Join(strings.Split(changes, "\n"), ", "))
		}
	}
	if opts.dryRun {
		cmd.Println(strings.Join([]string{"would remove " + name, infoShape(vcs.ShapeJJWorkspace), path}, shipSep))
		return nil
	}
	if _, err := render.RunCLI(ctx, l.dir(), "jj", []string{"workspace", "forget", name}); err != nil {
		return fmt.Errorf("worktree rm: jj workspace forget %s: %w", name, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("worktree rm: remove %s: %w", path, err)
	}
	cmd.Println(strings.Join([]string{"removed " + name, infoShape(vcs.ShapeJJWorkspace), path}, shipSep))
	return nil
}

// guardTrunkHolder refuses the one removal that is never safe: the checkout
// holding trunk pins the branch every restack rebases onto. A repository that
// designates no default branch has no trunk to protect — git remote add sets
// none until set-head — so that provable miss, and only it, skips the guard.
// Every other outcome surfaces: a git that could not answer is not a repository
// without a trunk, and reading it as one would skip a destructive-operation
// guard over a trunk that exists.
func guardTrunkHolder(ctx context.Context, l lane, wt vcs.Worktree) error {
	remote, err := vcs.GitRemoteFor(ctx, l.dir(), wt.Branch)
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	trunk, err := vcs.ResolveTrunk(ctx, l.dir(), remote)
	if errors.Is(err, vcs.ErrNoTrunk) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	if wt.Branch == trunk.Name() {
		return fmt.Errorf("worktree rm: %q holds trunk %s — every restack rebases onto it; check out another branch there first", wt.Path, trunk.Name())
	}
	return nil
}

// jjWorkspaceOf reports whether path is a jj workspace of c's repository, read
// off the pointer files rather than by asking jj: jj workspace list names its
// workspaces without saying where they are. A working copy that is simply no
// workspace of c's comes back false, the clean miss rm reads as "no such name";
// a checkout whose own pointer files resolve to nothing is an error instead,
// never that same false arrived at by accident — a broken workspace reported as
// one that never existed sends the user to delete by hand what rm would have
// forgotten from jj first.
func jjWorkspaceOf(path string, c vcs.Checkout) (bool, error) {
	ck, err := vcs.ResolveCheckout(path)
	if err != nil {
		return false, fmt.Errorf("worktree rm: resolve %s: %w", path, err)
	}
	return ck.Shape == vcs.ShapeJJWorkspace && ck.JJStore == c.JJStore, nil
}

func runWorktreeRepair(cmd *cobra.Command, dryRun bool) error {
	ctx := cmd.Context()
	l, err := resolveLaneReport(ctx, "worktree repair", workingDir(ctx), true, false)
	if err != nil {
		return err
	}
	root, paths, err := worktreeRepairPlan(ctx, l)
	if err != nil {
		return err
	}
	argv := append([]string{"worktree", "repair"}, paths...)
	if dryRun {
		cmd.Println("dry-run" + shipSep + "git -C " + root + " " + strings.Join(argv, " "))
		return nil
	}
	_, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(root), "git", argv)
	if err != nil {
		return fmt.Errorf("worktree repair: git worktree repair: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("worktree repair: git worktree repair exited %d: %s", code, strings.TrimSpace(stderr))
	}
	// git reports each pointer it rewrote on stderr and stays silent when there
	// was nothing to rewrite.
	if report := strings.TrimSpace(stderr); report != "" {
		cmd.Println(report)
		cmd.Println(worktreeCount(len(paths)) + " checked")
		return nil
	}
	cmd.Println("nothing to repair" + shipSep + worktreeCount(len(paths)) + " checked")
	return nil
}

// worktreeRepairPlan names the working copy to run git from and the paths to
// hand it. git repairs both halves of a broken link — the admin dir's gitdir
// file and the worktree's .git file — but only from a tree it can open, so a
// broken checkout is repaired from the repository its own pointer names.
func worktreeRepairPlan(ctx context.Context, l lane) (string, []string, error) {
	if l.broken != nil {
		root, err := repairRootFor(l.broken)
		if err != nil {
			return "", nil, err
		}
		return root, []string{l.root}, nil
	}
	if l.checkout.MainRoot == "" {
		return "", nil, fmt.Errorf("worktree repair: %q has no working copy to run git from", l.checkout.RepoKey())
	}
	entries, err := collectWorktrees(ctx, "worktree repair", l.checkout)
	if err != nil {
		return "", nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return l.checkout.MainRoot, paths, nil
}

// repairRootFor names the working copy a dangling gitdir pointer was written
// against: git spells a linked worktree's admin dir <common>/worktrees/<name>,
// so the repository the pointer meant is two levels above it.
func repairRootFor(b *vcs.BrokenCheckout) (string, error) {
	worktrees := filepath.Dir(b.Target)
	common := filepath.Dir(worktrees)
	if filepath.Base(worktrees) != "worktrees" || filepath.Base(common) != ".git" {
		return "", fmt.Errorf("worktree repair: %s — that pointer names no linked-worktree admin dir, so there is no repository to repair from", b.Error())
	}
	root := filepath.Dir(common)
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("worktree repair: %s — the repository it points at is gone too: %w", b.Error(), err)
	}
	return root, nil
}

func worktreeCount(n int) string {
	if n == 1 {
		return "1 working copy"
	}
	return fmt.Sprintf("%d working copies", n)
}
