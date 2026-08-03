package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestJJWorkingCopyFlag(t *testing.T) {
	tests := []struct {
		name   string
		run    func(t *testing.T) *vcstest.Fixture
		wantAt string
	}{
		{
			name: "commit and push",
			run: func(t *testing.T) *vcstest.Fixture {
				f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return f
			},
			wantAt: "fix: frobnicate",
		},
		{
			name: "track the push target",
			run: func(t *testing.T) *vcstest.Fixture {
				f := shipRepo(t, vcstest.JJ(), vcstest.Dirty())
				shipJJRemotes(t, f, "backup")
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return f
			},
			wantAt: "fix: frobnicate",
		},
		{
			name: "amend",
			run: func(t *testing.T) *vcstest.Fixture {
				f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
				shipAmendable(t, f, vcs.JJ)
				_, _ = runShipCmd(t, "--amend", "--no-push")
				return f
			},
			wantAt: "wip",
		},
		{
			name: "create a bookmark",
			run: func(t *testing.T) *vcstest.Fixture {
				f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
				return f
			},
			wantAt: "fix: frobnicate",
		},
		{
			name: "conflicted rebase rolls back",
			run: func(t *testing.T) *vcstest.Fixture {
				f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
				shipDivergeRemote(t, f, "main", "f.txt", "upstream\n")
				shipResetLog(t, f)
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return f
			},
			wantAt: "fix: frobnicate",
		},
		{
			name: "hunk-scoped",
			run: func(t *testing.T) *vcstest.Fixture {
				f := jjHunkRepo(t, hunkBase, hunkCurrent)
				ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--skip-hunk", ref, "f.txt")
				return f
			},
			wantAt: "fix: frobnicate",
		},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.run(t)
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if inv[0] != "jj" {
					continue
				}
				argv := inv[1:]
				flagged := argv[0] == "--ignore-working-copy"
				if flagged {
					argv = argv[1:]
				}
				verb := jjVerb(argv)
				want, ok := jjIgnoresWorkingCopy[verb]
				if !ok {
					t.Errorf("unclassified jj verb %q: %v", verb, inv)
					continue
				}
				if flagged != want {
					t.Errorf("jj %s: --ignore-working-copy = %v, want %v: %v", verb, flagged, want, inv)
				}
				seen[verb] = true
			}
			if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != tt.wantAt {
				t.Errorf("@- = %q, want %q — the scenario never reached the repository", subject, tt.wantAt)
			}
		})
	}
	for verb := range jjIgnoresWorkingCopy {
		if !seen[verb] {
			t.Errorf("no scenario ran jj %s", verb)
		}
	}
}

func TestShipCommitPushWatch(t *testing.T) {
	tests := []struct {
		name string
		jj   bool
		want func(sha string) [][]string
	}{
		{
			name: "jj happy path",
			jj:   true,
			want: func(sha string) [][]string {
				return [][]string{
					{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
					{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
					{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
					{"jj", "diff", "--name-only"},
					{"jj", "commit", "-m", "fix: frobnicate"},
					{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
					{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
					{"jj", "git", "fetch"},
					{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
					{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
					{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
					{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
					{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
					{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id"},
					ghRunListArgvFor(sha),
					ghRunWatchArgv,
					ghRunViewArgv,
					ghRunListArgvFor(sha),
					ghRunListArgvFor(sha),
				}
			},
		},
		{
			name: "git happy path",
			want: func(sha string) [][]string {
				return [][]string{
					{"git", "branch", "--show-current"},
					gitTrunkArgv,
					{"git", "add", "-A"},
					{"git", "commit", "-m", "fix: frobnicate"},
					{"git", "branch", "--show-current"},
					{"git", "log", "-1", "--format=%h%x00%s"},
					{"git", "config", "--get", "branch.main.remote"},
					{"git", "fetch", "origin"},
					{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
					{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
					{"git", "push", "origin", "main"},
					{"git", "rev-parse", "HEAD"},
					ghRunListArgvFor(sha),
					ghRunWatchArgv,
					ghRunViewArgv,
					ghRunListArgvFor(sha),
					ghRunListArgvFor(sha),
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, shipOptsFor(tt.jj, vcstest.Remote(), vcstest.Dirty())...)
			writeShipGH(t, f)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			invocations := vcstest.Invocations(t, f.ArgvLog)
			assertInvocations(t, invocations, tt.want(shipHead(t, f)))
			if want := shipCommitted(t, f, shipKind(tt.jj)) + " · pushed main → origin · CI success"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if n := remoteCount(t, f, "main"); n != 2 {
				t.Errorf("origin main holds %d commits, want the pushed one on top of init", n)
			}
		})
	}
}

func TestShipHooksPass(t *testing.T) {
	tests := []struct {
		name string
		jj   bool
		want [][]string
	}{
		{
			name: "jj",
			jj:   true,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "diff", "--name-only"},
				{"uvx", "prek", "run", "--cd", "ROOT", "--files", "f1.go"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "git",
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z"},
				{"uvx", "prek", "run", "--cd", "ROOT", "--files", "f1.go"},
				{"git", "commit", "-m", "fix: frobnicate", "--no-verify"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := shipKind(tt.jj)
			f := shipRepo(t, shipOptsFor(tt.jj, vcstest.Remote())...)
			shipHookRepo(t, f, kind, 0, "", "f1.go")

			for i, rec := range tt.want {
				for j, field := range rec {
					if field == "ROOT" {
						tt.want[i][j] = f.Dir
					}
				}
			}

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), tt.want)
			if want := "hooks ok · " + shipCommitted(t, f, kind) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if status := gitAt(t, f.Dir, "status", "--porcelain"); status != "" {
				t.Errorf("working copy left dirty: %q", status)
			}
		})
	}
}

func TestShipHooksJJAmend(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote())
	shipHookRepo(t, f, vcs.JJ, 0, "", "folded.go")

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "diff", "--name-only"},
		{"uvx", "prek", "run", "--cd", f.Dir, "--files", "folded.go"},
		{"jj", "squash", "--use-destination-message"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
	})
	if want := "hooks ok · " + shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if files := jjAt(t, f.Dir, "@-", `diff.files().map(|e| e.path()).join(" ")`); !strings.Contains(files, "folded.go") {
		t.Errorf("@- touches %q, want folded.go squashed into it", files)
	}
}

// TestShipHooksSubdirRunsAtRoot proves ship runs from the repository root
// whatever directory it was invoked in: jj resolves a -- path against the
// process's own cwd, so the root-relative sub/x.go it is handed here matches
// nothing at all from sub/ — the ship would refuse with nothing to commit.
func TestShipHooksSubdirRunsAtRoot(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote())
	shipHookRepo(t, f, vcs.JJ, 0, "", "sub/x.go")
	t.Chdir(filepath.Join(f.Dir, "sub"))

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "x.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"uvx", "prek", "run", "--cd", f.Dir, "--files", "sub/x.go"},
		{"jj", "commit", "-m", "fix: frobnicate", "--", "x.go"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
	})
	if want := "hooks ok · " + shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestShipHooksAutoFixLeavingNothingAborts(t *testing.T) {
	for _, jj := range []bool{true, false} {
		t.Run(kindLabel(shipKind(jj)), func(t *testing.T) {
			kind := shipKind(jj)
			f := shipRepo(t, shipOptsFor(jj, vcstest.Remote())...)
			shipHookRepo(t, f, kind, 1, "rm f1.go", "f1.go")

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
				t.Fatalf("ship error = %v, want nothing-to-commit", err)
			}
			uvxCount, jjDiffCount := 0, 0
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if inv[0] == "uvx" {
					uvxCount++
				}
				if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "diff" && inv[2] == "--name-only" {
					jjDiffCount++
				}
				if inv[0] == "jj" && (inv[1] == "commit" || inv[1] == "squash") {
					t.Errorf("jj commit ran after hooks emptied the change: %v", inv)
				}
				if inv[0] == "git" && inv[1] == "commit" {
					t.Errorf("git commit ran after hooks emptied the change: %v", inv)
				}
			}
			if uvxCount != 1 {
				t.Errorf("uvx invocation count = %d, want 1 (no retry on an empty re-derive)", uvxCount)
			}
			if jj && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
			if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "main"); subject != "hooks" {
				t.Errorf("main tip = %q, want no commit cut", subject)
			}
		})
	}
}

func TestShipHooksAutoFixThenPass(t *testing.T) {
	for _, jj := range []bool{true, false} {
		t.Run(kindLabel(shipKind(jj)), func(t *testing.T) {
			kind := shipKind(jj)
			f := shipRepo(t, shipOptsFor(jj, vcstest.Remote())...)
			shipHookRepo(t, f, kind, 1, "printf 'fixed' > f1.go", "f1.go")

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			uvxCount, gitAddCount := 0, 0
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if inv[0] == "uvx" {
					uvxCount++
				}
				if !jj && len(inv) >= 3 && inv[0] == "git" && inv[1] == "add" && inv[2] == "-A" {
					gitAddCount++
				}
			}
			if want := "hooks fixed · " + shipCommitted(t, f, kind) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if uvxCount != 2 {
				t.Errorf("uvx invocation count = %d, want 2", uvxCount)
			}
			if !jj && gitAddCount != 2 {
				t.Errorf("git add -A invocation count = %d, want 2", gitAddCount)
			}
			if got := gitAt(t, f.Dir, "show", "HEAD:f1.go"); got != "fixed" {
				t.Errorf("committed f1.go = %q, want the auto-fixer's content", got)
			}
		})
	}
}

func TestShipHooksRetryRederivesFiles(t *testing.T) {
	for _, jj := range []bool{true, false} {
		t.Run(kindLabel(shipKind(jj)), func(t *testing.T) {
			kind := shipKind(jj)
			f := shipRepo(t, shipOptsFor(jj, vcstest.Remote())...)
			shipHookRepo(t, f, kind, 1, "rm first.go && printf x > generated.go", "first.go")

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			jjDiffCount := 0
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "diff" && inv[2] == "--name-only" {
					jjDiffCount++
				}
			}
			assertInvocations(t, shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx"), [][]string{
				{"uvx", "prek", "run", "--cd", f.Dir, "--files", "first.go"},
				{"uvx", "prek", "run", "--cd", f.Dir, "--files", "generated.go"},
			})
			if want := "hooks fixed · " + shipCommitted(t, f, kind) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if jj && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
			if files := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "generated.go" {
				t.Errorf("committed files = %q, want the re-derived generated.go alone", files)
			}
		})
	}
}

func TestShipHooksPersistentFailure(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 2, "", "f1.go")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "ship: hooks:") {
		t.Fatalf("ship error = %v, want containing %q", err, "ship: hooks:")
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if inv[0] == "git" && len(inv) > 1 && inv[1] == "commit" {
			t.Errorf("commit ran after persistent hook failure: %v", inv)
		}
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "hooks" {
		t.Errorf("HEAD subject = %q, want no commit cut", subject)
	}
}

func TestShipHooksNoVerify(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range invocations {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked with --no-verify: %v", inv)
		}
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate", "--no-verify"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v", commit, wantCommit)
	}
}

func TestShipHooksNoConfig(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	writeShipUvx(t, f, 0, "")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range invocations {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked without a config file: %v", inv)
		}
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v — a repo's own git hooks must still run", commit, wantCommit)
	}
}

// TestShipHooksCommitMsgStage pins the one config shape ship must not suppress:
// prek run --files never reaches the message stages --no-verify would silence.
func TestShipHooksCommitMsgStage(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")
	writeShipFile(t, f.Dir, ".pre-commit-config.yaml", "repos:\n  - repo: local\n    hooks:\n      - id: gitlint\n        stages: [commit-msg]\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := "hooks ok · " + shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range invocations {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v — a commit-msg stage still needs git's hook run", commit, wantCommit)
	}
}

// TestShipHooksUvxMissing takes the fixture's uvx away entirely: vcstest's PATH
// carries the system directories alone, where uvx never lives.
func TestShipHooksUvxMissing(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")
	if err := os.Remove(filepath.Join(f.ShimBin, "uvx")); err != nil {
		t.Fatalf("remove uvx: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := "hooks uvx-missing · " + shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range invocations {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v — hooks ccx could not run stay git's job", commit, wantCommit)
	}
}

func TestShipHooksJJNoGitMarker(t *testing.T) {
	f := shipJJPlainRepo(t)
	writeShipHookFiles(t, f.Dir, "f1.go")
	writeShipUvx(t, f, 0, "")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := "hooks no-git · " + shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range invocations {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked for a jj repo without a .git marker: %v", inv)
		}
	}
	if files := jjAt(t, f.Dir, "@-", `diff.files().map(|e| e.path()).join(" ")`); !strings.Contains(files, "f1.go") {
		t.Errorf("@- touches %q, want the unhooked change committed", files)
	}
}

// TestShipHooksEmptyFilesSkipSoftGuards covers the two ways a hook run reaches
// an empty file list: a jj working copy with nothing in it at all, and a git
// change that is a deletion alone, which --diff-filter=d empties.
func TestShipHooksEmptyFilesSkipSoftGuards(t *testing.T) {
	t.Run("jj with nothing to commit", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote())
		shipHookRepo(t, f, vcs.JJ, 0, "")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
			t.Fatalf("ship error = %v, want nothing-to-commit", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		assertInvocations(t, invocations, [][]string{
			{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
			{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
			{"jj", "diff", "--name-only"},
			{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
			{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		})
		for _, inv := range invocations {
			if inv[0] == "uvx" || inv[0] == "jj" && (inv[1] == "commit" || inv[1] == "squash") {
				t.Errorf("empty jj ship ran hooks or committed: %v", inv)
			}
		}
	})

	t.Run("git deleting its only change, without uvx", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote())
		writeShipFile(t, f.Dir, "doomed.go", "x")
		shipHookRepo(t, f, vcs.Git, 0, "")
		if err := os.Remove(filepath.Join(f.ShimBin, "uvx")); err != nil {
			t.Fatalf("remove uvx: %v", err)
		}
		if err := os.Remove(filepath.Join(f.Dir, "doomed.go")); err != nil {
			t.Fatalf("remove doomed.go: %v", err)
		}

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		for _, inv := range invocations {
			if inv[0] == "uvx" {
				t.Errorf("uvx invoked with no changed files: %v", inv)
			}
		}
		if files := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "doomed.go" {
			t.Errorf("committed files = %q, want the deletion", files)
		}
	})
}

func TestShipHooksScopedPaths(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	shipHookRepo(t, f, vcs.Git, 0, "", "src/a.go", "unscoped.go")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A", "--", "src/a.go"},
		{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z", "--", "src/a.go"},
		{"uvx", "prek", "run", "--cd", f.Dir, "--files", "src/a.go"},
		{"git", "commit", "-m", "fix: frobnicate", "--no-verify", "--", "src/a.go"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
	if want := "hooks ok · " + shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if files := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "src/a.go" {
		t.Errorf("committed files = %q, want the scoped path alone", files)
	}
}

// TestShipHooksFiltersMissingFile runs the jj lane, where a deletion reaches the
// hook file list at all: git's own --diff-filter=d has already dropped it.
func TestShipHooksFiltersMissingFile(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote())
	writeShipFile(t, f.Dir, "gone.go", "x")
	shipHookRepo(t, f, vcs.JJ, 0, "", "f1.go")
	if err := os.Remove(filepath.Join(f.Dir, "gone.go")); err != nil {
		t.Fatalf("remove gone.go: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, shipInvocationsOf(vcstest.Invocations(t, f.ArgvLog), "uvx"), [][]string{
		{"uvx", "prek", "run", "--cd", f.Dir, "--files", "f1.go"},
	})
	if want := "hooks ok · " + shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestShipHooksPreserveHookableFilenames(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		create   func(*testing.T, string)
	}{
		{
			name:     "non-ASCII",
			filename: "café.go",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			},
		},
		{
			name:     "broken symlink",
			filename: "broken.go",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink("missing-target", path); err != nil {
					t.Fatalf("symlink %s: %v", path, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			shipHookRepo(t, f, vcs.Git, 0, "")
			tt.create(t, filepath.Join(f.Dir, tt.filename))

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			invocations := vcstest.Invocations(t, f.ArgvLog)
			if want := "hooks ok · " + shipCommitted(t, f, vcs.Git) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			assertInvocations(t, shipInvocationsOf(invocations, "uvx"), [][]string{
				{"uvx", "prek", "run", "--cd", f.Dir, "--files", tt.filename},
			})
		})
	}
}

func TestShipJJNeverInvokesGitCommit(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
	shipCIPollInterval = 0

	if _, err := runShipCmd(t, "-m", "fix: frobnicate"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if inv[0] == "git" {
			t.Errorf("jj path invoked git: %v", inv)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the jj-lane push to have landed", n)
	}
}

func TestShipCommitOnlyVariants(t *testing.T) {
	tests := []struct {
		name string
		jj   bool
		args []string
		want [][]string
	}{
		{
			name: "jj no-push",
			jj:   true,
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "git no-push",
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
		{
			name: "jj amend no message",
			jj:   true,
			args: []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "jj amend with message",
			jj:   true,
			args: []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "git amend no message",
			args: []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
		{
			name: "git amend no verify",
			args: []string{"--amend", "--no-verify", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit", "--no-verify"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
		{
			name: "jj scoped paths",
			jj:   true,
			args: []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only", "--", "src/a.go", "docs"},
				{"jj", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "git scoped paths",
			args: []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A", "--", "src/a.go", "docs"},
				{"git", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
		{
			name: "jj amend scoped no message",
			jj:   true,
			args: []string{"--amend", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message", "--", "src/a.go"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "jj amend scoped with message",
			jj:   true,
			args: []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name: "git amend scoped",
			args: []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A", "--", "src/a.go"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := shipKind(tt.jj)
			f := shipRepo(t, shipOptsFor(tt.jj, vcstest.Remote(), vcstest.Dirty())...)
			if tt.jj && slices.Contains(tt.args, "--amend") {
				// jj protects the commit origin's bookmark carries, so an amend
				// needs a local commit of its own to fold into.
				mustRun(t, f.Dir, "jj", "commit", "-m", "wip")
				writeShipFile(t, f.Dir, "f.txt", "amended\n")
				shipResetLog(t, f)
			}
			writeShipFile(t, f.Dir, "src/a.go", "x")
			writeShipFile(t, f.Dir, "docs/d.md", "x")

			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), tt.want)
			if want := shipCommitted(t, f, kind) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if n := remoteCount(t, f, "main"); n != 1 {
				t.Errorf("origin main holds %d commits, want the commit-only ship to have pushed nothing", n)
			}
		})
	}
}

// TestShipJJNoPushMovesBookmarkLive proves a --no-push ship lands the bookmark
// on the commit it just made. The failure is silent and deferred: a bookmark
// left behind pushes clean later, reporting "Nothing changed" over commits that
// were never pushed.
func TestShipJJNoPushMovesBookmarkLive(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "commit", args: []string{"-m", "second", "--no-push"}},
		{name: "amend", args: []string{"--amend", "-m", "amended", "--no-push"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireLiveVCS(t, "git", "jj")
			dir := setupLiveJJRepo(t, "base\n", "edited\n")
			mustRun(t, dir, "jj", "bookmark", "create", "main", "-r", "@-")
			mustRun(t, dir, "jj", "commit", "-m", "unbookmarked")
			if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("edited again\n"), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write second edit: %v", err)
			}

			if _, err := runShipCmd(t, tt.args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}

			if got, want := jjRevID(t, dir, `bookmarks(exact:"main")`), jjRevID(t, dir, "@-"); got != want {
				t.Errorf("bookmark main = %s, want @- %s", got, want)
			}
		})
	}
}

// TestShipJJNoPushMovesAtNamedBookmarkLive proves the --no-push move quotes the
// bookmark name it hands jj: a bare exact:foo@bar reads as bookmark@remote and
// fails to parse, so the move never happens. The name arrives through git, which
// accepts an '@' in a branch name where jj bookmark create refuses it.
func TestShipJJNoPushMovesAtNamedBookmarkLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	dir := setupLiveJJRepo(t, "base\n", "edited\n")
	mustRun(t, dir, "git", "branch", "foo@bar", jjRevID(t, dir, "@-"))
	mustRun(t, dir, "jj", "bookmark", "list")
	mustRun(t, dir, "jj", "commit", "-m", "unbookmarked")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("edited again\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write second edit: %v", err)
	}

	if _, err := runShipCmd(t, "-m", "second", "--no-push", "--bookmark", "foo@bar"); err != nil {
		t.Fatalf("ship error = %v", err)
	}

	if got, want := jjRevID(t, dir, `bookmarks(exact:"foo@bar")`), jjRevID(t, dir, "@-"); got != want {
		t.Errorf("bookmark foo@bar = %s, want @- %s", got, want)
	}
}

// TestJJBookmarkNamesAtNameLive proves the readback undoes the quoting jj's
// template applies to a name it would otherwise reread as a symbol. A name that
// arrives with its quotes still attached matches no bookmark in any argument
// built from it, so auto-discovery of such a bookmark is broken on its own.
func TestJJBookmarkNamesAtNameLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	dir := setupLiveJJRepo(t, "base\n", "edited\n")
	mustRun(t, dir, "git", "branch", "foo@bar", jjRevID(t, dir, "@-"))
	mustRun(t, dir, "jj", "bookmark", "list")

	names, err := jjBookmarkNames(context.Background(), "test", jjNearestBookmarkRevset)
	if err != nil {
		t.Fatalf("jjBookmarkNames error = %v", err)
	}
	if want := []string{"foo@bar"}; !reflect.DeepEqual(names, want) {
		t.Errorf("jjBookmarkNames = %q, want %q", names, want)
	}
}

// TestJJTrunkBookmarkNamesAtNameLive is TestJJBookmarkNamesAtNameLive for the
// trunk readback, which quotes the same names off the same template. A trunk
// name that arrives quoted is compared against --branch and the nearest
// bookmarks as a name no repository holds, so ship reads the trunk it is on as
// a branch off trunk.
func TestJJTrunkBookmarkNamesAtNameLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	dir := setupLiveJJRepo(t, "base\n", "edited\n")
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, dir, "git", "init", "--bare", "--initial-branch=main", remote)
	mustRun(t, dir, "git", "branch", "main", jjRevID(t, dir, "@-"))
	mustRun(t, dir, "git", "branch", "foo@bar", jjRevID(t, dir, "@-"))
	mustRun(t, dir, "git", "remote", "add", "origin", remote)
	mustRun(t, dir, "git", "push", "origin", "main", "foo@bar")
	mustRun(t, dir, "jj", "bookmark", "list")

	names, err := jjTrunkBookmarkNames(context.Background(), "test")
	if err != nil {
		t.Fatalf("jjTrunkBookmarkNames error = %v", err)
	}
	if want := []string{"foo@bar", "main"}; !reflect.DeepEqual(names, want) {
		t.Errorf("jjTrunkBookmarkNames = %q, want %q", names, want)
	}
}

// TestShipJJPushesAtNamedBookmarkLive proves an '@'-named bookmark survives the
// whole push path: discovered by name out of the template, moved onto the new
// commit, and pushed. A bare --bookmark exact:foo@bar reads as bookmark@remote and
// fails to parse, with the commit already landed.
func TestShipJJPushesAtNamedBookmarkLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	const branch = "foo@bar"
	dir := setupLiveJJRepo(t, "base\n", "edited\n")
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, dir, "git", "init", "--bare", "--initial-branch=main", remote)
	mustRun(t, dir, "git", "branch", branch, jjRevID(t, dir, "@-"))
	mustRun(t, dir, "git", "remote", "add", "origin", remote)
	mustRun(t, dir, "git", "push", "origin", branch)
	mustRun(t, dir, "jj", "bookmark", "list")

	got, err := runShipCmd(t, "-m", "second", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if want := `committed ` + jjRevID(t, dir, "@-")[:12] + ` "second" · pushed foo@bar → origin`; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	pushed := strings.TrimSpace(mustRun(t, dir, "git", "--git-dir="+remote, "rev-parse", "refs/heads/"+branch))
	if want := jjRevID(t, dir, "@-"); pushed != want {
		t.Errorf("remote %s = %s, want the shipped commit %s", branch, pushed, want)
	}
}

// TestShipJJNothingToCommitHintPastesLive runs the recovery hint ship printed,
// as an operator would: through a shell, verbatim. For an '@'-named bookmark an
// unquoted exact:foo@bar is refused by jj's own pattern parser, so the advice
// ship gives is unusable exactly where the commit is already landed and only
// the bookmark is behind.
func TestShipJJNothingToCommitHintPastesLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	const branch = "foo@bar"
	dir := setupLiveJJRepo(t, "base\n", "edited\n")
	remote := filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, dir, "git", "init", "--bare", "--initial-branch=main", remote)
	mustRun(t, dir, "git", "branch", branch, jjRevID(t, dir, "@-"))
	mustRun(t, dir, "git", "remote", "add", "origin", remote)
	mustRun(t, dir, "git", "push", "origin", branch)
	mustRun(t, dir, "jj", "bookmark", "list")
	if _, err := runShipCmd(t, "-m", "second", "--no-watch"); err != nil {
		t.Fatalf("first ship error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("edited again\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write third edit: %v", err)
	}
	mustRun(t, dir, "jj", "commit", "-m", "landed outside ship")

	_, err := runShipCmd(t, "-m", "third", "--no-watch")
	if err == nil {
		t.Fatal("second ship error = nil, want nothing to commit")
	}
	_, hint, ok := strings.Cut(err.Error(), "push it: ")
	if !ok {
		t.Fatalf("second ship error = %q, want a push hint", err)
	}

	mustRun(t, dir, "sh", "-c", hint)

	pushed := strings.TrimSpace(mustRun(t, dir, "git", "--git-dir="+remote, "rev-parse", "refs/heads/"+branch))
	if want := jjRevID(t, dir, "@-"); pushed != want {
		t.Errorf("remote %s = %s, want the commit the hint recovers %s", branch, pushed, want)
	}
}

// TestShipJJExplicitMissingBookmarkRefuses covers the append an amend forms
// without consulting the repository. jj bookmark move exits 0 on a name that
// matches nothing, so an unrefused --no-push ship would report a branch the
// repository never had, over a commit already landed.
func TestShipJJExplicitMissingBookmarkRefuses(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())

	_, err := runShipCmd(t, "--amend", "--no-push", "--branch", "missing")
	if err == nil || err.Error() != `ship: bookmark "missing" not found` {
		t.Errorf("ship error = %v, want bookmark \"missing\" not found", err)
	}
	got := vcstest.Invocations(t, f.ArgvLog)
	assertNoShipMutation(t, got)
	assertInvocations(t, got, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"missing")`, "--no-graph", "-T", jjStackLineTemplate},
	})
}

// TestShipJJEmptyRefuses proves an empty working copy is refused before any
// mutation, with the hint naming the bookmark the plan resolved, and that a
// merge working copy is the one shape that commits anyway.
func TestShipJJEmptyRefuses(t *testing.T) {
	stack := func(target string) []string {
		return []string{"jj", "--ignore-working-copy", "log", "-r", jjBookmarksRevset(target), "--no-graph", "-T", jjStackLineTemplate}
	}
	atState := []string{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate}
	describe := []string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate}
	diff := []string{"jj", "diff", "--name-only"}
	tests := []struct {
		name    string
		args    []string
		opts    []vcstest.Opt
		build   func(t *testing.T, f *vcstest.Fixture)
		target  string
		scope   string
		commits bool
		want    [][]string
	}{
		{
			name:   "unscoped",
			args:   []string{"-m", "fix: frobnicate", "--no-watch"},
			target: "main",
			want:   append(jjPlanArgv(), stack("main"), diff, atState, describe),
		},
		{
			name: "path scoped",
			args: []string{"-m", "fix: frobnicate", "--no-watch", "src/a.go"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				writeShipFile(t, f.Dir, "src/a.go", "package a\n")
				mustRun(t, f.Dir, "jj", "commit", "-m", "add a")
				mustRun(t, f.Dir, "jj", "bookmark", "move", "main", "--to", "@-")
			},
			target: "main",
			scope:  " in src/a.go",
			want: append(jjPlanArgv(), stack("main"),
				[]string{"jj", "diff", "--name-only", "--", "src/a.go"}, atState, describe),
		},
		{
			name: "bookmark hint",
			args: []string{"-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "bookmark", "create", "someone/probe", "-r", "@-")
			},
			target: "someone/probe",
			want:   append(jjPlanArgv(), stack("someone/probe"), diff, atState, describe),
		},
		{
			// The hint names the target under --no-push too: the branch plan
			// resolves once for every lane, push or not.
			name: "no push still names the target",
			args: []string{"-m", "fix: frobnicate", "--no-push", "--bookmark", "someone/probe"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "bookmark", "create", "someone/probe", "-r", "@-")
			},
			target: "someone/probe",
			want:   append(jjPlanArgv(), stack("someone/probe"), diff, atState, describe),
		},
		{
			name: "description only working copy refuses",
			args: []string{"-m", "description only", "--no-push"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "describe", "-m", "description only")
			},
			target: "main",
			want:   append(jjPlanArgv(), diff, atState, describe),
		},
		{
			name:    "merge working copy commits",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			build:   shipJJMergeWorkingCopy,
			target:  "main",
			commits: true,
			want: append(jjPlanArgv(), diff, atState,
				[]string{"jj", "commit", "-m", "fix: frobnicate"}, describe,
				[]string{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"}),
		},
		{
			name: "conflicted single-parent working copy refuses",
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			opts: []vcstest.Opt{vcstest.Conflicted()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "new")
			},
			target: "main",
			want:   append(jjPlanArgv(), diff, atState, describe),
		},
		{
			// @- is the root commit, whose id is all zeros and whose description
			// is empty — the hint quotes both back rather than inventing a subject.
			name: "empty root refuses",
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "new", "root()")
			},
			target: "main",
			want:   append(jjPlanArgv(), diff, atState, describe),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, append([]vcstest.Opt{vcstest.JJ(), vcstest.Remote()}, tt.opts...)...)
			if tt.build != nil {
				tt.build(t, f)
			}
			before := ""
			if !tt.commits {
				before = jjRevID(t, f.Dir, "@-")
			}
			shipResetLog(t, f)

			got, err := runShipCmd(t, tt.args...)
			invocations := vcstest.Invocations(t, f.ArgvLog)
			assertInvocations(t, invocations, tt.want)
			if tt.commits {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if want := shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
					t.Errorf("summary = %q, want %q", got, want)
				}
				if n := remoteCount(t, f, "main"); n != 1 {
					t.Errorf("origin main holds %d commits, want the pre-ship 1 — --no-push pushed anyway", n)
				}
				return
			}
			if err == nil {
				t.Fatal("expected empty ship refusal, got nil")
			}
			if want := shipEmptyRefusal(t, f, tt.scope, tt.target); err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			assertNoShipMutation(t, invocations)
			if head := jjRevID(t, f.Dir, "@-"); head != before {
				t.Errorf("@- = %s, want it unmoved at %s", head, before)
			}
		})
	}
}

// TestSplitDescribeTruncated proves a describe read cut short of its separator
// is reported as malformed rather than parsed as a bare short id. The bytes are
// jj's own, truncated where a partial read ends.
func TestSplitDescribeTruncated(t *testing.T) {
	f := shipRepo(t, vcstest.JJ())
	out := mustRun(t, f.Dir, "jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate)
	short, _, ok := strings.Cut(out, "\n")
	if !ok {
		t.Fatalf("jj describe output %q carries no separator", out)
	}
	_, _, err := splitDescribe(short, "\n")
	if want := fmt.Sprintf("ship: malformed commit description %q", short); err == nil || err.Error() != want {
		t.Errorf("splitDescribe(%q) error = %v, want %q", short, err, want)
	}
}

// TestShipJJEmptyAmendExempt proves --amend skips the empty-working-copy
// refusal: the amend's whole point is folding a change that is already there.
func TestShipJJEmptyAmendExempt(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	mustRun(t, f.Dir, "jj", "commit", "-m", "wip")
	shipResetLog(t, f)

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, invocations, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "squash", "--use-destination-message"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
	})
}

// TestShipGitDetachedHeadRefusesBeforeCommit proves a detached HEAD is refused
// rather than guessed at, under --no-push too: the plan resolves before any
// mutation on every lane.
func TestShipGitDetachedHeadRefusesBeforeCommit(t *testing.T) {
	for _, args := range [][]string{{"--no-watch"}, {"--no-push"}} {
		t.Run(args[0], func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote(), vcstest.Detached(), vcstest.Dirty())
			before := gitAt(t, f.Dir, "rev-parse", "HEAD")

			_, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate"}, args...)...)
			if err == nil || err.Error() != "ship: detached HEAD — check out a branch before shipping" {
				t.Fatalf("ship error = %v, want detached HEAD refusal", err)
			}
			assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
			if head := gitAt(t, f.Dir, "rev-parse", "HEAD"); head != before {
				t.Errorf("HEAD = %s, want it unmoved at %s", head, before)
			}
			if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "" {
				t.Errorf("branch = %q, want HEAD left detached", branch)
			}
			if gitAt(t, f.Dir, "status", "--porcelain") == "" {
				t.Error("the refused edit must stay uncommitted")
			}
		})
	}
}

// TestShipDetachedHeadAfterCommitSelfHeals proves a commit that leaves HEAD
// detached — twice observed from a gt-lane ship in a linked worktree — is
// repaired with git checkout -B instead of reported as a success.
func TestShipDetachedHeadAfterCommitSelfHeals(t *testing.T) {
	f := shipGTFeature(t)
	shipDetachHook(t, f, "")
	shipResetLog(t, f)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := shipGTInvocations(t, f)
	head := shipHead(t, f)
	if want := shipCommitted(t, f, vcs.Git) + " · branch feature · healed detached HEAD onto feature · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var healed []string
	for _, inv := range invocations {
		if inv[0] == "git" && inv[1] == "checkout" {
			healed = inv
		}
	}
	if want := []string{"git", "checkout", "-B", "feature", head}; !reflect.DeepEqual(healed, want) {
		t.Errorf("heal argv = %v, want %v", healed, want)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "feature" {
		t.Errorf("branch = %q, want HEAD reattached to feature", branch)
	}
	if tip := gitAt(t, f.Dir, "rev-parse", "feature"); tip != head {
		t.Errorf("feature = %s, want it moved to the commit at %s", tip, head)
	}
}

// TestShipGitUsesPostCommitBranch proves ship re-reads the branch after the
// commit rather than reusing the one it planned against. A repository's own
// post-commit hook is what moves it here, which is the shape that produced the
// bug: the branch under HEAD is not ship's to assume across a commit.
func TestShipGitUsesPostCommitBranch(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	writeShipExecutable(t, filepath.Join(f.Dir, ".git", "hooks"), "post-commit",
		"#!/bin/sh\ngit branch -f other HEAD\ngit symbolic-ref HEAD refs/heads/other\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.Git) + " · pushed other → origin"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "other"); n != 2 {
		t.Errorf("origin other holds %d commits, want the branch the hook left ship on", n)
	}
	assertInvocations(t, invocations, [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch.other.remote"},
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/other"},
		{"git", "push", "origin", "other"},
	})
}

func TestShipSessionTrailer(t *testing.T) {
	tests := []struct {
		name string
		jj   bool
		args []string
		want [][]string
		body string
	}{
		{
			name: "jj commit appends trailer",
			jj:   true,
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			body: "fix: frobnicate\n\nClaude-Session-Id: some-uuid",
		},
		{
			name: "git commit appends trailer",
			args: []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			body: "fix: frobnicate\n\nClaude-Session-Id: some-uuid",
		},
		{
			name: "jj amend with message appends trailer",
			jj:   true,
			args: []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			body: "fix: frobnicate\n\nClaude-Session-Id: some-uuid",
		},
		{
			name: "git amend with message appends trailer",
			args: []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			body: "fix: frobnicate\n\nClaude-Session-Id: some-uuid",
		},
		{
			name: "jj amend without message carries no trailer",
			jj:   true,
			args: []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			body: "wip",
		},
		{
			name: "git amend without message carries no trailer",
			args: []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			body: "wip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := shipKind(tt.jj)
			f := shipRepo(t, shipOptsFor(tt.jj, vcstest.Remote(), vcstest.Dirty())...)
			if slices.Contains(tt.args, "--amend") {
				shipAmendable(t, f, kind)
			}
			t.Setenv(envClaudeSessionKey, "some-uuid")

			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), tt.want)
			if want := shipCommitted(t, f, kind) + " · branch main · not pushed"; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if body := gitAt(t, f.Dir, "log", "-1", "--format=%B"); body != tt.body {
				t.Errorf("commit message = %q, want %q", body, tt.body)
			}
		})
	}
}

// TestShipGitAmendFastForwardPush amends a commit origin has never seen, so the
// amended tip still fast-forwards the remote branch.
func TestShipGitAmendFastForwardPush(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	shipAmendable(t, f, vcs.Git)

	got, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.Git) + " · pushed main → origin"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the amended commit on top of init", n)
	}
	if subject := gitAt(t, f.Dir, "--git-dir="+f.RemoteDir, "log", "-1", "--format=%s", "main"); subject != "fix: frobnicate" {
		t.Errorf("origin main tip = %q, want the amended commit", subject)
	}
	assertInvocations(t, invocations, [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "rev-parse", "HEAD"},
		{"git", "add", "-A"},
		{"git", "commit", "--amend", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch.main.remote"},
		{"git", "push", "origin", "main"},
	})
	// An amend of an unpushed commit fast-forwards: a plain push lands with no
	// force at all, and the lane must never fetch (a fetch would refresh the lease).
	for _, inv := range invocations {
		if len(inv) >= 2 && inv[0] == "git" && inv[1] == "fetch" {
			t.Errorf("fast-forward amend must not fetch, got %v", inv)
		}
		for _, arg := range inv {
			if strings.HasPrefix(arg, "--force-with-lease") {
				t.Errorf("fast-forward amend must not force-push, got %v", inv)
			}
		}
	}
}

// TestShipGitRebase covers the git lane's fetch-rebase-push sequence against a
// remote that moved: a divergence is replayed onto the real tip, a conflicting
// one aborts back to where it started, and a rebase that never begins is
// reported as itself rather than as a conflict.
func TestShipGitRebase(t *testing.T) {
	plan := [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	}
	remoteRef := "refs/remotes/origin/main"
	tests := []struct {
		name     string
		args     []string
		opts     []vcstest.Opt
		build    func(t *testing.T, f *vcstest.Fixture)
		want     [][]string
		branch   string
		remote   string
		rebased  int
		wantErr  []string
		wantWarn bool
	}{
		{
			name:   "no divergence pushes clean",
			branch: "main",
			remote: "origin",
			want: append(plan,
				[]string{"git", "config", "--get", "branch.main.remote"},
				[]string{"git", "fetch", "origin"},
				[]string{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				[]string{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				[]string{"git", "push", "origin", "main"}),
		},
		{
			name: "diverged rebases then pushes",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
			},
			branch:  "main",
			remote:  "origin",
			rebased: 1,
			want: append(plan,
				[]string{"git", "config", "--get", "branch.main.remote"},
				[]string{"git", "fetch", "origin"},
				[]string{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				[]string{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				[]string{"git", "rev-list", "--count", remoteRef + "..HEAD"},
				[]string{"git", "rebase", "--autostash", remoteRef},
				[]string{"git", "push", "origin", "main"},
				[]string{"git", "log", "-1", "--format=%h%x00%s"}),
		},
		{
			name: "rebase conflict aborts and reports",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "f.txt", "upstream\n")
			},
			want: append(plan,
				[]string{"git", "config", "--get", "branch.main.remote"},
				[]string{"git", "fetch", "origin"},
				[]string{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				[]string{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				[]string{"git", "rev-list", "--count", remoteRef + "..HEAD"},
				[]string{"git", "rebase", "--autostash", remoteRef},
				[]string{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				[]string{"git", "diff", "--name-only", "--diff-filter=U"},
				[]string{"git", "rebase", "--abort"}),
			wantErr: []string{"rebase onto origin/main conflicts in: f.txt", "resolve manually"},
		},
		{
			// origin carries no counterpart for a branch nobody has pushed, so
			// rev-parse exits 1 and the rebase is skipped outright.
			name:   "missing remote branch skips rebase",
			opts:   []vcstest.Opt{vcstest.Branch("feature")},
			branch: "feature",
			remote: "origin",
			want: append(plan,
				[]string{"git", "config", "--get", "branch.feature.remote"},
				[]string{"git", "fetch", "origin"},
				[]string{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/feature"},
				[]string{"git", "push", "origin", "feature"}),
		},
		{
			// b.txt stays out of the scoped commit, so the rebase autostashes it
			// onto an upstream that rewrote the same file: the pop conflicts and
			// the changes stay in the stash.
			name: "autostash pop conflict warns",
			args: []string{"f.txt"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				writeShipFile(t, f.Dir, "b.txt", "base\n")
				mustRun(t, f.Dir, "git", "add", "b.txt")
				mustRun(t, f.Dir, "git", "commit", "-qm", "add b")
				mustRun(t, f.Dir, "git", "push", "-q", "origin", "main")
				shipDivergeRemote(t, f, "main", "b.txt", "upstream\n")
				writeShipFile(t, f.Dir, "b.txt", "mine\n")
			},
			branch:   "main",
			remote:   "origin",
			rebased:  1,
			wantWarn: true,
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A", "--", "f.txt"},
				{"git", "commit", "-m", "fix: frobnicate", "--", "f.txt"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				{"git", "rev-list", "--count", remoteRef + "..HEAD"},
				{"git", "rebase", "--autostash", remoteRef},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
		},
		{
			name: "resolves the configured remote",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipSecondRemote(t, f, "backup")
				mustRun(t, f.Dir, "git", "config", "branch.main.remote", "backup")
			},
			branch: "main",
			remote: "backup",
			want: append(plan,
				[]string{"git", "config", "--get", "branch.main.remote"},
				[]string{"git", "fetch", "backup"},
				[]string{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/backup/main"},
				[]string{"git", "merge-base", "--is-ancestor", "refs/remotes/backup/main", "HEAD"},
				[]string{"git", "push", "backup", "main"}),
		},
		{
			// A crashed rebase left its state directory behind, so this one exits
			// before touching the working tree: no REBASE_HEAD, so no abort.
			name: "rebase failing before it starts is not a conflict",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
				if err := os.MkdirAll(filepath.Join(f.Dir, ".git", "rebase-merge"), 0o750); err != nil {
					t.Fatalf("mkdir rebase-merge: %v", err)
				}
			},
			want: append(plan,
				[]string{"git", "config", "--get", "branch.main.remote"},
				[]string{"git", "fetch", "origin"},
				[]string{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				[]string{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				[]string{"git", "rev-list", "--count", remoteRef + "..HEAD"},
				[]string{"git", "rebase", "--autostash", remoteRef},
				[]string{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"}),
			wantErr: []string{"ship: git rebase onto origin/main", "already a rebase-merge directory"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, append([]vcstest.Opt{vcstest.Remote(), vcstest.Dirty()}, tt.opts...)...)
			if tt.build != nil {
				tt.build(t, f)
			}
			before := shipHead(t, f)
			remoteBefore := remoteCount(t, f, "main")
			shipResetLog(t, f)
			buf := captureSlog(t)

			got, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)...)
			invocations := vcstest.Invocations(t, f.ArgvLog)
			assertInvocations(t, invocations, tt.want)
			if warned := strings.Contains(buf.String(), "git stash pop"); warned != tt.wantWarn {
				t.Errorf("autostash warning = %v, want %v (log: %q)", warned, tt.wantWarn, buf.String())
			}
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				if parent := gitAt(t, f.Dir, "rev-parse", "HEAD^"); parent != before {
					t.Errorf("HEAD^ = %s, want the pre-ship %s — the branch did not come back", parent, before)
				}
				if n := remoteCount(t, f, "main"); n != remoteBefore {
					t.Errorf("origin main holds %d commits, want the pre-ship %d", n, remoteBefore)
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := shipCommitted(t, f, vcs.Git)
			if tt.rebased > 0 {
				want += fmt.Sprintf(" · rebased %d commit(s) onto %s", tt.rebased, tt.branch)
			}
			want += fmt.Sprintf(" · pushed %s → %s", tt.branch, tt.remote)
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			bare := gitAt(t, f.Dir, "remote", "get-url", tt.remote)
			pushed := gitAt(t, f.Dir, "--git-dir="+bare, "rev-parse", "refs/heads/"+tt.branch)
			if head := shipHead(t, f); pushed != head {
				t.Errorf("%s/%s = %s, want the shipped HEAD %s", tt.remote, tt.branch, pushed, head)
			}
			if pushed == before {
				t.Errorf("%s/%s still sits at the pre-ship %s", tt.remote, tt.branch, before)
			}
		})
	}
}

// TestShipGitPushRetry covers the git lane against a remote that advances
// mid-ship: a rejected push re-fetches, rebases onto the tip that beat it, and
// pushes again, while a hook decline, a conflicting replay, and an amend are
// each terminal on the first refusal.
func TestShipGitPushRetry(t *testing.T) {
	plan := [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch.main.remote"},
	}
	remoteRef := "refs/remotes/origin/main"
	attempt := [][]string{
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", remoteRef},
		{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
		{"git", "push", "origin", "main"},
	}
	rebasingAttempt := [][]string{
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", remoteRef},
		{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
		{"git", "rev-list", "--count", remoteRef + "..HEAD"},
		{"git", "rebase", "--autostash", remoteRef},
		{"git", "push", "origin", "main"},
	}
	describe := []string{"git", "log", "-1", "--format=%h%x00%s"}
	tests := []struct {
		name        string
		args        []string
		build       func(t *testing.T, f *vcstest.Fixture)
		want        [][]string
		rebased     int
		remoteCount int
		lease       bool
		wantErr     []string
	}{
		{
			name: "rejected push refetches and lands",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "git", "push*", "u.txt", 1)
			},
			rebased:     1,
			remoteCount: 3,
			want:        slices.Concat(plan, attempt, rebasingAttempt, [][]string{describe}),
		},
		{
			name: "retries exhausted names the remedy",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "git", "push*", "u.txt", 3)
			},
			remoteCount: 4,
			want:        slices.Concat(plan, attempt, rebasingAttempt, rebasingAttempt),
			wantErr:     []string{"rejected 3 times", "git fetch origin && git rebase --autostash origin/main && git push", "non-fast-forward"},
		},
		{
			name: "conflict during retry rebase is terminal",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "git", "push*", "f.txt", 1)
			},
			remoteCount: 2,
			want: slices.Concat(plan, attempt, [][]string{
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", remoteRef},
				{"git", "merge-base", "--is-ancestor", remoteRef, "HEAD"},
				{"git", "rev-list", "--count", remoteRef + "..HEAD"},
				{"git", "rebase", "--autostash", remoteRef},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				{"git", "diff", "--name-only", "--diff-filter=U"},
				{"git", "rebase", "--abort"},
			}),
			wantErr: []string{"rebase onto origin/main conflicts", "f.txt"},
		},
		{
			// The amend push never fetches, so the lease is pinned to the commit
			// this checkout last saw on the remote — which the upstream commit has
			// already moved past.
			name: "amend stale lease never fetches or retries",
			args: []string{"--amend"},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipAmendable(t, f, vcs.Git)
				mustRun(t, f.Dir, "git", "push", "-q", "origin", "main")
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
			},
			remoteCount: 3,
			lease:       true,
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "rev-parse", "HEAD"},
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "push", "origin", "main"},
			},
			wantErr: []string{"built on the commit you amended"},
		},
		{
			name: "hook decline does not retry",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDeclineRemote(t, f)
			},
			remoteCount: 1,
			want:        slices.Concat(plan, attempt),
			wantErr:     []string{"ship: git push:", "pre-receive hook declined"},
		},
		{
			name: "both attempts rebase reports the count once",
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
				shipRaceRemote(t, f, "git", "push*", "r.txt", 1)
			},
			rebased:     1,
			remoteCount: 4,
			want:        slices.Concat(plan, rebasingAttempt, rebasingAttempt, [][]string{describe}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
			tt.build(t, f)
			before := shipHead(t, f)
			shipResetLog(t, f)

			got, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)...)
			want := tt.want
			if tt.lease {
				want = append(want, []string{"git", "push", "origin", "--force-with-lease=main:" + before, "main"})
			}
			assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), want)
			if n := remoteCount(t, f, "main"); n != tt.remoteCount {
				t.Errorf("origin main holds %d commits, want %d", n, tt.remoteCount)
			}
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				if tip := gitAt(t, f.Dir, "--git-dir="+f.RemoteDir, "log", "-1", "--format=%s"); tip == "fix: frobnicate" {
					t.Error("origin main carries the refused commit")
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			summary := shipCommitted(t, f, vcs.Git) +
				fmt.Sprintf(" · rebased %d commit(s) onto main · pushed main → origin", tt.rebased)
			if got != summary {
				t.Errorf("summary = %q, want %q", got, summary)
			}
			pushed := gitAt(t, f.Dir, "--git-dir="+f.RemoteDir, "rev-parse", "refs/heads/main")
			if head := shipHead(t, f); pushed != head {
				t.Errorf("origin main = %s, want the shipped HEAD %s", pushed, head)
			}
			if pushed == before {
				t.Errorf("origin main still sits at the pre-ship %s", before)
			}
		})
	}
}

// TestGitPushRejectedClassifies proves the retry veto reads git's per-ref reason
// tokens: a push that mixes a non-fast-forward ref with a hook-declined one is
// terminal, however the two lines are ordered. The bytes are one real git push's.
func TestGitPushRejectedClassifies(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
	writeShipFile(t, f.Dir, "g.txt", "feature\n")
	mustRun(t, f.Dir, "git", "add", "-A")
	mustRun(t, f.Dir, "git", "commit", "-qm", "feature")
	mustRun(t, f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "f.txt", "mine\n")
	mustRun(t, f.Dir, "git", "commit", "-qam", "mine")
	shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
	writeShipExecutable(t, filepath.Join(f.RemoteDir, "hooks"), "update",
		"#!/bin/sh\ntest \"$1\" != refs/heads/feature\n")

	mustRun(t, f.Dir, "git", "fetch", "-q", "origin")

	lone, err := combinedRun(t, f.Dir, "git", "push", "origin", "main")
	if err == nil {
		t.Fatalf("git push succeeded, want main refused:\n%s", lone)
	}
	if !strings.Contains(lone, "(non-fast-forward)") {
		t.Fatalf("git push output carries no (non-fast-forward):\n%s", lone)
	}
	if !gitPushRejected(errors.New(lone)) {
		t.Errorf("gitPushRejected = false for a lone non-fast-forward rejection:\n%s", lone)
	}

	mixed, err := combinedRun(t, f.Dir, "git", "push", "origin", "main", "feature")
	if err == nil {
		t.Fatalf("git push succeeded, want both refs refused:\n%s", mixed)
	}
	if !strings.Contains(mixed, "[remote rejected]") {
		t.Fatalf("git push output carries no [remote rejected]:\n%s", mixed)
	}
	if gitPushRejected(errors.New(mixed)) {
		t.Errorf("gitPushRejected = true for a push carrying a remote rejection:\n%s", mixed)
	}
}

// TestShipNoWatchSkipsCI proves --no-watch still lands the whole jj lane: the
// commit is cut, the trunk bookmark moves onto it, and the bare origin gains
// the commit — with no gh call to confirm anything afterwards.
func TestShipNoWatchSkipsCI(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	before := remoteCount(t, f, "main")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if inv[0] == "gh" {
			t.Errorf("--no-watch must reach no gh call, got %v", inv)
		}
	}
	head := strings.TrimSpace(mustRun(t, f.Dir, "jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id.short()"))
	if want := fmt.Sprintf(`committed %s "fix: frobnicate" · pushed main → origin`, head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if after := remoteCount(t, f, "main"); after != before+1 {
		t.Errorf("remote main count = %d, want %d", after, before+1)
	}
	if subject := gitAt(t, f.Dir, "--git-dir="+f.RemoteDir, "log", "-1", "--format=%s", "main"); subject != "fix: frobnicate" {
		t.Errorf("remote main tip = %q, want the shipped commit", subject)
	}
}

func TestShipCIStates(t *testing.T) {
	tests := []struct {
		name      string
		withGh    bool
		runList   string
		viewJSON  string
		watchExit string
		ci        string
		wantErr   bool
		wantWatch bool
	}{
		{
			name:      "gh missing",
			withGh:    false,
			ci:        "gh-missing",
			wantWatch: false,
		},
		{
			name:      "no run",
			withGh:    true,
			runList:   "[]",
			ci:        "no-run",
			wantWatch: false,
		},
		{
			name:      "failure",
			withGh:    true,
			runList:   fakeRunListJSON,
			viewJSON:  ghStdout(t, "run-view-failed"),
			watchExit: "1",
			ci:        "failure",
			wantErr:   true,
			wantWatch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
			if tt.withGh {
				writeShipGH(t, f)
			} else {
				f.OnlyShimPATH(t)
				if path, err := exec.LookPath("gh"); err == nil {
					t.Fatalf("gh resolved to %s; this row must run with none on PATH", path)
				}
			}
			if tt.runList != "" {
				t.Setenv("GH_RUN_LIST_JSON", tt.runList)
			}
			if tt.viewJSON != "" {
				t.Setenv("GH_RUN_VIEW_JSON", tt.viewJSON)
			}
			if tt.watchExit != "" {
				t.Setenv("GH_WATCH_EXIT", tt.watchExit)
			}
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if want := shipCommitted(t, f, vcs.JJ) + " · pushed main → origin · CI " + tt.ci; got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			watched := false
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					watched = true
				}
			}
			if watched != tt.wantWatch {
				t.Errorf("gh run watch invoked = %v, want %v", watched, tt.wantWatch)
			}
			if n := remoteCount(t, f, "main"); n != 2 {
				t.Errorf("origin main holds %d commits, want the pushed one on top of init", n)
			}
		})
	}
}

func TestShipCINoRunWithWorkflowIsUnconfirmed(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	writeShipFile(t, f.Dir, filepath.Join(".github", "workflows", "ci.yml"), "name: ci\n")
	t.Setenv("GH_RUN_LIST_JSON", "[]")
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when workflows exist but no run was registered")
	}
	if want := "· CI unconfirmed"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	for _, want := range []string{"no CI run was registered", "paths-filtered", "dispatch-only", "on: workflow_dispatch", "gh run list --commit " + shipHead(t, f)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the push an unconfirmed CI must not undo", n)
	}
}

func TestShipHeadSHAFailurePrintsCommitPushSummary(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	shipJJFails(t, f, "*commit_id")

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected head SHA error, got nil")
	}
	if want := "committed "; !strings.HasPrefix(out, want) {
		t.Errorf("output = %q, want it to open with the commit segment", out)
	}
	if want := " · pushed main → origin\n"; !strings.HasSuffix(out, want) {
		t.Errorf("output = %q, want it to end at the push segment", out)
	}
	if strings.Contains(out, "CI ") {
		t.Errorf("head SHA failure must not print a CI segment, got %q", out)
	}
	if !strings.Contains(err.Error(), "jj log commit_id") {
		t.Errorf("error = %v, want jj log commit_id failure", err)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if inv[0] == "gh" {
			t.Errorf("head SHA failure must stop before gh, got invocation %v", inv)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the push that landed before the read failed", n)
	}
}

func TestJJExactPattern(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "main", `exact:"main"`},
		{"slash", "someone/probe", `exact:"someone/probe"`},
		{"at sign", "foo@bar", `exact:"foo@bar"`},
		{"double quote", `has"quote`, `exact:"has\"quote"`},
		{"backslash", `back\slash`, `exact:"back\\slash"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vcs.JJExactPattern(tt.in); got != tt.want {
				t.Errorf("vcs.JJExactPattern(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestJJRevsetsParseLive pins every revset ship builds to the grammar jj
// actually parses. Go's %q spells a rune it considers unprintable as \a, \b,
// \f, \v, \u, or \U, and jj 0.43's revset parser accepts none of those — while
// git happily allows several of them in a ref name, so a bookmark carrying one
// turned every revset built for it into a syntax error.
func TestJJRevsetsParseLive(t *testing.T) {
	requireLiveVCS(t, "git", "jj")
	dir := setupLiveJJRepo(t, "a\n", "b\n")
	// jj's own bookmark-name parser refuses a non-breaking space, so the branch
	// enters through git and jj imports it on the next command.
	const branch = "feat\u00a0x"
	mustRun(t, dir, "git", "branch", branch)
	head := strings.TrimSpace(mustRun(t, dir, "jj", "log", "-r", "@-", "-T", "commit_id.short()", "--no-graph"))

	tests := []struct {
		name   string
		revset string
		want   string
	}{
		{"bookmarks", jjBookmarksRevset(branch), head},
		{"ancestor", jjAncestorRevset(branch), head},
		{"stack", jjStackRevset(branch), ""},
		{"conflict", jjConflictRevset(branch), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.TrimSpace(mustRun(t, dir, "jj", "log", "-r", tt.revset, "-T", "commit_id.short()", "--no-graph"))
			if got != tt.want {
				t.Errorf("jj log -r %s = %q, want %q", tt.revset, got, tt.want)
			}
		})
	}
}

// TestGitIsAncestorPrefix pins the caller's own prefix onto the ancestry error:
// restack shares the helper, and a hardcoded one reads "restack: … : ship: …".
// TestGitIsAncestorPrefix pins the caller's own prefix onto the ancestry error:
// restack shares the helper, and a hardcoded one reads "restack: … : ship: …".
// git answers a ref no repository holds with a fatal at exit 128.
func TestGitIsAncestorPrefix(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	for _, prefix := range []string{"ship", "restack"} {
		t.Run(prefix, func(t *testing.T) {
			shipResetLog(t, f)

			_, err := gitIsAncestor(context.Background(), prefix, "refs/heads/no-such-ref", "HEAD")
			want := prefix + ": git merge-base --is-ancestor: exit 128: fatal: Not a valid object name refs/heads/no-such-ref"
			if err == nil || err.Error() != want {
				t.Fatalf("gitIsAncestor error = %v, want %q", err, want)
			}
			assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), [][]string{
				{"git", "merge-base", "--is-ancestor", "refs/heads/no-such-ref", "HEAD"},
			})
		})
	}
}

// TestShipJJRebase covers the jj lane's fetch-rebase-move-push sequence: an
// untracked counterpart is tracked before the fetch that needs it, a divergence
// is replayed and the bookmark advanced only after, and every refusal past the
// commit leaves the bookmark where the fetch found it.
func TestShipJJRebase(t *testing.T) {
	stack := func(target string) []string {
		return []string{"jj", "--ignore-working-copy", "log", "-r", jjBookmarksRevset(target), "--no-graph", "-T", jjStackLineTemplate}
	}
	ancestors := func(target string) []string {
		return []string{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset(target), "--no-graph", "-T", jjBookmarkTemplate}
	}
	stackRevset := func(target string) []string {
		return []string{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset(target), "--no-graph", "-T", jjStackLineTemplate}
	}
	conflicts := func(target string) []string {
		return []string{"jj", "--ignore-working-copy", "log", "-r", jjConflictRevset(target), "--no-graph", "-T", jjStackLineTemplate}
	}
	describe := []string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate}
	opLog := []string{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate}
	fetch := []string{"jj", "git", "fetch"}
	revert := []string{"jj", "op", "revert", shipOpIDMark}
	prologue := func(target string) [][]string {
		return slices.Concat(jjPlanArgv(), [][]string{
			stack(target),
			{"jj", "diff", "--name-only"},
			{"jj", "commit", "-m", "fix: frobnicate"},
			describe,
			{"jj", "bookmark", "list", vcs.JJExactPattern(target), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
		})
	}
	advance := func(target string) [][]string {
		return [][]string{
			{"jj", "bookmark", "move", vcs.JJExactPattern(target), "--to", "@-"},
			opLog,
			{"jj", "git", "push", "--bookmark", vcs.JJExactPattern(target)},
		}
	}
	plainAttempt := func(target string) [][]string {
		return slices.Concat([][]string{fetch, stack(target), ancestors(target)}, advance(target))
	}
	replay := func(target string) [][]string {
		return [][]string{
			stackRevset(target),
			{"jj", "rebase", "-b", "@-", "--destination", jjBookmarksRevset(target)},
			opLog,
			conflicts(target),
		}
	}
	rebasingAttempt := func(target string) [][]string {
		return slices.Concat([][]string{fetch, stack(target), ancestors(target)}, replay(target), advance(target))
	}

	tests := []struct {
		name    string
		args    []string
		opts    []vcstest.Opt
		build   func(t *testing.T, f *vcstest.Fixture)
		target  string
		pushed  string
		want    [][]string
		rebased int
		wantErr []string
	}{
		{
			name:  "untracked trunk auto-tracks before fetch then pushes",
			build: func(t *testing.T, f *vcstest.Fixture) { shipJJRemotes(t, f, "", "backup") },
			want: slices.Concat(prologue("main"), [][]string{
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=origin"},
			}, plainAttempt("main")),
		},
		{
			// The trunk's counterpart is untracked on a non-origin remote while
			// main@origin is tracked: ship tracks the remote the untracked
			// counterpart actually sits on, not a hard-coded origin.
			name:  "untracked counterpart on a non-origin remote tracks that remote",
			build: func(t *testing.T, f *vcstest.Fixture) { shipJJRemotes(t, f, "", "origin") },
			want: slices.Concat(prologue("main"), [][]string{
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=backup"},
			}, plainAttempt("main")),
		},
		{
			// Two remotes carry an untracked counterpart, so ship breaks the tie on
			// the remote jj git push targets — the git.push config setting.
			name:   "multiple untracked counterparts track the push target",
			pushed: "backup",
			build:  func(t *testing.T, f *vcstest.Fixture) { shipJJRemotes(t, f, "backup") },
			want: slices.Concat(prologue("main"), [][]string{
				{"jj", "--ignore-working-copy", "config", "get", "git.push"},
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=backup"},
			}, plainAttempt("main")),
		},
		{
			name: "diverged trunk rebases",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
			},
			rebased: 1,
			want:    slices.Concat(prologue("main"), rebasingAttempt("main"), [][]string{describe}),
		},
		{
			name: "diverged --bookmark rebases",
			args: []string{"--bookmark", "someone/probe"},
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "jj", "bookmark", "create", "someone/probe", "-r", "@-")
				mustRun(t, f.Dir, "jj", "git", "push", "--bookmark", "someone/probe")
				shipDivergeRemote(t, f, "someone/probe", "u.txt", "upstream\n")
			},
			target:  "someone/probe",
			rebased: 1,
			want: slices.Concat(prologue("someone/probe"), rebasingAttempt("someone/probe"),
				[][]string{describe}),
		},
		{
			name:    "conflicted target refuses",
			opts:    []vcstest.Opt{vcstest.Remote(), vcstest.ConflictedBookmark()},
			target:  "feat",
			want:    append(jjPlanArgv(), stack("feat")),
			wantErr: []string{`bookmark "feat" is conflicted (2 heads)`, "resolve it"},
		},
		{
			name: "conflicted rebase rolls back",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "f.txt", "upstream\n")
			},
			want: slices.Concat(prologue("main"), [][]string{fetch, stack("main"), ancestors("main")},
				replay("main"), [][]string{revert}),
			wantErr: []string{`rebase onto "main" conflicts in 2 commit(s)`, "rolled back"},
		},
		{
			name: "conflict check failure rolls back",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
				shipJJFails(t, f, `*"conflicts()"*`)
			},
			want: slices.Concat(prologue("main"), [][]string{fetch, stack("main"), ancestors("main")},
				replay("main"), [][]string{revert}),
			wantErr: []string{`conflict check after rebase onto "main" failed (rebase rolled back)`},
		},
		{
			// Someone else pushed this very commit and one more on top, so the
			// stack the rebase would replay is empty.
			name:  "already landed refuses",
			opts:  []vcstest.Opt{vcstest.Remote()},
			build: shipRaceLanded,
			want: slices.Concat(prologue("main"),
				[][]string{fetch, stack("main"), ancestors("main"), stackRevset("main")}),
			wantErr: []string{"already landed", "refusing to move the bookmark backwards"},
		},
		{
			name: "fetch failure is fatal",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				mustRun(t, f.Dir, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent.git"))
			},
			want:    slices.Concat(prologue("main"), [][]string{fetch}),
			wantErr: []string{"ship: jj git fetch", "Could not find repository"},
		},
		{
			name: "rejected push restores and lands",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "jj", `"git push"*`, "u.txt", 1)
			},
			rebased: 1,
			want: slices.Concat(prologue("main"), plainAttempt("main"), [][]string{revert},
				rebasingAttempt("main"), [][]string{describe}),
		},
		{
			name: "retries exhausted restores last state",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "jj", `"git push"*`, "u.txt", 3)
			},
			want: slices.Concat(prologue("main"), plainAttempt("main"), [][]string{revert},
				rebasingAttempt("main"), [][]string{revert},
				rebasingAttempt("main"), [][]string{revert}),
			wantErr: []string{"rejected 3 times", "jj git fetch && jj rebase -b @-", "unexpectedly moved"},
		},
		{
			name: "amend rejection refuses",
			args: []string{"--amend"},
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipAmendable(t, f, vcs.JJ)
				shipRaceRemote(t, f, "jj", `"git push"*`, "u.txt", 1)
			},
			want: slices.Concat(jjPlanArgv(), [][]string{
				stack("main"),
				{"jj", "squash", "-m", "fix: frobnicate"},
				describe,
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
			}, plainAttempt("main"), [][]string{revert}),
			wantErr: []string{"not force-retrying over their work"},
		},
		{
			name:    "permission failure passes through",
			opts:    []vcstest.Opt{vcstest.Remote()},
			build:   shipDeclineRemote,
			want:    slices.Concat(prologue("main"), plainAttempt("main")),
			wantErr: []string{"ship: jj git push:", "pre-receive hook declined"},
		},
		{
			name: "conflict during retry rebase rolls back",
			opts: []vcstest.Opt{vcstest.Remote()},
			build: func(t *testing.T, f *vcstest.Fixture) {
				shipRaceRemote(t, f, "jj", `"git push"*`, "f.txt", 1)
			},
			want: slices.Concat(prologue("main"), plainAttempt("main"), [][]string{revert},
				[][]string{fetch, stack("main"), ancestors("main")}, replay("main"), [][]string{revert}),
			wantErr: []string{`rebase onto "main" conflicts`, "rolled back"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, append([]vcstest.Opt{vcstest.JJ(), vcstest.Dirty()}, tt.opts...)...)
			if tt.build != nil {
				tt.build(t, f)
			}
			shipResetLog(t, f)

			got, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)...)
			assertInvocations(t, shipMaskOpIDs(vcstest.Invocations(t, f.ArgvLog)), tt.want)
			target, remote := "main", "origin"
			if tt.target != "" {
				target = tt.target
			}
			if tt.pushed != "" {
				remote = tt.pushed
			}
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				if tip := shipRemoteTip(t, f, remote, target); tip == jjRevID(t, f.Dir, "@-") {
					t.Errorf("%s %s carries the commit ship refused to land", remote, target)
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := shipCommitted(t, f, vcs.JJ)
			if tt.rebased > 0 {
				want += fmt.Sprintf(" · rebased %d commit(s) onto %s", tt.rebased, target)
			}
			want += fmt.Sprintf(" · pushed %s → origin", target)
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if tip, at := shipRemoteTip(t, f, remote, target), jjRevID(t, f.Dir, "@-"); tip != at {
				t.Errorf("%s %s = %s, want the shipped commit %s", remote, target, tip, at)
			}
		})
	}
}

// TestShipJJPushRevertTargetsBookmarkMove proves a rejected push undoes only
// the bookmark move: the first attempt rebases and then moves, and the
// operation the rollback names is the move, so the rebase survives to be
// replayed onto the tip that beat it.
func TestShipJJPushRevertTargetsBookmarkMove(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
	shipRaceRemote(t, f, "jj", `"git push"*`, "u.txt", 1)
	shipResetLog(t, f)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	reverted := shipRevertedOps(vcstest.Invocations(t, f.ArgvLog))
	if len(reverted) != 1 {
		t.Fatalf("op revert targets = %v, want exactly the bookmark move", reverted)
	}
	if desc := shipOpDescription(t, f, reverted[0]); !strings.HasPrefix(desc, "point bookmark main to commit") {
		t.Errorf("reverted operation = %q, want the bookmark move", desc)
	}
	if tip, at := shipRemoteTip(t, f, "origin", "main"), jjRevID(t, f.Dir, "@-"); tip != at {
		t.Errorf("origin main = %s, want the replayed commit %s", tip, at)
	}
}

// TestShipJJPushRevertFailureIsTerminal proves a rollback that itself fails ends
// the ship where it stands: the manual revert is named and no second attempt
// fetches.
func TestShipJJPushRevertFailureIsTerminal(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipJJFails(t, f, `"op revert"*`)
	shipRaceRemote(t, f, "jj", `"git push"*`, "u.txt", 1)
	shipResetLog(t, f)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil {
		t.Fatal("expected terminal error when op revert fails")
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	reverted := shipRevertedOps(invocations)
	if len(reverted) != 1 {
		t.Fatalf("op revert targets = %v, want the one that failed", reverted)
	}
	if want := "jj op revert " + reverted[0]; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err, want)
	}
	fetches := 0
	for _, inv := range invocations {
		if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "git" && inv[2] == "fetch" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("jj git fetch count = %d, want 1 (a failed undo must not retry)", fetches)
	}
	if tip := shipRemoteTip(t, f, "origin", "main"); tip == jjRevID(t, f.Dir, "@-") {
		t.Error("origin main carries the commit the rejected push never landed")
	}
}

// TestShipJJRebasePreservesHookSummary proves the hook segment survives the
// rebase the push runs into: the report carries one hooks segment and one
// commit segment, not a second pass's copy of either.
func TestShipJJRebasePreservesHookSummary(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipHookRepo(t, f, vcs.JJ, 0, "", "f1.go")
	mustRun(t, f.Dir, "jj", "git", "push", "--bookmark", "main")
	shipDivergeRemote(t, f, "main", "u.txt", "upstream\n")
	shipResetLog(t, f)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "hooks ok · " + shipCommitted(t, f, vcs.JJ) + " · rebased 1 commit(s) onto main · pushed main → origin"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if count := strings.Count(got, "hooks ok"); count != 1 {
		t.Errorf("hooks segment count = %d, want 1 in %q", count, got)
	}
	if count := strings.Count(got, "committed "); count != 1 {
		t.Errorf("committed segment count = %d, want 1 in %q", count, got)
	}
	if tip, at := shipRemoteTip(t, f, "origin", "main"), jjRevID(t, f.Dir, "@-"); tip != at {
		t.Errorf("origin main = %s, want the rebased commit %s", tip, at)
	}
}

// TestShipJJNonTrunkBookmarkAppends proves the old "nearest bookmark is not
// trunk" refusal is gone: the answer it demanded was always the bookmark the
// working copy already sat on, so ship appends to it and names it in the report.
func TestShipJJNonTrunkBookmarkAppends(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipJJBookmarks(t, f, "someone/probe")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.JJ) + " · branch someone/probe · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, invocations, append(
		jjPlanArgv(),
		[]string{"jj", "diff", "--name-only"},
		[]string{"jj", "commit", "-m", "fix: frobnicate"},
		[]string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		[]string{"jj", "bookmark", "move", vcs.JJExactPattern("someone/probe"), "--to", "@-"},
	))
	if got, want := jjRevID(t, f.Dir, `bookmarks(exact:"someone/probe")`), jjRevID(t, f.Dir, "@-"); got != want {
		t.Errorf("someone/probe = %s, want the commit at @- %s", got, want)
	}
	if got, want := jjRevID(t, f.Dir, `bookmarks(exact:"main")`), jjRevID(t, f.Dir, "@--"); got == want {
		t.Errorf("trunk main moved to %s, want it left behind", got)
	}
}

// TestShipJJNonTrunkBookmarkPushesItself proves the appended bookmark is the one
// pushed, not trunk.
func TestShipJJNonTrunkBookmarkPushesItself(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipJJBookmarks(t, f, "someone/probe")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.JJ) + " · pushed someone/probe → origin"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range invocations {
		if inv[0] == "jj" && inv[1] == "git" && inv[2] == "push" && inv[len(inv)-1] != vcs.JJExactPattern("someone/probe") {
			t.Errorf("push argv = %v, want it to target %s", inv, vcs.JJExactPattern("someone/probe"))
		}
	}
	if n := remoteCount(t, f, "someone/probe"); n != 3 {
		t.Errorf("origin someone/probe holds %d commits, want init, wip, and the shipped one", n)
	}
	if n := remoteCount(t, f, "main"); n != 1 {
		t.Errorf("origin main holds %d commits, want the trunk left where it was", n)
	}
}

func TestShipJJMultipleNearestBookmarksFails(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	shipJJBookmarks(t, f, "feat-a", "feat-b")

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when several bookmarks are nearest, got nil")
	}
	if !strings.Contains(err.Error(), "multiple nearest bookmarks feat-a, feat-b (trunk main is not among them)") {
		t.Errorf("error = %v, want it to name every candidate and the trunk none of them is", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	assertInvocations(t, invocations, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"git", "--git-dir", filepath.Join(f.Dir, ".git"), "worktree", "list", "--porcelain", "-z", "--end-of-options"},
	})
	assertNoShipMutation(t, invocations)
	if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "wip" {
		t.Errorf("@- = %q, want no commit cut", subject)
	}
}

func TestShipJJNearestBookmarksResolve(t *testing.T) {
	t.Run("trunk among the candidates wins, and the report says so", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipJJBookmarks(t, f, "feat-a", "main", "feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := "bookmark main (trunk, chosen over feat-a, feat-b) · " + shipCommitted(t, f, vcs.JJ) + " · pushed main → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := remoteCount(t, f, "main"); n != 3 {
			t.Errorf("origin main holds %d commits, want init, wip, and the shipped one", n)
		}
	})

	t.Run("--branch naming a candidate picks it", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipJJBookmarks(t, f, "feat-a", "feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "feat-b")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed feat-b → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		for _, inv := range invocations {
			if inv[0] == "jj" && inv[1] == "git" && inv[2] == "push" && inv[len(inv)-1] != vcs.JJExactPattern("feat-b") {
				t.Errorf("push argv = %v, want it to target %s", inv, vcs.JJExactPattern("feat-b"))
			}
		}
		if n := remoteCount(t, f, "feat-b"); n != 3 {
			t.Errorf("origin feat-b holds %d commits, want init, wip, and the shipped one", n)
		}
		if gitBranchExists(t, f.RemoteDir, "feat-a") {
			t.Error("origin carries feat-a — the candidate --branch passed over was pushed")
		}
	})

	t.Run("--branch naming a bookmark elsewhere retracts the trunk segment", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		mustRun(t, f.Dir, "jj", "bookmark", "create", "other", "-r", "@-")
		shipJJBookmarks(t, f, "main", "feat-a")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "other")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed other → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := remoteCount(t, f, "other"); n != 3 {
			t.Errorf("origin other holds %d commits, want init, wip, and the shipped one", n)
		}
	})
}

// TestShipJJBookmarkTieHolders pins the holder tiebreak as a layer over the
// trunk-alias rule, not a replacement: a nearest bookmark another working copy
// has checked out is that copy's rather than an equal alternative, while a tie
// no holder answers for is still decided exactly as it was before — the common
// case, since git names a holder only while some checkout holds the branch.
func TestShipJJBookmarkTieHolders(t *testing.T) {
	t.Run("a candidate another working copy holds loses the tie", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipJJBookmarks(t, f, "main", "feat-a")
		other := shipHoldBranch(t, f, "main")
		shipResetLog(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := fmt.Sprintf("bookmark feat-a (chosen over main held in %s) · ", other) + shipCommitted(t, f, vcs.JJ) + " · pushed feat-a → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(invocations); n != 1 {
			t.Errorf("holder lookups = %d, want the one the tie needed", n)
		}
		if n := remoteCount(t, f, "feat-a"); n != 3 {
			t.Errorf("origin feat-a holds %d commits, want the candidate no checkout held", n)
		}
		if n := remoteCount(t, f, "main"); n != 1 {
			t.Errorf("origin main holds %d commits, want the held candidate left alone", n)
		}
	})

	t.Run("no holder leaves the trunk alias to decide", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipJJBookmarks(t, f, "feat-a", "main", "feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := "bookmark main (trunk, chosen over feat-a, feat-b) · " + shipCommitted(t, f, vcs.JJ) + " · pushed main → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(invocations); n != 1 {
			t.Errorf("holder lookups = %d, want the one the tie needed", n)
		}
		if n := remoteCount(t, f, "main"); n != 3 {
			t.Errorf("origin main holds %d commits, want the trunk alias's pick pushed", n)
		}
	})

	t.Run("a lone candidate never asks who holds it", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipHoldBranch(t, f, "main")
		shipResetLog(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed main → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(invocations); n != 0 {
			t.Errorf("holder lookups = %d, want none when there is no tie to break", n)
		}
		if n := remoteCount(t, f, "main"); n != 2 {
			t.Errorf("origin main holds %d commits, want the lone candidate pushed", n)
		}
	})

	t.Run("--branch settles the tie before any holder is asked", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		shipJJBookmarks(t, f, "feat-a", "feat-b")
		shipHoldBranch(t, f, "feat-b")
		shipResetLog(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "feat-b")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed feat-b → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(invocations); n != 0 {
			t.Errorf("holder lookups = %d, want none when the caller already chose", n)
		}
		if n := remoteCount(t, f, "feat-b"); n != 3 {
			t.Errorf("origin feat-b holds %d commits, want the branch the caller named pushed", n)
		}
	})
}

// TestShipHealRefusedNamesHolder covers the guard on the self-heal's failure
// path: a heal git refuses because a sibling checkout has the branch out says
// which one, and a refusal no holder explains keeps the message it always had.
func TestShipHealRefusedNamesHolder(t *testing.T) {
	// git reports a worktree by its canonical path, so the fixture spells the
	// sibling checkout the same way rather than through the symlinked temp root.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	other := filepath.Join(base, "wt")
	tests := []struct {
		name string
		// rest is what the post-commit hook does after detaching HEAD, which is
		// what makes git refuse the heal: a sibling checkout takes the branch, or
		// a stale index.lock blocks the checkout with no holder to blame.
		rest string
		want string
	}{
		{
			name: "a sibling checkout holds the branch",
			rest: "git worktree add -q '" + other + "' feature\n",
			want: "git checkout -B feature failed — that branch is checked out in " + other,
		},
		{
			name: "nobody holds it",
			rest: ": > \"$(git rev-parse --git-dir)/index.lock\"\n",
			want: "git checkout -B feature failed: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTFeature(t)
			shipDetachHook(t, f, tt.rest)
			shipResetLog(t, f)

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatal("expected an error when the heal checkout is refused, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if n := holderLookups(shipGTInvocations(t, f)); n != 1 {
				t.Errorf("holder lookups = %d, want the one the refusal needed", n)
			}
			if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "" {
				t.Errorf("branch = %q, want HEAD left detached by the refused heal", branch)
			}
		})
	}
}

// TestShipHealSuccessAsksNoHolder proves the guard sits on the failure path
// alone: a heal that works spawns no holder lookup.
func TestShipHealSuccessAsksNoHolder(t *testing.T) {
	f := shipGTFeature(t)
	shipDetachHook(t, f, "")
	shipResetLog(t, f)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := shipGTInvocations(t, f)
	if want := shipCommitted(t, f, vcs.Git) + " · branch feature · healed detached HEAD onto feature · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := holderLookups(invocations); n != 0 {
		t.Errorf("holder lookups = %d, want none from a heal that succeeded", n)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "feature" {
		t.Errorf("branch = %q, want HEAD reattached to feature", branch)
	}
}

// TestShipJJAmbiguousTrunkFails proves several trunk candidates are refused,
// listing them: that is genuine ambiguity, not something to guess at.
func TestShipJJAmbiguousTrunkFails(t *testing.T) {
	t.Run("two real remotes refuse", func(t *testing.T) {
		f := shipAmbiguousTrunk(t)

		_, err := runShipCmd(t, "-m", "fix: frobnicate")
		if err == nil {
			t.Fatal("expected error when trunk is ambiguous, got nil")
		}
		if !strings.Contains(err.Error(), `cannot resolve the trunk bookmark from ["dev" "main"]`) {
			t.Errorf("error = %v, want it to list both candidates", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		assertInvocations(t, invocations, [][]string{
			{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		})
		assertNoShipMutation(t, invocations)
		if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "init" {
			t.Errorf("@- = %q, want no commit cut", subject)
		}
	})

	// A colocated repository gives every local bookmark a counterpart on jj's
	// @git pseudo-remote, so feat-x reaches the unfiltered trunk template and
	// the filter is the only thing keeping it out of the candidate set.
	t.Run("a local branch resting on trunk is not a candidate", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
		mustRun(t, f.Dir, "jj", "bookmark", "create", "feat-x", "-r", "@-")
		shipResetLog(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := "bookmark main (trunk, chosen over feat-x) · " + shipCommitted(t, f, vcs.JJ) + " · pushed main → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := remoteCount(t, f, "main"); n != 2 {
			t.Errorf("origin main holds %d commits, want the trunk the filter resolved", n)
		}
	})
}

// TestShipJJAmbiguousTrunkBranch proves --branch settles several trunk
// candidates only by naming one of them, and that the name it picks becomes the
// trunk every guard downstream compares against.
func TestShipJJAmbiguousTrunkBranch(t *testing.T) {
	t.Run("a --branch naming no candidate still refuses", func(t *testing.T) {
		f := shipAmbiguousTrunk(t)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature")
		if err == nil || !strings.Contains(err.Error(), `cannot resolve the trunk bookmark from ["dev" "main"]`) {
			t.Fatalf("error = %v, want the trunk resolution refusal", err)
		}
		invocations := vcstest.Invocations(t, f.ArgvLog)
		assertInvocations(t, invocations, [][]string{
			{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		})
		assertNoShipMutation(t, invocations)
		if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "init" {
			t.Errorf("@- = %q, want no commit cut", subject)
		}
	})

	t.Run("the candidate it names is the trunk the guards weigh", func(t *testing.T) {
		f := shipAmbiguousTrunk(t)
		seedLaneRecords(t, f.Dir, laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "main")
		if err == nil || !strings.Contains(err.Error(), "pass --allow-trunk to advance it deliberately") {
			t.Fatalf("error = %v, want the org-trunk refusal", err)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
		if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "init" {
			t.Errorf("@- = %q, want no commit cut", subject)
		}
	})

	t.Run("a trunk of your own it names is committed onto", func(t *testing.T) {
		f := shipAmbiguousTrunk(t)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "main", "--pr-title", "Better title")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed main → origin · no PR (on trunk)"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := remoteCount(t, f, "main"); n != 2 {
			t.Errorf("origin main holds %d commits, want the candidate --branch named", n)
		}
		if n := remoteCount(t, f, "dev"); n != 1 {
			t.Errorf("origin dev holds %d commits, want the candidate it passed over left alone", n)
		}
	})
}

// TestShipJJNoTrunkBookmark proves an unnamed trunk stops mattering once the
// working copy sits on a bookmark: only a push with nothing at all to target
// still refuses. jj's trunk() revset knows main, master, and trunk by name, so
// a repository whose branch is none of those has no trunk bookmark at all.
func TestShipJJNoTrunkBookmark(t *testing.T) {
	t.Run("a nearest bookmark is pushed regardless", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Trunk("mainline"), vcstest.Dirty())

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := shipCommitted(t, f, vcs.JJ) + " · pushed mainline → origin"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := remoteCount(t, f, "mainline"); n != 2 {
			t.Errorf("origin mainline holds %d commits, want the nearest bookmark pushed", n)
		}
	})

	t.Run("no bookmark at all still commits under --no-push", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Dirty())
		mustRun(t, f.Dir, "jj", "bookmark", "delete", "main")
		shipResetLog(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if want := shipCommitted(t, f, vcs.JJ) + " · not pushed"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "fix: frobnicate" {
			t.Errorf("@- = %q, want the commit ship cut", subject)
		}
	})

	t.Run("no bookmark at all refuses a push", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.Dirty())
		mustRun(t, f.Dir, "jj", "bookmark", "delete", "main")
		shipResetLog(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err == nil || !strings.Contains(err.Error(), "cannot resolve the trunk bookmark") {
			t.Fatalf("error = %v, want the trunk resolution refusal", err)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
		if subject := jjAt(t, f.Dir, "@-", "description.first_line()"); subject != "init" {
			t.Errorf("@- = %q, want no commit cut", subject)
		}
	})
}

func TestShipJJBookmarkOverride(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	mustRun(t, f.Dir, "jj", "bookmark", "create", "someone/probe", "-r", "@-")
	shipResetLog(t, f)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.JJ) + " · pushed someone/probe → origin"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "someone/probe"); n != 2 {
		t.Errorf("origin someone/probe holds %d commits, want the override pushed", n)
	}
	assertInvocations(t, invocations, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "diff", "--name-only"},
		{"jj", "commit", "-m", "fix: frobnicate"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "list", vcs.JJExactPattern("someone/probe"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
		{"jj", "git", "fetch"},
		{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("someone/probe"), "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("someone/probe"), "--to", "@-"},
		{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
		{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("someone/probe")},
	})
}

// TestShipJJNewBranch proves a --bookmark naming no existing bookmark now
// creates it: jj bookmark create -r @- runs after the commit, and the push
// targets the new bookmark rather than trunk.
func TestShipJJNewBranch(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := vcstest.Invocations(t, f.ArgvLog)
	if want := shipCommitted(t, f, vcs.JJ) + " · created someone/probe · pushed someone/probe → origin"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := remoteCount(t, f, "someone/probe"); n != 2 {
		t.Errorf("origin someone/probe holds %d commits, want the bookmark ship created and pushed", n)
	}
	if n := remoteCount(t, f, "main"); n != 1 {
		t.Errorf("origin main holds %d commits, want trunk left where it was", n)
	}

	create, push := -1, -1
	for i, inv := range invocations {
		switch {
		case inv[0] == "jj" && inv[1] == "bookmark" && inv[2] == "create":
			create = i
			if want := []string{"jj", "bookmark", "create", "someone/probe", "-r", "@-"}; !reflect.DeepEqual(inv, want) {
				t.Errorf("create argv = %v, want %v", inv, want)
			}
		case inv[0] == "jj" && inv[1] == "git" && inv[2] == "push":
			push = i
			if want := []string{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("someone/probe")}; !reflect.DeepEqual(inv, want) {
				t.Errorf("push argv = %v, want %v", inv, want)
			}
		}
	}
	switch {
	case create < 0:
		t.Fatalf("no jj bookmark create in %v", invocations)
	case push < 0:
		t.Fatalf("no jj git push in %v", invocations)
	case create > push:
		t.Errorf("jj bookmark create ran after the push (%d > %d)", create, push)
	}
}

func TestShipGitBookmarkFlagFails(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	head := shipHead(t, f)
	shipResetLog(t, f)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--bookmark", "main")
	if err == nil {
		t.Fatal("expected error for --bookmark in a git repo, got nil")
	}
	if !strings.Contains(err.Error(), "applies only to jj") {
		t.Errorf("error = %v, want it to say --bookmark applies only to jj", err)
	}
	if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
		t.Errorf("no VCS command should run when --bookmark is rejected, got %v", inv)
	}
	assertShipRefusedClean(t, f, head)
}

// TestShipBookmarkGuardReadsTheSpelling pins the guard to the flag the caller
// typed rather than the field it binds. --bookmark and --branch share o.branch,
// and the jj-only rule belongs to the --bookmark spelling alone; it also reads
// the detected lane kind, never the graphite lane's coercion to git, which is
// what once reported "applies only to jj repositories" inside a jj repository.
func TestShipBookmarkGuardReadsTheSpelling(t *testing.T) {
	t.Run("--bookmark refuses in a colocated jj graphite repo", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.GT(), vcstest.Remote())
		shipGTReady(t, f)
		head := shipHead(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--bookmark", "main")
		wantErr := "ship: --bookmark does not apply in the graphite lane; pass --no-gt to advance a jj bookmark instead"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoGTCommit(t, shipGTInvocations(t, f))
		assertShipRefusedClean(t, f, head)
	})

	t.Run("--branch carries no such restriction", func(t *testing.T) {
		f := shipGTFeature(t)
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertGTCommit(t, shipGTInvocations(t, f))
		if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
			t.Errorf("HEAD subject = %q, want the commit gt cut on feature", subject)
		}
	})
}

func TestShipRequiresMessage(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	head := shipHead(t, f)
	shipResetLog(t, f)

	_, err := runShipCmd(t)
	if err == nil {
		t.Fatal("expected error when message missing, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to mention message required", err)
	}
	if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
		t.Errorf("no VCS command should run when message is missing, got %v", inv)
	}
	assertShipRefusedClean(t, f, head)
}

// TestShipNoRepoFails ships from a directory outside every repository the test
// built: the fixture's shim still leads PATH, so a VCS call ship made would be
// recorded.
func TestShipNoRepoFails(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote())
	writeShipGH(t, f)
	t.Chdir(t.TempDir())
	shipResetLog(t, f)

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error outside a repo, got nil")
	}
	if !strings.Contains(err.Error(), "no git or jj repository") {
		t.Errorf("error = %v, want it to mention no repository", err)
	}
	if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
		t.Errorf("no VCS command should run outside a repo, got %v", inv)
	}
	if n := remoteCount(t, f, "main"); n != 1 {
		t.Errorf("origin main holds %d commits, want the untouched fixture", n)
	}
}

func TestShipCISuccessReportLine(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "Guides · success · 12s · https://github.com/yasyf/cc-context/actions/runs/30744524405"
	if !strings.Contains(out, want) {
		t.Errorf("output missing run line %q\ngot:\n%s", want, out)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit the run reports on", n)
	}
}

func TestShipCIFailureDetail(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-failed"))
	t.Setenv("GH_WATCH_EXIT", "1")
	t.Setenv("GH_LOG_FAILED", ghStdout(t, "run-log-failed"))
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--budget", "0")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	for _, want := range []string{
		"· CI failure",
		"failed: autobump / autobump · Detect drift and bump",
		"##[error]Process completed with exit code 1.",
		"full log: gh run view 42 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want a red CI to leave the push alone", n)
	}
}

func TestShipCIBudgetCapsLog(t *testing.T) {
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB

	tests := []struct {
		name       string
		args       []string
		wantCapped bool
	}{
		{"default budget caps the excerpt", nil, true},
		{"budget 0 leaves it uncapped", []string{"--budget", "0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
			writeShipGH(t, f)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-failed"))
			t.Setenv("GH_LOG_FAILED", bigLog)
			shipCIPollInterval = 0

			args := append([]string{"-m", "fix: frobnicate"}, tt.args...)
			out, _, err := runShipCmdFull(t, args...)
			if err == nil {
				t.Fatal("expected error on CI failure, got nil")
			}
			capped := strings.Contains(out, "tokens omitted")
			if capped != tt.wantCapped {
				t.Errorf("capped = %v, want %v", capped, tt.wantCapped)
			}
			if !tt.wantCapped && !strings.Contains(out, bigLog[:len(bigLog)-1]) {
				t.Errorf("uncapped output should contain the whole log")
			}
			// The pointer line survives regardless of capping.
			if !strings.Contains(out, "full log: gh run view 42 --log-failed") {
				t.Errorf("missing full-log pointer\ngot tail:\n%s", out[max(0, len(out)-200):])
			}
			if n := remoteCount(t, f, "main"); n != 2 {
				t.Errorf("origin main holds %d commits, want the commit the excerpt reports on", n)
			}
		})
	}
}

func TestShipCIStripsANSI(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-failed"))
	t.Setenv("GH_LOG_FAILED", "\x1b[31mERROR\x1b[0m the build \x1b[1mboom\x1b[0m\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("ANSI escapes leaked into output: %q", out)
	}
	if !strings.Contains(out, "ERROR the build boom") {
		t.Errorf("stripped log text missing\ngot:\n%s", out)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit the excerpt reports on", n)
	}
}

func TestShipCITransientPollTolerated(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	marker := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("GH_LIST_FAIL_MARKER", marker)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("transient list error should be tolerated, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	listCalls := 0
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
	}
	if listCalls < 2 {
		t.Errorf("expected the poll to retry (>=2 list calls), got %d", listCalls)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit the poll retried for", n)
	}
}

func TestShipCIAllPollsFailStillReports(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_LIST_FAIL", "1")
	shipCIPollTries = 3
	shipCIPollInterval = 0
	t.Cleanup(func() { shipCIPollTries = 12 })

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when every poll fails, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q (abort-before-summary regression)", summary, want)
	}
	if want := "check: gh run list --commit " + shipHead(t, f); !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the push the watch could not confirm", n)
	}
}

func TestShipCIViewFailureIsError(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	// GH_RUN_VIEW_JSON unset: gh run view emits empty stdout, so the parse fails.
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when gh run view cannot be parsed, got nil")
	}
	if want := "· CI error"; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the push an unreadable view must not undo", n)
	}
}

func TestShipCIWatchErrViewGreenIsSuccess(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
	t.Setenv("GH_WATCH_EXIT", "1") // watch drops, view says success — view wins
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("view-green run should heal a dropped watch, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit the view healed", n)
	}
}

func TestShipCIMultiRunWatchesAll(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", ghStdout(t, "run-view-success"))
	t.Setenv("GH_RUN_VIEW_JSON_43", ghStdout(t, "run-view-failed"))
	t.Setenv("GH_WATCH_EXIT_43", "1")
	t.Setenv("GH_LOG_FAILED_43", ghStdout(t, "run-log-failed"))
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when one of several runs is red, got nil")
	}
	watched := map[string]bool{}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if !watched["42"] || !watched["43"] {
		t.Errorf("expected both runs watched, got %v", watched)
	}
	for _, want := range []string{
		"· CI failure",
		"Guides · success",
		"Autobump · failure",
		"failed: autobump / autobump · Detect drift and bump",
		"full log: gh run view 43 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit both runs report on", n)
	}
}

func TestShipCIMoreThanTenRunsWatchesAll(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	var runList strings.Builder
	runList.WriteByte('[')
	for id := 100; id < 112; id++ {
		if id > 100 {
			runList.WriteByte(',')
		}
		fmt.Fprintf(&runList, `{"databaseId":%d,"workflowName":"workflow-%d","status":"completed","url":"https://x/%d"}`, id, id, id)
		t.Setenv(fmt.Sprintf("GH_RUN_VIEW_JSON_%d", id), fmt.Sprintf(`{"workflowName":"workflow-%d","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:01Z","url":"https://x/%d","jobs":[]}`, id, id))
	}
	runList.WriteByte(']')
	t.Setenv("GH_RUN_LIST_JSON", runList.String())
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]int{}
	limit50 := false
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]]++
		}
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			for i := 3; i+1 < len(inv); i++ {
				if inv[i] == "--limit" && inv[i+1] == "50" {
					limit50 = true
				}
			}
		}
	}
	if !limit50 {
		t.Error("gh run list did not use --limit 50")
	}
	if len(watched) != 12 {
		t.Errorf("watched %d runs, want 12: %v", len(watched), watched)
	}
	for id := 100; id < 112; id++ {
		key := fmt.Sprintf("%d", id)
		if watched[key] != 1 {
			t.Errorf("run %s watched %d times, want 1", key, watched[key])
		}
		want := fmt.Sprintf("workflow-%d · success · 1s · https://x/%d", id, id)
		if !strings.Contains(out, want) {
			t.Errorf("output missing report line %q\ngot:\n%s", want, out)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit all twelve runs report on", n)
	}
}

func TestShipCISettleWatchesLateRuns(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	marker := filepath.Join(t.TempDir(), "settle")
	t.Setenv("GH_LIST_SETTLE_MARKER", marker)
	t.Setenv("GH_LIST_SETTLE_AFTER", "2")
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON) // first list: run 42 only
	t.Setenv("GH_RUN_LIST_JSON_2", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":44,"workflowName":"settle-late","status":"completed","url":"https://x/44"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", ghStdout(t, "run-view-success"))
	t.Setenv("GH_RUN_VIEW_JSON_44", `{"workflowName":"settle-late","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:10Z","url":"https://x/44","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]bool{}
	listCalls := 0
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if listCalls < 5 {
		t.Errorf("expected initial discovery, a quiet re-list, the straggler, and the quiet horizon; got %d list calls", listCalls)
	}
	if !watched["42"] || !watched["44"] {
		t.Errorf("expected the settle pass to watch both runs, got %v", watched)
	}
	for _, want := range []string{"Guides · success", "settle-late · success"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing settle report line %q\ngot:\n%s", want, out)
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit the straggler reports on", n)
	}
}

func TestShipCIBudgetFloorsPerRunShare(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", ghStdout(t, "run-view-failed"))
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_LOG_FAILED_42", bigLog)
	t.Setenv("GH_LOG_FAILED_43", bigLog)
	shipCIPollInterval = 0

	// --budget 1 with two red runs floors the per-run share to 1 (not 0 = uncapped).
	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--budget", "1")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	if !strings.Contains(out, "tokens omitted") {
		t.Errorf("expected both excerpts capped (tokens omitted footer)\ngot tail:\n%s", out[max(0, len(out)-400):])
	}
	if strings.Contains(out, bigLog[:len(bigLog)-1]) {
		t.Errorf("full log leaked past the floored budget")
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the commit both excerpts report on", n)
	}
}

func TestShipCIEmptyConclusionIsIndeterminate(t *testing.T) {
	f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
	writeShipGH(t, f)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", `{"workflowName":"ci","conclusion":"","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/42","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when a run has not concluded, got nil")
	}
	summary := out
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = summary[:i]
	}
	if want := "· CI error"; !strings.Contains(summary, want) {
		t.Errorf("summary = %q, want it to contain %q", summary, want)
	}
	if want := "run 42 has not concluded; check: gh run view 42"; !strings.Contains(out, want) {
		t.Errorf("output missing not-concluded pointer %q\ngot:\n%s", want, out)
	}
	for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
		for _, a := range inv {
			if a == "--log-failed" {
				t.Errorf("indeterminate run must not fetch --log-failed, got %v", inv)
			}
		}
	}
	if n := remoteCount(t, f, "main"); n != 2 {
		t.Errorf("origin main holds %d commits, want the push an unconcluded run must not undo", n)
	}
}

func TestShipCIStreamingSeam(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		wantCompact bool
		wantErrText bool
	}{
		{"tty streams to stderr with --compact", true, true, true},
		{"non-tty buffers watch output away", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.JJ(), vcstest.Remote(), vcstest.Dirty())
			writeShipGH(t, f)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
			shipCIPollInterval = 0

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			_, errStr, err := runShipCmdFull(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			compact := false
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					for _, a := range inv {
						if a == "--compact" {
							compact = true
						}
					}
				}
			}
			if compact != tt.wantCompact {
				t.Errorf("watch --compact = %v, want %v", compact, tt.wantCompact)
			}
			if got := strings.Contains(errStr, "watch stream 42"); got != tt.wantErrText {
				t.Errorf("stderr carries watch stream = %v, want %v (stderr=%q)", got, tt.wantErrText, errStr)
			}
			if n := remoteCount(t, f, "main"); n != 2 {
				t.Errorf("origin main holds %d commits, want the commit the watch streamed for", n)
			}
		})
	}
}

// TestWatchCIRunBounded drives both branches of the watch against a gh that never
// concludes: each must carry shipCIWatchTimeout, since gh run watch blocks until
// the run is over and so outlives any generic bound by design — and the default
// must stay past GitHub's own 6h job ceiling, or a long green build reports as a
// CI error.
func TestWatchCIRunBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell scripts are POSIX-only")
	}
	if shipCIWatchTimeout < 6*time.Hour {
		t.Errorf("shipCIWatchTimeout = %s, want it past GitHub's 6h job ceiling", shipCIWatchTimeout)
	}
	tests := []struct {
		name   string
		stream bool
	}{
		{"buffered watch is bounded", false},
		{"streaming watch is bounded", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			gh := filepath.Join(binDir, "gh")
			if err := os.WriteFile(gh, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
				t.Fatalf("write fake gh: %v", err)
			}
			t.Setenv("PATH", binDir)

			oldStream := shipStreamCI
			t.Cleanup(func() { shipStreamCI = oldStream })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			oldTimeout := shipCIWatchTimeout
			shipCIWatchTimeout = 100 * time.Millisecond
			t.Cleanup(func() { shipCIWatchTimeout = oldTimeout })

			start := time.Now()
			err := watchCIRun(context.Background(), io.Discard, "42")
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("watchCIRun = nil, want the bound to kill a watch that never concludes")
			}
			if elapsed > 10*time.Second {
				t.Errorf("watchCIRun returned after %s; the watch outlived its bound", elapsed)
			}
		})
	}
}

func TestCIDuration(t *testing.T) {
	start := time.Date(2026, 7, 8, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		want  string
	}{
		{"normal span", start, start.Add(58 * time.Second), "58s"},
		{"zero start omits", time.Time{}, start, ""},
		{"negative span omits", start, start.Add(-time.Second), ""},
		{"equal is zero seconds", start, start, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciDuration(tt.start, tt.end); got != tt.want {
				t.Errorf("ciDuration = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithSessionTrailer(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		message string
		want    string
	}{
		{"env set appends trailer", "sess-abc", "fix: frobnicate", "fix: frobnicate\n\nClaude-Session-Id: sess-abc"},
		{"env empty leaves message", "", "fix: frobnicate", "fix: frobnicate"},
		{"empty message stays empty", "sess-abc", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envClaudeSessionKey, tt.id)
			if got := withSessionTrailer(tt.message); got != tt.want {
				t.Errorf("withSessionTrailer(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

func TestCIGreen(t *testing.T) {
	tests := []struct {
		conclusion string
		want       bool
	}{
		{"success", true},
		{"skipped", true},
		{"neutral", true},
		{"failure", false},
		{"cancelled", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.conclusion, func(t *testing.T) {
			if got := ciGreen(tt.conclusion); got != tt.want {
				t.Errorf("ciGreen(%q) = %v, want %v", tt.conclusion, got, tt.want)
			}
		})
	}
}

func TestShipGTPrecedenceOverJJ(t *testing.T) {
	// jj git init --colocate leaves git's HEAD detached, so the graphite half
	// checks a branch out with git first: the gt lane commits onto a git branch,
	// and ship refuses a detached HEAD before it ever reaches one.
	t.Run("graphite wins over a colocated jj repository", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.GT(), vcstest.Remote())
		mustRun(t, f.Dir, "git", "switch", "-qc", "feature")
		mustRun(t, f.Dir, "gt", "track", "-f", "--no-interactive")
		shipGTReady(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := shipGTInvocations(t, f)
		if want := shipCommitted(t, f, vcs.Git) + " · branch feature · not pushed"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, invocations, [][]string{
			nogtProbe,
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"gt", "add", "--no-interactive", "-A"},
			{"git", "diff", "--cached", "--quiet"},
			{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
			{"git", "branch", "--show-current"},
			{"git", "log", "-1", "--format=%h%x00%s"},
		})
	})

	t.Run("--no-gt falls back to jj", func(t *testing.T) {
		f := shipRepo(t, vcstest.JJ(), vcstest.GT(), vcstest.Remote())
		shipGTReady(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-gt")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := shipGTInvocations(t, f)
		if want := shipCommitted(t, f, vcs.JJ) + " · branch main · not pushed"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, invocations, append(
			jjPlanArgv(),
			[]string{"jj", "diff", "--name-only"},
			[]string{"jj", "commit", "-m", "fix: frobnicate"},
			[]string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			[]string{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
		))
		if tip := jjAt(t, f.Dir, "main", "description.first_line()"); tip != "fix: frobnicate" {
			t.Errorf("main = %q, want the bookmark moved onto the new commit", tip)
		}
	})
}

func TestShipGTStackedHappyPath(t *testing.T) {
	tests := []struct {
		name       string
		branch     string
		stateJSON  string
		dryRun     bool
		prBranches []string
		wantSeg    string
	}{
		{
			name:       "depth 1",
			branch:     "feature",
			stateJSON:  `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`,
			prBranches: []string{"feature"},
			wantSeg:    "submitted feature → PR #7 https://github.com/x/pull/7",
		},
		{
			name:   "depth 2",
			branch: "feature2",
			stateJSON: `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]},` +
				`"feature2":{"parents":[{"ref":"feature","sha":"beadfeed"}]}}`,
			dryRun:     true,
			prBranches: []string{"feature", "feature2"},
			wantSeg:    "submitted feature2 → PR #7 https://github.com/x/pull/7 (stack of 2: feature, feature2)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, true)
			t.Setenv("GIT_BRANCH", tt.branch)
			t.Setenv("GT_STATE_JSON", tt.stateJSON)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
			t.Setenv("GH_PR_VIEW_JSON", `{"number":7,"url":"https://github.com/x/pull/7","body":"why"}`)
			shipCIPollInterval = 0

			got, err := runShipCmd(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if summary := `committed a1b2c3d "fix: frobnicate" · ` + tt.wantSeg + ` · CI success`; got != summary {
				t.Errorf("summary = %q, want %q", got, summary)
			}
			want := [][]string{
				nogtProbe,
				{"git", "branch", "--show-current"},
				{"gt", "state"},
				{"gt", "add", "--no-interactive", "-A"},
				{"git", "diff", "--cached", "--quiet"},
				{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"gt", "state"},
			}
			// gt submit force-pushes the whole downstack, so a stack deeper than
			// one branch is reported by --dry-run before anything is pushed.
			if tt.dryRun {
				want = append(want, []string{"gt", "submit", "--dry-run", "--no-interactive"})
			}
			want = append(want,
				[]string{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"},
				ghDownstackPRArgv(tt.prBranches...),
			)
			assertInvocations(t, readInvocations(t, log), append(
				want,
				[]string{"git", "rev-parse", "HEAD"},
				ghRunListArgv, ghRunWatchArgv, ghRunViewArgv, ghRunListArgv, ghRunListArgv,
			))
		})
	}
}

// TestShipGTTrunkStacksBranch replaces TestShipGTTrunkAutoCreate: the branch
// is named by ccx rather than left to gt's message-derived guess, and the
// report says so before the submit.
func TestShipGTTrunkStacksBranch(t *testing.T) {
	log := setupShipGT(t, true)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
	t.Setenv("GT_STATE_JSON_2", `{"main":{"trunk":true},"fix-frobnicate":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
	t.Setenv("GT_STATE_JSON_MARKER", filepath.Join(t.TempDir(), "gt-state"))
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
	t.Setenv("GH_PR_VIEW_JSON", `{"number":9,"url":"https://github.com/x/pull/9","body":"why"}`)
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · created fix-frobnicate · submitted fix-frobnicate → PR #9 https://github.com/x/pull/9 · CI success`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		nogtProbe,
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"gt", "add", "--no-interactive", "-A"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "create", "fix-frobnicate", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"gt", "state"},
		{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", "--publish"},
		ghDownstackPRArgv("fix-frobnicate"),
		{"git", "rev-parse", "HEAD"},
		ghRunListArgv, ghRunWatchArgv, ghRunViewArgv, ghRunListArgv, ghRunListArgv,
	})
}

func TestShipGTBodylessPR(t *testing.T) {
	t.Run("a bodyless submit is reported", func(t *testing.T) {
		setupShipGT(t, true)
		t.Setenv("GH_PR_VIEW_JSON", `{"number":7,"url":"https://github.com/x/pull/7","body":"  "}`)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · submitted feature → PR #7 https://github.com/x/pull/7 · bodyless PR #7 feature`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("a body this ship writes settles it", func(t *testing.T) {
		setupShipGT(t, true)
		seedPRViews(t, map[string]string{"feature": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`})
		body := writePRBody(t, "body.md", "why this change\n")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", body)
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · submitted feature → PR #7 https://github.com/x/pull/7 · set PR #7 body`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("an empty body file settles nothing", func(t *testing.T) {
		setupShipGT(t, true)
		seedPRViews(t, map[string]string{"feature": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`})
		body := writePRBody(t, "empty.md", "  \n\t\n")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--pr-body-file", body)
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · submitted feature → PR #7 https://github.com/x/pull/7 · ` +
			`set PR #7 body · bodyless PR #7 feature`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("a bodyless downstack under a bodied tip is named too", func(t *testing.T) {
		setupShipGT(t, true)
		t.Setenv("GIT_BRANCH", "feature2")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
			`"feature2":{"parents":[{"ref":"feature","sha":"beadfeed"}]}}`)
		seedPRViews(t, map[string]string{
			"feature":  `{"number":6,"url":"https://github.com/x/pull/6","body":""}`,
			"feature2": `{"number":7,"url":"https://github.com/x/pull/7","body":"why"}`,
		})

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · submitted feature2 → PR #7 https://github.com/x/pull/7 ` +
			`(stack of 2: feature, feature2) · bodyless PR #6 feature`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})
}

// TestShipTrunkPersonalAppends and TestShipTrunkOrgCreates are the Personal()
// split: outside the graphite lane, trunk in your own repository is committed
// to directly, and an org trunk gets a branch instead.
func TestShipTrunkPersonalAppends(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	head := gitAt(t, f.Dir, "log", "-1", "--format=%h")
	if want := fmt.Sprintf(`committed %s "fix: frobnicate" · branch main · not pushed`, head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "main" {
		t.Errorf("branch after ship = %q, want main", branch)
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "main"); subject != "fix: frobnicate" {
		t.Errorf("main tip = %q, want the shipped commit", subject)
	}
}

func TestShipTrunkOrgCreates(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	seedLaneRecords(t, f.Dir, laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	head := gitAt(t, f.Dir, "log", "-1", "--format=%h")
	if want := fmt.Sprintf(`committed %s "fix: frobnicate" · created fix-frobnicate · not pushed`, head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "fix-frobnicate" {
		t.Errorf("branch after ship = %q, want the branch ship cut", branch)
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "main"); subject != "init" {
		t.Errorf("main tip = %q, want the org trunk left where it was", subject)
	}
}

// TestShipGitNewBranch proves --new-branch cuts the branch before the commit,
// so the commit lands on it rather than on trunk.
func TestShipGitNewBranch(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	head := gitAt(t, f.Dir, "log", "-1", "--format=%h")
	if want := fmt.Sprintf(`committed %s "fix: frobnicate" · created feat-x · not pushed`, head); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "feat-x" {
		t.Errorf("branch after ship = %q, want feat-x", branch)
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "feat-x"); subject != "fix: frobnicate" {
		t.Errorf("feat-x tip = %q, want the shipped commit", subject)
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "main"); subject != "init" {
		t.Errorf("main tip = %q, want trunk left where it was", subject)
	}
}

// TestShipGitNewBranchRollback proves a refusal after the branch cut leaves the
// repository where it started: ship switches back, deletes the branch it cut,
// leaves the edit uncommitted, and still reports the failure that refused it.
func TestShipGitNewBranchRollback(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	writeShipHookFiles(t, f.Dir, "f1.go")
	writeShipUvx(t, f, 2, "")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err == nil || !strings.Contains(err.Error(), "ship: hooks:") {
		t.Fatalf("ship error = %v, want the hook failure", err)
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "main" {
		t.Errorf("branch after rollback = %q, want main", branch)
	}
	if gitBranchExists(t, f.Dir, "feat-x") {
		t.Error("feat-x survived the rollback")
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "init" {
		t.Errorf("HEAD subject = %q, want no commit cut", subject)
	}
	if status := gitAt(t, f.Dir, "status", "--porcelain", "--", "f1.go"); status == "" {
		t.Error("f1.go is committed or gone; the rollback must leave the edit uncommitted")
	}
}

// TestShipGitNewBranchRollbackFailure proves a rollback that cannot finish is
// reported alongside the refusal that triggered it, never instead of it.
// TestShipGitNewBranchRollbackFailure proves a rollback that cannot finish is
// reported alongside the refusal that triggered it, never instead of it. The
// last failing hook run leaves an index.lock behind, which is what makes git
// refuse the switch back — the same state a crashed git process leaves.
func TestShipGitNewBranchRollbackFailure(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	writeShipHookFiles(t, f.Dir, "f1.go")
	lock := filepath.Join(f.Dir, ".git", "index.lock")
	writeShipUvx(t, f, 2, `test "$(cat "$SHIP_PREK_MARKER")" != 0 || : > `+lock)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	for _, want := range []string{"ship: hooks:", "ship: rollback: git switch main", "the working copy is left on feat-x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "feat-x" {
		t.Errorf("branch = %q, want the working copy left where the failed rollback put it", branch)
	}
	if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "init" {
		t.Errorf("HEAD subject = %q, want no commit cut", subject)
	}
}

// TestShipGTCreateNamesExplicitly proves ship always hands gt a branch name, so
// gt never derives one from a message carrying the session trailer.
func TestShipGTCreateNamesExplicitly(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"explicit name", []string{"--new-branch=newbranch"}, []string{"gt", "create", "newbranch", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
		{"bare new-branch derives from the subject", []string{"--new-branch"}, []string{"gt", "create", "fix-frobnicate", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
		{"the deprecated --create alias still works", []string{"--create=newbranch"}, []string{"gt", "create", "newbranch", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
		{"--parent stacks the branch onto it", []string{"--new-branch=newbranch", "--parent", "base"}, []string{"gt", "create", "newbranch", "--onto", "base", "-m", "fix: frobnicate", "--no-ai", "--no-interactive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "base", "feature")
			shipGTReady(t, f)
			args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range shipGTInvocations(t, f) {
				if inv[0] == "gt" && (inv[1] == "create" || inv[1] == "modify") {
					commit = inv
				}
			}
			if !reflect.DeepEqual(commit, tt.want) {
				t.Errorf("commit argv = %v, want %v", commit, tt.want)
			}
			if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != tt.want[2] {
				t.Errorf("branch = %q, want the created %q", branch, tt.want[2])
			}
			if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s"); subject != "fix: frobnicate" {
				t.Errorf("HEAD subject = %q, want the commit gt cut on the new branch", subject)
			}
		})
	}
}

func TestShipCreateExplicitEmpty(t *testing.T) {
	for _, flag := range []string{"--create=", "--new-branch="} {
		t.Run(flag, func(t *testing.T) {
			f := shipGTFeature(t)
			head := shipHead(t, f)

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", flag)
			wantErr := "ship: " + strings.TrimSuffix(flag, "=") + " requires a branch name or no value"
			if err == nil || err.Error() != wantErr {
				t.Fatalf("error = %v, want %q", err, wantErr)
			}
			assertNoGTCommit(t, shipGTInvocations(t, f))
			assertShipRefusedClean(t, f, head)
		})
	}
}

// TestShipCreateSwallowsPathOperand covers the flag shape that silently
// committed a subset: cobra's NoOptDefVal never consumes the next token, so
// "--new-branch docs" filed docs as the only path to commit.
func TestShipCreateSwallowsPathOperand(t *testing.T) {
	for _, flag := range []string{"--new-branch", "--create"} {
		t.Run(flag, func(t *testing.T) {
			f := shipGTFeature(t)
			head := shipHead(t, f)
			shipResetLog(t, f)

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", flag, "docs")
			wantErr := `ship: "docs" is not a path — did you mean --new-branch=docs?`
			if err == nil || err.Error() != wantErr {
				t.Fatalf("error = %v, want %q", err, wantErr)
			}
			invocations := shipGTInvocations(t, f)
			assertNoGTCommit(t, invocations)
			if invocations != nil {
				t.Errorf("no VCS command may run before the path-operand refusal, got %v", invocations)
			}
			assertShipRefusedClean(t, f, head)
		})
	}

	t.Run("a real path is still a path", func(t *testing.T) {
		f := shipGTFeature(t)
		writeShipFile(t, f.Dir, "docs/d.md", "d\n")

		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch", "docs"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		var add []string
		for _, inv := range shipGTInvocations(t, f) {
			if inv[0] == "gt" && inv[1] == "add" {
				add = inv
			}
		}
		if want := []string{"gt", "add", "--no-interactive", "-A", "--", "docs"}; !reflect.DeepEqual(add, want) {
			t.Errorf("add argv = %v, want %v", add, want)
		}
		if names := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); names != "docs/d.md" {
			t.Errorf("committed %q, want the path operand alone", names)
		}
		if status := gitAt(t, f.Dir, "status", "--porcelain"); status != "M f.txt" {
			t.Errorf("working copy = %q, want the unscoped edit left behind", status)
		}
	})
}

// TestShipIllegalBranchName proves an explicit branch name is refused by ship,
// before any lane work: only a derived name passes through legalBranchName
// otherwise, leaving the refusal to whichever backend's argv parser ran first.
func TestShipIllegalBranchName(t *testing.T) {
	// Every row refuses before touching the repository, so one fixture serves
	// the whole matrix.
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	head := shipHead(t, f)
	for _, flag := range []struct{ spelling, canonical string }{
		{"--branch", "--branch"},
		{"--bookmark", "--branch"},
		{"--new-branch", "--new-branch"},
		{"--create", "--new-branch"},
	} {
		for _, name := range []string{"--force", "a..b", "x.lock", "feature/", "feature."} {
			t.Run(flag.spelling+"="+name, func(t *testing.T) {
				shipResetLog(t, f)

				_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", flag.spelling+"="+name)
				wantErr := fmt.Sprintf("ship: %s %q is not a legal branch name", flag.canonical, name)
				if err == nil || err.Error() != wantErr {
					t.Fatalf("error = %v, want %q", err, wantErr)
				}
				invocations := vcstest.Invocations(t, f.ArgvLog)
				assertNoShipMutation(t, invocations)
				if invocations != nil {
					t.Errorf("no VCS command may run before the illegal-name refusal, got %v", invocations)
				}
				assertShipRefusedClean(t, f, head)
			})
		}
	}
}

// TestShipBranchFlag covers --branch's three outcomes: appending to the branch
// already checked out, cutting one that does not exist, and refusing to switch
// to one that exists elsewhere.
func TestShipBranchFlag(t *testing.T) {
	t.Run("naming the current branch appends", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"), vcstest.Dirty())

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
			if inv[0] == "git" && inv[1] == "switch" {
				t.Errorf("git switch ran for a branch already checked out: %v", inv)
			}
		}
		head := gitAt(t, f.Dir, "log", "-1", "--format=%h")
		if want := fmt.Sprintf(`committed %s "fix: frobnicate" · branch feature · not pushed`, head); got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if subject := gitAt(t, f.Dir, "log", "-1", "--format=%s", "feature"); subject != "fix: frobnicate" {
			t.Errorf("feature tip = %q, want the shipped commit", subject)
		}
	})

	t.Run("naming an existing branch refuses", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"), vcstest.Dirty())
		mustRun(t, f.Dir, "git", "branch", "other")
		before := gitAt(t, f.Dir, "rev-parse", "HEAD")
		setup := len(vcstest.Invocations(t, f.ArgvLog))

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "other")
		wantErr := "ship: branch other already exists — check it out first; ship does not switch branches mid-commit"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog)[setup:])
		if head := gitAt(t, f.Dir, "rev-parse", "HEAD"); head != before {
			t.Errorf("HEAD = %s, want it unmoved at %s", head, before)
		}
	})

	t.Run("naming a missing branch creates it here", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"), vcstest.Dirty())

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "other")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		head := gitAt(t, f.Dir, "log", "-1", "--format=%h")
		if want := fmt.Sprintf(`committed %s "fix: frobnicate" · created other · not pushed`, head); got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if branch := gitAt(t, f.Dir, "branch", "--show-current"); branch != "other" {
			t.Errorf("branch after ship = %q, want other", branch)
		}
	})

	t.Run("naming an org trunk refuses without --allow-trunk", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"), vcstest.Dirty())
		seedLaneRecords(t, f.Dir, laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})
		before := gitAt(t, f.Dir, "rev-parse", "HEAD")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "main")
		if err == nil || !strings.Contains(err.Error(), "pass --allow-trunk to advance it deliberately") {
			t.Fatalf("error = %v, want the org-trunk refusal", err)
		}
		assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
		if head := gitAt(t, f.Dir, "rev-parse", "HEAD"); head != before {
			t.Errorf("HEAD = %s, want it unmoved at %s", head, before)
		}
	})
}

// TestShipAppendFlag proves --append refuses on trunk, where the branch it
// would append to is the one ship exists to keep commits off.
// TestShipTrunkTagRefuses drives the whole ship through an origin/HEAD pointed
// at a tag — a state one git command reaches and no fake that prefixes its
// answer with "origin/" can produce. symbolic-ref --short prints the tag at
// exit 0, so ship must refuse rather than ship onto it.
func TestShipTrunkTagRefuses(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	mustRun(t, f.Dir, "git", "tag", "v1")
	mustRun(t, f.Dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/tags/v1")
	before := gitAt(t, f.Dir, "rev-parse", "HEAD")
	setup := len(vcstest.Invocations(t, f.ArgvLog))

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err == nil || !strings.Contains(err.Error(), `points at "v1", which names no branch of origin`) {
		t.Fatalf("ship error = %v, want the tag-trunk refusal", err)
	}
	assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog)[setup:])
	if head := gitAt(t, f.Dir, "rev-parse", "HEAD"); head != before {
		t.Errorf("HEAD = %s, want it unmoved at %s", head, before)
	}
}

func TestShipAppendFlag(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	before := gitAt(t, f.Dir, "rev-parse", "HEAD")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--append")
	wantErr := "ship: append would commit onto trunk — pass --new-branch"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("error = %v, want %q", err, wantErr)
	}
	assertNoShipMutation(t, vcstest.Invocations(t, f.ArgvLog))
	if head := gitAt(t, f.Dir, "rev-parse", "HEAD"); head != before {
		t.Errorf("HEAD = %s, want it unmoved at %s", head, before)
	}
}

// TestShipGTTrackReportsParent proves the parent gt track -f picked is named in
// the report, and that --parent replaces -f rather than being overridden by it.
func TestShipGTTrackReportsParent(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantTrack []string
		wantSeg   string
	}{
		{
			name:      "gt track -f reports the ancestor it picked",
			wantTrack: []string{"gt", "track", "-f", "--no-interactive"},
			wantSeg:   "tracked feature onto base",
		},
		{
			name:      "--parent drops -f, which would take precedence over it",
			args:      []string{"--parent", "base"},
			wantTrack: []string{"gt", "track", "--parent", "base", "--no-interactive"},
			wantSeg:   "tracked feature onto base",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "base")
			shipGTUntracked(t, f, "feature")
			shipGTReady(t, f)

			got, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			invocations := shipGTInvocations(t, f)
			if !strings.HasPrefix(got, tt.wantSeg+" · ") {
				t.Errorf("summary = %q, want it to lead with %q", got, tt.wantSeg)
			}
			var track []string
			for _, inv := range invocations {
				if inv[0] == "gt" && inv[1] == "track" {
					track = inv
				}
			}
			if !reflect.DeepEqual(track, tt.wantTrack) {
				t.Errorf("track argv = %v, want %v", track, tt.wantTrack)
			}
			var state gtState
			if err := json.Unmarshal([]byte(mustRun(t, f.Dir, "gt", "state")), &state); err != nil {
				t.Fatalf("parse gt state: %v", err)
			}
			if parents := state["feature"].Parents; len(parents) != 1 || parents[0].Ref != "base" {
				t.Errorf("gt state feature parents = %v, want the adopted base", parents)
			}
		})
	}
}

func TestShipGTAmend(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"with message", []string{"--amend", "-m", "fix: frobnicate"}, []string{"gt", "modify", "-m", "fix: frobnicate", "--no-interactive"}},
		{"without message", []string{"--amend"}, []string{"gt", "modify", "--no-interactive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTFeature(t)
			args := append(append([]string{}, tt.args...), "--no-push")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range shipGTInvocations(t, f) {
				if inv[0] == "gt" && inv[1] == "modify" {
					commit = inv
				}
				if inv[0] == "git" && inv[1] == "diff" {
					t.Errorf("amend must not probe git diff --cached --quiet: %v", inv)
				}
			}
			if !reflect.DeepEqual(commit, tt.want) {
				t.Errorf("commit argv = %v, want %v", commit, tt.want)
			}
			if n := gitAt(t, f.Dir, "rev-list", "--count", "main..feature"); n != "1" {
				t.Errorf("feature holds %s commits over main, want the amended one", n)
			}
			if content := gitAt(t, f.Dir, "show", "HEAD:f.txt"); content != "dirty" {
				t.Errorf("HEAD:f.txt = %q, want the amended edit", content)
			}
		})
	}

	t.Run("amend on trunk refuses", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTReady(t, f)
		head := shipHead(t, f)

		_, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-push")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		assertNoGTCommit(t, shipGTInvocations(t, f))
		assertShipRefusedClean(t, f, head)
	})
}

func TestShipGTPathScoped(t *testing.T) {
	f := shipGTFeature(t)
	writeShipFile(t, f.Dir, "src/a.go", "a\n")
	writeShipFile(t, f.Dir, "docs/d.md", "d\n")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, shipGTInvocations(t, f), [][]string{
		nogtProbe,
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"gt", "add", "--no-interactive", "-A", "--", "src/a.go", "docs"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
	if names := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); names != "docs/d.md\nsrc/a.go" {
		t.Errorf("committed %q, want the scoped paths alone", names)
	}
	if status := gitAt(t, f.Dir, "status", "--porcelain"); status != "M f.txt" {
		t.Errorf("working copy = %q, want the unscoped edit left behind", status)
	}
}

// TestShipGTHunkScoped drives the throwaway-index technique the gt lane borrows
// from the git lane: only the named hunk reaches the commit, the working copy
// file is never rewritten, and the real index is restored afterwards — which is
// what the temp GIT_INDEX_FILE buys, and what the repository can attest to.
func TestShipGTHunkScoped(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	writeShipFile(t, f.Dir, "f.txt", hunkBase)
	mustRun(t, f.Dir, "git", "add", "f.txt")
	mustRun(t, f.Dir, "git", "commit", "-qm", "base")
	writeShipFile(t, f.Dir, "f.txt", hunkCurrent)
	writeShipHookFiles(t, f.Dir)
	shipResetLog(t, f)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := shipGTInvocations(t, f)
	blob := gitAt(t, f.Dir, "rev-parse", "HEAD:f.txt")
	if want := "hooks hunk-skip · " + shipCommitted(t, f, vcs.Git) + " · branch feature · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, invocations, [][]string{
		nogtProbe,
		{"git", "rev-parse", "--show-toplevel"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"git", "read-tree", "HEAD"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "hash-object", "-w", "--stdin"},
		{"git", "update-index", "--add", "--cacheinfo", "100644," + blob + ",f.txt"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "restore", "--staged", "--", "f.txt"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
	if committed := gitAt(t, f.Dir, "show", "HEAD:f.txt"); committed != "A\nb\nc\nd\ne" {
		t.Errorf("HEAD:f.txt = %q, want the first hunk alone", committed)
	}
	if worktree := readFileStr(t, filepath.Join(f.Dir, "f.txt")); worktree != hunkCurrent {
		t.Errorf("worktree f.txt = %q, want it never rewritten", worktree)
	}
	if status := gitAt(t, f.Dir, "status", "--porcelain", "--", "f.txt"); status != "M f.txt" {
		t.Errorf("f.txt = %q, want the unselected hunk left uncommitted and unstaged", status)
	}
}

// TestShipGTHunkScopedRefusesALyingExitZero pins the hunk-scoped commit to the
// same gt boundary every other verb runs on. The verb here carries an env-only
// GIT_INDEX_FILE, and a runner that took the env but returned stdout alone would
// both trust the exit 0 and discard the sentence that contradicts it.
func TestShipGTHunkScopedRefusesALyingExitZero(t *testing.T) {
	setupShipGT(t, false)
	if err := os.WriteFile("f.txt", []byte(hunkCurrent), 0o600); err != nil {
		t.Fatalf("write f.txt: %v", err)
	}
	t.Setenv("GIT_FILE_SHOW_BASE", hunkBase)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	const diagnostic = gtErrorPrefix + "Could not modify feature: its branch is frozen."
	t.Setenv("GT_MODIFY_STDERR", diagnostic)

	got, stderr, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err == nil {
		t.Fatalf("ship reported %q, want a refusal", got)
	}
	want := "ship: gt modify: exit 0 but reported an error: " + diagnostic
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if !strings.Contains(stderr, diagnostic) {
		t.Errorf("stderr = %q, want it to carry gt's own diagnostic", stderr)
	}
}

func TestShipGTRefusals(t *testing.T) {
	t.Run("needs restack", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTStack(t, f, "base", "feature")
		// A commit onto base after feature was cut is what leaves the stack
		// unrestacked, and gt state is the one oracle for that verdict.
		mustRun(t, f.Dir, "git", "switch", "-q", "base")
		writeShipFile(t, f.Dir, "base2.txt", "base2\n")
		mustRun(t, f.Dir, "git", "add", "base2.txt")
		mustRun(t, f.Dir, "git", "commit", "-qm", "base2")
		mustRun(t, f.Dir, "git", "switch", "-q", "feature")
		shipGTReady(t, f)
		head := shipHead(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		wantErr := "ship: stack needs restack — run gt restack (gt continue / gt abort on conflict), then re-run ship"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoGTCommit(t, shipGTInvocations(t, f))
		assertShipRefusedClean(t, f, head)
	})

	t.Run("staged empty", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTStack(t, f, "feature")
		shipResetLog(t, f)
		head := shipHead(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		wantErr := fmt.Sprintf("ship: nothing to commit — did a prior ship already land %s %q?",
			gitAt(t, f.Dir, "log", "-1", "--format=%h"), gitAt(t, f.Dir, "log", "-1", "--format=%s"))
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoGTCommit(t, shipGTInvocations(t, f))
		if got := shipHead(t, f); got != head {
			t.Errorf("HEAD moved to %s, want the pre-ship %s", got, head)
		}
	})

	t.Run("untracked branch auto-tracks", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTUntracked(t, f, "feature")
		shipGTReady(t, f)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		invocations := shipGTInvocations(t, f)
		if want := `tracked feature onto main · ` + shipCommitted(t, f, vcs.Git) + " · branch feature · not pushed"; got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, invocations, [][]string{
			nogtProbe,
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"gt", "track", "-f", "--no-interactive"},
			{"gt", "state"},
			{"gt", "add", "--no-interactive", "-A"},
			{"git", "diff", "--cached", "--quiet"},
			{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
			{"git", "branch", "--show-current"},
			{"git", "log", "-1", "--format=%h%x00%s"},
		})
	})

	// --parent names a branch gt cannot find, which is a track gt itself refuses
	// — the refusal below is ccx's answer to gt's own exit 1, not to a knob.
	t.Run("untracked branch auto-track fails", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTUntracked(t, f, "feature")
		shipGTReady(t, f)
		head := shipHead(t, f)
		shipResetLog(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--parent", "nope")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: branch feature is not tracked by graphite — run gt track, or pass --no-gt"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		assertInvocations(t, shipGTInvocations(t, f), [][]string{
			nogtProbe,
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"gt", "track", "--parent", "nope", "--no-interactive"},
		})
		assertShipRefusedClean(t, f, head)
	})

	// The refusal above replaces gt's sentence with the one step that fixes it,
	// which is exactly why gt's own words must reach the reader some other way:
	// on stderr as gt wrote them, and behind the advice as its cause.
	t.Run("an auto-track failure surfaces gt's error and keeps it as the cause", func(t *testing.T) {
		f := shipGTRepo(t)
		shipGTUntracked(t, f, "feature")
		shipGTReady(t, f)
		head := shipHead(t, f)

		_, errOut, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push", "--parent", "nope")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: branch feature is not tracked by graphite — run gt track, or pass --no-gt"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		if line := gtErrorPrefix + "Could not find branch nope."; !strings.Contains(errOut, line) {
			t.Errorf("stderr = %q, want it to carry gt's own error %q", errOut, line)
		}
		var gtErr *gtError
		if !errors.As(err, &gtErr) {
			t.Errorf("errors.As reached no *gtError through %#v — the advice discarded gt's failure", err)
		}
		assertNoGTCommit(t, shipGTInvocations(t, f))
		assertShipRefusedClean(t, f, head)
	})

	// gt track exits 0 while reporting a branch it refused to adopt, and ccx has
	// no second oracle for a parent gt never wrote — so the ERROR: is the verdict.
	t.Run("a track that reports an error at exit 0 still refuses", func(t *testing.T) {
		log := setupShipGT(t, false)
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		line := gtErrorPrefix + "Cannot track feature: it has no commits of its own."
		t.Setenv("GT_TRACK_STDERR", line)

		_, errOut, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: branch feature is not tracked by graphite — run gt track, or pass --no-gt"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		if !strings.Contains(errOut, line) {
			t.Errorf("stderr = %q, want it to carry gt's own error %q", errOut, line)
		}
		var gtErr *gtError
		if !errors.As(err, &gtErr) || gtErr.Code != 0 {
			t.Errorf("errors.As reached %#v, want the exit-0 gt failure — the ERROR: was read as a success", err)
		}
		// The refusal lands on gt's word alone: a state re-read never runs, so
		// nothing but the ERROR: could have produced it.
		assertInvocations(t, readInvocations(t, log), [][]string{
			nogtProbe,
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"gt", "track", "-f", "--no-interactive"},
		})
	})
}

// TestShipGTExitZeroErrorRefuses covers the gt verbs ship has no second oracle
// for. gt exits 0 while printing an ERROR: naming work it did not do, so for
// these the sentence is the verdict — and the policy is a per-call-site
// argument, which is why each site is pinned rather than one standing in for
// the rest.
func TestShipGTExitZeroErrorRefuses(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		wantPrefix string
	}{
		{name: "gt state", env: "GT_STATE_STDERR", wantPrefix: "ship: gt state: exit 0 but reported an error:"},
		{name: "gt add", env: "GT_ADD_STDERR", wantPrefix: "ship: gt add: exit 0 but reported an error:"},
		{name: "gt modify", env: "GT_MODIFY_STDERR", wantPrefix: "ship: gt modify: exit 0 but reported an error:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupShipGT(t, false)
			line := gtErrorPrefix + "Could not reach the Graphite server."
			t.Setenv(tt.env, line)

			out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatalf("ship reported %q, want a refusal — gt exited 0 saying it did not do the work", out)
			}
			if !strings.HasPrefix(err.Error(), tt.wantPrefix) || !strings.Contains(err.Error(), line) {
				t.Errorf("error = %q, want it to lead with %q and carry %q", err.Error(), tt.wantPrefix, line)
			}
		})
	}
}

// TestShipGTClassifySubmit drives every wording gt is known to refuse a submit
// with, on each stream and at each exit code it is known to use. The exit-0 rows
// are the live repro this classification exists for: gt prints an ERROR: naming
// a submit it did not make and exits 0, which ccx once reported as a success.
func TestShipGTClassifySubmit(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		stdout  string
		exit    string
		wantErr string
	}{
		{name: "restack needed (primary wording)", stderr: gtRestackNeeded1, wantErr: "ship: stack drifted since preflight — run gt restack, then re-run ship"},
		{name: "restack needed (conflict wording)", stderr: gtRestackNeeded2 + "feature", wantErr: "ship: stack drifted since preflight — run gt restack, then re-run ship"},
		{name: "trunk stale", stderr: gtTrunkStale, wantErr: "ship: trunk is out of sync — run gt sync (or ccx vcs restack), then re-run ship"},
		{name: "remote changed (updated wording)", stderr: gtRemoteChanged1, wantErr: "ship: remote branch changed since last submit — reconcile manually (gt sync), then re-run ship"},
		{name: "remote changed (lease wording)", stderr: gtRemoteChanged2, wantErr: "ship: remote branch changed since last submit — reconcile manually (gt sync), then re-run ship"},
		{name: "auth required (please wording)", stderr: gtAuthRequired1, wantErr: "ship: graphite auth required — run gt auth"},
		{name: "auth required (invalid wording)", stderr: gtAuthRequired2, wantErr: "ship: graphite auth required — run gt auth"},
		{
			name: "the same wording on stdout classifies the same way", stdout: gtTrunkStale, exit: "1",
			wantErr: "ship: trunk is out of sync — run gt sync (or ccx vcs restack), then re-run ship",
		},
		{
			name:   "an exit-0 submit reporting an ERROR: is a failure, not a success",
			stderr: gtErrorPrefix + "Could not submit feature: your Graphite token lacks write access.", exit: "0",
			wantErr: "ship: gt submit: exit 0 but reported an error: " + gtErrorPrefix +
				"Could not submit feature: your Graphite token lacks write access.",
		},
		{
			name:   "an exit-0 ERROR: on stdout is classified, not just surfaced",
			stdout: gtErrorPrefix + gtAuthRequired1, exit: "0",
			wantErr: "ship: graphite auth required — run gt auth",
		},
		{
			name: "a WARNING: at exit 0 is not a failure", stderr: gtWarningPrefix + "This command has been renamed.",
			exit: "0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			t.Setenv("GT_SUBMIT_FAIL_STDERR", tt.stderr)
			t.Setenv("GT_SUBMIT_STDOUT", tt.stdout)
			t.Setenv("GT_SUBMIT_EXIT", tt.exit)
			_, errOut, err := runShipCmdFull(t, "-m", "fix: frobnicate")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ship error = %v, want a warning to leave the submit alone", err)
				}
				if !strings.Contains(errOut, tt.stderr) {
					t.Errorf("stderr = %q, want it to carry gt's warning %q", errOut, tt.stderr)
				}
				return
			}
			if err == nil {
				t.Fatal("expected submit failure, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
			submits := 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "submit" {
					submits++
				}
			}
			if submits != 1 {
				t.Errorf("submit ran %d times, want exactly 1 (gt owns restacking, ship never retries)", submits)
			}
		})
	}

	t.Run("unknown stderr wraps verbatim", func(t *testing.T) {
		setupShipGT(t, false)
		t.Setenv("GT_SUBMIT_FAIL_STDERR", "some other gt error")
		_, err := runShipCmd(t, "-m", "fix: frobnicate")
		if err == nil {
			t.Fatal("expected submit failure, got nil")
		}
		if !strings.Contains(err.Error(), "ship: gt submit:") || !strings.Contains(err.Error(), "some other gt error") {
			t.Errorf("error = %q, want it to wrap ship: gt submit: and the raw stderr", err.Error())
		}
	})
}

func TestShipGTDraftPublish(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"draft", []string{"--draft"}, "--draft"},
		{"default publishes", nil, "--publish"},
		{"explicit publish", []string{"--publish"}, "--publish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			args := append([]string{"-m", "fix: frobnicate"}, tt.args...)
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var submit []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "submit" {
					submit = inv
				}
			}
			want := []string{"gt", "submit", "--no-interactive", "--no-edit", "--no-ai", "--no-stack", tt.want}
			if !reflect.DeepEqual(submit, want) {
				t.Errorf("submit argv = %v, want %v", submit, want)
			}
		})
	}

	t.Run("draft and publish are mutually exclusive", func(t *testing.T) {
		log := setupShipGT(t, false)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--draft", "--publish")
		wantErr := "if any flags in the group [draft publish] are set none of the others can be; [draft publish] were all set"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := readInvocations(t, log); inv != nil {
			t.Errorf("no VCS command may run before flag validation, got %v", inv)
		}
	})
}

// TestShipGTFlagsOutsideGTLane covers --parent alone: --draft and --publish
// stopped being graphite-only when ship took over the pull request in every
// lane, where they toggle the draft state through gh.
func TestShipGTFlagsOutsideGTLane(t *testing.T) {
	f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
	head := shipHead(t, f)
	shipResetLog(t, f)
	wantErr := "ship: --parent applies only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop it"
	_, err := runShipCmd(t, "--parent", "base", "--no-push")
	if err == nil || err.Error() != wantErr {
		t.Errorf("error = %v, want %q", err, wantErr)
	}
	if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
		t.Errorf("no VCS command may run before the graphite-only flag check, got %v", inv)
	}
	assertShipRefusedClean(t, f, head)
}

func TestShipGTGHMissing(t *testing.T) {
	log := setupShipGT(t, false)
	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · submitted feature · CI gh-missing`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" {
			t.Errorf("gh invoked despite missing from PATH: %v", inv)
		}
	}
}

func TestShipGTNoPush(t *testing.T) {
	f := shipGTFeature(t)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	invocations := shipGTInvocations(t, f)
	if want := shipCommitted(t, f, vcs.Git) + " · branch feature · not pushed"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	sawState, sawSubmit := false, false
	for _, inv := range invocations {
		if inv[0] == "gt" && inv[1] == "state" {
			sawState = true
		}
		if inv[0] == "gt" && inv[1] == "submit" {
			sawSubmit = true
		}
	}
	if !sawState {
		t.Error("gt state never ran — preflight must run even under --no-push")
	}
	if sawSubmit {
		t.Error("gt submit ran despite --no-push")
	}
	if gitBranchExists(t, f.RemoteDir, "feature") {
		t.Error("origin carries feature — the commit was pushed despite --no-push")
	}
}

func TestShipGTNoVerify(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range shipGTInvocations(t, f) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked despite --no-verify: %v", inv)
		}
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive", "--no-verify"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
	if names := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); names != "f1.go" {
		t.Errorf("committed %q, want the unverified change", names)
	}
}

// TestShipGTHooksSuppressGitRun pins the gt lane's half of the single-run
// guarantee: ccx's own prek pass, then --no-verify so gt's commit does not
// fire the same hooks again through git.
func TestShipGTHooksSuppressGitRun(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	shipHookRepo(t, f, vcs.Git, 0, "", "f1.go")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var uvx, commit []string
	for _, inv := range shipGTInvocations(t, f) {
		if inv[0] == "uvx" {
			if uvx != nil {
				t.Errorf("uvx invoked more than once: %v", inv)
			}
			uvx = inv
		}
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	wantUVX := []string{"uvx", "prek", "run", "--cd", f.Dir, "--files", "f1.go"}
	if !reflect.DeepEqual(uvx, wantUVX) {
		t.Errorf("uvx argv = %v, want %v", uvx, wantUVX)
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive", "--no-verify"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
	if names := gitAt(t, f.Dir, "show", "--name-only", "--format=", "HEAD"); names != "f1.go" {
		t.Errorf("committed %q, want the hooked change", names)
	}
}

func TestShipGTSessionTrailer(t *testing.T) {
	f := shipGTFeature(t)
	t.Setenv(envClaudeSessionKey, "some-uuid")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range shipGTInvocations(t, f) {
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid", "--no-interactive"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
	if body := gitAt(t, f.Dir, "log", "-1", "--format=%B"); body != "fix: frobnicate\n\nClaude-Session-Id: some-uuid" {
		t.Errorf("commit message = %q, want the trailer gt recorded", body)
	}
}

func TestShipReviewsWiring(t *testing.T) {
	t.Run("--reviews requires push", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
		head := shipHead(t, f)
		shipResetLog(t, f)

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--reviews")
		wantErr := "ship: --reviews requires push (drop --no-push)"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := vcstest.Invocations(t, f.ArgvLog); inv != nil {
			t.Errorf("no VCS command may run before the --reviews/--no-push refusal, got %v", inv)
		}
		assertShipRefusedClean(t, f, head)
	})

	t.Run("git lane with no open PR", func(t *testing.T) {
		f := shipRepo(t, vcstest.Remote(), vcstest.Dirty())
		writeShipGH(t, f)
		stubReviewsAPI(t)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
		shipCIPollInterval = 0

		out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		if n := remoteCount(t, f, "main"); n != 2 {
			t.Errorf("origin main holds %d commits, want the commit the reviews watch attached to", n)
		}
		summaryIdx := strings.Index(out, shipCommitted(t, f, vcs.Git)+" · pushed main → origin · CI success")
		notFoundIdx := strings.Index(out, "reviews: no open PR for main")
		if summaryIdx < 0 || notFoundIdx < 0 {
			t.Fatalf("stdout missing expected lines:\n%s", out)
		}
		if notFoundIdx < summaryIdx {
			t.Errorf("the ship report must print before the reviews no-PR note:\n%s", out)
		}
		if !strings.HasSuffix(strings.TrimRight(out, "\n"), "reviews: no open PR for main") {
			t.Errorf("the reviews no-PR note must be the last line:\n%s", out)
		}
	})

	t.Run("gt lane watches every downstack branch", func(t *testing.T) {
		log := setupShipGT(t, true)
		api := stubReviewsAPI(t)
		t.Setenv("GIT_BRANCH", "feature2")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
			`"feature2":{"parents":[{"ref":"feature","sha":"beadfeed"}]}}`)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-success"))
		t.Setenv("GH_PR_VIEW_NOT_FOUND", "1")
		shipCIPollInterval = 0

		out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		var batched [][]string
		for _, inv := range readInvocations(t, log) {
			if len(inv) > 2 && inv[0] == "gh" && inv[1] == "api" && inv[2] == "graphql" {
				batched = append(batched, inv)
			}
		}
		assertInvocations(t, batched, [][]string{ghDownstackPRArgv("feature", "feature2")})

		calls := api.graphQLCalls()
		if len(calls) != 1 {
			t.Fatalf("reviews GraphQL calls = %d, want 1 batch for the whole downstack", len(calls))
		}
		want := map[string]any{"owner": "yasyf", "repo": "cc-context", "p0": "feature2", "p1": "feature"}
		if !reflect.DeepEqual(calls[0].vars, want) {
			t.Errorf("reviews batch vars = %v, want %v", calls[0].vars, want)
		}
		for _, w := range []string{"reviews: no open PR for feature2", "reviews: no open PR for feature"} {
			if !strings.Contains(out, w) {
				t.Errorf("stdout %q missing %q", out, w)
			}
		}
	})

	t.Run("red CI plus clean reviews watch preserves the CI error", func(t *testing.T) {
		setupShipGT(t, true)
		stubReviewsAPI(t)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", ghStdout(t, "run-view-failed"))
		t.Setenv("GH_WATCH_EXIT", "1")
		t.Setenv("GH_LOG_FAILED", "go test failed\n")
		t.Setenv("GH_PR_VIEW_NOT_FOUND", "1")
		shipCIPollInterval = 0

		_, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err == nil {
			t.Fatal("expected a non-nil error from the failed CI run")
		}
		if !strings.Contains(err.Error(), "ship: CI failed for 1 run(s) on the pushed commit") {
			t.Errorf("error = %q, want it to preserve the CI failure", err.Error())
		}
	})
}

// TestShipReviewsWatchExitCodeFirewall proves shipReviewsWatch's %v wrap keeps
// an ErrNotFound-shaped cause from leaking through errors.Join: joined with a
// red-CI error, ExitCode must pick the CI failure's code (1), not the reviews
// watch's would-be 3.
func TestShipReviewsWatchExitCodeFirewall(t *testing.T) {
	s := setupReviews(t)
	s.branch("main", 5)

	var out bytes.Buffer
	reviewsErr := shipReviewsWatch(context.Background(), &out, []string{"main"})
	if reviewsErr == nil {
		t.Fatal("expected shipReviewsWatch to return a non-nil error")
	}
	if errors.Is(reviewsErr, ErrNotFound) {
		t.Errorf("shipReviewsWatch error = %v, must not still match ErrNotFound", reviewsErr)
	}

	ciErr := errors.New("ship: CI failed for 1 run(s) on the pushed commit")
	if code := ExitCode(errors.Join(ciErr, reviewsErr)); code != 1 {
		t.Errorf("ExitCode(errors.Join(ciErr, reviewsErr)) = %d, want 1 (the CI failure's code)", code)
	}
}

// TestGitTrunkBranchLive proves gitTrunkBranch reads origin/HEAD's target rather
// than trusting the prefix strip: git accepts
// `git symbolic-ref refs/remotes/origin/HEAD refs/tags/v1` and `--short` prints
// `v1` at exit 0, so a discarded CutPrefix ok made ship adopt a tag as trunk and
// ship onto it. An origin/HEAD that does not resolve at all stays the documented
// empty answer — that is the local-only repository, which has no trunk.
func TestGitTrunkBranchLive(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		want    string
		wantErr bool
	}{
		{name: "branch target", target: "refs/remotes/origin/main", want: "main"},
		{name: "tag target refuses", target: "refs/tags/v1", wantErr: true},
		{name: "unresolved origin/HEAD has no trunk", target: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireLiveVCS(t, "git")
			dir := setupLiveGitRepo(t, "base\n", "edited\n")
			mustRun(t, dir, "git", "tag", "v1")
			mustRun(t, dir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")
			if tt.target != "" {
				mustRun(t, dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD", tt.target)
			}

			got, err := gitTrunkBranch(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("gitTrunkBranch() = %q, nil; want an error naming %s", got, tt.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitTrunkBranch(): %v", err)
			}
			if got != tt.want {
				t.Errorf("gitTrunkBranch() = %q, want %q", got, tt.want)
			}
		})
	}
}
