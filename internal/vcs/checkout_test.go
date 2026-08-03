package vcs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// handFixtures stands up one working copy in each shape ResolveCheckout
// classifies, plus the pointers it must refuse, writing every pointer file by
// hand: these layouts are git's and jj's on-disk contract, so the table needs
// neither tool to build them and allows neither to resolve them. The live tests
// below keep the hand-written shapes honest against what the tools really write.
type handFixtures struct {
	gitMain        string
	gitLinked      string
	relLinked      string
	submodule      string
	coloc          string
	colocLinked    string
	jjWorkspace    string
	colocWS        string
	jjNoStore      string
	danglingGit    string
	danglingJJ     string
	emptyJJ        string
	danglingCommon string
	emptyCommon    string
	notAPointer    string
	plain          string
	linkedAdmin    string
	colocAdmin     string
	submodAdmin    string
	colocLinkAdm   string
}

func newHandFixtures(t *testing.T) handFixtures {
	t.Helper()
	base := canon(t, t.TempDir())
	var fx handFixtures

	fx.gitMain = filepath.Join(base, "main")
	mustMkdir(t, filepath.Join(fx.gitMain, ".git"))

	fx.gitLinked = filepath.Join(base, "linked")
	fx.linkedAdmin = filepath.Join(fx.gitMain, ".git", "worktrees", "linked")
	writeGitWorktree(t, fx.gitMain, fx.gitLinked, "linked", fx.linkedAdmin)

	// A real linked worktree's pointer is absolute; the relative form is what a
	// hand-written fixture reaches for, and both have to resolve.
	fx.relLinked = filepath.Join(base, "rel-linked")
	writeGitWorktree(t, fx.gitMain, fx.relLinked, "rel-linked", "../main/.git/worktrees/rel-linked")

	fx.submodule = filepath.Join(fx.gitMain, "vendor")
	fx.submodAdmin = filepath.Join(fx.gitMain, ".git", "modules", "vendor")
	mustMkdir(t, fx.submodAdmin)
	mustMkdir(t, fx.submodule)
	mustWriteFile(t, filepath.Join(fx.submodule, ".git"), gitdirPrefix+"../.git/modules/vendor\n")

	fx.coloc = filepath.Join(base, "coloc")
	fx.colocAdmin = filepath.Join(fx.coloc, ".git")
	mustMkdir(t, fx.colocAdmin)
	writeColocatedJJ(t, fx.coloc)

	fx.colocLinked = filepath.Join(base, "coloc-linked")
	fx.colocLinkAdm = filepath.Join(fx.colocAdmin, "worktrees", "coloc-linked")
	writeGitWorktree(t, fx.coloc, fx.colocLinked, "coloc-linked", fx.colocLinkAdm)
	writeColocatedJJ(t, fx.colocLinked)

	fx.jjWorkspace = filepath.Join(base, "ws")
	writeJJWorkspace(t, fx.jjWorkspace, filepath.Join(fx.coloc, ".jj", "repo"))
	fx.colocWS = filepath.Join(base, "coloc-ws")
	writeJJWorkspace(t, fx.colocWS, filepath.Join(fx.colocLinked, ".jj", "repo"))

	fx.jjNoStore = filepath.Join(base, "jj-no-store")
	mustMkdir(t, filepath.Join(fx.jjNoStore, ".jj"))

	fx.danglingGit = filepath.Join(base, "dangling-git")
	mustMkdir(t, fx.danglingGit)
	mustWriteFile(t, filepath.Join(fx.danglingGit, ".git"), gitdirPrefix+"../nowhere/.git/worktrees/dangling\n")

	fx.danglingJJ = filepath.Join(base, "dangling-jj")
	mustMkdir(t, filepath.Join(fx.danglingJJ, ".jj"))
	mustWriteFile(t, filepath.Join(fx.danglingJJ, ".jj", "repo"), "../../nowhere/.jj/repo\n")

	// A truncated pointer resolves to the .jj holding it, which exists — the one
	// shape a stat check alone accepts as a workspace owning its own store.
	fx.emptyJJ = filepath.Join(base, "empty-jj")
	mustMkdir(t, filepath.Join(fx.emptyJJ, ".jj"))
	mustWriteFile(t, filepath.Join(fx.emptyJJ, ".jj", "repo"), "\n")

	fx.danglingCommon = filepath.Join(base, "dangling-common")
	writeGitWorktree(t, fx.gitMain, fx.danglingCommon, "dangling-common", filepath.Join(fx.gitMain, ".git", "worktrees", "dangling-common"))
	mustWriteFile(t, filepath.Join(fx.gitMain, ".git", "worktrees", "dangling-common", "commondir"), "../../../nowhere\n")

	// An empty commondir resolves to the worktree's own admin dir, which would
	// key it apart from the repository it belongs to rather than failing.
	fx.emptyCommon = filepath.Join(base, "empty-common")
	writeGitWorktree(t, fx.gitMain, fx.emptyCommon, "empty-common", filepath.Join(fx.gitMain, ".git", "worktrees", "empty-common"))
	mustWriteFile(t, filepath.Join(fx.gitMain, ".git", "worktrees", "empty-common", "commondir"), "\n")

	fx.notAPointer = filepath.Join(base, "not-a-pointer")
	mustMkdir(t, fx.notAPointer)
	mustWriteFile(t, filepath.Join(fx.notAPointer, ".git"), "not a gitdir line\n")

	fx.plain = filepath.Join(base, "plain")
	mustMkdir(t, fx.plain)
	return fx
}

// TestResolveCheckout is the whole classification table, run with PATH pointed
// at an empty directory: any subprocess on a layout git or jj actually writes
// fails the row rather than silently costing ship's preflight a fork. A refused
// row checks the two fields the error contract promises — Kind and Root — and
// nothing else, since the rest is what failed to resolve.
func TestResolveCheckout(t *testing.T) {
	fx := newHandFixtures(t)
	gitCommon := filepath.Join(fx.gitMain, ".git")

	tests := []struct {
		name       string
		dir        string
		want       Checkout
		wantKey    string
		wantErr    bool
		wantBroken bool
	}{
		{
			// The shape every cli fixture builds: an empty .git directory.
			name: "git main checkout",
			dir:  fx.gitMain,
			want: Checkout{
				Kind: Git, Root: fx.gitMain, Shape: ShapeMain,
				GitDir: gitCommon, CommonDir: gitCommon, MainRoot: fx.gitMain,
			},
			wantKey: gitCommon,
		},
		{
			name: "linked git worktree with an absolute pointer",
			dir:  fx.gitLinked,
			want: Checkout{
				Kind: Git, Root: fx.gitLinked, Shape: ShapeGitWorktree,
				GitDir: fx.linkedAdmin, CommonDir: gitCommon, MainRoot: fx.gitMain,
			},
			wantKey: gitCommon,
		},
		{
			name: "linked git worktree with a relative pointer",
			dir:  fx.relLinked,
			want: Checkout{
				Kind: Git, Root: fx.relLinked, Shape: ShapeGitWorktree,
				GitDir:    filepath.Join(gitCommon, "worktrees", "rel-linked"),
				CommonDir: gitCommon, MainRoot: fx.gitMain,
			},
			wantKey: gitCommon,
		},
		{
			// A submodule's admin dir carries no commondir precisely because it is
			// its own repository, so it is its own common dir and keys itself.
			name: "submodule",
			dir:  fx.submodule,
			want: Checkout{
				Kind: Git, Root: fx.submodule, Shape: ShapeMain,
				GitDir: fx.submodAdmin, CommonDir: fx.submodAdmin, MainRoot: fx.submodule,
			},
			wantKey: fx.submodAdmin,
		},
		{
			name: "colocated main checkout",
			dir:  fx.coloc,
			want: Checkout{
				Kind: JJ, Root: fx.coloc, Shape: ShapeMain,
				GitDir: fx.colocAdmin, CommonDir: fx.colocAdmin, MainRoot: fx.coloc,
				JJStore: filepath.Join(fx.coloc, ".jj", "repo"),
			},
			wantKey: fx.colocAdmin,
		},
		{
			name: "linked git worktree carrying its own colocated jj",
			dir:  fx.colocLinked,
			want: Checkout{
				Kind: JJ, Root: fx.colocLinked, Shape: ShapeColocatedWorktree,
				GitDir: fx.colocLinkAdm, CommonDir: fx.colocAdmin, MainRoot: fx.coloc,
				JJStore: filepath.Join(fx.colocLinked, ".jj", "repo"),
			},
			wantKey: fx.colocAdmin,
		},
		{
			name: "jj workspace of a colocated main checkout",
			dir:  fx.jjWorkspace,
			want: Checkout{
				Kind: JJ, Root: fx.jjWorkspace, Shape: ShapeJJWorkspace,
				GitDir: fx.colocAdmin, CommonDir: fx.colocAdmin, MainRoot: fx.coloc,
				JJStore: filepath.Join(fx.coloc, ".jj", "repo"),
			},
			wantKey: fx.colocAdmin,
		},
		{
			// The store's git_target names its own workspace's .git, which here is
			// a pointer file: the chain has to be followed to reach the common dir
			// rather than stopping at the first thing named .git.
			name: "jj workspace of a colocated linked worktree",
			dir:  fx.colocWS,
			want: Checkout{
				Kind: JJ, Root: fx.colocWS, Shape: ShapeJJWorkspace,
				GitDir: fx.colocLinkAdm, CommonDir: fx.colocAdmin, MainRoot: fx.colocLinked,
				JJStore: filepath.Join(fx.colocLinked, ".jj", "repo"),
			},
			wantKey: fx.colocAdmin,
		},
		{
			// The other shape every cli fixture builds: a .jj holding no repo
			// entry, which shares no store and keys its own root.
			name:    "jj marker with no store",
			dir:     fx.jjNoStore,
			want:    Checkout{Kind: JJ, Root: fx.jjNoStore, Shape: ShapeMain, MainRoot: fx.jjNoStore},
			wantKey: fx.jjNoStore,
		},
		{
			name:    "no repository at all",
			dir:     fx.plain,
			want:    Checkout{Kind: None, Root: fx.plain, Shape: ShapeMain, MainRoot: fx.plain},
			wantKey: fx.plain,
		},
		{
			name: "gitdir pointer resolving to nothing", dir: fx.danglingGit,
			want: Checkout{Kind: Git, Root: fx.danglingGit}, wantErr: true, wantBroken: true,
		},
		{
			name: "jj workspace pointer resolving to nothing", dir: fx.danglingJJ,
			want: Checkout{Kind: JJ, Root: fx.danglingJJ}, wantErr: true, wantBroken: true,
		},
		{
			name: "empty jj workspace pointer", dir: fx.emptyJJ,
			want: Checkout{Kind: JJ, Root: fx.emptyJJ}, wantErr: true, wantBroken: true,
		},
		{
			name: "commondir resolving to nothing", dir: fx.danglingCommon,
			want: Checkout{Kind: Git, Root: fx.danglingCommon}, wantErr: true, wantBroken: true,
		},
		{
			name: "empty commondir", dir: fx.emptyCommon,
			want: Checkout{Kind: Git, Root: fx.emptyCommon}, wantErr: true, wantBroken: true,
		},
		{
			name: "git file holding no gitdir pointer", dir: fx.notAPointer,
			want: Checkout{Kind: Git, Root: fx.notAPointer}, wantErr: true, wantBroken: true,
		},
	}

	t.Setenv("PATH", t.TempDir())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveCheckout(tt.dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveCheckout(%q) = %+v, want an error", tt.dir, got)
				}
				var broken *BrokenCheckout
				if errors.As(err, &broken) != tt.wantBroken {
					t.Fatalf("ResolveCheckout(%q) error = %v, want a *BrokenCheckout: %v", tt.dir, err, tt.wantBroken)
				}
				if got.Kind != tt.want.Kind || got.Root != tt.want.Root {
					t.Fatalf("ResolveCheckout(%q) = (%v, %q), want the partial (%v, %q)", tt.dir, got.Kind, got.Root, tt.want.Kind, tt.want.Root)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveCheckout(%q): %v", tt.dir, err)
			}
			if got != tt.want {
				t.Fatalf("ResolveCheckout(%q) =\n\t%+v\nwant\n\t%+v", tt.dir, got, tt.want)
			}
			if key := got.RepoKey(); key != tt.wantKey {
				t.Errorf("RepoKey() = %q, want %q", key, tt.wantKey)
			}
			if linked := got.Linked(); linked != (tt.want.Shape != ShapeMain) {
				t.Errorf("Linked() = %v, want %v", linked, tt.want.Shape != ShapeMain)
			}
		})
	}
}

// TestResolveCheckoutRelativeCommonDir pins the one line that would defeat the
// whole re-keying while every other assertion still passed: git writes
// commondir as "../.." against the admin dir it sits in, not against the .git
// file's own directory, so a worktree resolving it wrong keys itself apart from
// the repository it belongs to.
func TestResolveCheckoutRelativeCommonDir(t *testing.T) {
	fx := newHandFixtures(t)
	c, err := ResolveCheckout(fx.gitLinked)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", fx.gitLinked, err)
	}
	want := filepath.Join(fx.gitMain, ".git")
	if c.CommonDir != want {
		t.Fatalf("CommonDir = %q, want the main .git %q", c.CommonDir, want)
	}
	if strings.Contains(c.CommonDir, "worktrees") {
		t.Errorf("CommonDir = %q, want it resolved out of the per-worktree admin dir", c.CommonDir)
	}
	if c.GitDir == c.CommonDir {
		t.Errorf("GitDir = CommonDir = %q, want the linked worktree's own admin dir", c.GitDir)
	}
}

// TestResolveCheckoutLive drives the shapes git itself writes, so the
// hand-written fixtures above stay honest, and pins the thesis of the type: a
// linked worktree and the main checkout key the same repository, byte for byte.
func TestResolveCheckoutLive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	mainCheckout, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	wantMain := Checkout{
		Kind: Git, Root: canon(t, main), Shape: ShapeMain,
		GitDir:   filepath.Join(canon(t, main), ".git"),
		MainRoot: canon(t, main),
	}
	wantMain.CommonDir = wantMain.GitDir
	if mainCheckout != wantMain {
		t.Fatalf("ResolveCheckout(%q) =\n\t%+v\nwant\n\t%+v", main, mainCheckout, wantMain)
	}

	linkedCheckout, err := ResolveCheckout(linked)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", linked, err)
	}
	wantLinked := Checkout{
		Kind: Git, Root: canon(t, linked), Shape: ShapeGitWorktree,
		GitDir:    filepath.Join(canon(t, main), ".git", "worktrees", "linked"),
		CommonDir: filepath.Join(canon(t, main), ".git"),
		MainRoot:  canon(t, main),
	}
	if linkedCheckout != wantLinked {
		t.Fatalf("ResolveCheckout(%q) =\n\t%+v\nwant\n\t%+v", linked, linkedCheckout, wantLinked)
	}
	if linkedCheckout.RepoKey() != mainCheckout.RepoKey() {
		t.Fatalf("RepoKey mismatch: linked %q, main %q", linkedCheckout.RepoKey(), mainCheckout.RepoKey())
	}
	if other, err := ResolveCheckout(initLiveGitRepo(t)); err != nil {
		t.Fatalf("ResolveCheckout of a second repo: %v", err)
	} else if other.RepoKey() == mainCheckout.RepoKey() {
		t.Errorf("separate repositories share the key %q", other.RepoKey())
	}
}

// TestResolveCheckoutSymlinkedAdminLive covers the identity a canonical root
// cannot reach: when .git is itself a symlink to an admin dir living elsewhere,
// git resolves it before writing a linked worktree's pointer, so a main checkout
// keeping the symlink spelling would key one repository under two names.
func TestResolveCheckoutSymlinkedAdminLive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	admin := filepath.Join(canon(t, t.TempDir()), "admin")
	if err := os.Rename(filepath.Join(main, ".git"), admin); err != nil {
		t.Fatalf("move admin dir: %v", err)
	}
	if err := os.Symlink(admin, filepath.Join(main, ".git")); err != nil {
		t.Fatalf("symlink admin dir: %v", err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	mainCheckout, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	if mainCheckout.CommonDir != admin {
		t.Errorf("CommonDir = %q, want the symlink target %q", mainCheckout.CommonDir, admin)
	}
	if mainCheckout.MainRoot != canon(t, main) {
		t.Errorf("MainRoot = %q, want %q", mainCheckout.MainRoot, canon(t, main))
	}
	linkedCheckout, err := ResolveCheckout(linked)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", linked, err)
	}
	if linkedCheckout.RepoKey() != mainCheckout.RepoKey() {
		t.Fatalf("RepoKey mismatch: linked %q, main %q", linkedCheckout.RepoKey(), mainCheckout.RepoKey())
	}
}

// TestResolveCheckoutNoMainRootLive covers the two layouts whose common dir is
// not a <root>/.git: a bare repository and the admin dir a --separate-git-dir
// checkout points at. Neither names a working copy — git's own worktree list
// reports the common dir itself — so MainRoot stays empty rather than naming the
// unrelated directory one level up.
func TestResolveCheckoutNoMainRootLive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	seed := initLiveGitRepo(t)
	runGit(t, seed, "branch", "-M", "trunk")
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, seed, "clone", "-q", "--bare", seed, bare)
	fromBare := filepath.Join(t.TempDir(), "from-bare")
	runGit(t, bare, "worktree", "add", "-q", fromBare, "trunk")

	base := t.TempDir()
	admin := filepath.Join(base, "admin")
	separate := filepath.Join(base, "main")
	runGit(t, base, "init", "-q", "--separate-git-dir", admin, separate)
	runGit(t, separate, "config", "user.email", "t@t.t")
	runGit(t, separate, "config", "user.name", "t")
	runGit(t, separate, "commit", "-q", "--allow-empty", "-m", "c")
	fromSeparate := filepath.Join(t.TempDir(), "from-separate")
	runGit(t, separate, "worktree", "add", "-q", "-b", "feature", fromSeparate)

	tests := []struct {
		name      string
		dir       string
		wantComon string
	}{
		{"worktree of a bare repository", fromBare, canon(t, bare)},
		{"worktree of a --separate-git-dir checkout", fromSeparate, canon(t, admin)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveCheckout(tt.dir)
			if err != nil {
				t.Fatalf("ResolveCheckout(%q): %v", tt.dir, err)
			}
			if got.CommonDir != tt.wantComon {
				t.Fatalf("CommonDir = %q, want %q", got.CommonDir, tt.wantComon)
			}
			if got.MainRoot != "" {
				t.Errorf("MainRoot = %q, want no main working copy", got.MainRoot)
			}
		})
	}
}

// TestResolveCheckoutSubmoduleLive proves the rule against git's own layout: a
// real submodule's admin dir under .git/modules carries no commondir, and that
// absence is what says it owns the dir outright rather than sharing the
// superproject's.
func TestResolveCheckoutSubmoduleLive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	sub := initLiveGitRepo(t)
	runGit(t, main, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor")

	vendor := filepath.Join(main, "vendor")
	got, err := ResolveCheckout(vendor)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", vendor, err)
	}
	admin := filepath.Join(canon(t, main), ".git", "modules", "vendor")
	want := Checkout{
		Kind: Git, Root: canon(t, vendor), Shape: ShapeMain,
		GitDir: admin, CommonDir: admin, MainRoot: canon(t, vendor),
	}
	if got != want {
		t.Fatalf("ResolveCheckout(%q) =\n\t%+v\nwant\n\t%+v", vendor, got, want)
	}
	if got.RepoKey() == filepath.Join(canon(t, main), ".git") {
		t.Errorf("submodule keys the superproject's repository %q", got.RepoKey())
	}
}

// TestResolveCheckoutJJWorkspaceLive drives a real `jj workspace add`, so the
// hand-written workspace fixtures stay honest about the layout jj writes.
func TestResolveCheckoutJJWorkspaceLive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	main := initLiveGitRepo(t)
	runJJ(t, main, "git", "init", "--colocate")
	ws := filepath.Join(t.TempDir(), "ws")
	runJJ(t, main, "workspace", "add", ws)

	got, err := ResolveCheckout(ws)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", ws, err)
	}
	want := Checkout{
		Kind: JJ, Root: canon(t, ws), Shape: ShapeJJWorkspace,
		GitDir:    filepath.Join(canon(t, main), ".git"),
		CommonDir: filepath.Join(canon(t, main), ".git"),
		MainRoot:  canon(t, main),
		JJStore:   filepath.Join(canon(t, main), ".jj", "repo"),
	}
	if got != want {
		t.Fatalf("ResolveCheckout(%q) =\n\t%+v\nwant\n\t%+v", ws, got, want)
	}
	mainCheckout, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	if got.RepoKey() != mainCheckout.RepoKey() {
		t.Fatalf("RepoKey mismatch: workspace %q, main %q", got.RepoKey(), mainCheckout.RepoKey())
	}
}

// TestWorktrees pins the porcelain parse over the attributes git emits for a
// live checkout: a branch, a detached HEAD, and a lock carrying its reason.
func TestWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	runGit(t, main, "branch", "-M", "trunk")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	held := filepath.Join(t.TempDir(), "held")
	runGit(t, main, "worktree", "add", "-q", "-b", "held", held)
	runGit(t, main, "worktree", "lock", "--reason", "an agent has it", held)
	loose := filepath.Join(t.TempDir(), "loose")
	runGit(t, main, "worktree", "add", "-q", "--detach", loose)
	head := gitOutput(t, main, "rev-parse", "HEAD")

	c, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	got, err := Worktrees(context.Background(), c)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	want := map[string]Worktree{
		canon(t, main):   {Path: canon(t, main), HEAD: head, Branch: "trunk"},
		canon(t, linked): {Path: canon(t, linked), HEAD: head, Branch: "feature"},
		canon(t, held):   {Path: canon(t, held), HEAD: head, Branch: "held", Locked: "an agent has it"},
		canon(t, loose):  {Path: canon(t, loose), HEAD: head, Detached: true},
	}
	if len(got) != len(want) {
		t.Fatalf("Worktrees returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	byPath := make(map[string]Worktree, len(got))
	for _, wt := range got {
		byPath[wt.Path] = wt
	}
	if !reflect.DeepEqual(byPath, want) {
		t.Fatalf("Worktrees =\n\t%+v\nwant\n\t%+v", byPath, want)
	}
}

// TestWorktreesBare covers a bare main repository, whose record carries no HEAD
// and no branch, and pins that the list is read from the common dir rather than
// from any working copy.
func TestWorktreesBare(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	seed := initLiveGitRepo(t)
	runGit(t, seed, "branch", "-M", "trunk")
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, seed, "clone", "-q", "--bare", seed, bare)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, bare, "worktree", "add", "-q", linked, "trunk")

	got, err := Worktrees(context.Background(), Checkout{CommonDir: canon(t, bare)})
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	want := []Worktree{
		{Path: canon(t, bare), Bare: true},
		{Path: canon(t, linked), HEAD: gitOutput(t, seed, "rev-parse", "HEAD"), Branch: "trunk"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Worktrees =\n\t%+v\nwant\n\t%+v", got, want)
	}
}

// TestWorktreesPrunable covers the attribute whose value is git's own prose
// rather than this package's contract: a worktree whose directory is gone keeps
// its branch and carries a reason passed through verbatim.
func TestWorktreesPrunable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	gone := filepath.Join(t.TempDir(), "gone")
	runGit(t, main, "worktree", "add", "-q", "-b", "abandoned", gone)
	gonePath := canon(t, gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	c, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	got, err := Worktrees(context.Background(), c)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	var found *Worktree
	for i := range got {
		if got[i].Path == gonePath {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("Worktrees dropped the removed checkout %q: %+v", gonePath, got)
	}
	if found.Branch != "abandoned" {
		t.Errorf("Branch = %q, want %q", found.Branch, "abandoned")
	}
	if found.Prunable == "" {
		t.Errorf("Prunable = %q, want git's reason", found.Prunable)
	}
}

// TestBranchHolders pins the map and every gap the doc promises, now that the
// entries derive from the worktree list rather than from a ref query: a branch
// nobody holds is absent rather than mapped to the empty string, a detached
// checkout contributes nothing at all, and a checkout whose directory is gone
// still holds its branch until someone prunes it — the answer git's own ref
// query gave, so the derivation loses no entry.
func TestBranchHolders(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	runGit(t, main, "branch", "-M", "trunk")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	loose := filepath.Join(t.TempDir(), "loose")
	runGit(t, main, "worktree", "add", "-q", "--detach", loose)
	gone := filepath.Join(t.TempDir(), "gone")
	runGit(t, main, "worktree", "add", "-q", "-b", "abandoned", gone)
	gonePath := canon(t, gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	runGit(t, main, "branch", "unheld")

	c, err := ResolveCheckout(main)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", main, err)
	}
	got, err := BranchHolders(context.Background(), c)
	if err != nil {
		t.Fatalf("BranchHolders: %v", err)
	}
	want := map[string]string{
		"trunk":     canon(t, main),
		"feature":   canon(t, linked),
		"abandoned": gonePath,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BranchHolders =\n\t%+v\nwant\n\t%+v", got, want)
	}
}

// TestBranchHoldersBare covers the repository shape whose main checkout is no
// checkout at all: the bare record carries neither branch nor HEAD, so it adds
// no entry while the linked worktree beside it does.
func TestBranchHoldersBare(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	seed := initLiveGitRepo(t)
	runGit(t, seed, "branch", "-M", "trunk")
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, seed, "clone", "-q", "--bare", seed, bare)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, bare, "worktree", "add", "-q", linked, "trunk")

	got, err := BranchHolders(context.Background(), Checkout{CommonDir: canon(t, bare)})
	if err != nil {
		t.Fatalf("BranchHolders: %v", err)
	}
	want := map[string]string{"trunk": canon(t, linked)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BranchHolders =\n\t%+v\nwant\n\t%+v", got, want)
	}
}

// TestResolveCheckoutBrokenRootCanonical pins the half of the error contract a
// fixture built on an already-canonical path cannot test: the Root returned
// beside a *BrokenCheckout is canonicalized exactly as a healthy checkout's is,
// so a caller reporting both never prints one path in two spellings.
func TestResolveCheckoutBrokenRootCanonical(t *testing.T) {
	base := canon(t, t.TempDir())
	dir := filepath.Join(base, "wt")
	mustMkdir(t, dir)
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink %q: %v", dir, err)
	}
	mustWriteFile(t, filepath.Join(dir, ".git"), gitdirPrefix+"gone/worktrees/wt\n")

	got, err := ResolveCheckout(link)
	var broken *BrokenCheckout
	if !errors.As(err, &broken) {
		t.Fatalf("ResolveCheckout(%q) error = %v, want a *BrokenCheckout", link, err)
	}
	if got.Root != dir {
		t.Errorf("Root = %q, want the canonical %q rather than the symlinked %q", got.Root, dir, link)
	}
	if got.Root != broken.Root {
		t.Errorf("Root = %q, error names %q", got.Root, broken.Root)
	}
	if got.Kind != Git {
		t.Errorf("Kind = %v, want %v", got.Kind, Git)
	}
}

func TestBrokenCheckoutError(t *testing.T) {
	err := &BrokenCheckout{Root: "/repo/wt", Target: "/repo/.git/worktrees/wt", Reason: "gitdir pointer resolves to nothing"}
	want := `broken checkout "/repo/wt": gitdir pointer resolves to nothing: "/repo/.git/worktrees/wt"`
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// writeGitWorktree hand-builds a linked git worktree: the admin dir under the
// main .git carrying the relative commondir git writes there, and the root's
// .git file naming that admin dir.
func writeGitWorktree(t *testing.T, main, root, name, pointer string) {
	t.Helper()
	admin := filepath.Join(main, ".git", "worktrees", name)
	mustMkdir(t, admin)
	mustWriteFile(t, filepath.Join(admin, "commondir"), "../..\n")
	mustMkdir(t, root)
	mustWriteFile(t, filepath.Join(root, ".git"), gitdirPrefix+pointer+"\n")
}

// writeColocatedJJ hand-builds the .jj a colocated jj repo keeps beside its
// .git, whose store points back at that .git through the store directory.
func writeColocatedJJ(t *testing.T, root string) {
	t.Helper()
	store := filepath.Join(root, ".jj", "repo", "store")
	mustMkdir(t, store)
	mustWriteFile(t, filepath.Join(store, "git_target"), "../../../.git")
}

// writeJJWorkspace hand-builds the .jj a `jj workspace add` checkout keeps: a
// repo pointer file naming the shared store relative to the .jj holding it.
func writeJJWorkspace(t *testing.T, root, store string) {
	t.Helper()
	jjDir := filepath.Join(root, ".jj")
	mustMkdir(t, jjDir)
	rel, err := filepath.Rel(jjDir, store)
	if err != nil {
		t.Fatalf("relativize %q against %q: %v", store, jjDir, err)
	}
	mustWriteFile(t, filepath.Join(jjDir, "repo"), rel)
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// canon resolves a fixture path the way ResolveCheckout does, so assertions
// compare against the symlink-free form git writes into its own pointers rather
// than the /var path a macOS temp dir hands back.
func canon(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}
	return resolved
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	cmd.Env = isolatedGitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
