package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcs"
)

func TestJJWorkingCopyFlag(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T) string
	}{
		{
			name: "commit and push",
			run: func(t *testing.T) string {
				log := setupShip(t, ".jj", true)
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return log
			},
		},
		{
			name: "track the push target",
			run: func(t *testing.T) string {
				log := setupShip(t, ".jj", true)
				t.Setenv("JJ_UNTRACKED_REMOTES", "backup origin")
				t.Setenv("JJ_PUSH_REMOTE", "backup")
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return log
			},
		},
		{
			name: "amend",
			run: func(t *testing.T) string {
				log := setupShip(t, ".jj", true)
				_, _ = runShipCmd(t, "--amend", "--no-push")
				return log
			},
		},
		{
			name: "create a bookmark",
			run: func(t *testing.T) string {
				log := setupShip(t, ".jj", true)
				t.Setenv("JJ_BOOKMARK_HEADS", "0")
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
				return log
			},
		},
		{
			name: "conflicted rebase rolls back",
			run: func(t *testing.T) string {
				log := setupShip(t, ".jj", true)
				t.Setenv("JJ_NO_BOOKMARK", "1")
				t.Setenv("JJ_DIVERGED", "1")
				t.Setenv("JJ_CONFLICTS", "c0ffee1 fix: frobnicate\n")
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
				return log
			},
		},
		{
			name: "hunk-scoped",
			run: func(t *testing.T) string {
				log := setupHunkShip(t, "f.txt")
				ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
				_, _ = runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--skip-hunk", ref, "f.txt")
				return log
			},
		},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, inv := range readInvocations(t, tt.run(t)) {
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
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj happy path",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
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
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
		{
			name:   "git happy path",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate"},
			want: [][]string{
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
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "watch", "42", "--exit-status"},
				{"gh", "run", "view", "42", "--json", "workflowName,conclusion,startedAt,updatedAt,url,jobs"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
				{"gh", "run", "list", "--commit", fakeHeadSHA, "--limit", "50", "--json", "databaseId,workflowName,status,url"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			shipCIPollInterval = 0

			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipHooksPass(t *testing.T) {
	tests := []struct {
		name   string
		marker string
		want   [][]string
	}{
		{
			name:   "jj",
			marker: ".jj",
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
			name:   "git",
			marker: ".git",
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
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

			for i, rec := range tt.want {
				for j, field := range rec {
					if field == "ROOT" {
						tt.want[i][j] = root
					}
				}
			}

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipHooksJJAmend(t *testing.T) {
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeShipHookFiles(t, root, "folded.go")
	t.Setenv("JJ_DIFF_NAMES", "folded.go\n")

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "diff", "--name-only"},
		{"uvx", "prek", "run", "--cd", root, "--files", "folded.go"},
		{"jj", "squash", "--use-destination-message"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
	})
}

func TestShipHooksSubdirRunsAtRoot(t *testing.T) {
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeShipHookFiles(t, root, "sub/x.go")
	t.Setenv("JJ_DIFF_NAMES", "sub/x.go\n")
	t.Setenv("JJ_LOG_PWD", "1")
	if err := os.Chdir(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("chdir sub: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "x.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"pwd", root},
		{"jj", "diff", "--name-only", "--", "sub/x.go"},
		{"pwd", root},
		{"uvx", "prek", "run", "--cd", root, "--files", "sub/x.go"},
		{"jj", "commit", "-m", "fix: frobnicate", "--", "x.go"},
		{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
	})
}

func TestShipHooksAutoFixLeavingNothingAborts(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
			// The hooks' second derivation returns nothing: the auto-fixer reverted the change.
			namesMarker := filepath.Join(root, "names.marker")
			if err := os.WriteFile(namesMarker, []byte("0"), 0o600); err != nil {
				t.Fatalf("write names marker: %v", err)
			}
			t.Setenv("SHIP_DIFF_NAMES_MARKER", namesMarker)
			t.Setenv("JJ_DIFF_NAMES_2", "")
			t.Setenv("GIT_DIFF_NAMES_2", "")
			if tt.marker == ".jj" {
				t.Setenv("SHIP_DIFF_NAMES_SWITCH_AFTER", "2")
			}
			failMarker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(failMarker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write fail marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", failMarker)

			_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
				t.Fatalf("ship error = %v, want nothing-to-commit", err)
			}
			uvxCount, jjDiffCount := 0, 0
			for _, inv := range readInvocations(t, log) {
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
			if tt.marker == ".jj" && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
		})
	}
}

func TestShipHooksAutoFixThenPass(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "f1.go")
			t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
			t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
			marker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", marker)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks fixed · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			uvxCount, gitAddCount := 0, 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvxCount++
				}
				if tt.marker == ".git" && len(inv) >= 3 && inv[0] == "git" && inv[1] == "add" && inv[2] == "-A" {
					gitAddCount++
				}
			}
			if uvxCount != 2 {
				t.Errorf("uvx invocation count = %d, want 2", uvxCount)
			}
			if tt.marker == ".git" && gitAddCount != 2 {
				t.Errorf("git add -A invocation count = %d, want 2", gitAddCount)
			}
		})
	}
}

func TestShipHooksRetryRederivesFiles(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{"jj", ".jj"},
		{"git", ".git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if tt.marker == ".jj" {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
					t.Fatalf("mkdir .git: %v", err)
				}
			}
			writeShipHookFiles(t, root, "first.go", "generated.go")
			t.Setenv("JJ_DIFF_NAMES", "first.go\n")
			t.Setenv("JJ_DIFF_NAMES_2", "generated.go\n")
			t.Setenv("GIT_DIFF_NAMES", "first.go\n")
			t.Setenv("GIT_DIFF_NAMES_2", "generated.go\n")
			diffMarker := filepath.Join(root, "diff.marker")
			t.Setenv("SHIP_DIFF_NAMES_MARKER", diffMarker)
			if tt.marker == ".jj" {
				t.Setenv("SHIP_DIFF_NAMES_SWITCH_AFTER", "2")
			}
			failMarker := filepath.Join(root, "prek.marker")
			if err := os.WriteFile(failMarker, []byte("1"), 0o600); err != nil {
				t.Fatalf("write prek marker: %v", err)
			}
			t.Setenv("UVX_PREK_FAIL_MARKER", failMarker)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks fixed · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			var uvx [][]string
			jjDiffCount := 0
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvx = append(uvx, inv)
				}
				if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "diff" && inv[2] == "--name-only" {
					jjDiffCount++
				}
			}
			wantUVX := [][]string{
				{"uvx", "prek", "run", "--cd", root, "--files", "first.go"},
				{"uvx", "prek", "run", "--cd", root, "--files", "generated.go"},
			}
			assertInvocations(t, uvx, wantUVX)
			if tt.marker == ".jj" && jjDiffCount != 3 {
				t.Errorf("jj diff --name-only invocation count = %d, want 3", jjDiffCount)
			}
		})
	}
}

func TestShipHooksPersistentFailure(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	marker := filepath.Join(root, "prek.marker")
	if err := os.WriteFile(marker, []byte("2"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("UVX_PREK_FAIL_MARKER", marker)

	_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "ship: hooks:") {
		t.Fatalf("ship error = %v, want containing %q", err, "ship: hooks:")
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" && len(inv) > 1 && inv[1] == "commit" {
			t.Errorf("commit ran after persistent hook failure: %v", inv)
		}
	}
}

func TestShipHooksNoVerify(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
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
	log := setupShip(t, ".git", false)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
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
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	config := "repos:\n  - repo: local\n    hooks:\n      - id: gitlint\n        stages: [commit-msg]\n"
	if err := os.WriteFile(filepath.Join(root, ".pre-commit-config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write pre-commit config: %v", err)
	}
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "commit" {
			commit = inv
		}
	}
	wantCommit := []string{"git", "commit", "-m", "fix: frobnicate"}
	if !reflect.DeepEqual(commit, wantCommit) {
		t.Errorf("commit argv = %v, want %v — a commit-msg stage still needs git's hook run", commit, wantCommit)
	}
}

func TestShipHooksUvxMissing(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	if err := os.Remove(filepath.Join(filepath.Dir(log), "bin", "uvx")); err != nil {
		t.Fatalf("remove fake uvx: %v", err)
	}

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks uvx-missing · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
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
	log := setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("JJ_DIFF_NAMES", "f1.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks no-git · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			t.Errorf("uvx invoked for a jj repo without a .git marker: %v", inv)
		}
	}
}

func TestShipHooksEmptyFilesSkipSoftGuards(t *testing.T) {
	tests := []struct {
		name      string
		marker    string
		removeUVX bool
	}{
		{"jj without git", ".jj", false},
		{"git without uvx", ".git", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			writeShipHookFiles(t, root)
			if tt.marker == ".jj" {
				t.Setenv("JJ_DIFF_NAMES", "")
				_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
				if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
					t.Fatalf("ship error = %v, want nothing-to-commit", err)
				}
				invocations := readInvocations(t, log)
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
				return
			}
			if tt.removeUVX {
				if err := os.Remove(filepath.Join(filepath.Dir(log), "bin", "uvx")); err != nil {
					t.Fatalf("remove fake uvx: %v", err)
				}
			}

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					t.Errorf("uvx invoked with no changed files: %v", inv)
				}
			}
		})
	}
}

func TestShipHooksScopedPaths(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o750); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write src/a.go: %v", err)
	}
	t.Setenv("GIT_DIFF_NAMES", "src/a.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	want2 := [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A", "--", "src/a.go"},
		{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z", "--", "src/a.go"},
		{"uvx", "prek", "run", "--cd", root, "--files", "src/a.go"},
		{"git", "commit", "-m", "fix: frobnicate", "--no-verify", "--", "src/a.go"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	}
	assertInvocations(t, readInvocations(t, log), want2)
}

func TestShipHooksFiltersMissingFile(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\ngone.go\n")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "uvx" {
			for _, f := range inv {
				if f == "gone.go" {
					t.Errorf("--files listed a deleted file: %v", inv)
				}
			}
		}
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
			log := setupShip(t, ".git", false)
			root, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			writeShipHookFiles(t, root)
			tt.create(t, filepath.Join(root, tt.filename))
			t.Setenv("GIT_DIFF_NAMES", tt.filename+"\n")

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			want := `hooks ok · committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
			if got != want {
				t.Errorf("summary = %q, want %q", got, want)
			}
			var uvx []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "uvx" {
					uvx = inv
				}
			}
			wantUVX := []string{"uvx", "prek", "run", "--cd", root, "--files", tt.filename}
			if !reflect.DeepEqual(uvx, wantUVX) {
				t.Errorf("uvx argv = %v, want %v", uvx, wantUVX)
			}
		})
	}
}

func TestShipJJNeverInvokesGitCommit(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	if _, err := runShipCmd(t, "-m", "fix: frobnicate"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" {
			t.Errorf("jj path invoked git: %v", inv)
		}
	}
}

func TestShipCommitOnlyVariants(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj no-push",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git no-push",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend no message",
			marker: ".jj",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend with message",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git amend no message",
			marker: ".git",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git amend no verify",
			marker: ".git",
			args:   []string{"--amend", "--no-verify", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit", "--no-verify"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj scoped paths",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only", "--", "src/a.go", "docs"},
				{"jj", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git scoped paths",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A", "--", "src/a.go", "docs"},
				{"git", "commit", "-m", "fix: frobnicate", "--", "src/a.go", "docs"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend scoped no message",
			marker: ".jj",
			args:   []string{"--amend", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message", "--", "src/a.go"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend scoped with message",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git amend scoped",
			marker: ".git",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push", "src/a.go"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A", "--", "src/a.go"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate", "--", "src/a.go"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
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
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_BOOKMARK_HEADS", "0")

	_, err := runShipCmd(t, "--amend", "--no-push", "--branch", "missing")
	if err == nil || err.Error() != `ship: bookmark "missing" not found` {
		t.Errorf("ship error = %v, want bookmark \"missing\" not found", err)
	}
	got := readInvocations(t, log)
	assertNoShipMutation(t, got)
	assertInvocations(t, got, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"missing")`, "--no-graph", "-T", jjStackLineTemplate},
	})
}

func TestShipJJEmptyRefuses(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantErr     string
		wantSummary string
		want        [][]string
	}{
		{
			name:    "unscoped",
			args:    []string{"-m", "fix: frobnicate", "--no-watch"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"main"' --to @- && jj git push --bookmark 'exact:"main"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "path scoped",
			args:    []string{"-m", "fix: frobnicate", "--no-watch", "src/a.go"},
			wantErr: `ship: nothing to commit in src/a.go — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"main"' --to @- && jj git push --bookmark 'exact:"main"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only", "--", "src/a.go"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "bookmark hint",
			args:    []string{"-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"someone/probe"' --to @- && jj git push --bookmark 'exact:"someone/probe"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			// The hint names the target under --no-push too: the branch plan
			// resolves once for every lane, push or not.
			name:    "no push still names the target",
			args:    []string{"-m", "fix: frobnicate", "--no-push", "--bookmark", "someone/probe"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"someone/probe"' --to @- && jj git push --bookmark 'exact:"someone/probe"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"someone/probe")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "description only working copy refuses",
			args:    []string{"-m", "description only", "--no-push"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"main"' --to @- && jj git push --bookmark 'exact:"main"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:        "conflicted merge working copy commits",
			args:        []string{"-m", "fix: frobnicate", "--no-push"},
			env:         map[string]string{"JJ_AT_PARENTS": "2"},
			wantSummary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
		},
		{
			name:    "conflicted single-parent working copy refuses",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"? push it: jj bookmark move 'exact:"main"' --to @- && jj git push --bookmark 'exact:"main"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "empty root refuses",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			env:     map[string]string{"JJ_DESCRIBE_OUTPUT": "000000000000\n"},
			wantErr: `ship: nothing to commit — did a prior ship already land 000000000000 ""? push it: jj bookmark move 'exact:"main"' --to @- && jj git push --bookmark 'exact:"main"'`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
		{
			name:    "description without separator errors",
			args:    []string{"-m", "fix: frobnicate", "--no-push"},
			env:     map[string]string{"JJ_DESCRIBE_OUTPUT": "000000000000"},
			wantErr: `ship: malformed commit description "000000000000"`,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", jjAtStateTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", false)
			t.Setenv("JJ_DIFF_NAMES", "")
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			got, err := runShipCmd(t, tt.args...)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected empty ship refusal, got nil")
				}
				if err.Error() != tt.wantErr {
					t.Errorf("error = %q, want %q", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.wantSummary {
					t.Errorf("summary = %q, want %q", got, tt.wantSummary)
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJEmptyAmendExempt(t *testing.T) {
	log := setupShip(t, ".jj", false)
	t.Setenv("JJ_DIFF_NAMES", "")

	got, err := runShipCmd(t, "--amend", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
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
			log := setupShip(t, ".git", false)
			t.Setenv("GIT_BRANCH", "")

			_, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate"}, args...)...)
			if err == nil || err.Error() != "ship: detached HEAD — check out a branch before shipping" {
				t.Fatalf("ship error = %v, want detached HEAD refusal", err)
			}
			invocations := readInvocations(t, log)
			assertInvocations(t, invocations, [][]string{{"git", "branch", "--show-current"}, gitTrunkArgv})
			assertNoShipMutation(t, invocations)
		})
	}
}

// TestShipDetachedHeadAfterCommitSelfHeals proves a commit that leaves HEAD
// detached — twice observed from a gt-lane ship in a linked worktree — is
// repaired with git checkout -B instead of reported as a success.
func TestShipDetachedHeadAfterCommitSelfHeals(t *testing.T) {
	log := setupShipGT(t, false)
	t.Setenv("GIT_DETACHED_AFTER_COMMIT", "1")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch feature · healed detached HEAD onto feature · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	var healed []string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "git" && inv[1] == "checkout" {
			healed = inv
		}
	}
	if want := []string{"git", "checkout", "-B", "feature", fakeHeadSHA}; !reflect.DeepEqual(healed, want) {
		t.Errorf("heal argv = %v, want %v", healed, want)
	}
}

func TestShipGitUsesPostCommitBranch(t *testing.T) {
	log := setupShip(t, ".git", false)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GIT_BRANCH_AFTER_COMMIT", "other")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed other → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
		{"git", "config", "--get", "branch.other.remote"},
		{"git", "fetch", "origin"},
		{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/other"},
		{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/other", "HEAD"},
		{"git", "push", "origin", "other"},
	})
}

func TestShipSessionTrailer(t *testing.T) {
	tests := []struct {
		name    string
		marker  string
		args    []string
		want    [][]string
		summary string
	}{
		{
			name:   "jj commit appends trailer",
			marker: ".jj",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git commit appends trailer",
			marker: ".git",
			args:   []string{"-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend with message appends trailer",
			marker: ".jj",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git amend with message appends trailer",
			marker: ".git",
			args:   []string{"--amend", "-m", "fix: frobnicate", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "jj amend without message carries no trailer",
			marker: ".jj",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "squash", "--use-destination-message"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
		{
			name:   "git amend without message carries no trailer",
			marker: ".git",
			args:   []string{"--amend", "--no-push"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "--amend", "--no-edit"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, tt.marker, true)
			t.Setenv(envClaudeSessionKey, "some-uuid")
			got, err := runShipCmd(t, tt.args...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipGitAmendFastForwardPush(t *testing.T) {
	log := setupShip(t, ".git", true)
	got, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	invocations := readInvocations(t, log)
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

func TestShipGitRebase(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		want     [][]string
		summary  string
		wantErr  []string
		wantWarn bool
	}{
		{
			name: "no divergence pushes clean",
			want: [][]string{
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
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name: "diverged rebases then pushes",
			env:  map[string]string{"GIT_DIVERGED": "1"},
			want: [][]string{
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
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "rebase conflict aborts and reports",
			env:  map[string]string{"GIT_DIVERGED": "1", "GIT_REBASE_CONFLICT": "1"},
			want: [][]string{
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
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				{"git", "diff", "--name-only", "--diff-filter=U"},
				{"git", "rebase", "--abort"},
			},
			wantErr: []string{"rebase onto origin/main conflicts in: f.txt", "resolve manually"},
		},
		{
			name: "missing remote branch skips rebase",
			env:  map[string]string{"GIT_REMOTE_REF_MISSING": "1"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name:     "autostash pop conflict warns",
			env:      map[string]string{"GIT_DIVERGED": "1", "GIT_AUTOSTASH_WARN": "1"},
			wantWarn: true,
			want: [][]string{
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
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "resolves the configured remote",
			env:  map[string]string{"GIT_BRANCH_REMOTE": "backup"},
			want: [][]string{
				{"git", "branch", "--show-current"},
				gitTrunkArgv,
				{"git", "add", "-A"},
				{"git", "commit", "-m", "fix: frobnicate"},
				{"git", "branch", "--show-current"},
				{"git", "log", "-1", "--format=%h%x00%s"},
				{"git", "config", "--get", "branch.main.remote"},
				{"git", "fetch", "backup"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/backup/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/backup/main", "HEAD"},
				{"git", "push", "backup", "main"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → backup`,
		},
		{
			name: "rebase failing before it starts is not a conflict",
			env:  map[string]string{"GIT_DIVERGED": "1", "GIT_REBASE_NO_START": "1"},
			want: [][]string{
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
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
			},
			wantErr: []string{"git rebase onto origin/main", "uncommitted changes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			buf := captureSlog(t)

			got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			if warned := strings.Contains(buf.String(), "git stash pop"); warned != tt.wantWarn {
				t.Errorf("autostash warning = %v, want %v (log: %q)", warned, tt.wantWarn, buf.String())
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipGitPushRetry(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		env            map[string]string
		pushReject     int
		divergedSwitch bool
		want           [][]string
		summary        string
		wantErr        []string
	}{
		{
			name:           "rejected push refetches and lands",
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
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
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:       "retries exhausted names the remedy",
			pushReject: 3,
			want: [][]string{
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
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "push", "origin", "main"},
			},
			wantErr: []string{"rejected 3 times", "git fetch origin && git rebase --autostash origin/main && git push", "non-fast-forward"},
		},
		{
			name:           "conflict during retry rebase is terminal",
			pushReject:     1,
			divergedSwitch: true,
			env:            map[string]string{"GIT_REBASE_CONFLICT": "1"},
			want: [][]string{
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
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "rev-parse", "--verify", "--quiet", "REBASE_HEAD"},
				{"git", "diff", "--name-only", "--diff-filter=U"},
				{"git", "rebase", "--abort"},
			},
			wantErr: []string{"rebase onto origin/main conflicts", "f.txt"},
		},
		{
			name: "amend stale lease never fetches or retries",
			args: []string{"--amend"},
			env:  map[string]string{"GIT_AMEND_PLAIN_NONFF": "1", "GIT_LEASE_STALE": "1"},
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
				{"git", "push", "origin", "--force-with-lease=main:" + fakeHeadSHA, "main"},
			},
			wantErr: []string{"built on the commit you amended"},
		},
		{
			name: "hook decline does not retry",
			env:  map[string]string{"GIT_PUSH_FAIL_STDERR": "! [remote rejected] main -> main (pre-receive hook declined)"},
			want: [][]string{
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
			},
			wantErr: []string{"ship: git push:", "pre-receive hook declined"},
		},
		{
			name:       "both attempts rebase reports the count once",
			pushReject: 1,
			env:        map[string]string{"GIT_DIVERGED": "1"},
			want: [][]string{
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
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "fetch", "origin"},
				{"git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"},
				{"git", "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"},
				{"git", "rev-list", "--count", "refs/remotes/origin/main..HEAD"},
				{"git", "rebase", "--autostash", "refs/remotes/origin/main"},
				{"git", "push", "origin", "main"},
				{"git", "log", "-1", "--format=%h%x00%s"},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name: "remote-rejected veto beats a mixed non-fast-forward token",
			env:  map[string]string{"GIT_PUSH_FAIL_STDERR": "! [remote rejected] main -> main (pre-receive hook declined)\n! [rejected] feature -> feature (non-fast-forward)"},
			want: [][]string{
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
			},
			wantErr: []string{"ship: git push:", "pre-receive hook declined"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".git", false)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if tt.pushReject > 0 {
				marker := filepath.Join(t.TempDir(), "gitpush")
				if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d", tt.pushReject)), 0o600); err != nil {
					t.Fatalf("write push marker: %v", err)
				}
				t.Setenv("GIT_PUSH_REJECT_MARKER", marker)
			}
			if tt.divergedSwitch {
				t.Setenv("GIT_DIVERGED_MARKER", filepath.Join(t.TempDir(), "gitdiverged"))
			}

			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)
			got, err := runShipCmd(t, args...)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipNoWatchSkipsCI(t *testing.T) {
	log := setupShip(t, ".jj", true)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
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
	})
}

func TestShipCIStates(t *testing.T) {
	tests := []struct {
		name      string
		withGh    bool
		runList   string
		viewJSON  string
		watchExit string
		summary   string
		wantErr   bool
		wantWatch bool
	}{
		{
			name:      "gh missing",
			withGh:    false,
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI gh-missing`,
			wantWatch: false,
		},
		{
			name:      "no run",
			withGh:    true,
			runList:   "[]",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI no-run`,
			wantWatch: false,
		},
		{
			name:      "failure",
			withGh:    true,
			runList:   fakeRunListJSON,
			viewJSON:  fakeRunViewFailure,
			watchExit: "1",
			summary:   `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI failure`,
			wantErr:   true,
			wantWatch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", tt.withGh)
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
			if got != tt.summary {
				t.Errorf("summary = %q, want %q", got, tt.summary)
			}
			watched := false
			for _, inv := range readInvocations(t, log) {
				if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
					watched = true
				}
			}
			if watched != tt.wantWatch {
				t.Errorf("gh run watch invoked = %v, want %v", watched, tt.wantWatch)
			}
		})
	}
}

func TestShipCINoRunWithWorkflowIsUnconfirmed(t *testing.T) {
	setupShip(t, ".jj", true)
	workflowDir := filepath.Join(".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o750); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), []byte("name: ci\n"), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("GH_RUN_LIST_JSON", "[]")
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when workflows exist but no run was registered")
	}
	if want := "· CI unconfirmed"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	for _, want := range []string{"no CI run was registered", "paths-filtered", "dispatch-only", "on: workflow_dispatch", "gh run list --commit " + fakeHeadSHA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestShipHeadSHAFailurePrintsCommitPushSummary(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_COMMIT_ID_FAIL", "1")

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected head SHA error, got nil")
	}
	want := "committed a1b2c3d \"fix: frobnicate\" · pushed main → origin\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if strings.Contains(out, "CI ") {
		t.Errorf("head SHA failure must not print a CI segment, got %q", out)
	}
	if !strings.Contains(err.Error(), "jj log commit_id") {
		t.Errorf("error = %v, want jj log commit_id failure", err)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gh" {
			t.Errorf("head SHA failure must stop before gh, got invocation %v", inv)
		}
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
func TestGitIsAncestorPrefix(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{"ship", "ship", "ship: git merge-base --is-ancestor: exit 2: fatal: not a valid object name"},
		{"restack", "restack", "restack: git merge-base --is-ancestor: exit 2: fatal: not a valid object name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupShip(t, ".git", false)
			t.Setenv("GIT_ANCESTOR_EXIT", "2")
			_, err := gitIsAncestor(context.Background(), tt.prefix, "main", "HEAD")
			if err == nil || err.Error() != tt.want {
				t.Fatalf("gitIsAncestor error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestShipJJRebase(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		env            map[string]string
		describeMarker bool
		pushReject     int
		divergedSwitch bool
		want           [][]string
		summary        string
		wantErr        []string
	}{
		{
			name: "untracked trunk auto-tracks before fetch then pushes",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "origin"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=origin"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			// The trunk's counterpart is untracked on a non-origin remote (main@backup)
			// while main@origin is tracked: ship must track the remote the untracked
			// counterpart actually sits on, not a hard-coded origin.
			name: "untracked counterpart on a non-origin remote tracks that remote",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "backup"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=backup"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			// Two remotes carry an untracked counterpart, so ship breaks the tie on the
			// remote jj git push targets — the git.push config setting.
			name: "multiple untracked counterparts track the push target",
			env:  map[string]string{"JJ_UNTRACKED_REMOTES": "backup origin", "JJ_PUSH_REMOTE": "backup"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "--ignore-working-copy", "config", "get", "git.push"},
				{"jj", "bookmark", "track", vcs.JJExactPattern("main"), "--remote=backup"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
			},
			summary: `committed a1b2c3d "fix: frobnicate" · pushed main → origin`,
		},
		{
			name:           "diverged trunk rebases",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_DIVERGED": "1"},
			describeMarker: true,
			want: [][]string{
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
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:           "diverged --bookmark rebases",
			args:           []string{"--bookmark", "someone/probe"},
			env:            map[string]string{"JJ_DIVERGED": "1"},
			describeMarker: true,
			want: [][]string{
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
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("someone/probe"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"someone/probe")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"someone/probe")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("someone/probe"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("someone/probe")},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto someone/probe · pushed someone/probe → origin`,
		},
		{
			name: "conflicted target refuses",
			env: map[string]string{
				"JJ_NO_BOOKMARK":    "1",
				"JJ_BOOKMARK_HEADS": "2",
			},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
			},
			wantErr: []string{`bookmark "main" is conflicted (2 heads)`, "resolve it"},
		},
		{
			name: "conflicted rebase rolls back",
			env: map[string]string{
				"JJ_NO_BOOKMARK": "1",
				"JJ_DIVERGED":    "1",
				"JJ_CONFLICTS":   "c0ffee1 fix: frobnicate\n",
			},
			want: [][]string{
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
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`rebase onto "main" conflicts in 1 commit`, "c0ffee1", "rolled back"},
		},
		{
			name: "conflict check failure rolls back",
			env: map[string]string{
				"JJ_NO_BOOKMARK":         "1",
				"JJ_DIVERGED":            "1",
				"JJ_CONFLICT_CHECK_FAIL": "1",
			},
			want: [][]string{
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
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`conflict check after rebase onto "main" failed (rebase rolled back)`},
		},
		{
			name: "already landed refuses",
			env: map[string]string{
				"JJ_NO_BOOKMARK": "1",
				"JJ_DIVERGED":    "1",
				"JJ_STACK_EMPTY": "1",
			},
			want: [][]string{
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
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
			},
			wantErr: []string{"already landed", "refusing to move the bookmark backwards"},
		},
		{
			name: "fetch failure is fatal",
			env:  map[string]string{"JJ_FETCH_FAIL": "1"},
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "diff", "--name-only"},
				{"jj", "commit", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
			},
			wantErr: []string{"jj git fetch"},
		},
		{
			name:           "rejected push restores and lands",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1"},
			describeMarker: true,
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
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
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			},
			summary: `committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`,
		},
		{
			name:       "retries exhausted restores last state",
			env:        map[string]string{"JJ_NO_BOOKMARK": "1"},
			pushReject: 3,
			want: [][]string{
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
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{"rejected 3 times", "jj git fetch && jj rebase -b @-", "unexpectedly moved"},
		},
		{
			name:       "amend rejection refuses",
			args:       []string{"--amend"},
			env:        map[string]string{"JJ_NO_BOOKMARK": "1"},
			pushReject: 1,
			want: [][]string{
				{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "squash", "-m", "fix: frobnicate"},
				{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
				{"jj", "bookmark", "list", vcs.JJExactPattern("main"), "--all-remotes", "-T", jjRemoteBookmarkTemplate},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "git", "push", "--bookmark", vcs.JJExactPattern("main")},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{"not force-retrying over their work"},
		},
		{
			name:       "permission failure passes through",
			env:        map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_PUSH_FAIL_STDERR": "The remote rejected the following updates:\nError: Failed to push some bookmarks"},
			pushReject: 1,
			want: [][]string{
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
			},
			wantErr: []string{"ship: jj git push:", "Failed to push some bookmarks"},
		},
		{
			name:           "conflict during retry rebase rolls back",
			env:            map[string]string{"JJ_NO_BOOKMARK": "1", "JJ_CONFLICTS": "c0ffee1 fix: frobnicate\n"},
			pushReject:     1,
			divergedSwitch: true,
			want: [][]string{
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
				{"jj", "op", "revert", "op123abc"},
				{"jj", "git", "fetch"},
				{"jj", "--ignore-working-copy", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjAncestorRevset("main"), "--no-graph", "-T", jjBookmarkTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", jjStackRevset("main"), "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "rebase", "-b", "@-", "--destination", `bookmarks(exact:"main")`},
				{"jj", "--ignore-working-copy", "op", "log", "-n", "1", "--no-graph", "-T", jjOpIDTemplate},
				{"jj", "--ignore-working-copy", "log", "-r", `conflicts() & (bookmarks(exact:"main")..@-)::`, "--no-graph", "-T", jjStackLineTemplate},
				{"jj", "op", "revert", "op123abc"},
			},
			wantErr: []string{`rebase onto "main" conflicts`, "c0ffee1", "rolled back"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShip(t, ".jj", true)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if tt.describeMarker {
				marker := filepath.Join(t.TempDir(), "describe")
				if err := os.WriteFile(marker, nil, 0o600); err != nil {
					t.Fatalf("write describe marker: %v", err)
				}
				t.Setenv("JJ_DESCRIBE_MARKER", marker)
			}
			if tt.pushReject > 0 {
				marker := filepath.Join(t.TempDir(), "jjpush")
				if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d", tt.pushReject)), 0o600); err != nil {
					t.Fatalf("write push marker: %v", err)
				}
				t.Setenv("JJ_PUSH_REJECT_MARKER", marker)
			}
			if tt.divergedSwitch {
				t.Setenv("JJ_DIVERGED_MARKER", filepath.Join(t.TempDir(), "jjdiverged"))
			}

			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.args...)
			got, err := runShipCmd(t, args...)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ship error = %v", err)
				}
				if got != tt.summary {
					t.Errorf("summary = %q, want %q", got, tt.summary)
				}
			} else {
				if err == nil {
					t.Fatal("expected ship error, got nil")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			}
			assertInvocations(t, readInvocations(t, log), tt.want)
		})
	}
}

func TestShipJJPushRevertTargetsBookmarkMove(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_DIVERGED", "1")
	t.Setenv("JJ_OP_LOG_COUNTER", filepath.Join(t.TempDir(), "opcounter"))
	marker := filepath.Join(t.TempDir(), "jjpush")
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatalf("write push marker: %v", err)
	}
	t.Setenv("JJ_PUSH_REJECT_MARKER", marker)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	// Attempt 1 logs op001 after the rebase and op002 after the bookmark move; the
	// rejected push must revert op002 (the move), never op001 (the rebase, which is
	// kept and replayed onto the fresh remote tip).
	var reverted []string
	for _, inv := range readInvocations(t, log) {
		if len(inv) == 4 && inv[0] == "jj" && inv[1] == "op" && inv[2] == "revert" {
			reverted = append(reverted, inv[3])
		}
	}
	if want := []string{"op002"}; !reflect.DeepEqual(reverted, want) {
		t.Errorf("op revert targets = %v, want %v", reverted, want)
	}
}

func TestShipJJPushRevertFailureIsTerminal(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_OP_REVERT_FAIL", "1")
	marker := filepath.Join(t.TempDir(), "jjpush")
	if err := os.WriteFile(marker, []byte("1"), 0o600); err != nil {
		t.Fatalf("write push marker: %v", err)
	}
	t.Setenv("JJ_PUSH_REJECT_MARKER", marker)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil {
		t.Fatal("expected terminal error when op revert fails")
	}
	if !strings.Contains(err.Error(), "jj op revert op123abc") {
		t.Errorf("error = %q, want it to name the manual revert command", err)
	}
	// A failed undo is terminal: exactly one fetch, no retry.
	fetches := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "jj" && inv[1] == "git" && inv[2] == "fetch" {
			fetches++
		}
	}
	if fetches != 1 {
		t.Errorf("jj git fetch count = %d, want 1 (a failed undo must not retry)", fetches)
	}
}

func TestShipJJRebasePreservesHookSummary(t *testing.T) {
	setupShip(t, ".jj", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("JJ_DIFF_NAMES", "f1.go\n")
	t.Setenv("JJ_NO_BOOKMARK", "1")
	t.Setenv("JJ_DIVERGED", "1")
	describeMarker := filepath.Join(t.TempDir(), "describe")
	if err := os.WriteFile(describeMarker, nil, 0o600); err != nil {
		t.Fatalf("write describe marker: %v", err)
	}
	t.Setenv("JJ_DESCRIBE_MARKER", describeMarker)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks ok · committed e9f8a7b "fix: frobnicate" · rebased 2 commit(s) onto main · pushed main → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if count := strings.Count(got, "hooks ok"); count != 1 {
		t.Errorf("hooks segment count = %d, want 1 in %q", count, got)
	}
	if count := strings.Count(got, "committed "); count != 1 {
		t.Errorf("committed segment count = %d, want 1 in %q", count, got)
	}
}

// TestShipJJNonTrunkBookmarkAppends proves the old "nearest bookmark is not
// trunk" refusal is gone: the answer it demanded was always the bookmark the
// working copy already sat on, so ship appends to it and names it in the report.
func TestShipJJNonTrunkBookmarkAppends(t *testing.T) {
	log := setupShip(t, ".jj", false)
	t.Setenv("JJ_BOOKMARK_NAMES", "someone/probe")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch someone/probe · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), append(
		jjPlanArgv(),
		[]string{"jj", "diff", "--name-only"},
		[]string{"jj", "commit", "-m", "fix: frobnicate"},
		[]string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
		[]string{"jj", "bookmark", "move", vcs.JJExactPattern("someone/probe"), "--to", "@-"},
	))
}

// TestShipJJNonTrunkBookmarkPushesItself proves the appended bookmark is the one
// pushed, not trunk.
func TestShipJJNonTrunkBookmarkPushesItself(t *testing.T) {
	log := setupShip(t, ".jj", false)
	t.Setenv("JJ_BOOKMARK_NAMES", "someone/probe")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed someone/probe → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "jj" && inv[1] == "git" && inv[2] == "push" && inv[len(inv)-1] != vcs.JJExactPattern("someone/probe") {
			t.Errorf("push argv = %v, want it to target %s", inv, vcs.JJExactPattern("someone/probe"))
		}
	}
}

func TestShipJJMultipleNearestBookmarksFails(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("JJ_BOOKMARK_NAMES", "feat-a feat-b")

	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when several bookmarks are nearest, got nil")
	}
	if !strings.Contains(err.Error(), "multiple nearest bookmarks feat-a, feat-b (trunk main is not among them)") {
		t.Errorf("error = %v, want it to name every candidate and the trunk none of them is", err)
	}
	invocations := readInvocations(t, log)
	assertInvocations(t, invocations, [][]string{
		{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		{"jj", "--ignore-working-copy", "log", "-r", jjNearestBookmarkRevset, "--no-graph", "-T", jjBookmarkTemplate},
	})
	assertNoShipMutation(t, invocations)
}

func TestShipJJNearestBookmarksResolve(t *testing.T) {
	t.Run("trunk among the candidates wins, and the report says so", func(t *testing.T) {
		setupShip(t, ".jj", true)
		t.Setenv("JJ_BOOKMARK_NAMES", "feat-a main feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `bookmark main (trunk, chosen over feat-a, feat-b) · committed a1b2c3d "fix: frobnicate" · pushed main → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("--branch naming a candidate picks it", func(t *testing.T) {
		log := setupShip(t, ".jj", true)
		t.Setenv("JJ_BOOKMARK_NAMES", "feat-a feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "feat-b")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed feat-b → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		for _, inv := range readInvocations(t, log) {
			if inv[0] == "jj" && inv[1] == "git" && inv[2] == "push" && inv[len(inv)-1] != vcs.JJExactPattern("feat-b") {
				t.Errorf("push argv = %v, want it to target %s", inv, vcs.JJExactPattern("feat-b"))
			}
		}
	})

	t.Run("--branch naming a bookmark elsewhere retracts the trunk segment", func(t *testing.T) {
		setupShip(t, ".jj", true)
		t.Setenv("JJ_BOOKMARK_NAMES", "main feat-a")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "other")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed other → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
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
		log := setupShipColocated(t)
		other := filepath.Join(t.TempDir(), "wt")
		t.Setenv("JJ_BOOKMARK_NAMES", "main feat-a")
		t.Setenv("GIT_HOLDERS", "main "+other)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := fmt.Sprintf(`bookmark feat-a (chosen over main held in %s) · committed a1b2c3d "fix: frobnicate" · pushed feat-a → origin`, other)
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(t, log); n != 1 {
			t.Errorf("holder lookups = %d, want the one the tie needed", n)
		}
	})

	t.Run("no holder leaves the trunk alias to decide", func(t *testing.T) {
		log := setupShipColocated(t)
		t.Setenv("JJ_BOOKMARK_NAMES", "feat-a main feat-b")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `bookmark main (trunk, chosen over feat-a, feat-b) · committed a1b2c3d "fix: frobnicate" · pushed main → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(t, log); n != 1 {
			t.Errorf("holder lookups = %d, want the one the tie needed", n)
		}
	})

	t.Run("a lone candidate never asks who holds it", func(t *testing.T) {
		log := setupShipColocated(t)
		t.Setenv("GIT_HOLDERS", "main "+filepath.Join(t.TempDir(), "wt"))

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(t, log); n != 0 {
			t.Errorf("holder lookups = %d, want none when there is no tie to break", n)
		}
	})

	t.Run("--branch settles the tie before any holder is asked", func(t *testing.T) {
		log := setupShipColocated(t)
		t.Setenv("JJ_BOOKMARK_NAMES", "feat-a feat-b")
		t.Setenv("GIT_HOLDERS", "feat-b "+filepath.Join(t.TempDir(), "wt"))

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "feat-b")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed feat-b → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		if n := holderLookups(t, log); n != 0 {
			t.Errorf("holder lookups = %d, want none when the caller already chose", n)
		}
	})
}

// TestShipHealRefusedNamesHolder covers the guard on the self-heal's failure
// path: a heal git refuses because a sibling checkout has the branch out says
// which one, and a refusal no holder explains keeps the message it always had.
func TestShipHealRefusedNamesHolder(t *testing.T) {
	other := filepath.Join(t.TempDir(), "wt")
	tests := []struct {
		name    string
		holders string
		want    string
	}{
		{"a sibling checkout holds the branch", "feature " + other, "git checkout -B feature failed — that branch is checked out in " + other},
		{"nobody holds it", "", "git checkout -B feature failed: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			t.Setenv("GIT_DETACHED_AFTER_COMMIT", "1")
			t.Setenv("GIT_CHECKOUT_B_FAIL", other)
			t.Setenv("GIT_HOLDERS", tt.holders)

			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatal("expected an error when the heal checkout is refused, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if n := holderLookups(t, log); n != 1 {
				t.Errorf("holder lookups = %d, want the one the refusal needed", n)
			}
		})
	}
}

// TestShipHealSuccessAsksNoHolder proves the guard sits on the failure path
// alone: a heal that works spawns no holder lookup.
func TestShipHealSuccessAsksNoHolder(t *testing.T) {
	log := setupShipGT(t, false)
	t.Setenv("GIT_DETACHED_AFTER_COMMIT", "1")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch feature · healed detached HEAD onto feature · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if n := holderLookups(t, log); n != 0 {
		t.Errorf("holder lookups = %d, want none from a heal that succeeded", n)
	}
}

// TestShipJJAmbiguousTrunkFails proves several trunk candidates are refused,
// listing them: that is genuine ambiguity, not something to guess at.
func TestShipJJAmbiguousTrunkFails(t *testing.T) {
	t.Run("two real remotes refuse", func(t *testing.T) {
		log := setupShip(t, ".jj", true)
		t.Setenv("JJ_TRUNK_NAMES", "main dev")

		_, err := runShipCmd(t, "-m", "fix: frobnicate")
		if err == nil {
			t.Fatal("expected error when trunk is ambiguous, got nil")
		}
		if !strings.Contains(err.Error(), `cannot resolve the trunk bookmark from ["main" "dev"]`) {
			t.Errorf("error = %v, want it to list both candidates", err)
		}
		invocations := readInvocations(t, log)
		assertInvocations(t, invocations, [][]string{
			{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		})
		assertNoShipMutation(t, invocations)
	})

	t.Run("a local branch resting on trunk is not a candidate", func(t *testing.T) {
		setupShip(t, ".jj", true)
		t.Setenv("JJ_TRUNK_NAMES", "main feat-x")
		t.Setenv("JJ_TRUNK_NAMES_FILTERED", "main")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})
}

// TestShipJJAmbiguousTrunkBranch proves --branch settles several trunk
// candidates only by naming one of them, and that the name it picks becomes the
// trunk every guard downstream compares against.
func TestShipJJAmbiguousTrunkBranch(t *testing.T) {
	t.Run("a --branch naming no candidate still refuses", func(t *testing.T) {
		log := setupShip(t, ".jj", true)
		t.Setenv("JJ_TRUNK_NAMES", "main dev")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature")
		if err == nil || !strings.Contains(err.Error(), `cannot resolve the trunk bookmark from ["main" "dev"]`) {
			t.Fatalf("error = %v, want the trunk resolution refusal", err)
		}
		invocations := readInvocations(t, log)
		assertInvocations(t, invocations, [][]string{
			{"jj", "--ignore-working-copy", "log", "-r", "trunk()", "--no-graph", "-T", jjTrunkBookmarkTemplate},
		})
		assertNoShipMutation(t, invocations)
	})

	t.Run("the candidate it names is the trunk the guards weigh", func(t *testing.T) {
		log := setupShip(t, ".jj", true)
		t.Setenv("JJ_TRUNK_NAMES", "main dev")
		seedLaneRecords(t, ".", laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "main")
		if err == nil || !strings.Contains(err.Error(), "pass --allow-trunk to advance it deliberately") {
			t.Fatalf("error = %v, want the org-trunk refusal", err)
		}
		assertNoShipMutation(t, readInvocations(t, log))
	})

	t.Run("a trunk of your own it names is committed onto", func(t *testing.T) {
		setupShip(t, ".jj", true)
		t.Setenv("JJ_TRUNK_NAMES", "main dev")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--branch", "main", "--pr-title", "Better title")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin · no PR (on trunk)`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})
}

// TestShipJJNoTrunkBookmark proves an unnamed trunk stops mattering once the
// working copy sits on a bookmark: only a push with nothing at all to target
// still refuses.
func TestShipJJNoTrunkBookmark(t *testing.T) {
	t.Run("a nearest bookmark is pushed regardless", func(t *testing.T) {
		setupShip(t, ".jj", false)
		t.Setenv("JJ_TRUNK_NAMES", "")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · pushed main → origin`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("no bookmark at all still commits under --no-push", func(t *testing.T) {
		setupShip(t, ".jj", false)
		t.Setenv("JJ_TRUNK_NAMES", "")
		t.Setenv("JJ_NO_BOOKMARK", "1")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("no bookmark at all refuses a push", func(t *testing.T) {
		log := setupShip(t, ".jj", false)
		t.Setenv("JJ_TRUNK_NAMES", "")
		t.Setenv("JJ_NO_BOOKMARK", "1")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch")
		if err == nil || !strings.Contains(err.Error(), "cannot resolve the trunk bookmark") {
			t.Fatalf("error = %v, want the trunk resolution refusal", err)
		}
		assertNoShipMutation(t, readInvocations(t, log))
	})
}

func TestShipJJBookmarkOverride(t *testing.T) {
	log := setupShip(t, ".jj", true)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · pushed someone/probe → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
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
	log := setupShip(t, ".jj", false)
	t.Setenv("JJ_BOOKMARK_HEADS", "0")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-watch", "--bookmark", "someone/probe")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · created someone/probe · pushed someone/probe → origin`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}

	invocations := readInvocations(t, log)
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
	log := setupShip(t, ".git", true)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--bookmark", "main")
	if err == nil {
		t.Fatal("expected error for --bookmark in a git repo, got nil")
	}
	if !strings.Contains(err.Error(), "applies only to jj") {
		t.Errorf("error = %v, want it to say --bookmark applies only to jj", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run when --bookmark is rejected, got %v", inv)
	}
}

// TestShipBookmarkGuardReadsTheSpelling pins the guard to the flag the caller
// typed rather than the field it binds. --bookmark and --branch share o.branch,
// and the jj-only rule belongs to the --bookmark spelling alone; it also reads
// the detected lane kind, never the graphite lane's coercion to git, which is
// what once reported "applies only to jj repositories" inside a jj repository.
func TestShipBookmarkGuardReadsTheSpelling(t *testing.T) {
	t.Run("--bookmark refuses in a colocated jj graphite repo", func(t *testing.T) {
		log := setupShipGT(t, false)
		if err := os.Mkdir(".jj", 0o750); err != nil {
			t.Fatalf("mkdir .jj: %v", err)
		}
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--bookmark", "main")
		wantErr := "ship: --bookmark does not apply in the graphite lane; pass --no-gt to advance a jj bookmark instead"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoGTCommit(t, readInvocations(t, log))
	})

	t.Run("--branch carries no such restriction", func(t *testing.T) {
		log := setupShipGT(t, false)
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		assertGTCommit(t, readInvocations(t, log))
	})
}

func TestShipRequiresMessage(t *testing.T) {
	log := setupShip(t, ".jj", true)
	_, err := runShipCmd(t)
	if err == nil {
		t.Fatal("expected error when message missing, got nil")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to mention message required", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run when message is missing, got %v", inv)
	}
}

func TestShipNoRepoFails(t *testing.T) {
	log := setupShip(t, "", true)
	_, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error outside a repo, got nil")
	}
	if !strings.Contains(err.Error(), "no git or jj repository") {
		t.Errorf("error = %v, want it to mention no repository", err)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command should run outside a repo, got %v", inv)
	}
}

func TestShipCISuccessReportLine(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := "ci · success · 58s · https://github.com/x/actions/runs/42"
	if !strings.Contains(out, want) {
		t.Errorf("output missing run line %q\ngot:\n%s", want, out)
	}
}

func TestShipCIFailureDetail(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
	t.Setenv("GH_WATCH_EXIT", "1")
	t.Setenv("GH_LOG_FAILED", "test\tgo test ./...\t##[error]FAIL: TestFrobnicate (0.01s)\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error on CI failure, got nil")
	}
	for _, want := range []string{
		"· CI failure",
		"failed: test · go test ./...",
		"##[error]FAIL: TestFrobnicate",
		"full log: gh run view 42 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
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
			setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
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
		})
	}
}

func TestShipCIStripsANSI(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
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
}

func TestShipCITransientPollTolerated(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "fail-once")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("GH_LIST_FAIL_MARKER", marker)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("transient list error should be tolerated, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
	listCalls := 0
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 3 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "list" {
			listCalls++
		}
	}
	if listCalls < 2 {
		t.Errorf("expected the poll to retry (>=2 list calls), got %d", listCalls)
	}
}

func TestShipCIAllPollsFailStillReports(t *testing.T) {
	setupShip(t, ".jj", true)
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
	if want := "check: gh run list --commit " + fakeHeadSHA; !strings.Contains(out, want) {
		t.Errorf("output missing %q\ngot:\n%s", want, out)
	}
}

func TestShipCIViewFailureIsError(t *testing.T) {
	setupShip(t, ".jj", true)
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
}

func TestShipCIWatchErrViewGreenIsSuccess(t *testing.T) {
	setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
	t.Setenv("GH_WATCH_EXIT", "1") // watch drops, view says success — view wins
	shipCIPollInterval = 0

	got, err := runShipCmd(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("view-green run should heal a dropped watch, got %v", err)
	}
	if want := "· CI success"; !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to contain %q", got, want)
	}
}

func TestShipCIMultiRunWatchesAll(t *testing.T) {
	log := setupShip(t, ".jj", true)
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_43", `{"workflowName":"cc-notes","conclusion":"failure","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:05Z","url":"https://x/43","jobs":[{"name":"notes","conclusion":"failure","steps":[{"name":"sync","conclusion":"failure"}]}]}`)
	t.Setenv("GH_WATCH_EXIT_43", "1")
	t.Setenv("GH_LOG_FAILED_43", "notes sync exploded\n")
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err == nil {
		t.Fatal("expected error when one of several runs is red, got nil")
	}
	watched := map[string]bool{}
	for _, inv := range readInvocations(t, log) {
		if len(inv) >= 4 && inv[0] == "gh" && inv[1] == "run" && inv[2] == "watch" {
			watched[inv[3]] = true
		}
	}
	if !watched["42"] || !watched["43"] {
		t.Errorf("expected both runs watched, got %v", watched)
	}
	for _, want := range []string{
		"· CI failure",
		"ci · success",
		"cc-notes · failure",
		"failed: notes · sync",
		"full log: gh run view 43 --log-failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIMoreThanTenRunsWatchesAll(t *testing.T) {
	log := setupShip(t, ".jj", true)
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
	for _, inv := range readInvocations(t, log) {
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
}

func TestShipCISettleWatchesLateRuns(t *testing.T) {
	log := setupShip(t, ".jj", true)
	marker := filepath.Join(t.TempDir(), "settle")
	t.Setenv("GH_LIST_SETTLE_MARKER", marker)
	t.Setenv("GH_LIST_SETTLE_AFTER", "2")
	t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON) // first list: run 42 only
	t.Setenv("GH_RUN_LIST_JSON_2", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":44,"workflowName":"settle-late","status":"completed","url":"https://x/44"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewSuccess)
	t.Setenv("GH_RUN_VIEW_JSON_44", `{"workflowName":"settle-late","conclusion":"success","startedAt":"2026-07-08T18:00:00Z","updatedAt":"2026-07-08T18:00:10Z","url":"https://x/44","jobs":[]}`)
	shipCIPollInterval = 0

	out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	watched := map[string]bool{}
	listCalls := 0
	for _, inv := range readInvocations(t, log) {
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
	for _, want := range []string{"ci · success", "settle-late · success"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing settle report line %q\ngot:\n%s", want, out)
		}
	}
}

func TestShipCIBudgetFloorsPerRunShare(t *testing.T) {
	setupShip(t, ".jj", true)
	bigLog := strings.Repeat("a padded log line stretched to about fifty chars\n", 900) // ~44 KB
	t.Setenv("GH_RUN_LIST_JSON", `[`+
		`{"databaseId":42,"workflowName":"ci","status":"completed","url":"https://x/42"},`+
		`{"databaseId":43,"workflowName":"cc-notes","status":"completed","url":"https://x/43"}]`)
	t.Setenv("GH_RUN_VIEW_JSON_42", fakeRunViewFailure)
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
}

func TestShipCIEmptyConclusionIsIndeterminate(t *testing.T) {
	log := setupShip(t, ".jj", true)
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
	for _, inv := range readInvocations(t, log) {
		for _, a := range inv {
			if a == "--log-failed" {
				t.Errorf("indeterminate run must not fetch --log-failed, got %v", inv)
			}
		}
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
			log := setupShip(t, ".jj", true)
			t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
			shipCIPollInterval = 0

			old := shipStreamCI
			t.Cleanup(func() { shipStreamCI = old })
			shipStreamCI = func(io.Writer) bool { return tt.stream }

			_, errStr, err := runShipCmdFull(t, "-m", "fix: frobnicate")
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			compact := false
			for _, inv := range readInvocations(t, log) {
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
	t.Run("graphite wins over a colocated jj marker", func(t *testing.T) {
		log := setupShipGT(t, false)
		if err := os.Mkdir(".jj", 0o750); err != nil {
			t.Fatalf("mkdir .jj: %v", err)
		}
		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · branch feature · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, readInvocations(t, log), [][]string{
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
		log := setupShipGT(t, false)
		if err := os.Mkdir(".jj", 0o750); err != nil {
			t.Fatalf("mkdir .jj: %v", err)
		}
		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-gt")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, readInvocations(t, log), append(
			jjPlanArgv(),
			[]string{"jj", "diff", "--name-only"},
			[]string{"jj", "commit", "-m", "fix: frobnicate"},
			[]string{"jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", jjDescribeTemplate},
			[]string{"jj", "bookmark", "move", vcs.JJExactPattern("main"), "--to", "@-"},
		))
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
			t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
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
	t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
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
	setupShip(t, ".git", false)
	t.Setenv("GIT_TRUNK", "main")

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch main · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestShipTrunkOrgCreates(t *testing.T) {
	log := setupShip(t, ".git", false)
	t.Setenv("GIT_TRUNK", "main")
	seedLaneRecords(t, ".", laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · created fix-frobnicate · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "switch", "-c", "fix-frobnicate"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
}

// TestShipGitNewBranch proves --new-branch cuts the branch with git switch -c
// before the commit, so the commit lands on it rather than on trunk.
func TestShipGitNewBranch(t *testing.T) {
	log := setupShip(t, ".git", false)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · created feat-x · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "switch", "-c", "feat-x"},
		{"git", "add", "-A"},
		{"git", "commit", "-m", "fix: frobnicate"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
}

// TestShipGitNewBranchRollback proves a refusal after the branch cut leaves the
// working copy where it started: ship switches back, deletes the branch it cut,
// and still reports the failure that refused the ship.
func TestShipGitNewBranchRollback(t *testing.T) {
	log := setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	marker := filepath.Join(root, "prek.marker")
	if err := os.WriteFile(marker, []byte("2"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("UVX_PREK_FAIL_MARKER", marker)

	_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err == nil || !strings.Contains(err.Error(), "ship: hooks:") {
		t.Fatalf("ship error = %v, want the hook failure", err)
	}
	hookArgv := []string{"uvx", "prek", "run", "--cd", root, "--files", "f1.go"}
	assertInvocations(t, readInvocations(t, log), [][]string{
		{"git", "branch", "--show-current"},
		gitTrunkArgv,
		{"git", "switch", "-c", "feat-x"},
		{"git", "add", "-A"},
		{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z"},
		hookArgv,
		{"git", "add", "-A"},
		{"git", "diff", "--cached", "--name-only", "--diff-filter=d", "-z"},
		hookArgv,
		{"git", "switch", "main"},
		{"git", "branch", "-D", "feat-x"},
	})
}

// TestShipGitNewBranchRollbackFailure proves a rollback that cannot finish is
// reported alongside the refusal that triggered it, never instead of it.
func TestShipGitNewBranchRollbackFailure(t *testing.T) {
	setupShip(t, ".git", false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")
	marker := filepath.Join(root, "prek.marker")
	if err := os.WriteFile(marker, []byte("2"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Setenv("UVX_PREK_FAIL_MARKER", marker)
	t.Setenv("GIT_SWITCH_BACK_FAIL", "1")

	_, err = runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x")
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	for _, want := range []string{"ship: hooks:", "ship: rollback: git switch main", "the working copy is left on feat-x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
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
			log := setupShipGT(t, false)
			args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && (inv[1] == "create" || inv[1] == "modify") {
					commit = inv
				}
			}
			if !reflect.DeepEqual(commit, tt.want) {
				t.Errorf("commit argv = %v, want %v", commit, tt.want)
			}
		})
	}
}

func TestShipCreateExplicitEmpty(t *testing.T) {
	for _, flag := range []string{"--create=", "--new-branch="} {
		t.Run(flag, func(t *testing.T) {
			log := setupShipGT(t, false)
			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", flag)
			wantErr := "ship: " + strings.TrimSuffix(flag, "=") + " requires a branch name or no value"
			if err == nil || err.Error() != wantErr {
				t.Fatalf("error = %v, want %q", err, wantErr)
			}
			assertNoGTCommit(t, readInvocations(t, log))
		})
	}
}

// TestShipCreateSwallowsPathOperand covers the flag shape that silently
// committed a subset: cobra's NoOptDefVal never consumes the next token, so
// "--new-branch docs" filed docs as the only path to commit.
func TestShipCreateSwallowsPathOperand(t *testing.T) {
	for _, flag := range []string{"--new-branch", "--create"} {
		t.Run(flag, func(t *testing.T) {
			log := setupShipGT(t, false)
			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", flag, "docs")
			wantErr := `ship: "docs" is not a path — did you mean --new-branch=docs?`
			if err == nil || err.Error() != wantErr {
				t.Fatalf("error = %v, want %q", err, wantErr)
			}
			invocations := readInvocations(t, log)
			assertNoGTCommit(t, invocations)
			if invocations != nil {
				t.Errorf("no VCS command may run before the path-operand refusal, got %v", invocations)
			}
		})
	}

	t.Run("a real path is still a path", func(t *testing.T) {
		log := setupShipGT(t, false)
		if err := os.Mkdir("docs", 0o750); err != nil {
			t.Fatalf("mkdir docs: %v", err)
		}
		if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch", "docs"); err != nil {
			t.Fatalf("ship error = %v", err)
		}
		var add []string
		for _, inv := range readInvocations(t, log) {
			if inv[0] == "gt" && inv[1] == "add" {
				add = inv
			}
		}
		if want := []string{"gt", "add", "--no-interactive", "-A", "--", "docs"}; !reflect.DeepEqual(add, want) {
			t.Errorf("add argv = %v, want %v", add, want)
		}
	})
}

// TestShipIllegalBranchName proves an explicit branch name is refused by ship,
// before any lane work: only a derived name passes through legalBranchName
// otherwise, leaving the refusal to whichever backend's argv parser ran first.
func TestShipIllegalBranchName(t *testing.T) {
	for _, f := range []struct{ flag, canonical string }{
		{"--branch", "--branch"},
		{"--bookmark", "--branch"},
		{"--new-branch", "--new-branch"},
		{"--create", "--new-branch"},
	} {
		for _, name := range []string{"--force", "a..b", "x.lock", "feature/", "feature."} {
			t.Run(f.flag+"="+name, func(t *testing.T) {
				log := setupShip(t, ".git", false)

				_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", f.flag+"="+name)
				wantErr := fmt.Sprintf("ship: %s %q is not a legal branch name", f.canonical, name)
				if err == nil || err.Error() != wantErr {
					t.Fatalf("error = %v, want %q", err, wantErr)
				}
				invocations := readInvocations(t, log)
				assertNoShipMutation(t, invocations)
				if invocations != nil {
					t.Errorf("no VCS command may run before the illegal-name refusal, got %v", invocations)
				}
			})
		}
	}
}

// TestShipBranchFlag covers --branch's three outcomes: appending to the branch
// already checked out, cutting one that does not exist, and refusing to switch
// to one that exists elsewhere.
func TestShipBranchFlag(t *testing.T) {
	t.Run("naming the current branch appends", func(t *testing.T) {
		log := setupShip(t, ".git", false)
		t.Setenv("GIT_BRANCH", "feature")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "feature")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · branch feature · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		for _, inv := range readInvocations(t, log) {
			if inv[0] == "git" && inv[1] == "switch" {
				t.Errorf("git switch ran for a branch already checked out: %v", inv)
			}
		}
	})

	t.Run("naming an existing branch refuses", func(t *testing.T) {
		log := setupShip(t, ".git", false)
		t.Setenv("GIT_BRANCH", "feature")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "other")
		wantErr := "ship: branch other already exists — check it out first; ship does not switch branches mid-commit"
		if err == nil || err.Error() != wantErr {
			t.Fatalf("error = %v, want %q", err, wantErr)
		}
		assertNoShipMutation(t, readInvocations(t, log))
	})

	t.Run("naming a missing branch creates it here", func(t *testing.T) {
		setupShip(t, ".git", false)
		t.Setenv("GIT_BRANCH", "feature")
		t.Setenv("GIT_REMOTE_REF_MISSING", "1")

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "other")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `committed a1b2c3d "fix: frobnicate" · created other · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
	})

	t.Run("naming an org trunk refuses without --allow-trunk", func(t *testing.T) {
		log := setupShip(t, ".git", false)
		t.Setenv("GIT_BRANCH", "feature")
		t.Setenv("GIT_TRUNK", "main")
		seedLaneRecords(t, ".", laneSeed{nameWithOwner: "anthropics/claude-code", owner: "anthropics", public: true, permission: "WRITE", unaffiliated: true})

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--branch", "main")
		if err == nil || !strings.Contains(err.Error(), "pass --allow-trunk to advance it deliberately") {
			t.Fatalf("error = %v, want the org-trunk refusal", err)
		}
		assertNoShipMutation(t, readInvocations(t, log))
	})
}

// TestShipAppendFlag proves --append refuses on trunk, where the branch it
// would append to is the one ship exists to keep commits off.
func TestShipAppendFlag(t *testing.T) {
	log := setupShip(t, ".git", false)
	t.Setenv("GIT_TRUNK", "main")

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--append")
	wantErr := "ship: append would commit onto trunk — pass --new-branch"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("error = %v, want %q", err, wantErr)
	}
	assertNoShipMutation(t, readInvocations(t, log))
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
			log := setupShipGT(t, false)
			t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"base":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
			t.Setenv("GT_STATE_JSON_2", `{"main":{"trunk":true},"base":{"parents":[{"ref":"main","sha":"deadbeef"}]},`+
				`"feature":{"parents":[{"ref":"base","sha":"beadfeed"}]}}`)
			t.Setenv("GT_STATE_JSON_MARKER", filepath.Join(t.TempDir(), "gt-state"))

			got, err := runShipCmd(t, append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)...)
			if err != nil {
				t.Fatalf("ship error = %v", err)
			}
			if !strings.HasPrefix(got, tt.wantSeg+" · ") {
				t.Errorf("summary = %q, want it to lead with %q", got, tt.wantSeg)
			}
			var track []string
			for _, inv := range readInvocations(t, log) {
				if inv[0] == "gt" && inv[1] == "track" {
					track = inv
				}
			}
			if !reflect.DeepEqual(track, tt.wantTrack) {
				t.Errorf("track argv = %v, want %v", track, tt.wantTrack)
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
			log := setupShipGT(t, false)
			args := append(append([]string{}, tt.args...), "--no-push")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}
			var commit []string
			for _, inv := range readInvocations(t, log) {
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
		})
	}

	t.Run("amend on trunk refuses", func(t *testing.T) {
		log := setupShipGT(t, false)
		t.Setenv("GIT_BRANCH", "main")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		_, err := runShipCmd(t, "--amend", "-m", "fix: frobnicate", "--no-push")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: --amend on trunk is refused in the graphite lane — create a stacked branch instead (gt create)"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		assertNoGTCommit(t, readInvocations(t, log))
	})
}

func TestShipGTPathScoped(t *testing.T) {
	log := setupShipGT(t, false)
	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "src/a.go", "docs"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	assertInvocations(t, readInvocations(t, log), [][]string{
		nogtProbe,
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		{"gt", "add", "--no-interactive", "-A", "--", "src/a.go", "docs"},
		{"git", "diff", "--cached", "--quiet"},
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
}

func TestShipGTHunkScoped(t *testing.T) {
	log := setupShipGT(t, false)
	if err := os.WriteFile("f.txt", []byte(hunkCurrent), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write f.txt: %v", err)
	}
	t.Setenv("GIT_FILE_SHOW_BASE", hunkBase)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `hooks hunk-skip · committed a1b2c3d "fix: frobnicate" · branch feature · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	raw := readInvocations(t, log)
	var idxBasename string
	for _, inv := range raw {
		if inv[0] == "idx" {
			idxBasename = inv[1]
			break
		}
	}
	if idxBasename == "" {
		t.Fatal("no idx marker logged; gt hunk-scoped commit must run under a temp GIT_INDEX_FILE")
	}
	idxRec := []string{"idx", idxBasename}

	// The temp-index marker must gate exactly read-tree, update-index, and the
	// gt verb — the same throwaway-index technique the git lane uses — always
	// naming the same temp index.
	assertInvocations(t, raw, [][]string{
		nogtProbe,
		{"git", "rev-parse", "--show-toplevel"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "branch", "--show-current"},
		{"gt", "state"},
		idxRec,
		{"git", "read-tree", "HEAD"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "show", "--end-of-options", "HEAD:f.txt"},
		{"git", "ls-tree", "--full-tree", "-z", "--end-of-options", "HEAD", "--", "f.txt"},
		{"git", "hash-object", "-w", "--stdin"},
		idxRec,
		{"git", "update-index", "--add", "--cacheinfo", "100644,2222222222222222222222222222222222222222,f.txt"},
		idxRec,
		{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive"},
		{"git", "restore", "--staged", "--", "f.txt"},
		{"git", "branch", "--show-current"},
		{"git", "log", "-1", "--format=%h%x00%s"},
	})
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
	tests := []struct {
		name    string
		setup   func(t *testing.T)
		wantErr string
	}{
		{
			name: "needs restack",
			setup: func(t *testing.T) {
				t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true},"feature":{"needs_restack":true,"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
			},
			wantErr: "ship: stack needs restack — run gt restack (gt continue / gt abort on conflict), then re-run ship",
		},
		{
			name: "staged empty",
			setup: func(t *testing.T) {
				t.Setenv("GIT_STAGED_EMPTY", "1")
			},
			wantErr: `ship: nothing to commit — did a prior ship already land a1b2c3d "fix: frobnicate"?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupShipGT(t, false)
			tt.setup(t)
			_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
			assertNoGTCommit(t, readInvocations(t, log))
		})
	}

	t.Run("untracked branch auto-tracks", func(t *testing.T) {
		log := setupShipGT(t, false)
		marker := filepath.Join(t.TempDir(), "gt-state")
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		t.Setenv("GT_STATE_JSON_2", `{"main":{"trunk":true},"feature":{"parents":[{"ref":"main","sha":"deadbeef"}]}}`)
		t.Setenv("GT_STATE_JSON_MARKER", marker)

		got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		want := `tracked feature onto main · committed a1b2c3d "fix: frobnicate" · branch feature · not pushed`
		if got != want {
			t.Errorf("summary = %q, want %q", got, want)
		}
		assertInvocations(t, readInvocations(t, log), [][]string{
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

	t.Run("untracked branch auto-track fails", func(t *testing.T) {
		log := setupShipGT(t, false)
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		t.Setenv("GT_TRACK_FAIL", "1")

		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
		if err == nil {
			t.Fatal("expected refusal, got nil")
		}
		wantErr := "ship: branch feature is not tracked by graphite — run gt track, or pass --no-gt"
		if err.Error() != wantErr {
			t.Errorf("error = %q, want %q", err.Error(), wantErr)
		}
		assertInvocations(t, readInvocations(t, log), [][]string{
			nogtProbe,
			{"git", "branch", "--show-current"},
			{"gt", "state"},
			{"gt", "track", "-f", "--no-interactive"},
		})
		assertNoGTCommit(t, readInvocations(t, log))
	})

	// The refusal above replaces gt's sentence with the one step that fixes it,
	// which is exactly why gt's own words must reach the reader some other way:
	// on stderr as gt wrote them, and behind the advice as its cause.
	t.Run("an auto-track failure surfaces gt's error and keeps it as the cause", func(t *testing.T) {
		log := setupShipGT(t, false)
		t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)
		t.Setenv("GT_TRACK_FAIL", "1")
		line := gtErrorPrefix + "Cannot track feature: its parent base is not tracked."
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
		if !errors.As(err, &gtErr) {
			t.Errorf("errors.As reached no *gtError through %#v — the advice discarded gt's failure", err)
		}
		assertNoGTCommit(t, readInvocations(t, log))
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
	log := setupShip(t, ".git", false)
	wantErr := "ship: --parent applies only to graphite repos; pass --no-gt only when .git/.graphite_repo_config exists, or drop it"
	_, err := runShipCmd(t, "--parent", "base", "--no-push")
	if err == nil || err.Error() != wantErr {
		t.Errorf("error = %v, want %q", err, wantErr)
	}
	if inv := readInvocations(t, log); inv != nil {
		t.Errorf("no VCS command may run before the graphite-only flag check, got %v", inv)
	}
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
	log := setupShipGT(t, false)
	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	want := `committed a1b2c3d "fix: frobnicate" · branch feature · not pushed`
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	sawState, sawSubmit := false, false
	for _, inv := range readInvocations(t, log) {
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
}

func TestShipGTNoVerify(t *testing.T) {
	log := setupShipGT(t, false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
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
}

// TestShipGTHooksSuppressGitRun pins the gt lane's half of the single-run
// guarantee: ccx's own prek pass, then --no-verify so gt's commit does not
// fire the same hooks again through git.
func TestShipGTHooksSuppressGitRun(t *testing.T) {
	log := setupShipGT(t, false)
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeShipHookFiles(t, root, "f1.go")
	t.Setenv("GIT_DIFF_NAMES", "f1.go\n")

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var uvx, commit []string
	for _, inv := range readInvocations(t, log) {
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
	wantUVX := []string{"uvx", "prek", "run", "--cd", root, "--files", "f1.go"}
	if !reflect.DeepEqual(uvx, wantUVX) {
		t.Errorf("uvx argv = %v, want %v", uvx, wantUVX)
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate", "--no-interactive", "--no-verify"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
}

func TestShipGTSessionTrailer(t *testing.T) {
	log := setupShipGT(t, false)
	t.Setenv(envClaudeSessionKey, "some-uuid")
	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push"); err != nil {
		t.Fatalf("ship error = %v", err)
	}
	var commit []string
	for _, inv := range readInvocations(t, log) {
		if inv[0] == "gt" && inv[1] == "modify" {
			commit = inv
		}
	}
	want := []string{"gt", "modify", "-c", "-m", "fix: frobnicate\n\nClaude-Session-Id: some-uuid", "--no-interactive"}
	if !reflect.DeepEqual(commit, want) {
		t.Errorf("commit argv = %v, want %v", commit, want)
	}
}

func TestShipReviewsWiring(t *testing.T) {
	t.Run("--reviews requires push", func(t *testing.T) {
		log := setupShip(t, ".git", false)
		_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--reviews")
		wantErr := "ship: --reviews requires push (drop --no-push)"
		if err == nil || err.Error() != wantErr {
			t.Errorf("error = %v, want %q", err, wantErr)
		}
		if inv := readInvocations(t, log); inv != nil {
			t.Errorf("no VCS command may run before the --reviews/--no-push refusal, got %v", inv)
		}
	})

	t.Run("git lane with no open PR", func(t *testing.T) {
		setupShip(t, ".git", true)
		stubReviewsAPI(t)
		t.Setenv("GH_RUN_LIST_JSON", fakeRunListJSON)
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
		shipCIPollInterval = 0

		out, _, err := runShipCmdFull(t, "-m", "fix: frobnicate", "--reviews")
		if err != nil {
			t.Fatalf("ship error = %v", err)
		}
		summaryIdx := strings.Index(out, `committed a1b2c3d "fix: frobnicate" · pushed main → origin · CI success`)
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
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewSuccess)
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
		t.Setenv("GH_RUN_VIEW_JSON", fakeRunViewFailure)
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
