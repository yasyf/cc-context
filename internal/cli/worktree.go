package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// jjColocateRefusal is jj's own answer to `jj git init --colocate` inside a git
// worktree.
const jjColocateRefusal = "Error: Cannot create a colocated jj repo inside a Git worktree.\n" +
	"Hint: Run `jj git init` in the main Git repository instead, or use `jj workspace add` to create additional jj workspaces."

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
--jj picks how the new copy attaches: "none" is a git worktree, "workspace" is a
jj workspace, and "colocate" is impossible — jj refuses to create a colocated
repo inside a git worktree. Without --jj, a jj workspace mints another workspace
and everything else mints a git worktree.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorktreeAdd(cmd, args[0], mode)
		},
	}
	cmd.Flags().StringVar(&mode, "jj", "", "how the new working copy attaches: none|workspace|colocate")
	return cmd
}

func newWorktreeRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove the working copy named <name>",
		Long: `Remove the working copy named <name>.

<name> is the working copy "add" minted under the repository's pool — rm
removes only what add created, so a worktree ccx never minted is refused and
left to "git worktree remove". Removing the checkout that holds trunk is
refused, since the branch it pins is the one every restack rebases onto, and
so is one holding uncommitted changes unless --force discards them. A jj
workspace is forgotten and its directory deleted — "jj workspace forget"
leaves the tree on disk with a live-looking pointer otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorktreeRm(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove a worktree with uncommitted changes")
	return cmd
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
	l, err := resolveLaneReport(ctx, "worktree list", workingDir(), true, false)
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
	l, err := resolveLane(ctx, "worktree add", workingDir(), true)
	if err != nil {
		return err
	}
	mode, err := worktreeMode(requested, l.checkout)
	if err != nil {
		return err
	}
	path, err := mintWorktreePath("worktree add", l.checkout, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("worktree add: mint pool for %q: %w", name, err)
	}
	switch mode {
	case jjModeWorkspace:
		if _, err := render.RunCLIDir(ctx, l.root, "jj", []string{"workspace", "add", "--name", name, path}); err != nil {
			return fmt.Errorf("worktree add: jj workspace add %s: %w", name, err)
		}
	default:
		if _, err := render.RunCLIDir(ctx, l.root, "git", []string{"worktree", "add", path}); err != nil {
			return fmt.Errorf("worktree add: git worktree add %s: %w", path, err)
		}
	}
	cmd.Println(strings.Join([]string{"added " + name, worktreeShapeOf(mode), path}, shipSep))
	return nil
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
func mintWorktreePath(prefix string, c vcs.Checkout, name string) (string, error) {
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) {
		return "", fmt.Errorf("%s: %q is not a worktree name — a name is one path element", prefix, name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s: resolve home directory: %w", prefix, err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return "", fmt.Errorf("%s: canonicalize home directory: %w", prefix, err)
	}
	return filepath.Join(home, ".claude", "worktrees", filepath.Base(c.MainRoot), name), nil
}

func runWorktreeRm(cmd *cobra.Command, name string, force bool) error {
	ctx := cmd.Context()
	l, err := resolveLane(ctx, "worktree rm", workingDir(), true)
	if err != nil {
		return err
	}
	minted, err := mintWorktreePath("worktree rm", l.checkout, name)
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
			return removeGitWorktree(ctx, cmd, l, *target, force)
		}
	}
	workspace, err := jjWorkspaceOf(minted, l.checkout)
	if err != nil {
		return err
	}
	if workspace {
		return removeJJWorkspace(ctx, cmd, l, name, minted, force)
	}
	return fmt.Errorf("worktree rm: no working copy named %q in this repository: %w", name, ErrNotFound)
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
		if filepath.Base(wt.Path) == name {
			foreign = append(foreign, wt.Path)
		}
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("worktree rm: %q is not in this repository's pool — ccx never minted %s; use git worktree remove for working copies ccx does not manage",
			name, strings.Join(foreign, ", "))
	}
	return nil, nil
}

func removeGitWorktree(ctx context.Context, cmd *cobra.Command, l lane, wt vcs.Worktree, force bool) error {
	if wt.Path == l.checkout.MainRoot {
		return fmt.Errorf("worktree rm: %q is the repository's own working copy, not a linked worktree", wt.Path)
	}
	if err := guardTrunkHolder(ctx, l, wt); err != nil {
		return err
	}
	argv := []string{"worktree", "remove"}
	if force {
		argv = append(argv, "--force")
	}
	if _, err := render.RunCLIDir(ctx, l.root, "git", append(argv, wt.Path)); err != nil {
		return fmt.Errorf("worktree rm: git worktree remove %s: %w", wt.Path, err)
	}
	cmd.Println(strings.Join([]string{"removed " + filepath.Base(wt.Path), infoShape(vcs.ShapeGitWorktree), wt.Path}, shipSep))
	return nil
}

// removeJJWorkspace forgets the workspace and deletes its tree: forget alone
// drops the entry while leaving the directory on disk carrying a live-looking
// .jj/repo pointer, which reads as a working copy until jj is asked about it.
// jj has no counterpart to git worktree remove's dirty-tree refusal — forget
// happily drops a workspace whose files were never snapshotted — so rm runs
// its own: jj diff snapshots the working copy first (--ignore-working-copy
// would suppress exactly that snapshot), and a non-empty answer refuses unless
// --force says to discard it, the git path's semantics.
func removeJJWorkspace(ctx context.Context, cmd *cobra.Command, l lane, name, path string, force bool) error {
	if !force {
		summary, err := render.RunCLIDir(ctx, path, "jj", []string{"diff", "--summary"})
		if err != nil {
			return fmt.Errorf("worktree rm: jj diff --summary in %s: %w", path, err)
		}
		if changes := strings.TrimSpace(summary); changes != "" {
			return fmt.Errorf("worktree rm: %s holds uncommitted changes (%s) — commit them there, or --force discards them",
				path, strings.Join(strings.Split(changes, "\n"), ", "))
		}
	}
	if _, err := render.RunCLIDir(ctx, l.root, "jj", []string{"workspace", "forget", name}); err != nil {
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
	remote, err := vcs.GitRemoteFor(ctx, l.root, wt.Branch)
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	trunk, err := vcs.ResolveTrunk(ctx, l.root, remote)
	if errors.Is(err, vcs.ErrNoTrunk) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("worktree rm: %w", err)
	}
	if holders[trunk.Name()] == wt.Path {
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
	l, err := resolveLaneReport(ctx, "worktree repair", workingDir(), true, false)
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
	_, code, stderr, err := render.RunCLIExitCodeDir(ctx, root, "git", argv)
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
