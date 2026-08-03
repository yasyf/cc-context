package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/anchor"
	"github.com/yasyf/cc-context/internal/hunk"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const (
	hunkBase    = "a\nb\nc\nd\ne\n"
	hunkCurrent = "A\nb\nc\nd\nE\n"
	// dupHunkBase/dupHunkCurrent yield two identical deletion hunks (same digest),
	// so a snapshot can carry a digest more times than pre-flight logged it.
	dupHunkBase    = "gone\na\ngone\n"
	dupHunkCurrent = "a\n"
)

// hunkRefFor renders the post-image ref (path:A-B#digest) for the i-th hunk
// between base and current, matching what ccx vcs hunks prints.
func hunkRefFor(t *testing.T, path, base, current string, i int) string {
	t.Helper()
	hunks := hunk.Compute([]byte(base), []byte(current))
	if i < 0 || i >= len(hunks) {
		t.Fatalf("hunk index %d out of range (%d hunks)", i, len(hunks))
	}
	return path + ":" + hunkRange(hunks[i]) + "#" + hunks[i].Digest.String()
}

// hunkRange renders a hunk's post-image line range as "A-B" (or "A").
func hunkRange(h hunk.Hunk) string {
	if h.NewEnd > h.NewStart {
		return fmt.Sprintf("%d-%d", h.NewStart, h.NewEnd)
	}
	return strconv.Itoa(h.NewStart)
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// gitHunkRepo stands up a real git repository whose HEAD carries base as f.txt
// and whose working copy carries current — the state a hunk-scoped ship selects
// from — plus an unrelated staged file that must ride through the throwaway-index
// commit untouched.
func gitHunkRepo(t *testing.T, base, current string) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t)
	writeRepoFile(t, f.Dir, "f.txt", base)
	writeRepoFile(t, f.Dir, "staged.txt", "staged\n")
	mustRun(t, f.Dir, "git", "add", "f.txt")
	mustRun(t, f.Dir, "git", "commit", "-qm", "base")
	mustRun(t, f.Dir, "git", "add", "staged.txt")
	writeRepoFile(t, f.Dir, "f.txt", current)
	return f
}

// jjHunkRepo is gitHunkRepo for a real colocated jj repository: @- carries base
// as f.txt, the working copy carries current, and CCX_TEST_APPLY_SELECTION arms
// TestMain so the diff tool jj spawns re-execs into ccx.
func jjHunkRepo(t *testing.T, base, current string) *vcstest.Fixture {
	t.Helper()
	f := vcstest.Repo(t, vcstest.JJ())
	t.Setenv("CCX_TEST_APPLY_SELECTION", "1")
	writeRepoFile(t, f.Dir, "f.txt", base)
	mustRun(t, f.Dir, "jj", "commit", "-m", "base")
	writeRepoFile(t, f.Dir, "f.txt", current)
	return f
}

// argvMark returns how many argv records the log already holds, the offset a
// later shipInvocations reads ship's own calls from.
func argvMark(t *testing.T, f *vcstest.Fixture) int {
	t.Helper()
	return len(vcstest.Invocations(t, f.ArgvLog))
}

// shipInvocations returns the argv records logged after mark, so a fixture's own
// construction calls never read as ship's.
func shipInvocations(t *testing.T, f *vcstest.Fixture, mark int) [][]string {
	t.Helper()
	inv := vcstest.Invocations(t, f.ArgvLog)
	if len(inv) < mark {
		t.Fatalf("argv log holds %d records, fewer than the %d logged at setup", len(inv), mark)
	}
	return inv[mark:]
}

// assertArgvOrder checks that each want prefix appears in got, in order, with any
// other calls interleaved — the narrowed shape an ordering gate needs, since only
// the relative order of these calls is invisible in the repository's final state.
func assertArgvOrder(t *testing.T, got, want [][]string) {
	t.Helper()
	i := 0
	for _, inv := range got {
		if i < len(want) && argvHasPrefix(inv, want[i]) {
			i++
		}
	}
	if i < len(want) {
		t.Errorf("argv order: matched %d of %d steps, stalled at %v\n got: %v", i, len(want), want[i], got)
	}
}

func argvHasPrefix(inv, prefix []string) bool {
	return len(inv) >= len(prefix) && slices.Equal(inv[:len(prefix)], prefix)
}

// gitHead returns dir's HEAD commit id, the value that must not move across a
// refused ship.
func gitHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, dir, "git", "rev-parse", "HEAD"))
}

// gitCommitCount returns how many commits HEAD carries.
func gitCommitCount(t *testing.T, dir string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(mustRun(t, dir, "git", "rev-list", "--count", "HEAD")))
	if err != nil {
		t.Fatalf("parse commit count: %v", err)
	}
	return n
}

// jjRevContent returns f.txt's content at rev.
func jjRevContent(t *testing.T, dir, rev string) string {
	t.Helper()
	return mustRun(t, dir, "jj", "file", "show", "-r", rev, "--", "f.txt")
}

// jjRevDescription returns rev's first description line.
func jjRevDescription(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(mustRun(t, dir, "jj", "log", "-r", rev, "--no-graph", "-T", "description.first_line()"))
}

// writeFailingPreCommitHook installs a native git pre-commit hook that always
// refuses, so a commit's hook gate is a real refusal rather than a modeled one.
func writeFailingPreCommitHook(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'pre-commit refuses' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // a git hook must be owner-executable to run
		t.Fatalf("write pre-commit hook: %v", err)
	}
}

func TestShipJJHunkSelection(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantCommitted   string
		wantDescription string
	}{
		{
			name:            "skip commits the complement",
			args:            []string{"-m", "fix: frobnicate", "--skip-hunk"},
			wantCommitted:   "a\nb\nc\nd\nE\n",
			wantDescription: "fix: frobnicate",
		},
		{
			name:            "only commits the named hunk",
			args:            []string{"-m", "fix: frobnicate", "--only-hunk"},
			wantCommitted:   "A\nb\nc\nd\ne\n",
			wantDescription: "fix: frobnicate",
		},
		{
			name:            "amend with message squashes into the base and redescribes it",
			args:            []string{"--amend", "-m", "fix: frobnicate", "--skip-hunk"},
			wantCommitted:   "a\nb\nc\nd\nE\n",
			wantDescription: "fix: frobnicate",
		},
		{
			name:            "amend without a message keeps the base description",
			args:            []string{"--amend", "--skip-hunk"},
			wantCommitted:   "a\nb\nc\nd\nE\n",
			wantDescription: "base",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := jjHunkRepo(t, hunkBase, hunkCurrent)
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			before := statOf(t, "f.txt")

			args := append(append([]string{}, tt.args...), ref, "--no-push", "f.txt")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}

			if got := jjRevContent(t, f.Dir, "@-"); got != tt.wantCommitted {
				t.Errorf("committed (@-) = %q, want %q", got, tt.wantCommitted)
			}
			if got := jjRevContent(t, f.Dir, "@"); got != hunkCurrent {
				t.Errorf("remainder (@) = %q, want %q (the excluded hunk stays in the working copy)", got, hunkCurrent)
			}
			if got := jjRevDescription(t, f.Dir, "@-"); got != tt.wantDescription {
				t.Errorf("@- description = %q, want %q", got, tt.wantDescription)
			}
			if got := readFileStr(t, "f.txt"); got != hunkCurrent {
				t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, hunkCurrent)
			}
			if after := statOf(t, "f.txt"); !after.equal(before) {
				t.Errorf("worktree stat changed: before=%+v after=%+v", before, after)
			}
			// The bookmark ship reported must be the one it moved: main sat a commit
			// behind @- until the ship landed it there.
			bookmarked := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", `bookmarks(exact:"main")`, "--no-graph", "-T", "commit_id"))
			at := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))
			if bookmarked != at {
				t.Errorf("bookmark main sits at %s, want the committed %s", bookmarked, at)
			}
		})
	}
}

func TestShipHunkHooksAreReportedSkipped(t *testing.T) {
	f := jjHunkRepo(t, hunkBase, hunkCurrent)
	writeShipHookFiles(t, f.Dir)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	// Any path that actually reached prek reports a different hook segment (or
	// fails outright, since the brew-free PATH carries no uvx), so the segment is
	// the proof the external hook run was skipped rather than attempted.
	sha := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id.short()"))
	want := fmt.Sprintf("hooks hunk-skip · committed %s %q · branch main · not pushed", sha, "fix: frobnicate")
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestShipHunkNoVerifySilencesHookSegment(t *testing.T) {
	f := jjHunkRepo(t, hunkBase, hunkCurrent)
	writeShipHookFiles(t, f.Dir)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)

	got, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--no-verify", "--only-hunk", ref, "f.txt")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	sha := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id.short()"))
	want := fmt.Sprintf("committed %s %q · branch main · not pushed", sha, "fix: frobnicate")
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// hunkRefusalCases are the selection refusals both lanes share: the two that
// resolve entirely from the flags must touch no repository at all, and none of
// them may leave a commit behind.
type hunkRefusalCase struct {
	name    string
	args    []string
	wantErr string
	// wantCalls is the exact number of VCS invocations the refusal may make; -1
	// leaves the count open and asserts only that nothing mutated.
	wantCalls int
}

func hunkRefusalCases(t *testing.T) []hunkRefusalCase {
	t.Helper()
	ref0 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	ref1 := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 1)
	driftRef := "f.txt:1#" + hunk.Compute([]byte("x\n"), []byte("Y\n"))[0].Digest.String()
	return []hunkRefusalCase{
		{
			name:      "mutually exclusive flags",
			args:      []string{"--skip-hunk", ref0, "--only-hunk", ref1, "f.txt"},
			wantErr:   "ship: --skip-hunk and --only-hunk cannot be combined",
			wantCalls: 0,
		},
		{
			name:      "malformed ref",
			args:      []string{"--skip-hunk", "not-a-ref", "f.txt"},
			wantErr:   `ship: invalid hunk ref "not-a-ref" (expected file:A-B#hash, from ccx vcs hunks)`,
			wantCalls: 0,
		},
		{
			name:      "ref outside shipped paths",
			args:      []string{"--skip-hunk", ref0, "other.txt"},
			wantErr:   "is outside the shipped paths",
			wantCalls: 1,
		},
		{
			name:      "drift",
			args:      []string{"--skip-hunk", driftRef, "f.txt"},
			wantErr:   "the diff changed since listing; re-run: ccx vcs hunks f.txt",
			wantCalls: -1,
		},
		{
			name:      "all excluded",
			args:      []string{"--skip-hunk", ref0, "--skip-hunk", ref1, "f.txt"},
			wantErr:   "ship: all changes excluded in f.txt; drop the file from the ship instead",
			wantCalls: -1,
		},
	}
}

// assertHunkRefusal runs the refusal and checks the shared claims: the error, the
// call budget, and that nothing in the log mutated the repository.
func assertHunkRefusal(t *testing.T, f *vcstest.Fixture, mark int, tt hunkRefusalCase) {
	t.Helper()
	args := append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.args...)
	_, err := runShipCmd(t, args...)
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !strings.Contains(err.Error(), tt.wantErr) {
		t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
	}
	inv := shipInvocations(t, f, mark)
	if tt.wantCalls >= 0 && len(inv) != tt.wantCalls {
		t.Errorf("ran %d VCS commands, want %d: %v", len(inv), tt.wantCalls, inv)
	}
	assertNoShipMutation(t, inv)
}

func TestShipJJHunkRefusals(t *testing.T) {
	for _, tt := range hunkRefusalCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			f := jjHunkRepo(t, hunkBase, hunkCurrent)
			head := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id"))

			assertHunkRefusal(t, f, argvMark(t, f), tt)

			if got := strings.TrimSpace(mustRun(t, f.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id")); got != head {
				t.Errorf("@- moved to %s, want the pre-ship %s", got, head)
			}
			if got := jjRevContent(t, f.Dir, "@"); got != hunkCurrent {
				t.Errorf("working copy = %q, want the untouched %q", got, hunkCurrent)
			}
		})
	}
}

func TestShipGitHunkRefusals(t *testing.T) {
	for _, tt := range hunkRefusalCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			f := gitHunkRepo(t, hunkBase, hunkCurrent)
			head := gitHead(t, f.Dir)

			assertHunkRefusal(t, f, argvMark(t, f), tt)

			if got := gitHead(t, f.Dir); got != head {
				t.Errorf("HEAD moved to %s, want the pre-ship %s", got, head)
			}
			if got := readFileStr(t, "f.txt"); got != hunkCurrent {
				t.Errorf("working copy = %q, want the untouched %q", got, hunkCurrent)
			}
		})
	}
}

// TestShipGitHunkTempIndexIsolation proves the throwaway-index technique end to
// end: every index-mutating call has to run against the temp index and the commit
// has to carry no pathspec, or the pre-staged sibling and the excluded hunk would
// both land in the commit. The ordering is asserted separately because a restore
// that ran before the commit would resync the real index to the old HEAD and leave
// no trace of having done so.
func TestShipGitHunkTempIndexIsolation(t *testing.T) {
	f := gitHunkRepo(t, hunkBase, hunkCurrent)
	writeRepoFile(t, f.Dir, "g.txt", "whole\n")
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	before := statOf(t, "f.txt")
	mark := argvMark(t, f)

	if _, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--skip-hunk", ref, "f.txt", "g.txt"); err != nil {
		t.Fatalf("ship error = %v", err)
	}

	const wantCommitted = "a\nb\nc\nd\nE\n"
	if got := mustRun(t, f.Dir, "git", "show", "HEAD:f.txt"); got != wantCommitted {
		t.Errorf("committed (HEAD:f.txt) = %q, want %q", got, wantCommitted)
	}
	if got := mustRun(t, f.Dir, "git", "show", "HEAD:g.txt"); got != "whole\n" {
		t.Errorf("committed (HEAD:g.txt) = %q, want the whole-shipped sibling", got)
	}
	if out, err := runGit(f.Dir, "show", "HEAD:staged.txt"); err == nil {
		t.Errorf("the pre-staged sibling was swept into the commit: %s", out)
	}
	status := statusSet(t, f.Dir)
	want := map[string]bool{"A  staged.txt": true, " M f.txt": true}
	if !mapEqual(status, want) {
		t.Errorf("git status --porcelain = %v, want %v", status, want)
	}
	if got := readFileStr(t, "f.txt"); got != hunkCurrent {
		t.Errorf("worktree f.txt = %q, want %q (byte-identical to pre-ship)", got, hunkCurrent)
	}
	if after := statOf(t, "f.txt"); !after.equal(before) {
		t.Errorf("worktree stat changed: before=%+v after=%+v", before, after)
	}

	inv := shipInvocations(t, f, mark)
	assertArgvOrder(t, inv, [][]string{
		{"git", "read-tree"},
		{"git", "add"},
		{"git", "hash-object"},
		{"git", "update-index"},
		{"git", "commit"},
		{"git", "restore", "--staged"},
	})
	for _, rec := range inv {
		if !argvHasPrefix(rec, []string{"git", "commit"}) {
			continue
		}
		if slices.Contains(rec, "--") {
			t.Errorf("temp-index commit carried a pathspec: %v", rec)
		}
	}
}

// TestShipGitHunkNewBranchRollback covers the rollback on the selection path,
// which reaches the commit through several more failure points than a whole-file
// ship: a refusing pre-commit hook there restores the branch just as a hook
// failure does on the whole-file path.
func TestShipGitHunkNewBranchRollback(t *testing.T) {
	f := gitHunkRepo(t, hunkBase, hunkCurrent)
	writeFailingPreCommitHook(t, f.Dir)
	ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
	head := gitHead(t, f.Dir)
	mark := argvMark(t, f)

	_, err := runShipCmd(t, "-m", "fix: frobnicate", "--no-push", "--new-branch=feat-x", "--only-hunk", ref, "f.txt")
	if err == nil || !strings.Contains(err.Error(), "ship: git commit:") {
		t.Fatalf("ship error = %v, want the temp-index commit failure", err)
	}

	if got := strings.TrimSpace(mustRun(t, f.Dir, "git", "branch", "--show-current")); got != "main" {
		t.Errorf("checked out %q after the refusal, want main", got)
	}
	if out, err := runGit(f.Dir, "rev-parse", "--verify", "refs/heads/feat-x"); err == nil {
		t.Errorf("feat-x survived the refusal: %s", out)
	}
	if got := gitHead(t, f.Dir); got != head {
		t.Errorf("HEAD moved to %s, want the pre-ship %s", got, head)
	}
	assertArgvOrder(t, shipInvocations(t, f, mark), [][]string{
		{"git", "switch", "-c", "feat-x"},
		{"git", "commit"},
		{"git", "switch", "main"},
		{"git", "branch", "-D", "feat-x"},
	})
}

// TestShipGitHunkNoVerify pins which hook gate the temp-index commit runs under:
// a native git hook is the repository's own and must keep refusing, a prek config
// is ccx's external gate and must not silence it, and --no-verify is the one flag
// that reaches git's own --no-verify.
func TestShipGitHunkNoVerify(t *testing.T) {
	tests := []struct {
		name       string
		hookConfig bool
		noVerify   bool
		wantCommit bool
	}{
		{"default preserves native hooks", false, false, false},
		{"prek config preserves native hooks", true, false, false},
		{"no verify reaches temp-index commit", false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := gitHunkRepo(t, hunkBase, hunkCurrent)
			writeFailingPreCommitHook(t, f.Dir)
			if tt.hookConfig {
				writeShipHookFiles(t, f.Dir)
			}
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			before := gitCommitCount(t, f.Dir)

			args := []string{"-m", "fix: frobnicate", "--no-push", "--only-hunk", ref, "f.txt"}
			if tt.noVerify {
				args = append(args, "--no-verify")
			}
			_, err := runShipCmd(t, args...)
			want := before
			if tt.wantCommit {
				want++
				if err != nil {
					t.Fatalf("ship error = %v, want --no-verify to bypass the hook", err)
				}
			} else if err == nil {
				t.Fatal("expected the refusing pre-commit hook to fail the ship, got nil")
			}
			if got := gitCommitCount(t, f.Dir); got != want {
				t.Errorf("commit count = %d, want %d", got, want)
			}
		})
	}
}

func TestShipGitHunkAmend(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantSubject string
	}{
		{"amend with message", []string{"--amend", "-m", "fix: frobnicate"}, "fix: frobnicate"},
		{"amend no message", []string{"--amend"}, "base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := gitHunkRepo(t, hunkBase, hunkCurrent)
			ref := hunkRefFor(t, "f.txt", hunkBase, hunkCurrent, 0)
			before := gitCommitCount(t, f.Dir)
			mark := argvMark(t, f)

			args := append(append([]string{}, tt.args...), "--no-push", "--skip-hunk", ref, "f.txt")
			if _, err := runShipCmd(t, args...); err != nil {
				t.Fatalf("ship error = %v", err)
			}

			if got := gitCommitCount(t, f.Dir); got != before {
				t.Errorf("commit count = %d, want the unchanged %d (an amend rewrites, never appends)", got, before)
			}
			if got := strings.TrimSpace(mustRun(t, f.Dir, "git", "log", "-1", "--format=%s")); got != tt.wantSubject {
				t.Errorf("HEAD subject = %q, want %q", got, tt.wantSubject)
			}
			const wantCommitted = "a\nb\nc\nd\nE\n"
			if got := mustRun(t, f.Dir, "git", "show", "HEAD:f.txt"); got != wantCommitted {
				t.Errorf("committed (HEAD:f.txt) = %q, want %q", got, wantCommitted)
			}
			// With only a hunk-scoped path, no whole file is staged, so no add runs.
			for _, rec := range shipInvocations(t, f, mark) {
				if argvHasPrefix(rec, []string{"git", "add"}) {
					t.Errorf("a sole hunk-scoped ship must run no git add, got %v", rec)
				}
			}
		})
	}
}

// writeTempPlan marshals plan into a tempfile and returns its path.
func writeTempPlan(t *testing.T, plan selectionPlan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func TestApplySelectionRewritesRight(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		base      string // "" writes no left file (new file, empty base)
		current   string
		mode      string
		hunkIdx   int
		wantRight string
		rightPerm os.FileMode
		wantLeft  string // expected left content afterwards (unchanged); "" = no left file
	}{
		{
			name:      "skip keeps the complement",
			base:      hunkBase,
			current:   hunkCurrent,
			mode:      "skip",
			hunkIdx:   0,
			wantRight: "a\nb\nc\nd\nE\n",
			rightPerm: 0o755,
			wantLeft:  hunkBase,
		},
		{
			name:      "only keeps the named hunk",
			base:      hunkBase,
			current:   hunkCurrent,
			mode:      "only",
			hunkIdx:   0,
			wantRight: "A\nb\nc\nd\ne\n",
			rightPerm: 0o644,
			wantLeft:  hunkBase,
		},
		{
			name:      "missing left is a new file",
			base:      "",
			current:   "new\ncontent\n",
			mode:      "only",
			hunkIdx:   0,
			wantRight: "new\ncontent\n",
			rightPerm: 0o644,
			wantLeft:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftDir := t.TempDir()
			rightDir := t.TempDir()
			leftPath := filepath.Join(leftDir, "f.txt")
			if tt.base != "" {
				if err := os.WriteFile(leftPath, []byte(tt.base), 0o644); err != nil { //nolint:gosec // test fixture
					t.Fatalf("write left: %v", err)
				}
			}
			rightPath := filepath.Join(rightDir, "f.txt")
			if err := os.WriteFile(rightPath, []byte(tt.current), tt.rightPerm); err != nil { //nolint:gosec // test asserts this perm survives
				t.Fatalf("write right: %v", err)
			}

			hunks := hunk.Compute([]byte(tt.base), []byte(tt.current))
			plan := selectionPlan{
				Files: map[string]planFile{
					"f.txt": {Mode: tt.mode, Hunks: []planHunk{{Range: hunkRange(hunks[tt.hunkIdx]), Digest: hunks[tt.hunkIdx].Digest.String()}}, KnownDigests: hunkDigestCounts(hunks)},
				},
				Result: filepath.Join(t.TempDir(), "sidecar"),
			}
			if err := runApplySelection(writeTempPlan(t, plan), leftDir, rightDir); err != nil {
				t.Fatalf("apply-selection error = %v", err)
			}

			got, err := os.ReadFile(rightPath) //nolint:gosec // test path
			if err != nil {
				t.Fatalf("read right: %v", err)
			}
			if string(got) != tt.wantRight {
				t.Errorf("right = %q, want %q", got, tt.wantRight)
			}
			info, err := os.Stat(rightPath)
			if err != nil {
				t.Fatalf("stat right: %v", err)
			}
			if info.Mode().Perm() != tt.rightPerm {
				t.Errorf("right perm = %v, want %v (mode must be preserved)", info.Mode().Perm(), tt.rightPerm)
			}
			left, err := os.ReadFile(leftPath) //nolint:gosec // test path
			switch {
			case tt.wantLeft == "":
				if !os.IsNotExist(err) {
					t.Errorf("left must stay absent, got err=%v content=%q", err, left)
				}
			case err != nil:
				t.Fatalf("read left: %v", err)
			case string(left) != tt.wantLeft:
				t.Errorf("left changed to %q, want %q (left is read-only)", left, tt.wantLeft)
			}
		})
	}
}

func TestApplySelectionFailureWritesSidecar(t *testing.T) {
	t.Parallel()
	driftHash := hunk.Compute([]byte("x\n"), []byte("Y\n"))[0].Digest
	changeHunks := hunk.Compute([]byte(hunkBase), []byte(hunkCurrent))
	foreignRef := hunkListRef("f.txt", changeHunks[1])
	dupHunks := hunk.Compute([]byte(dupHunkBase), []byte(dupHunkCurrent))
	dupForeignRef := hunkListRef("f.txt", dupHunks[1])

	tests := []struct {
		name       string
		base       string
		current    string
		mode       string
		planHunks  []planHunk
		known      map[string]int
		wantReason string
	}{
		{
			name:       "drift",
			base:       hunkBase,
			current:    hunkCurrent,
			mode:       "skip",
			planHunks:  []planHunk{{Range: "1", Digest: driftHash.String()}},
			wantReason: "the diff changed since listing; re-run: ccx vcs hunks f.txt",
		},
		{
			name:       "empty keep",
			base:       "a\n",
			current:    "A\n",
			mode:       "skip",
			planHunks:  nil, // filled from the single computed hunk below
			wantReason: "all changes excluded in f.txt; drop the file from the ship instead",
		},
		{
			// Skip hunk 0; hunk 1's digest is absent from the pre-flight set, so it
			// is a foreign change skip mode must refuse rather than sweep in.
			name:       "foreign hunk swept by skip",
			base:       hunkBase,
			current:    hunkCurrent,
			mode:       "skip",
			planHunks:  []planHunk{{Range: hunkRange(changeHunks[0]), Digest: changeHunks[0].Digest.String()}},
			known:      map[string]int{changeHunks[0].Digest.String(): 1},
			wantReason: "foreign hunk(s) appeared in f.txt since listing: " + foreignRef,
		},
		{
			// The snapshot carries the same deletion twice while pre-flight logged its
			// digest once: skip mode names hunk 0, and the identical second hunk is a
			// foreign duplicate that a digest set would mask but digest counts catch.
			name:       "duplicate foreign hunk swept by skip",
			base:       dupHunkBase,
			current:    dupHunkCurrent,
			mode:       "skip",
			planHunks:  []planHunk{{Range: hunkRange(dupHunks[0]), Digest: dupHunks[0].Digest.String()}},
			known:      map[string]int{dupHunks[0].Digest.String(): 1},
			wantReason: "foreign hunk(s) appeared in f.txt since listing: " + dupForeignRef,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leftDir := t.TempDir()
			rightDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(leftDir, "f.txt"), []byte(tt.base), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write left: %v", err)
			}
			if err := os.WriteFile(filepath.Join(rightDir, "f.txt"), []byte(tt.current), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatalf("write right: %v", err)
			}
			hunks := tt.planHunks
			if hunks == nil {
				h := hunk.Compute([]byte(tt.base), []byte(tt.current))[0]
				hunks = []planHunk{{Range: hunkRange(h), Digest: h.Digest.String()}}
			}
			sidecar := filepath.Join(t.TempDir(), "sidecar")
			plan := selectionPlan{
				Files:  map[string]planFile{"f.txt": {Mode: tt.mode, Hunks: hunks, KnownDigests: tt.known}},
				Result: sidecar,
			}
			err := runApplySelection(writeTempPlan(t, plan), leftDir, rightDir)
			if err == nil {
				t.Fatal("expected apply-selection to fail, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantReason)
			}
			reason, rerr := os.ReadFile(sidecar) //nolint:gosec // test path
			if rerr != nil {
				t.Fatalf("read sidecar: %v", rerr)
			}
			if !strings.Contains(string(reason), tt.wantReason) {
				t.Errorf("sidecar = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

// TestHunkRefResolvesDuplicateDeletions checks identical deletions list as
// distinct refs and each freshly-listed ref resolves to its own hunk.
func TestHunkRefResolvesDuplicateDeletions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		base    string
		current string
	}{
		{"identical adjacent deletions", "gone\na\ngone\n", "a\n"},
		{"interleaved identical deletions", "a\ngone\nb\nc\ngone\nd\n", "a\nb\nc\nd\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hunks := hunk.Compute([]byte(tt.base), []byte(tt.current))
			if len(hunks) != 2 || hunks[0].Digest != hunks[1].Digest {
				t.Fatalf("fixture must yield 2 same-digest hunks, got %d: %+v", len(hunks), hunks)
			}
			seen := map[string]bool{}
			for i := range hunks {
				refStr := hunkListRef("f.txt", hunks[i])
				if seen[refStr] {
					t.Fatalf("hunk %d re-lists an already-listed ref %q — identical deletions must get distinct lines", i, refStr)
				}
				seen[refStr] = true
				_, ref, err := hunk.ParseRef(refStr)
				if err != nil {
					t.Fatalf("ParseRef(%q): %v", refStr, err)
				}
				idx, err := matchHunkRef("f.txt", hunks, ref)
				if err != nil {
					t.Fatalf("matchHunkRef(%q): %v", refStr, err)
				}
				if idx != i {
					t.Errorf("freshly-listed ref %q for hunk %d resolved to hunk %d", refStr, i, idx)
				}
			}
		})
	}
}

// TestMatchHunkRefStaleDuplicateRefused checks a duplicate-digest ref whose line
// matches no hunk exactly is refused as drift, not silently nearest-matched.
func TestMatchHunkRefStaleDuplicateRefused(t *testing.T) {
	t.Parallel()
	base := "a\nx\nb\nc\nd\nx\ne\n"
	current := "a\nb\nc\nd\ne\n"
	hunks := hunk.Compute([]byte(base), []byte(current))
	if len(hunks) != 2 || hunks[0].Digest != hunks[1].Digest {
		t.Fatalf("fixture must yield 2 same-digest deletions, got %d: %+v", len(hunks), hunks)
	}
	// hunks sit at post-image lines 2 and 5; line 3 is nearest (non-tie) to hunk 0
	// yet matches neither exactly — a stale ref that must be refused, not mis-picked.
	stale := anchor.Ref{Line: 3, Hash: hunks[0].Digest}
	if _, err := matchHunkRef("f.txt", hunks, stale); err == nil {
		t.Fatal("a stale duplicate-digest ref must be refused, not silently nearest-matched")
	} else if !strings.Contains(err.Error(), "re-run: ccx vcs hunks f.txt") {
		t.Errorf("error = %q, want the drift/re-list wording", err)
	}
}

// TestShowFileBaseDistinguishesAbsentFromFailure checks a file absent from the
// base is an empty base while an unresolvable base tree propagates the failure.
func TestShowFileBaseDistinguishesAbsentFromFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("new file in a committed repo is an empty base", func(t *testing.T) {
		dir := initCliGitRepo(t)
		commitFile(t, dir, "tracked.txt", "x\n")
		if err := os.WriteFile("untracked.txt", []byte("new\n"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write untracked: %v", err)
		}
		base, err := showFileBase(ctx, vcs.Git, "untracked.txt")
		if err != nil {
			t.Fatalf("a new file must yield an empty base, got err %v", err)
		}
		if len(base) != 0 {
			t.Errorf("new-file base = %q, want empty", base)
		}
	})
	t.Run("unresolvable base tree propagates", func(t *testing.T) {
		initCliGitRepo(t)                                                   // git init with no commit: HEAD is unborn
		if err := os.WriteFile("f.txt", []byte("x\n"), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write f.txt: %v", err)
		}
		if _, err := showFileBase(ctx, vcs.Git, "f.txt"); err == nil {
			t.Fatal("an unresolvable base tree must propagate, not swallow into an empty base")
		}
	})
}

// TestFileInBaseJJWhitespaceName pins the one reading of jj file list that is
// not a guess: measured against jj 0.43.0, a path the base carries prints its
// name back verbatim (a file named " " prints " \n") and a path it lacks prints
// zero bytes, the warning going to stderr. Trimming the answer collapses the
// first case into the second, and a base read as absent diffs the file's hunks
// against nothing.
func TestFileInBaseJJWhitespaceName(t *testing.T) {
	f := vcstest.Repo(t, vcstest.JJ())
	const spaceName = " "
	writeRepoFile(t, f.Dir, spaceName, "x\n")
	mustRun(t, f.Dir, "jj", "commit", "-m", "space-named file")

	ctx := context.Background()
	present, err := fileInBase(ctx, vcs.JJ, spaceName)
	if err != nil {
		t.Fatalf("fileInBase(%q) error = %v", spaceName, err)
	}
	if !present {
		t.Errorf("fileInBase(%q) = false, want true — the base carries it", spaceName)
	}
	absent, err := fileInBase(ctx, vcs.JJ, "nope.txt")
	if err != nil {
		t.Fatalf("fileInBase(nope.txt) error = %v", err)
	}
	if absent {
		t.Error("fileInBase(nope.txt) = true, want false")
	}
}

// TestVcsHunksListsNamesGitEscapes pins the listing to real git's own encoding of
// a path: under the default core.quotePath a zero-width joiner and a double quote
// come back C-quoted while a leading space comes back bare, so a listing that
// unquoted neither — or trimmed the second — would drop the file entirely instead
// of handing back a ref ship can address.
func TestVcsHunksListsNamesGitEscapes(t *testing.T) {
	const quoted = "we\u200did\"name.txt"
	const spaced = " lead.txt"
	f := vcstest.Repo(t)
	for _, name := range []string{quoted, spaced} {
		writeRepoFile(t, f.Dir, name, hunkBase)
	}
	mustRun(t, f.Dir, "git", "add", "-A")
	mustRun(t, f.Dir, "git", "commit", "-qm", "base")
	for _, name := range []string{quoted, spaced} {
		writeRepoFile(t, f.Dir, name, hunkCurrent)
	}

	listed := map[string][]string{}
	for _, line := range strings.Split(strings.TrimRight(runHunksCmd(t), "\n"), "\n") {
		if line == "" {
			continue
		}
		ref := strings.SplitN(line, "\t", 2)[0]
		path, _, err := hunk.ParseRef(ref)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", ref, err)
		}
		listed[path] = append(listed[path], ref)
	}
	for _, name := range []string{quoted, spaced} {
		if len(listed[name]) != 2 {
			t.Fatalf("ccx vcs hunks listed %d refs for %q, want 2: %v", len(listed[name]), name, listed)
		}
	}

	if _, err := runShipCmd(t, "-m", "partial ship", "--no-push", "--only-hunk", listed[quoted][0], quoted); err != nil {
		t.Fatalf("ship error = %v, want the listed ref to address the file", err)
	}
	const wantCommitted = "A\nb\nc\nd\ne\n"
	if got := mustRun(t, f.Dir, "git", "show", "HEAD:"+quoted); got != wantCommitted {
		t.Errorf("committed (HEAD:%q) = %q, want %q", quoted, got, wantCommitted)
	}
}

// TestApplySelectionRefRoundTrip guards that the ref ccx vcs hunks emits
// (hunkRef) re-parses to the same anchor through the plan file, so listing and
// applying agree; the fixture carries a pure deletion so its post-image anchor is
// exercised too.
func TestApplySelectionRefRoundTrip(t *testing.T) {
	t.Parallel()
	// a->A change (hunk 0) plus a pure deletion of "c" (hunk 1).
	hunks := hunk.Compute([]byte("a\nb\nc\nd\ne\n"), []byte("A\nb\nd\ne\n"))
	if len(hunks) != 2 || len(hunks[1].New) != 0 {
		t.Fatalf("fixture must yield a change then a pure deletion, got %+v", hunks)
	}
	for i := range hunks {
		ref := hunkRef(hunks[i])
		refs, err := planRefs([]planHunk{{Range: refRange(ref), Digest: ref.Hash.String()}})
		if err != nil {
			t.Fatalf("planRefs: %v", err)
		}
		idx, err := matchHunkRef("f.txt", hunks, refs[0])
		if err != nil {
			t.Fatalf("matchHunkRef: %v", err)
		}
		if idx != i {
			t.Errorf("ref for hunk %d matched hunk %d", i, idx)
		}
	}
}

// TestRefuseForeignHunks checks the shared skip-mode guard both commit lanes call:
// a snapshot hunk absent at listing time is named and refused in skip mode, a
// digest carried more times than pre-flight logged it is caught by count (not
// masked by the original), every hunk is named when the whole set is foreign, and
// only mode is never guarded (its foreign hunks stay uncommitted by construction).
func TestRefuseForeignHunks(t *testing.T) {
	t.Parallel()
	changeHunks := hunk.Compute([]byte(hunkBase), []byte(hunkCurrent))
	if len(changeHunks) != 2 {
		t.Fatalf("fixture must yield 2 hunks, got %d", len(changeHunks))
	}
	dupHunks := hunk.Compute([]byte(dupHunkBase), []byte(dupHunkCurrent))
	if len(dupHunks) != 2 || dupHunks[0].Digest != dupHunks[1].Digest {
		t.Fatalf("dup fixture must yield 2 same-digest hunks, got %d: %+v", len(dupHunks), dupHunks)
	}
	ref0 := hunkListRef("f.txt", changeHunks[0])
	ref1 := hunkListRef("f.txt", changeHunks[1])
	dupRef1 := hunkListRef("f.txt", dupHunks[1])
	firstOnly := map[string]int{changeHunks[0].Digest.String(): 1}

	tests := []struct {
		name     string
		mode     selectMode
		hunks    []hunk.Hunk
		known    map[string]int
		wantErr  bool
		wantRefs []string
	}{
		{"skip clean passes", selectSkip, changeHunks, hunkDigestCounts(changeHunks), false, nil},
		{"skip foreign refuses names it", selectSkip, changeHunks, firstOnly, true, []string{ref1}},
		{"skip all-foreign names every hunk", selectSkip, changeHunks, map[string]int{}, true, []string{ref0, ref1}},
		{"only foreign never guarded", selectOnly, changeHunks, firstOnly, false, nil},
		{"skip duplicate over count refuses the extra", selectSkip, dupHunks, map[string]int{dupHunks[0].Digest.String(): 1}, true, []string{dupRef1}},
		{"skip duplicate within count passes", selectSkip, dupHunks, hunkDigestCounts(dupHunks), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseForeignHunks("f.txt", tt.mode, tt.hunks, tt.known)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("refuseForeignHunks() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a foreign-hunk refusal, got nil")
			}
			for _, ref := range tt.wantRefs {
				if !strings.Contains(err.Error(), ref) {
					t.Errorf("error = %q, want it to name %q", err, ref)
				}
			}
			if !strings.Contains(err.Error(), "re-run: ccx vcs hunks f.txt") {
				t.Errorf("error = %q, want the re-list hint", err)
			}
		})
	}
}

// TestGitStageSelectedForeignHunk drives the git lane's per-file staging directly
// against a real repository with a pre-flight fingerprint that omits a snapshot
// hunk: skip mode refuses and names the foreign hunk (including a duplicate the
// snapshot carries more times than pre-flight logged), only mode ignores it, and a
// complete fingerprint stages cleanly into the throwaway index.
func TestGitStageSelectedForeignHunk(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		current string
		mode    selectMode
		selIdx  int // the hunk index the selection names
		// preflight builds the pre-flight digest counts from the snapshot's hunks.
		preflight func(hunks []hunk.Hunk) map[string]int
		// wantForeignIdx is the snapshot hunk the refusal must name, or -1 for a clean stage.
		wantForeignIdx int
	}{
		{
			name: "skip refuses a foreign hunk", base: hunkBase, current: hunkCurrent, mode: selectSkip, selIdx: 0,
			preflight:      func(h []hunk.Hunk) map[string]int { return map[string]int{h[0].Digest.String(): 1} },
			wantForeignIdx: 1,
		},
		{
			name: "only ignores a foreign hunk", base: hunkBase, current: hunkCurrent, mode: selectOnly, selIdx: 0,
			preflight:      func(h []hunk.Hunk) map[string]int { return map[string]int{h[0].Digest.String(): 1} },
			wantForeignIdx: -1,
		},
		{
			name: "skip clean passes", base: hunkBase, current: hunkCurrent, mode: selectSkip, selIdx: 0,
			preflight:      func(h []hunk.Hunk) map[string]int { return hunkDigestCounts(h) },
			wantForeignIdx: -1,
		},
		{
			// A concurrent session duplicated the committed deletion: the snapshot carries
			// digest D twice while pre-flight logged it once, so skip mode must refuse the
			// second D rather than sweep it in — the multiplicity a digest set would miss.
			name: "skip refuses a duplicate foreign hunk", base: dupHunkBase, current: dupHunkCurrent, mode: selectSkip, selIdx: 0,
			preflight:      func(h []hunk.Hunk) map[string]int { return map[string]int{h[0].Digest.String(): 1} },
			wantForeignIdx: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := gitHunkRepo(t, tt.base, tt.current)
			hunks := hunk.Compute([]byte(tt.base), []byte(tt.current))
			sel := &shipSelection{
				root:      f.Dir,
				mode:      tt.mode,
				files:     map[string][]anchor.Ref{"f.txt": {hunkRef(hunks[tt.selIdx])}},
				preflight: map[string]map[string]int{"f.txt": tt.preflight(hunks)},
			}
			env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "idx")}
			err := gitStageSelected(context.Background(), "f.txt", sel, env)
			if tt.wantForeignIdx < 0 {
				if err != nil {
					t.Fatalf("gitStageSelected() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a foreign-hunk refusal, got nil")
			}
			want := "foreign hunk(s) appeared in f.txt since listing: " + hunkListRef("f.txt", hunks[tt.wantForeignIdx])
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		})
	}
}
