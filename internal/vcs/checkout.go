package vcs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// gitdirPrefix opens the single line a .git file holds when it points at an
// admin dir elsewhere — a linked worktree's, or a submodule's.
const gitdirPrefix = "gitdir: "

// Shape classifies how a working copy is attached to its repository.
type Shape int

const (
	// ShapeMain is the repository's own working copy.
	ShapeMain Shape = iota
	// ShapeGitWorktree is a `git worktree add` checkout: .git is a gitdir pointer
	// file rather than a directory.
	ShapeGitWorktree
	// ShapeJJWorkspace is a `jj workspace add` checkout: .jj/repo is a pointer
	// file naming the store its siblings share.
	ShapeJJWorkspace
	// ShapeColocatedWorktree is a linked git worktree carrying its own colocated
	// .jj.
	ShapeColocatedWorktree
)

// Checkout is one working copy together with the repository it belongs to —
// the third question DetectRoot's kind and root cannot answer between them, and
// the one that separates sibling worktrees sharing a ref namespace from
// unrelated repositories.
type Checkout struct {
	Kind  Kind
	Root  string
	Shape Shape
	// GitDir is this checkout's own git admin dir — <CommonDir>/worktrees/<name>
	// for a linked worktree, CommonDir itself for everything else.
	GitDir string
	// CommonDir is the git dir every sibling checkout shares, holding the refs,
	// objects, and Graphite state they contend over. Empty for a jj repository
	// with no git backing.
	CommonDir string
	// MainRoot is the repository's own working copy, equal to Root for ShapeMain
	// and empty for a repository that has none — a bare repository, or the admin
	// dir a --separate-git-dir checkout points at, neither of which names the
	// working copy behind it.
	MainRoot string
	// JJStore is the .jj/repo every workspace of a jj repository shares; empty
	// when Kind is not JJ.
	JJStore string
}

// Linked reports whether this checkout is a linked working copy rather than the
// repository's own.
func (c Checkout) Linked() bool { return c.Shape != ShapeMain }

// RepoKey identifies the repository c belongs to: two checkouts with the same
// key share one ref namespace and one Graphite database, however far apart their
// roots sit. A directory in no repository keys itself.
func (c Checkout) RepoKey() string {
	if c.CommonDir != "" {
		return c.CommonDir
	}
	if c.JJStore != "" {
		return c.JJStore
	}
	return c.Root
}

// BrokenCheckout is a working copy whose pointer into its repository resolves to
// nothing. It is deliberately distinct from every "not found" a caller might be
// asking about: a gitdir pointer that resolves nowhere is a broken repository,
// not a repository lacking Graphite, and answering the narrower question over a
// checkout in this state is a lie.
type BrokenCheckout struct {
	Root   string
	Target string
	Reason string
	// Orphaned reports a linked worktree whose admin dir does not exist: the
	// repository it was cut from deleted it, or was itself re-cloned or removed.
	Orphaned bool
}

func (e *BrokenCheckout) Error() string {
	return fmt.Sprintf("broken checkout %q: %s: %q", e.Root, e.Reason, e.Target)
}

// ResolveCheckout classifies dir's working copy and resolves the repository
// behind it, reading the pointer files git and jj write rather than asking
// either tool: it spawns no subprocess at all, so a preflight that pins its own
// argv can call it. A directory in no repository is not a failure — it resolves
// to a Kind None checkout keyed on itself.
//
// Every path it reports is absolute and symlink-free, because git canonicalizes
// the gitdir pointer it writes into a linked worktree: a root left as the caller
// spelled it would key that worktree's repository differently from the main
// checkout's, which is the one thing RepoKey exists to prevent.
//
// An error comes back with what was established before it, not the zero
// Checkout, and exactly two of those fields are trustworthy: Kind and Root, the
// latter in the canonical form above — enough to name the working copy in the
// same spelling a healthy one would use. Shape, GitDir, CommonDir, MainRoot,
// JJStore, and the RepoKey and Linked derived from them are unset or provisional
// on that path and a caller may rely on none of them: the failure is precisely
// that the repository behind the working copy did not resolve.
func ResolveCheckout(dir string) (Checkout, error) {
	kind, detected := DetectRoot(dir)
	if kind == None {
		detected = dir
	}
	root, err := canonicalRoot(detected)
	if err != nil {
		return Checkout{}, err
	}
	c := Checkout{Kind: kind, Root: root, Shape: ShapeMain, MainRoot: root}
	if kind == None {
		return c, nil
	}

	if kind == JJ {
		store, linked, err := jjStore(root)
		if err != nil {
			return c, err
		}
		c.JJStore = store
		if linked {
			c.Shape = ShapeJJWorkspace
			c.MainRoot = filepath.Dir(filepath.Dir(store))
		}
	}

	gitPath := filepath.Join(root, ".git")
	info, err := os.Stat(gitPath)
	if os.IsNotExist(err) {
		if c.JJStore == "" {
			return c, nil
		}
		return jjGitBacking(c)
	}
	if err != nil {
		return c, fmt.Errorf("stat %q: %w", gitPath, err)
	}
	if info.IsDir() {
		admin := canonical(gitPath)
		c.GitDir, c.CommonDir = admin, admin
		return c, nil
	}

	gitDir, common, err := gitPointer(gitPath)
	if err != nil {
		return c, err
	}
	c.GitDir, c.CommonDir = gitDir, common
	// A .git file is not enough to make a checkout linked: a submodule's points
	// at an admin dir under .git/modules that it owns outright, sharing neither
	// refs nor objects with the superproject.
	if gitDir == common {
		return c, nil
	}
	c.MainRoot = mainRootOf(common)
	c.Shape = ShapeGitWorktree
	if kind == JJ {
		c.Shape = ShapeColocatedWorktree
	}
	return c, nil
}

// canonicalRoot returns root absolute and symlink-free, the form git writes into
// its own pointer files. A root that cannot be resolved keeps its absolute form.
func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", root, err)
	}
	return canonical(abs), nil
}

// canonical resolves path's symlinks, the form git writes into the pointer files
// of a linked worktree — including the case a canonical root does not cover, a
// .git that is itself a symlink to an admin dir living elsewhere. A path that
// resolves to nothing keeps the spelling it was named by, so a broken pointer is
// reported as written.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// mainRootOf names the working copy behind a common dir, the way git's own
// worktree list does: <root>/.git is the only layout naming one, so a bare
// repository or the admin dir a --separate-git-dir checkout points at has none.
func mainRootOf(common string) string {
	if filepath.Base(common) != ".git" {
		return ""
	}
	return filepath.Dir(common)
}

// jjStore resolves the .jj/repo every workspace of root's repository shares, and
// reports whether root reached it through a workspace pointer file rather than
// owning it outright. A .jj holding no repo entry shares no store, which leaves
// the checkout keyed on its own root.
func jjStore(root string) (string, bool, error) {
	jjDir := filepath.Join(root, ".jj")
	repo := filepath.Join(jjDir, "repo")
	info, err := os.Stat(repo)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("stat %q: %w", repo, err)
	}
	if info.IsDir() {
		return repo, false, nil
	}
	raw, err := os.ReadFile(repo) //nolint:gosec // the .jj/repo pointer under the caller's own root, not untrusted input
	if err != nil {
		return "", false, fmt.Errorf("read %q: %w", repo, err)
	}
	pointer := strings.TrimSpace(string(raw))
	if pointer == "" {
		return "", false, &BrokenCheckout{Root: root, Target: repo, Reason: "jj workspace pointer is empty"}
	}
	store := resolveAgainst(jjDir, pointer)
	if _, err := os.Stat(store); err != nil { //nolint:gosec // the store the .jj/repo pointer just named, not untrusted input
		return "", false, &BrokenCheckout{Root: root, Target: store, Reason: "jj workspace pointer resolves to nothing"}
	}
	return store, true, nil
}

// jjGitBacking resolves the git repository behind a jj store that has no .git
// beside it. The store's git_target may name a linked worktree's own .git
// pointer file, so the target is followed through the gitdir chain rather than
// taken as the common dir; a store with no git_target at all has no git backing
// and leaves CommonDir empty for RepoKey to fall past.
func jjGitBacking(c Checkout) (Checkout, error) {
	storeDir := filepath.Join(c.JJStore, "store")
	raw, err := os.ReadFile(filepath.Join(storeDir, "git_target")) //nolint:gosec // the jj store under the caller's own root, not untrusted input
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read git target under %q: %w", storeDir, err)
	}
	target := resolveAgainst(storeDir, strings.TrimSpace(string(raw)))
	info, err := os.Stat(target) //nolint:gosec // the target the jj store's git_target just named, not untrusted input
	if err != nil {
		return c, &BrokenCheckout{Root: c.Root, Target: target, Reason: "jj store git target resolves to nothing"}
	}
	if info.IsDir() {
		c.GitDir, c.CommonDir = target, target
		return c, nil
	}
	gitDir, common, err := gitPointer(target)
	if err != nil {
		return c, err
	}
	c.GitDir, c.CommonDir = gitDir, common
	return c, nil
}

// gitPointer resolves a .git file into the admin dir it names and the common dir
// that admin dir shares with its siblings. Both pointers may be relative, and
// git writes each against a different base: the gitdir line against the .git
// file's own directory, commondir against the admin dir it sits in. An admin dir
// carrying no commondir file at all is its own repository — a submodule's under
// .git/modules — so it is its own common dir; a commondir that is present but
// unreadable, empty, or resolving to nothing is a broken repository instead,
// never that same answer arrived at by accident.
func gitPointer(gitFile string) (string, string, error) {
	base := filepath.Dir(gitFile)
	raw, err := os.ReadFile(gitFile) //nolint:gosec // the .git pointer under the caller's own root, not untrusted input
	if err != nil {
		return "", "", fmt.Errorf("read %q: %w", gitFile, err)
	}
	line := strings.TrimSpace(string(raw))
	pointer, ok := strings.CutPrefix(line, gitdirPrefix)
	if !ok {
		return "", "", &BrokenCheckout{Root: base, Target: line, Reason: "git file holds no gitdir pointer"}
	}
	gitDir := resolveAgainst(base, pointer)
	if _, err := os.Stat(gitDir); err != nil { //nolint:gosec // the admin dir the gitdir pointer just named, not untrusted input
		return "", "", &BrokenCheckout{Root: base, Target: gitDir, Reason: "gitdir pointer resolves to nothing", Orphaned: errors.Is(err, fs.ErrNotExist)}
	}
	commonFile := filepath.Join(gitDir, "commondir")
	raw, err = os.ReadFile(commonFile) //nolint:gosec // the admin dir the gitdir pointer just named, not untrusted input
	if os.IsNotExist(err) {
		return gitDir, gitDir, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read %q: %w", commonFile, err)
	}
	target := strings.TrimSpace(string(raw))
	if target == "" {
		return "", "", &BrokenCheckout{Root: base, Target: commonFile, Reason: "commondir pointer is empty"}
	}
	common := resolveAgainst(gitDir, target)
	if _, err := os.Stat(common); err != nil { //nolint:gosec // the common dir the commondir pointer just named, not untrusted input
		return "", "", &BrokenCheckout{Root: base, Target: common, Reason: "commondir pointer resolves to nothing"}
	}
	return gitDir, common, nil
}

// resolveAgainst joins a pointer's target to the directory it was written
// against, leaving an already-absolute target alone, then resolves the result's
// symlinks so two checkouts reaching one repository land on the same bytes.
func resolveAgainst(base, target string) string {
	if filepath.IsAbs(target) {
		return canonical(filepath.Clean(target))
	}
	return canonical(filepath.Join(base, target))
}

// Worktree is one entry of a repository's git worktree list. Path is git's own
// canonicalization of the checkout root and need not match a Checkout.Root that
// reached the same tree through a symlink. Locked carries the lock's reason,
// which is empty both for an unlocked checkout and for a lock taken without one.
type Worktree struct {
	Path     string
	HEAD     string
	Branch   string
	Detached bool
	Bare     bool
	Locked   string
	Prunable string
}

// Worktrees lists every working copy registered against c's repository,
// including c itself and the main one.
func Worktrees(ctx context.Context, c Checkout) ([]Worktree, error) {
	records, err := GitPorcelainRecords(ctx, GitArgs{Dir: render.Dir(c.Root), GitDir: c.CommonDir, Sub: []string{"worktree", "list"}})
	if err != nil {
		return nil, fmt.Errorf("git worktree list in %q: %w", c.CommonDir, err)
	}
	var list []Worktree
	for _, rec := range records {
		wt := Worktree{
			Path:     rec["worktree"],
			HEAD:     rec["HEAD"],
			Branch:   strings.TrimPrefix(rec["branch"], "refs/heads/"),
			Locked:   rec["locked"],
			Prunable: rec["prunable"],
		}
		_, wt.Detached = rec["detached"]
		_, wt.Bare = rec["bare"]
		list = append(list, wt)
	}
	return list, nil
}

// Holding is a working copy registered against a repository and the branch its
// HEAD names, empty when that HEAD is detached.
type Holding struct {
	Path   string
	Branch string
}

// Holdings lists the working copies git's worktree list names, with the branch
// each holds, read straight from the admin dir every checkout keeps under the
// common dir rather than through git worktree list, which resolves every HEAD
// to an object id and takes minutes over thousands of worktrees. A bare main
// repository holds nothing, and a main checkout whose common dir is not
// <root>/.git is named by the common dir, as git names it. A reftable
// repository keeps no symbolic HEAD in its admin dirs, so its list still comes
// from git. A linked working copy whose admin dir sits outside
// <common>/worktrees, which git's own list never names, is listed too: it is
// still the working copy holding its branch, git branch -d no longer refuses to
// delete that branch, and a caller that moves the branch must move it as well.
// The calling working copy is always listed, even when its own admin dir under
// <common>/worktrees no longer names it, as a removal the deletion queue has
// not yet run leaves it.
func Holdings(ctx context.Context, c Checkout) ([]Holding, error) {
	holdings, err := registeredHoldings(ctx, c)
	if err != nil || c.GitDir == c.CommonDir {
		return holdings, err
	}
	if slices.ContainsFunc(holdings, func(h Holding) bool { return h.Path == c.Root }) || slices.ContainsFunc(holdings, func(h Holding) bool { return canonical(h.Path) == c.Root }) {
		return holdings, nil
	}
	branch, err := unlistedBranch(ctx, c)
	if err != nil {
		return nil, err
	}
	return append(holdings, Holding{Path: c.Root, Branch: branch}), nil
}

func unlistedBranch(ctx context.Context, c Checkout) (string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(c.Root), "git", []string{"symbolic-ref", "--quiet", "HEAD"})
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref HEAD in %q: %w", c.Root, err)
	}
	if code > 1 {
		return "", fmt.Errorf("git symbolic-ref HEAD in %q: exit %d: %s", c.Root, code, strings.TrimSpace(stderr))
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "refs/heads/"), nil
}

func registeredHoldings(ctx context.Context, c Checkout) ([]Holding, error) {
	if _, err := os.Stat(filepath.Join(c.CommonDir, "reftable")); err == nil {
		list, err := Worktrees(ctx, c)
		if err != nil {
			return nil, err
		}
		holdings := make([]Holding, 0, len(list))
		for _, wt := range list {
			holdings = append(holdings, Holding{Path: wt.Path, Branch: wt.Branch})
		}
		return holdings, nil
	}
	var holdings []Holding
	bare, err := bareRepository(ctx, c.CommonDir)
	if err != nil {
		return nil, err
	}
	if !bare {
		branch, err := headBranch(c.CommonDir)
		if err != nil {
			return nil, err
		}
		holdings = append(holdings, Holding{Path: cmp.Or(mainRootOf(c.CommonDir), c.CommonDir), Branch: branch})
	}
	linked, err := adminHoldings(c.CommonDir)
	if err != nil {
		return nil, err
	}
	return append(holdings, linked...), nil
}

// adminHoldings reads every linked working copy from its own admin dir: the
// ones registered under common/worktrees and the ones moved out of it.
func adminHoldings(common string) ([]Holding, error) {
	linked, err := linkedHoldings(common)
	if err != nil {
		return nil, err
	}
	retired, err := retiredHoldings(common)
	if err != nil {
		return nil, err
	}
	return append(linked, retired...), nil
}

// linkedHoldings reads the linked working copies registered under common, each
// from its own admin dir.
func linkedHoldings(common string) ([]Holding, error) {
	admins, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the worktrees of %q: %w", common, err)
	}
	var holdings []Holding
	for _, admin := range admins {
		dir := filepath.Join(common, "worktrees", admin.Name())
		pointer, err := os.ReadFile(filepath.Join(dir, "gitdir")) //nolint:gosec // git's own admin file under the repository's common dir
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the gitdir of worktree %q: %w", dir, err)
		}
		path := strings.TrimSuffix(strings.TrimSpace(string(pointer)), "/.git")
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = resolveAgainst(dir, path)
		}
		branch, err := headBranch(dir)
		if err != nil {
			return nil, err
		}
		holdings = append(holdings, Holding{Path: path, Branch: branch})
	}
	return holdings, nil
}

// retiredHoldings reads the linked working copies whose admin dir was moved
// out of common/worktrees into another directory under common, with the
// working copy's .git file rewritten to follow it. Only an admin dir its
// working copy points back at counts, so a stale copy left behind holds nothing.
func retiredHoldings(common string) ([]Holding, error) {
	groups, err := os.ReadDir(common)
	if err != nil {
		return nil, fmt.Errorf("list the common dir %q: %w", common, err)
	}
	var holdings []Holding
	for _, group := range groups {
		if !group.IsDir() || group.Name() == "worktrees" {
			continue
		}
		admins, err := os.ReadDir(filepath.Join(common, group.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", filepath.Join(common, group.Name()), err)
		}
		for _, admin := range admins {
			if !admin.IsDir() {
				continue
			}
			dir := filepath.Join(common, group.Name(), admin.Name())
			path, ok := retiredWorktree(dir)
			if !ok {
				continue
			}
			branch, err := headBranch(dir)
			if err != nil {
				return nil, err
			}
			holdings = append(holdings, Holding{Path: path, Branch: branch})
		}
	}
	return holdings, nil
}

// retiredWorktree names the working copy admin belongs to when admin's gitdir
// names a .git file that points straight back at admin.
func retiredWorktree(admin string) (string, bool) {
	link, err := os.ReadFile(filepath.Join(admin, "gitdir")) //nolint:gosec // an admin file under the repository's common dir
	if err != nil {
		return "", false
	}
	dotGit := strings.TrimSpace(string(link))
	if !filepath.IsAbs(dotGit) {
		dotGit = filepath.Join(admin, dotGit)
	}
	pointer, err := os.ReadFile(dotGit) //nolint:gosec // the worktree .git file git recorded in admin/gitdir
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(pointer)), gitdirPrefix)
	if !ok {
		return "", false
	}
	want, err := os.Stat(admin)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(dotGit), target)
	}
	got, err := os.Stat(target) //nolint:gosec // only compared against admin, never read
	if err != nil || !os.SameFile(want, got) {
		return "", false
	}
	return filepath.Dir(dotGit), true
}

// CoHolders names the branch c has checked out and every working copy holding
// it, c first. It reads admin files alone, spawning git only for a reftable
// repository or when c is linked and the main checkout's HEAD names the same
// branch, where core.bare decides whether that main checkout holds it. A
// detached c holds no branch and names no holder.
func CoHolders(ctx context.Context, c Checkout) (string, []string, error) {
	if _, err := os.Stat(filepath.Join(c.CommonDir, "reftable")); err == nil {
		list, err := Holdings(ctx, c)
		if err != nil {
			return "", nil, err
		}
		return coHolders(c, list)
	}
	branch, err := headBranch(c.GitDir)
	if err != nil || branch == "" {
		return "", nil, err
	}
	list, err := adminHoldings(c.CommonDir)
	if err != nil {
		return "", nil, err
	}
	main, err := headBranch(c.CommonDir)
	if err != nil {
		return "", nil, err
	}
	if main == branch {
		holds := c.GitDir == c.CommonDir
		if !holds {
			bare, err := bareRepository(ctx, c.CommonDir)
			if err != nil {
				return "", nil, err
			}
			holds = !bare
		}
		if holds {
			list = append(list, Holding{Path: cmp.Or(mainRootOf(c.CommonDir), c.CommonDir), Branch: main})
		}
	}
	return coHolders(c, list)
}

func coHolders(c Checkout, list []Holding) (string, []string, error) {
	var branch string
	var others []string
	for _, h := range list {
		if canonical(h.Path) == c.Root {
			branch = h.Branch
		}
	}
	if branch == "" {
		return "", nil, nil
	}
	for _, h := range list {
		if h.Branch == branch && canonical(h.Path) != c.Root {
			others = append(others, h.Path)
		}
	}
	return branch, append([]string{c.Root}, others...), nil
}

// headBranch reads the branch an admin dir's HEAD names symbolically, the empty
// string for a detached HEAD or none at all, which git lists without a branch.
func headBranch(admin string) (string, error) {
	head, err := os.ReadFile(filepath.Join(admin, "HEAD")) //nolint:gosec // git's own admin file under the repository's common dir
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the HEAD of %q: %w", admin, err)
	}
	ref, symbolic := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if !symbolic {
		return "", nil
	}
	return ref, nil
}

// bareRepository reads core.bare, the setting git's worktree list reads to keep
// a bare main repository's HEAD from naming a branch anyone holds.
func bareRepository(ctx context.Context, common string) (bool, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(common), "git", []string{"--git-dir", common, "config", "--bool", "core.bare"})
	if err != nil {
		return false, fmt.Errorf("git config core.bare: %w", err)
	}
	switch code {
	case 0:
		return strings.TrimSpace(out) == "true", nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git config core.bare: exit %d: %s", code, strings.TrimSpace(stderr))
	}
}

// BranchHolders maps a branch to the working copy that currently has it checked
// out, derived from Holdings: a checkout's HEAD names its branch as a full
// refs/heads ref, which names the branch exactly, where a short-form ref query
// would hand back a spelling git itself re-lengthens differently once a ref of
// the same name exists elsewhere. The map is not total over the repository's
// branches: only a checkout holding a branch contributes an entry, so a bare or
// detached checkout adds none and a branch nobody holds — including one a pruned
// worktree left behind — is simply absent, which never means the branch does
// not exist.
func BranchHolders(ctx context.Context, c Checkout) (map[string]string, error) {
	list, err := Holdings(ctx, c)
	if err != nil {
		return nil, err
	}
	holders := make(map[string]string)
	for _, h := range list {
		if h.Branch != "" {
			holders[h.Branch] = h.Path
		}
	}
	return holders, nil
}
