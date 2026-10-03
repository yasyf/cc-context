package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

var shipRemoteDefaultRefs = []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main"}

func shipPartialClone(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	writeShipFile(t, f.Dir, ".pre-commit-config.yaml", "repos: []\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", ".pre-commit-config.yaml")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "hooks")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "HEAD:refs/heads/main", "HEAD:refs/heads/feature")
	mustRun(t, f.Env(), f.RemoteDir, "git", "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "partial")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--no-local", "--filter=blob:none", "--no-tags", "--single-branch", "--branch", "feature", f.RemoteDir, clone)
	for _, kv := range [][2]string{{"user.email", "t@t.t"}, {"user.name", "t"}, {"commit.gpgsign", "false"}} {
		mustRun(t, f.Env(), clone, "git", "config", kv[0], kv[1])
	}
	mustRun(t, f.Env(), clone, "git", "fetch", "-q", "origin", "main")
	if got := gitAt(t, f.Env(), clone, "config", "--get", "remote.origin.promisor"); got != "true" {
		t.Fatalf("clone remote.origin.promisor = %q, want a partial clone", got)
	}
	if got := gitAt(t, f.Env(), clone, "rev-parse", "FETCH_HEAD"); got != gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main") {
		t.Fatalf("FETCH_HEAD = %s, want origin's main", got)
	}
	shipRequireNoRemoteDefault(t, f, clone)
	seedLaneRecords(f.ContextIn(clone), t, clone, laneSeed{})
	writeShipUvx(t, f, 0, "")
	return clone
}

func shipRequireNoRemoteDefault(t *testing.T, f *vcstest.Fixture, dir string) {
	t.Helper()
	for _, ref := range shipRemoteDefaultRefs {
		if code := thinGitCode(t, f, dir, "rev-parse", "--verify", "--quiet", ref); code == 0 {
			t.Fatalf("%s resolves in %s, want a clone that names no trunk", ref, dir)
		}
	}
}

func TestShipPartialCloneHooksFollowThePRRoute(t *testing.T) {
	const title = "fix: frobnicate"
	pr := []string{"--parent", "main", "--pr-title", title}
	for _, tt := range []struct {
		name     string
		detached bool
		args     []string
		hooks    bool
		prs      int
	}{
		{name: "existing branch onto an explicit base", args: pr, prs: 1},
		{name: "detached cut onto an explicit base", detached: true, args: append([]string{"--new-branch=feature-pr"}, pr...), prs: 1},
		{name: "detached cut under --verify", detached: true, args: append([]string{"--new-branch=feature-pr", "--verify"}, pr...), hooks: true, prs: 1},
		{name: "existing branch under --verify", args: append([]string{"--verify"}, pr...), hooks: true, prs: 1},
		{name: "existing branch under --no-verify=false", args: append([]string{"--no-verify=false"}, pr...), hooks: true, prs: 1},
		{name: "existing branch under --no-verify", args: append([]string{"--no-verify"}, pr...), prs: 1},
		{name: "no pull request requested", hooks: true},
		{name: "--no-pr with an explicit base", args: []string{"--parent", "main", "--no-pr"}, hooks: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipPRFixture(t)
			shipPRCreated(t)
			dir, branch := shipPartialClone(t, f), "feature"
			if tt.detached {
				wt := filepath.Join(t.TempDir(), "detached")
				mustRun(t, f.Env(), dir, "git", "worktree", "add", "-q", "--detach", wt, "FETCH_HEAD")
				dir, branch = wt, "feature-pr"
				shipRequireNoRemoteDefault(t, f, dir)
			}
			writeShipFile(t, dir, "f1.go", "x")
			shipResetLog(t, f)

			got, err := runShipCmd(f.ContextIn(dir), t, append([]string{"-m", title, "--no-watch"}, tt.args...)...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			calls := vcstest.Invocations(t, f.ArgvLog)
			if ran := len(shipInvocationsOf(calls, "uvx")) > 0; ran != tt.hooks {
				t.Errorf("prek ran = %v, want %v", ran, tt.hooks)
			}
			if reported := strings.Contains(got, "hooks ok"+shipSep); reported != tt.hooks {
				t.Errorf("summary = %q, want a hook segment = %v", got, tt.hooks)
			}
			var push []string
			for _, inv := range shipInvocationsOf(calls, "git") {
				if len(inv) > 1 && inv[1] == "push" {
					push = inv
				}
			}
			if push == nil || push[len(push)-1] != branch || slices.Contains(push, "--no-verify") == tt.hooks {
				t.Errorf("push argv = %v, want %s pushed with --no-verify = %v", push, branch, !tt.hooks)
			}
			if current := gitAt(t, f.Env(), dir, "branch", "--show-current"); current != branch {
				t.Errorf("checked out %q, want %q", current, branch)
			}
			if head, pushed := gitAt(t, f.Env(), dir, "rev-parse", "HEAD"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "refs/heads/"+branch); head != pushed {
				t.Errorf("origin %s = %s, want the shipped head %s", branch, pushed, head)
			}
			posts := 0
			for _, inv := range calls {
				if !isPRCreate(inv) {
					continue
				}
				posts++
				if !slices.Contains(inv, "base=main") || !slices.Contains(inv, "head="+branch) {
					t.Errorf("pull request create = %v, want head=%s base=main", inv, branch)
				}
			}
			if posts != tt.prs {
				t.Errorf("created %d pull requests, want %d", posts, tt.prs)
			}
			shipRequireNoRemoteDefault(t, f, dir)
		})
	}
}

func TestShipPartialCloneUnknownBaseRefusesBeforeHooks(t *testing.T) {
	f := shipPRFixture(t)
	shipPRCreated(t)
	dir := shipPartialClone(t, f)
	writeShipFile(t, dir, "f1.go", "x")
	before := gitAt(t, f.Env(), dir, "rev-parse", "HEAD")
	shipResetLog(t, f)

	_, err := runShipCmd(f.ContextIn(dir), t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "fix: frobnicate")
	if !errors.Is(err, vcs.ErrNoTrunk) || !strings.Contains(err.Error(), "--parent") {
		t.Errorf("ship without a base = %v, want the actionable no-trunk refusal", err)
	}
	calls := vcstest.Invocations(t, f.ArgvLog)
	assertNoShipMutation(t, calls)
	if uvx := shipInvocationsOf(calls, "uvx"); len(uvx) > 0 {
		t.Errorf("prek ran before the refusal: %v", uvx)
	}
	for _, inv := range calls {
		if isPRCreate(inv) || (len(inv) > 1 && inv[0] == "git" && inv[1] == "push") {
			t.Errorf("unresolved base reached publication: %v", inv)
		}
	}
	if got := gitAt(t, f.Env(), dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("refusal moved HEAD from %s to %s", before, got)
	}
}

func TestShipKnownTrunkAppendKeepsHooksUnderPRFlags(t *testing.T) {
	t.Parallel()
	f := shipPRFixture(t)
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")

	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	calls := vcstest.Invocations(t, f.ArgvLog)
	assertNoPRStep(t, calls)
	if uvx := shipInvocationsOf(calls, "uvx"); len(uvx) == 0 {
		t.Error("prek skipped on a commit straight onto a known trunk")
	}
	if !strings.Contains(got, "hooks ok"+shipSep) || !strings.HasSuffix(got, " · pushed main → origin · no PR (on trunk)") {
		t.Errorf("summary = %q, want hooks run and no pull request on trunk", got)
	}
}
