package vcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		return "", "", &BrokenCheckout{Root: base, Target: gitDir, Reason: "gitdir pointer resolves to nothing"}
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

// BranchHolders maps a branch to the working copy that currently has it checked
// out, derived from the same worktree list Worktrees reads: a checkout's branch
// line is a full refs/heads ref, which names the branch exactly, where a
// short-form ref query would hand back a spelling git itself re-lengthens
// differently once a ref of the same name exists elsewhere. The map is not total
// over the repository's branches: only a checkout holding a branch contributes
// an entry, so a bare or detached checkout adds none and a branch nobody holds —
// including one a pruned worktree left behind — is simply absent, which never
// means the branch does not exist.
func BranchHolders(ctx context.Context, c Checkout) (map[string]string, error) {
	list, err := Worktrees(ctx, c)
	if err != nil {
		return nil, err
	}
	holders := make(map[string]string)
	for _, wt := range list {
		if wt.Branch != "" {
			holders[wt.Branch] = wt.Path
		}
	}
	return holders, nil
}
