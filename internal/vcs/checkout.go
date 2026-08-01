package vcs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// MainRoot is the repository's own working copy, equal to Root for ShapeMain.
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
			return Checkout{}, err
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
		return Checkout{}, fmt.Errorf("stat %q: %w", gitPath, err)
	}
	if info.IsDir() {
		c.GitDir, c.CommonDir = gitPath, gitPath
		return c, nil
	}

	gitDir, common, err := gitPointer(gitPath)
	if err != nil {
		return Checkout{}, err
	}
	c.GitDir, c.CommonDir = gitDir, common
	// A .git file is not enough to make a checkout linked: a submodule's points
	// at an admin dir under .git/modules that it owns outright, sharing neither
	// refs nor objects with the superproject.
	if gitDir == common {
		return c, nil
	}
	c.MainRoot = filepath.Dir(common)
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
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
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
	store := resolveAgainst(jjDir, strings.TrimSpace(string(raw)))
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
		return Checkout{}, fmt.Errorf("read git target under %q: %w", storeDir, err)
	}
	target := resolveAgainst(storeDir, strings.TrimSpace(string(raw)))
	info, err := os.Stat(target) //nolint:gosec // the target the jj store's git_target just named, not untrusted input
	if err != nil {
		return Checkout{}, &BrokenCheckout{Root: c.Root, Target: target, Reason: "jj store git target resolves to nothing"}
	}
	if info.IsDir() {
		c.GitDir, c.CommonDir = target, target
		return c, nil
	}
	gitDir, common, err := gitPointer(target)
	if err != nil {
		return Checkout{}, err
	}
	c.GitDir, c.CommonDir = gitDir, common
	return c, nil
}

// gitPointer resolves a .git file into the admin dir it names and the common dir
// that admin dir shares with its siblings. Both pointers may be relative, and
// git writes each against a different base: the gitdir line against the .git
// file's own directory, commondir against the admin dir it sits in. An admin dir
// carrying no commondir is its own repository — a submodule's under
// .git/modules — so it is its own common dir.
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
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // the admin dir the gitdir pointer just named, not untrusted input
	if err != nil {
		return gitDir, gitDir, nil
	}
	return gitDir, resolveAgainst(gitDir, strings.TrimSpace(string(common))), nil
}

// resolveAgainst joins a pointer's target to the directory it was written
// against, leaving an already-absolute target alone. It stays lexical so two
// checkouts reaching one repository resolve to the same bytes.
func resolveAgainst(base, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Join(base, target)
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
	out, err := exec.CommandContext(ctx, "git", "--git-dir", c.CommonDir, "worktree", "list", "--porcelain", "-z").Output() //nolint:gosec // fixed git argv; only the git dir varies
	if err != nil {
		return nil, fmt.Errorf("git worktree list in %q: %w", c.CommonDir, err)
	}
	var list []Worktree
	var wt Worktree
	for _, attr := range strings.Split(string(out), "\x00") {
		if attr == "" {
			if wt.Path != "" {
				list = append(list, wt)
			}
			wt = Worktree{}
			continue
		}
		key, value, _ := strings.Cut(attr, " ")
		switch key {
		case "worktree":
			wt.Path = value
		case "HEAD":
			wt.HEAD = value
		case "branch":
			wt.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "detached":
			wt.Detached = true
		case "bare":
			wt.Bare = true
		case "locked":
			wt.Locked = value
		case "prunable":
			wt.Prunable = value
		}
	}
	return list, nil
}

// BranchHolders maps a branch to the working copy that currently has it checked
// out. The map is not total over the repository's branches: git reports a holder
// only while some checkout holds the branch, so a branch no checkout holds —
// including one a pruned worktree left behind — is simply absent, and absence
// never means the branch does not exist.
func BranchHolders(ctx context.Context, c Checkout) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "git", "--git-dir", c.CommonDir, "for-each-ref", "--format=%(refname:short)%00%(worktreepath)", "refs/heads/").Output() //nolint:gosec // fixed git argv; only the git dir varies
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref in %q: %w", c.CommonDir, err)
	}
	holders := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		branch, path, _ := strings.Cut(line, "\x00")
		if path == "" {
			continue
		}
		holders[branch] = path
	}
	return holders, nil
}
