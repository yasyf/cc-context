package vcs

import (
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
	if err := os.WriteFile(filepath.Join(withConfig, ".git", ".graphite_repo_config"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write graphite config: %v", err)
	}

	withoutConfig := filepath.Join(root, "withoutconfig")
	mustMkdir(t, filepath.Join(withoutConfig, ".git"))

	// A linked git worktree's .git is a file (a "gitdir: …" pointer), not a
	// directory, so joining .graphite_repo_config onto it can never resolve.
	worktree := filepath.Join(root, "worktree")
	mustMkdir(t, worktree)
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../withconfig/.git/worktrees/worktree\n"), 0o600); err != nil {
		t.Fatalf("write worktree .git file: %v", err)
	}

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"graphite config present", withConfig, true},
		{"git dir without graphite config", withoutConfig, false},
		{"linked worktree (.git is a file) never matches", worktree, false},
		{"no .git at all", root, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GraphiteRepo(tt.dir); got != tt.want {
				t.Fatalf("GraphiteRepo(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
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
			want: []string{"jj", "file", "show", "-r", "@-", "--", `root:"internal/cli/ship.go"`},
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
