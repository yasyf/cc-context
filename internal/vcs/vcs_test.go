package vcs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetect(t *testing.T) {
	root := t.TempDir()

	jjDir := filepath.Join(root, "jjrepo")
	mustMkdir(t, filepath.Join(jjDir, ".jj"))
	mustMkdir(t, filepath.Join(jjDir, ".git")) // colocated: jj wins
	colocatedChild := filepath.Join(jjDir, "pkg", "sub")
	mustMkdir(t, colocatedChild)

	gitDir := filepath.Join(root, "gitrepo")
	mustMkdir(t, filepath.Join(gitDir, ".git"))
	gitChild := filepath.Join(gitDir, "internal")
	mustMkdir(t, gitChild)

	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)

	tests := []struct {
		id   string
		dir  string
		want Kind
	}{
		{"jj root", jjDir, JJ},
		{"jj wins over colocated git", jjDir, JJ},
		{"jj from nested child", colocatedChild, JJ},
		{"git root", gitDir, Git},
		{"git from nested child", gitChild, Git},
		{"none", plain, None},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := Detect(tt.dir); got != tt.want {
				t.Fatalf("Detect(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}

func TestDetectRoot(t *testing.T) {
	root := t.TempDir()

	jjDir := filepath.Join(root, "jjrepo")
	mustMkdir(t, filepath.Join(jjDir, ".jj"))
	mustMkdir(t, filepath.Join(jjDir, ".git")) // colocated: jj wins
	colocatedChild := filepath.Join(jjDir, "pkg", "sub")
	mustMkdir(t, colocatedChild)

	gitDir := filepath.Join(root, "gitrepo")
	mustMkdir(t, filepath.Join(gitDir, ".git"))
	gitChild := filepath.Join(gitDir, "internal")
	mustMkdir(t, gitChild)

	plain := filepath.Join(root, "plain")
	mustMkdir(t, plain)

	tests := []struct {
		id       string
		dir      string
		wantKind Kind
		wantRoot string
	}{
		{"jj root", jjDir, JJ, jjDir},
		{"jj colocated child resolves to jj root", colocatedChild, JJ, jjDir},
		{"git root", gitDir, Git, gitDir},
		{"git nested child resolves to git root", gitChild, Git, gitDir},
		{"none", plain, None, ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			gotKind, gotRoot := DetectRoot(tt.dir)
			if gotKind != tt.wantKind || gotRoot != tt.wantRoot {
				t.Fatalf("DetectRoot(%q) = (%v, %q), want (%v, %q)", tt.dir, gotKind, gotRoot, tt.wantKind, tt.wantRoot)
			}
		})
	}
}

func TestGraphiteRepo(t *testing.T) {
	root := t.TempDir()

	withConfig := filepath.Join(root, "withconfig")
	mustMkdir(t, filepath.Join(withConfig, ".git"))
	mustWriteFile(t, filepath.Join(withConfig, ".git", ".graphite_repo_config"), "{}")

	withoutConfig := filepath.Join(root, "withoutconfig")
	mustMkdir(t, filepath.Join(withoutConfig, ".git"))

	jjOnly := filepath.Join(root, "jjonly")
	mustMkdir(t, filepath.Join(jjOnly, ".jj"))

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "graphite config present", dir: withConfig, want: true},
		{name: "git dir without graphite config", dir: withoutConfig},
		{name: "jj repository with no git backing", dir: jjOnly},
		{name: "no repository at all", dir: root},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ResolveCheckout(tt.dir)
			if err != nil {
				t.Fatalf("ResolveCheckout(%q): %v", tt.dir, err)
			}
			got, err := GraphiteRepo(c)
			if err != nil {
				t.Fatalf("GraphiteRepo(%q): %v", tt.dir, err)
			}
			if got != tt.want {
				t.Fatalf("GraphiteRepo(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}

// TestGraphiteRepoDangling pins the distinction GraphiteRepo no longer has to
// make itself: a .git file whose gitdir pointer resolves nowhere is a broken
// repository, not a repository without Graphite, and ResolveCheckout types it
// as such rather than letting the narrower question answer false.
func TestGraphiteRepoDangling(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dangling")
	mustMkdir(t, dir)
	mustWriteFile(t, filepath.Join(dir, ".git"), "gitdir: ../nowhere/.git/worktrees/dangling\n")

	_, err := ResolveCheckout(dir)
	var broken *BrokenCheckout
	if !errors.As(err, &broken) {
		t.Fatalf("ResolveCheckout(%q) error = %v, want a *BrokenCheckout", dir, err)
	}
}

// TestGraphiteRepoUnreadable pins the distinction the bool alone cannot carry: a
// config that exists but cannot be stat'd must not answer the same false a repo
// Graphite has never seen answers, since that false routes a mutation off the gt
// lane.
func TestGraphiteRepoUnreadable(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	mustMkdir(t, common)
	config := filepath.Join(common, ".graphite_repo_config")
	if err := os.Symlink(config, config); err != nil {
		t.Fatalf("symlink %q: %v", config, err)
	}

	got, err := GraphiteRepo(Checkout{CommonDir: common})
	if err == nil {
		t.Fatalf("GraphiteRepo = (%v, nil), want a stat error", got)
	}
	if got {
		t.Errorf("GraphiteRepo = %v alongside the error, want false", got)
	}
}

// TestGraphiteRepoWorktree drives GraphiteRepo over a real linked worktree,
// where .git is a gitdir pointer file and the Graphite config lives in the
// common dir the main worktree owns — the same common dir that keys both
// checkouts onto one repository.
func TestGraphiteRepoWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	main := initLiveGitRepo(t)
	mustWriteFile(t, filepath.Join(main, ".git", ".graphite_repo_config"), "{}")
	linked := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	tests := []struct {
		name      string
		dir       string
		want      bool
		wantShape Shape
	}{
		{"main worktree", main, true, ShapeMain},
		{"linked worktree resolves the common dir", linked, true, ShapeGitWorktree},
		{"repo with no graphite config", initLiveGitRepo(t), false, ShapeMain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ResolveCheckout(tt.dir)
			if err != nil {
				t.Fatalf("ResolveCheckout(%q): %v", tt.dir, err)
			}
			if c.Shape != tt.wantShape {
				t.Errorf("Shape = %v, want %v", c.Shape, tt.wantShape)
			}
			got, err := GraphiteRepo(c)
			if err != nil {
				t.Fatalf("GraphiteRepo(%q): %v", tt.dir, err)
			}
			if got != tt.want {
				t.Fatalf("GraphiteRepo(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}

	mainKey := mustRepoKey(t, main)
	if linkedKey := mustRepoKey(t, linked); linkedKey != mainKey {
		t.Fatalf("RepoKey mismatch: linked %q, main %q", linkedKey, mainKey)
	}
	if mainKey != filepath.Join(canon(t, main), ".git") {
		t.Fatalf("RepoKey = %q, want the main .git", mainKey)
	}
}

func mustRepoKey(t *testing.T, dir string) string {
	t.Helper()
	c, err := ResolveCheckout(dir)
	if err != nil {
		t.Fatalf("ResolveCheckout(%q): %v", dir, err)
	}
	return c.RepoKey()
}

func TestTranslateRevset(t *testing.T) {
	tests := []struct {
		id     string
		source string
		want   translation
	}{
		{"empty is working tree", "", translationWorkingTree},
		{"uncommitted is working tree", "uncommitted", translationWorkingTree},
		{"@- maps to HEAD", "@-", translationHEAD},
		{"bare @ is jj-only", "@", translationJJOnly},
		{"staged maps to staged", "staged", translationStaged},
		{"trunk()..@ is default branch", "trunk()..@", translationDefaultBranch},
		{"main..@ is default branch", "main..@", translationDefaultBranch},
		{"master..@ is default branch", "master..@", translationDefaultBranch},
		{"HEAD~1 is ref vs working", "HEAD~1", translationRefVsWorking},
		{"git range passes through", "main..feat", translationPassthrough},
		{"sha is ref vs working", "a1b2c3d", translationRefVsWorking},
		{"branch name is ref vs working", "feature-x", translationRefVsWorking},
		{"single ref is ref vs working", "feature", translationRefVsWorking},
		{"@+ marker is jj-only", "@+", translationJJOnly},
		{"@-- chain is a git candidate", "@--", translationRefVsWorking},
		{"dag range is jj-only", "::@", translationJJOnly},
		{"ancestors operator is jj-only", "foo::bar", translationJJOnly},
		{"union operator is jj-only", "main | feat", translationJJOnly},
		{"intersection operator is jj-only", "x&y", translationJJOnly},
		{"negation operator is jj-only", "~x", translationJJOnly},
		{"embedded-@ ref is a git candidate resolveJJ disambiguates", "show@op", translationRefVsWorking},
		{"embedded-@ range stays jj (git cannot rev-parse a range)", "main@origin..feat", translationJJOnly},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := translateRevset(tt.source); got != tt.want {
				t.Fatalf("translateRevset(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

// TestShowFileArgv pins the base-image argv for each lane and that an absent VCS
// panics rather than returning a silent empty argv.
func TestShowFileArgv(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		path      string
		want      []string
		wantPanic bool
	}{
		{
			name: "git shows the HEAD blob past end-of-options", kind: Git, path: "internal/cli/ship.go",
			want: []string{"git", "show", "--end-of-options", "HEAD:internal/cli/ship.go"},
		},
		{
			name: "jj shows the parent revision", kind: JJ, path: "internal/cli/ship.go",
			want: []string{"jj", "--ignore-working-copy", "file", "show", "-r", "@-", "--", `root:"internal/cli/ship.go"`},
		},
		{name: "no vcs panics", kind: None, path: "a.go", wantPanic: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantPanic {
				defer func() {
					if recover() == nil {
						t.Errorf("ShowFileArgv(%v) did not panic", tt.kind)
					}
				}()
				ShowFileArgv(tt.kind, tt.path)
				return
			}
			if got := ShowFileArgv(tt.kind, tt.path); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ShowFileArgv = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

// initLiveGitRepo stands up a real git repo with two commits, so a relative ref
// like HEAD~1 resolves against real history.
func initLiveGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@t.t")
	runGit(t, dir, "config", "user.name", "t")
	seed := filepath.Join(dir, "seed.txt")
	for i, content := range []string{"one\n", "two\n"} {
		if err := os.WriteFile(seed, []byte(content), 0o600); err != nil {
			t.Fatalf("write seed rev %d: %v", i, err)
		}
		runGit(t, dir, "add", "-A")
		runGit(t, dir, "commit", "-qm", "c")
	}
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	cmd.Env = isolatedGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// isolatedGitEnv detaches git from the developer's ambient config so a global
// setting like commit.gpgsign cannot break the test-repo commits; identity comes
// from the repo-local user.name/user.email the helpers set.
func isolatedGitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}
