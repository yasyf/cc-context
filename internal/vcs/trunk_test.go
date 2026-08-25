package vcs

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// These tests drive real git. The repo's own history is why: a fake modelled
// `symbolic-ref`'s clean miss as exit 1 where git without --quiet exits 128, and
// the resolver that conflated the two passed for as long as the fake lied.

func TestResolveTrunkResolved(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote()).Dir

	trunk, err := ResolveTrunk(context.Background(), render.Dir(dir), "origin")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if trunk.Name() != "main" {
		t.Fatalf("Name() = %q, want main", trunk.Name())
	}
	if trunk.Remote() != "origin" {
		t.Fatalf("Remote() = %q, want origin", trunk.Remote())
	}
	if trunk.Ref() != GitRef("refs/remotes/origin/main") {
		t.Fatalf("Ref() = %q, want refs/remotes/origin/main", trunk.Ref())
	}
}

// TestResolveTrunkMissIsProvable is the tri-state --quiet buys: a repository that
// designates no default branch answers ErrNoTrunk, distinguishable from a git
// that could not answer at all.
func TestResolveTrunkMissIsProvable(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote(), vcstest.NoOriginHead()).Dir

	_, err := ResolveTrunk(context.Background(), render.Dir(dir), "origin")
	if !errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want ErrNoTrunk", err)
	}
	// The miss must not be mistaken for a repository that has no main branch at
	// all: refs/remotes/origin/main exists here, it is simply not designated.
	runGit(t, dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/main")
}

func TestResolveTrunkFailureIsNotAMiss(t *testing.T) {
	_, err := ResolveTrunk(context.Background(), render.Dir(vcstest.Repo(t, vcstest.BrokenGitDir()).Dir), "origin")
	if err == nil {
		t.Fatal("ResolveTrunk on a broken checkout succeeded")
	}
	if errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want a failure, not ErrNoTrunk", err)
	}
	if !strings.Contains(err.Error(), "exit 128") {
		t.Fatalf("err = %v, want git's exit 128 surfaced", err)
	}
}

// TestResolveTrunkRefBeatsTheDecoyBranch is D5: git resolves a short name through
// refs/heads before refs/remotes, so a local branch literally named "origin/main"
// answers plumbing in place of the remote-tracking ref — with a warning on stderr
// and exit 0. Trunk.Ref() is the qualified spelling that cannot be shadowed.
func TestResolveTrunkRefBeatsTheDecoyBranch(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote()).Dir
	// The decoy carries a commit the remote-tracking ref does not.
	write(t, dir, "decoy.go", "package a\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "decoy")
	runGit(t, dir, "branch", "origin/main", "HEAD")

	trunk, err := ResolveTrunk(context.Background(), render.Dir(dir), "origin")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if trunk.Ref() != GitRef("refs/remotes/origin/main") {
		t.Fatalf("Ref() = %q, want the qualified remote-tracking ref", trunk.Ref())
	}

	// The two spellings genuinely disagree: HEAD is an ancestor of the decoy but
	// not of the remote-tracking ref, so a short name here inverts the verdict.
	short := isAncestor(t, dir, trunk.Remote()+"/"+trunk.Name())
	qualified := isAncestor(t, dir, string(trunk.Ref()))
	if short == qualified {
		t.Fatalf("short and qualified refs agree (%v) — the decoy no longer shadows", short)
	}
	if !short || qualified {
		t.Fatalf("short = %v, qualified = %v, want the decoy to answer only the short name", short, qualified)
	}
}

// isAncestor reports whether HEAD is an ancestor of rev, the exact plumbing
// question restack asks before deciding a stack is current.
func isAncestor(t *testing.T, dir, rev string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", "HEAD", rev) //nolint:gosec // fixed git argv; dir is a test TempDir
	cmd.Env = isolatedGitEnv()
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("merge-base --is-ancestor HEAD %s: %v", rev, err)
	return false
}

// TestResolveTrunkRejectsATargetOutsideTheRemote is D6: origin/HEAD may legally
// point at any ref, and `--short` renders a tag exactly like a branch, so the old
// trim-the-prefix decode turned refs/tags/v1 into the branch name "v1". The full
// ref discriminates.
func TestResolveTrunkRejectsATargetOutsideTheRemote(t *testing.T) {
	// One fixture serves the table: every case overwrites origin/HEAD, the only
	// state it reads, so no case can observe another's.
	dir := vcstest.Repo(t, vcstest.Remote()).Dir
	runGit(t, dir, "tag", "v1", "HEAD")
	runGit(t, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")

	tests := []struct {
		name   string
		target string
	}{
		{"tag", "refs/tags/v1"},
		{"local branch", "refs/heads/main"},
		{"another remote", "refs/remotes/upstream/main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", tt.target)

			_, err := ResolveTrunk(context.Background(), render.Dir(dir), "origin")
			if err == nil {
				t.Fatalf("ResolveTrunk accepted %s", tt.target)
			}
			if errors.Is(err, ErrNoTrunk) {
				t.Fatalf("err = %v, want a misconfiguration, not ErrNoTrunk", err)
			}
			if !strings.Contains(err.Error(), tt.target) {
				t.Fatalf("err = %v, want the target named", err)
			}
		})
	}
}

func TestResolveTrunkNonOriginRemote(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote()).Dir
	runGit(t, dir, "remote", "add", "upstream", "https://example.com/u.git")
	runGit(t, dir, "update-ref", "refs/remotes/upstream/trunk", "HEAD")
	runGit(t, dir, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")

	trunk, err := ResolveTrunk(context.Background(), render.Dir(dir), "upstream")
	if err != nil {
		t.Fatalf("ResolveTrunk: %v", err)
	}
	if trunk.Name() != "trunk" || trunk.Ref() != GitRef("refs/remotes/upstream/trunk") {
		t.Fatalf("trunk = %+v", trunk)
	}
}

func TestTrunkFromName(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote()).Dir

	trunk, err := TrunkFromName(context.Background(), render.Dir(dir), "origin", "main")
	if err != nil {
		t.Fatalf("TrunkFromName: %v", err)
	}
	if trunk.Name() != "main" || trunk.Ref() != GitRef("refs/remotes/origin/main") {
		t.Fatalf("trunk = %+v", trunk)
	}

	if _, err := TrunkFromName(context.Background(), render.Dir(dir), "origin", "absent"); !errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want ErrNoTrunk", err)
	}

	// A name that only exists locally must not verify: the whole point is that the
	// caller is about to hand this to plumbing, where refs/heads would answer.
	runGit(t, dir, "branch", "local-only", "HEAD")
	if _, err := TrunkFromName(context.Background(), render.Dir(dir), "origin", "local-only"); !errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want ErrNoTrunk for a local-only branch", err)
	}

	err = func() error {
		_, err := TrunkFromName(context.Background(), render.Dir(vcstest.Repo(t, vcstest.BrokenGitDir()).Dir), "origin", "main")
		return err
	}()
	if err == nil || errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want a failure, not ErrNoTrunk", err)
	}
}

func TestGitRemoteFor(t *testing.T) {
	dir := vcstest.Repo(t, vcstest.Remote()).Dir

	got, err := GitRemoteFor(context.Background(), render.Dir(dir), "main")
	if err != nil {
		t.Fatalf("GitRemoteFor: %v", err)
	}
	if got != "origin" {
		t.Fatalf("remote = %q, want origin for an unconfigured branch", got)
	}

	runGit(t, dir, "config", "branch.main.remote", "upstream")
	got, err = GitRemoteFor(context.Background(), render.Dir(dir), "main")
	if err != nil {
		t.Fatalf("GitRemoteFor: %v", err)
	}
	if got != "upstream" {
		t.Fatalf("remote = %q, want upstream", got)
	}

	if _, err := GitRemoteFor(context.Background(), render.Dir(vcstest.Repo(t, vcstest.BrokenGitDir()).Dir), "main"); err == nil {
		t.Fatal("GitRemoteFor on a broken checkout succeeded")
	}
}
