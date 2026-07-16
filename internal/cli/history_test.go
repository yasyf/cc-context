package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestParseCommits(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []historyCommit
	}{
		{
			name: "empty",
			out:  "",
			want: nil,
		},
		{
			name: "single record with name-status",
			out:  "aaa1111\x002026-06-24\x00feat: rework dispatch\n\nM\tinternal/cli/run.go\n",
			want: []historyCommit{{"aaa1111", "2026-06-24", "feat: rework dispatch", "internal/cli/run.go"}},
		},
		{
			name: "multiple records, blank line before each status",
			out:  "aaa1111\x002026-06-24\x00feat: x\n\nM\ta.go\nbbb2222\x002026-06-23\x00chore: y\n\nM\tb.go\n",
			want: []historyCommit{
				{"aaa1111", "2026-06-24", "feat: x", "a.go"},
				{"bbb2222", "2026-06-23", "chore: y", "b.go"},
			},
		},
		{
			name: "rename status uses the destination path",
			out:  "ccc3333\x002026-06-20\x00refactor: rename\n\nR090\told.go\tnew.go\n",
			want: []historyCommit{{"ccc3333", "2026-06-20", "refactor: rename", "new.go"}},
		},
		{
			name: "subject containing colon and spaces is preserved whole",
			out:  "ddd4444\x002026-06-19\x00fix: keep a: b, c in subject\n\nM\tf.go\n",
			want: []historyCommit{{"ddd4444", "2026-06-19", "fix: keep a: b, c in subject", "f.go"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCommits(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCommits() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseNumstat(t *testing.T) {
	tests := []struct {
		name        string
		out         string
		wantParents []string
		wantAdded   int
		wantDeleted int
	}{
		{
			name:        "normal commit with one parent",
			out:         "parent0\n\n7\t2\tinternal/cli/run.go\n",
			wantParents: []string{"parent0"},
			wantAdded:   7,
			wantDeleted: 2,
		},
		{
			name:        "root commit has no parents",
			out:         "\n\n40\t0\tinternal/cli/run.go\n",
			wantParents: []string{},
			wantAdded:   40,
			wantDeleted: 0,
		},
		{
			name:        "merge commit lists two parents",
			out:         "p1 p2\n\n1\t1\tf.go\n",
			wantParents: []string{"p1", "p2"},
			wantAdded:   1,
			wantDeleted: 1,
		},
		{
			name:        "binary file reports dash counts as zero",
			out:         "parent0\n\n-\t-\tlogo.png\n",
			wantParents: []string{"parent0"},
			wantAdded:   0,
			wantDeleted: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotParents, gotAdded, gotDeleted := parseNumstat(tt.out)
			if !reflect.DeepEqual(gotParents, tt.wantParents) {
				t.Errorf("parents = %#v, want %#v", gotParents, tt.wantParents)
			}
			if gotAdded != tt.wantAdded || gotDeleted != tt.wantDeleted {
				t.Errorf("(+%d/-%d), want (+%d/-%d)", gotAdded, gotDeleted, tt.wantAdded, tt.wantDeleted)
			}
		})
	}
}

// TestCommitSummary drives commitSummary against a scripted real git repo: a root
// commit yields "(added)"; a commit with structural edits (Foo's body changed, Bar
// removed, Baz added) yields the sigil-tagged symbols from the native diff; and a
// comment-only commit with no symbol change degrades to the numstat. It needs git
// and ast-grep.
func TestCommitSummary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	dir := t.TempDir()
	historyGit(t, dir, "init", "-q")
	historyGit(t, dir, "config", "user.email", "t@t.t")
	historyGit(t, dir, "config", "user.name", "t")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c1: root")
	rootSha := historyShortSha(t, dir, "HEAD")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 11 }\nfunc Baz() int { return 3 }\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c2: rework symbols")
	symSha := historyShortSha(t, dir, "HEAD")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 11 }\nfunc Baz() int { return 3 }\n// trailing note\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c3: comment only")
	commentSha := historyShortSha(t, dir, "HEAD")

	t.Chdir(dir) // commitStat runs git in the cwd; ChangedSymbols resolves against dir

	if got, err := commitSummary(context.Background(), dir, rootSha, "a.go"); err != nil || got != "(added)" {
		t.Errorf("root commitSummary = %q, err %v, want %q", got, err, "(added)")
	}

	got, err := commitSummary(context.Background(), dir, symSha, "a.go")
	if err != nil {
		t.Fatalf("symbol commitSummary err: %v", err)
	}
	for _, want := range []string{"~Foo", "+Baz", "-Bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("symbol commitSummary = %q, missing %q", got, want)
		}
	}

	if got, err := commitSummary(context.Background(), dir, commentSha, "a.go"); err != nil || got != "(+1/-0)" {
		t.Errorf("comment-only commitSummary = %q, err %v, want %q", got, err, "(+1/-0)")
	}
}

// TestCommitSummaryJJ is the colocated-jj analogue of TestCommitSummary: in a jj
// working copy the native diff routes the first-parent..sha range through jj, which
// rejects a "sha^" endpoint — so this guards that commitSummary hands jj a resolved
// parent id instead. It needs jj, git, and ast-grep.
func TestCommitSummaryJJ(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	dir := t.TempDir()
	historyJJ(t, dir, "git", "init", "--colocate")
	historyJJ(t, dir, "config", "set", "--repo", "user.email", "t@t.t")
	historyJJ(t, dir, "config", "set", "--repo", "user.name", "t")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n")
	historyJJ(t, dir, "commit", "-m", "c1: root")
	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 11 }\nfunc Baz() int { return 3 }\n")
	historyJJ(t, dir, "commit", "-m", "c2: rework symbols")

	// The just-committed change is @-; resolve its git commit id through jj (git
	// cannot rev-parse the jj @- revset).
	out, err := exec.Command("jj", "-R", dir, "log", "--no-graph", "-r", "@-", "-T", "commit_id").Output() //nolint:gosec // fixed jj argv; dir is a test TempDir
	if err != nil {
		t.Fatalf("jj resolve @- commit id: %v", err)
	}
	symSha := strings.TrimSpace(string(out))
	t.Chdir(dir)

	got, err := commitSummary(context.Background(), dir, symSha, "a.go")
	if err != nil {
		t.Fatalf("commitSummary (jj lane) err: %v", err)
	}
	for _, want := range []string{"~Foo", "+Baz", "-Bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("jj-lane commitSummary = %q, missing %q", got, want)
		}
	}
}

// historyJJ runs a jj command in dir, failing the test on error.
func historyJJ(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("jj", args...) //nolint:gosec // fixed jj argv; dir is a test TempDir, args are literals
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, out)
	}
}

// TestHistoryLiveSmoke runs the real command against this repository. It is skipped
// unless CCX_LIVE_SMOKE is set, since it shells out to the real git history and
// ast-grep. Assertions are loose (shape, not content) so an evolving history does
// not make it flaky.
func TestHistoryLiveSmoke(t *testing.T) {
	if os.Getenv("CCX_LIVE_SMOKE") == "" {
		t.Skip("set CCX_LIVE_SMOKE=1 to run the live smoke against real git + ast-grep")
	}
	t.Chdir("../..") // repo root, so the pathspecs below resolve

	block := regexp.MustCompile(`(?m)^[0-9a-f]{7,} \d{4}-\d{2}-\d{2} .+\n {4}\S`)
	// The semble path spans a file move, so --follow must cross the rename to reach
	// its pre-move history — the case that crashed when a commit was scoped by path.
	for _, path := range []string{"AGENTS.md", "internal/cli/run.go", "internal/semble/semble.go"} {
		var out bytes.Buffer
		cmd := newHistoryCmd()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{path, "-n", "3"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("history %s: %v\noutput:\n%s", path, err, out.String())
		}
		t.Logf("history %s -n 3:\n%s", path, out.String())
		if !block.MatchString(out.String()) {
			t.Errorf("history %s produced no well-formed commit block:\n%s", path, out.String())
		}
	}
}

// historyGit runs a git command in dir with the developer's ambient config detached.
func historyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// historyWrite writes content to dir/name, failing the test on error.
func historyWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// historyShortSha resolves rev to its abbreviated commit id in dir.
func historyShortSha(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--short", rev) //nolint:gosec // fixed git argv; dir is a test TempDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}
