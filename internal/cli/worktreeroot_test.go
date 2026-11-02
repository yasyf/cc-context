package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func configureGitWorktreeRoot(t *testing.T, f *vcstest.Fixture, value string) {
	t.Helper()
	f.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(fixtureHome(t, f), ".gitconfig"))
	mustRun(t, f.Env(), f.Dir, "git", "config", "--global", worktreeRootKey, value)
}

func TestGitWorktreeRootLifecycle(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	legacy := addPoolWorktree(t, f, "legacy")
	configureGitWorktreeRoot(t, f, "~/repos/worktrees")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	head := mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "HEAD")
	configured := addPoolWorktree(t, f, "configured")
	want := filepath.Join(fixtureHome(t, f), "repos", "worktrees", filepath.Base(f.Dir), "configured")
	if configured != want {
		t.Fatalf("created %s, want %s", configured, want)
	}
	if got, err := os.Getwd(); err != nil || got != cwd {
		t.Fatalf("cwd = %q, %v; want %q", got, err, cwd)
	}
	if got := mustRun(t, f.Env(), f.Dir, "git", "rev-parse", "HEAD"); got != head {
		t.Fatalf("caller HEAD = %s, want %s", got, head)
	}
	for _, path := range []string{legacy, configured} {
		name := filepath.Base(path)
		out, err := runWorktreeCmd(t, f, "rm", name, "--dry-run")
		if err != nil || !strings.Contains(out, path) {
			t.Fatalf("preview %s = %q, %v", name, out, err)
		}
		if !worktreeRegistered(t, f.Env(), f.Dir, path) {
			t.Fatalf("preview unregistered %s", path)
		}
		if _, err := runWorktreeCmd(t, f, "rm", name); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
		if worktreeRegistered(t, f.Env(), f.Dir, path) {
			t.Fatalf("still registers %s after removal", path)
		}
	}
}

func TestWorktreePoolsReachTheConfiguredRoot(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	home := fixtureHome(t, f)
	if err := os.MkdirAll(filepath.Join(home, worktreeVolume), 0o750); err != nil {
		t.Fatal(err)
	}
	pools, err := worktreePools(f.Context(), "test", home)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{worktreePool(home)}; !slices.Equal(pools, want) {
		t.Errorf("pools = %v, want %v while ~/repos shares home's volume and nothing is configured", pools, want)
	}
	configureGitWorktreeRoot(t, f, "~/repos/worktrees")
	path := addPoolWorktree(t, f, "feat")
	orphanWorktree(t, f.Env(), path)
	if _, err := runWorktreeCmd(t, f, orphanRmArgs(path)...); err != nil {
		t.Fatalf("rm --path error = %v, want the configured pool's orphan deleted", err)
	}
	assertGone(t, path)
}

func TestGitWorktreeRootCollisionRequiresPath(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	legacy := addPoolWorktree(t, f, "same")
	root := filepath.Join(fixtureHome(t, f), "other-pool")
	configureGitWorktreeRoot(t, f, root)
	configured := filepath.Join(root, filepath.Base(f.Dir), "same")
	if err := os.MkdirAll(filepath.Dir(configured), 0o750); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-b", "another-branch", configured, "HEAD")
	for _, extra := range [][]string{nil, {"--force"}} {
		args := append([]string{"rm", "same"}, extra...)
		_, err := runWorktreeCmd(t, f, args...)
		if err == nil {
			t.Fatal("removed an ambiguous name")
		}
		for _, want := range []string{legacy, configured, "--path <absolute-path>"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not name %q", err, want)
			}
		}
		for _, path := range []string{legacy, configured} {
			if !worktreeRegistered(t, f.Env(), f.Dir, path) {
				t.Fatalf("collision unregistered %s", path)
			}
		}
	}
	out, err := runWorktreeCmd(t, f, "rm", "--path", configured, "--dry-run")
	if err != nil || !strings.Contains(out, configured) {
		t.Fatalf("explicit preview = %q, %v", out, err)
	}
}

func TestGitWorktreeRootCreationPaths(t *testing.T) {
	f := vcstest.Repo(t)
	root := filepath.Join(fixtureHome(t, f), "missing", "worktrees")
	configureGitWorktreeRoot(t, f, root)
	c, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.Context()
	paths := []struct {
		name string
		mint func() (string, error)
	}{
		{"add", func() (string, error) { return mintGitWorktreePath(ctx, "worktree add", c, "add") }},
		{"stack", func() (string, error) { return stackNewPath(ctx, c, "stack", "", c) }},
		{"conflict", func() (string, error) { return stackFreeWorkspace(ctx, lane{checkout: c, root: c.Root}, "conflict") }},
	}
	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.mint()
			want := filepath.Join(root, filepath.Base(f.Dir), tt.name)
			if err != nil || got != want {
				t.Fatalf("minted %q, %v; want %q", got, err, want)
			}
		})
	}
	configureGitWorktreeRoot(t, f, "invalid-relative-root")
	explicit := filepath.Join(fixtureHome(t, f), "explicit-destination")
	got, err := stackNewPath(f.Context(), c, "explicit", explicit, c)
	if err != nil || got != explicit {
		t.Fatalf("explicit path = %q, %v; want %q", got, err, explicit)
	}
}

func TestGitWorktreeRootRejectsInvalidSetting(t *testing.T) {
	for _, value := range []string{"", "relative-root"} {
		t.Run(value, func(t *testing.T) {
			f := vcstest.Repo(t)
			configureGitWorktreeRoot(t, f, value)
			c, err := vcs.ResolveCheckout(f.Dir)
			if err != nil {
				t.Fatal(err)
			}
			_, err = mintGitWorktreePath(f.Context(), "worktree add", c, "unused")
			if err == nil || !strings.Contains(err.Error(), worktreeRootKey) {
				t.Fatalf("setting %q: error = %v", value, err)
			}
		})
	}
}

func TestGitWorktreeRootRejectsSourceCheckout(t *testing.T) {
	f := vcstest.Repo(t)
	configureGitWorktreeRoot(t, f, filepath.Join(f.Dir, "nested"))
	_, err := runWorktreeCmd(t, f, "add", "unused")
	if err == nil || !strings.Contains(err.Error(), "inside checkout") {
		t.Fatalf("nested root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("invalid root was materialized: %v", err)
	}
}

func TestGitWorktreeRootCanonicalPoolOfClone(t *testing.T) {
	f := vcstest.Repo(t)
	home := fixtureHome(t, f)
	root := filepath.Join(home, "pool")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	configureGitWorktreeRoot(t, f, alias)
	for _, old := range []string{worktreePool(home), root} {
		c := vcs.Checkout{Root: f.Dir, MainRoot: filepath.Join(old, "repo", "seed"), Shape: vcs.ShapeGitWorktree}
		got, err := mintGitWorktreePath(f.Context(), "worktree add", c, "next")
		want := filepath.Join(root, "repo", "next")
		if err != nil || got != want {
			t.Fatalf("clone %s: minted %q, %v; want %q", c.MainRoot, got, err, want)
		}
	}
}

func TestGitWorktreeRootSurfacesConfigFailure(t *testing.T) {
	f := vcstest.Repo(t)
	c, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(fixtureHome(t, f), ".gitconfig")
	f.Setenv("GIT_CONFIG_GLOBAL", config)
	if err := os.WriteFile(config, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = mintGitWorktreePath(f.Context(), "worktree add", c, "unused")
	if err == nil || !strings.Contains(err.Error(), worktreeRootKey) {
		t.Fatalf("malformed config was ignored: %v", err)
	}
}

func TestGitWorktreeRootCanonicalRepoDirectory(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside", true: "inside"}[inside], func(t *testing.T) {
			f := vcstest.Repo(t)
			f.Isolate(t)
			home := fixtureHome(t, f)
			root := filepath.Join(home, "pool")
			target := filepath.Join(home, "real-repo-directory")
			if inside {
				target = f.Dir
			}
			for _, path := range []string{root, target} {
				if err := os.MkdirAll(path, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(root, filepath.Base(f.Dir))); err != nil {
				t.Fatal(err)
			}
			configureGitWorktreeRoot(t, f, root)
			out, err := runWorktreeCmd(t, f, "add", "feat")
			if inside {
				if err == nil || !strings.Contains(err.Error(), "inside checkout") {
					t.Fatalf("symlinked source destination: %v", err)
				}
				if _, err := os.Stat(filepath.Join(f.Dir, "feat")); !os.IsNotExist(err) {
					t.Fatalf("materialized inside source checkout: %v", err)
				}
				return
			}
			want := filepath.Join(target, "feat")
			if err != nil || worktreeSummaryPath(t, out) != want {
				t.Fatalf("created %q, %v; want %s", out, err, want)
			}
			if !worktreeRegistered(t, f.Env(), f.Dir, want) {
				t.Fatalf("Git did not register canonical path %s", want)
			}
			out, err = runWorktreeCmd(t, f, "rm", "feat", "--dry-run")
			if err != nil || !strings.Contains(out, want) {
				t.Fatalf("canonical removal preview = %q, %v", out, err)
			}
		})
	}
}

func TestGitWorktreeRootPreservesJJWorkspacePool(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ())
	f.Isolate(t)
	configureGitWorktreeRoot(t, f, "relative-root-unused-by-jj")
	out, err := runWorktreeCmd(t, f, "add", "feat", "--jj", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	path := worktreeSummaryPath(t, out)
	want := filepath.Join(fixtureHome(t, f), ".claude", "worktrees", filepath.Base(f.Dir), "feat")
	if path != want {
		t.Fatalf("JJ path = %s, want %s", path, want)
	}
	if _, err := runWorktreeCmd(t, f, "rm", "feat"); err != nil {
		t.Fatalf("remove JJ workspace: %v", err)
	}
}

func TestGitWorktreeRootDoesNotFollowFinalSymlinkForRemoval(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	home := fixtureHome(t, f)
	root := filepath.Join(home, "pool")
	configured := filepath.Join(root, filepath.Base(f.Dir), "requested")
	victim := filepath.Join(home, "outside", "requested")
	addLinkedWorktree(t, f.Env(), f.Dir, victim, "")
	if err := os.MkdirAll(filepath.Dir(configured), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, configured); err != nil {
		t.Fatal(err)
	}
	configureGitWorktreeRoot(t, f, root)
	_, err := runWorktreeCmd(t, f, "rm", "requested", "--force")
	if err == nil || !strings.Contains(err.Error(), "--path <absolute-path>") {
		t.Fatalf("final symlink selected a foreign registration: %v", err)
	}
	if !worktreeRegistered(t, f.Env(), f.Dir, victim) {
		t.Fatalf("final symlink unregistered %s", victim)
	}
	if _, err := os.Stat(filepath.Join(victim, "f.txt")); err != nil {
		t.Fatalf("final symlink removed the foreign checkout: %v", err)
	}
}
