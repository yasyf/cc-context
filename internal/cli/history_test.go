package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

// TestHistoryCommitWalk drives the commit walk against a real repository: git's own
// bytes supply every expected hash and date, and --follow carries the walk past a
// rename so each commit is labelled with the name the file had then.
func TestHistoryCommitWalk(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "a.go", "package a\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "feat: keep a: b, c in subject")
	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() {}\n")
	historyGit(t, dir, "commit", "-qam", "chore: edit")
	historyGit(t, dir, "mv", "a.go", "b.go")
	historyGit(t, dir, "commit", "-qm", "refactor: rename")

	want := []historyCommit{
		{historyShow(t, dir, "HEAD", "%h"), historyShow(t, dir, "HEAD", "%ad"), "refactor: rename", "b.go"},
		{historyShow(t, dir, "HEAD~1", "%h"), historyShow(t, dir, "HEAD~1", "%ad"), "chore: edit", "a.go"},
		{historyShow(t, dir, "HEAD~2", "%h"), historyShow(t, dir, "HEAD~2", "%ad"), "feat: keep a: b, c in subject", "a.go"},
	}
	got, err := logCommits(context.Background(), render.Dir(dir), "b.go", 10)
	if err != nil {
		t.Fatalf("logCommits: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logCommits = %+v, want %+v", got, want)
	}

	capped, err := logCommits(context.Background(), render.Dir(dir), "b.go", 2)
	if err != nil {
		t.Fatalf("logCommits -n 2: %v", err)
	}
	if !slices.Equal(capped, want[:2]) {
		t.Fatalf("logCommits(-n 2) = %+v, want %+v", capped, want[:2])
	}
}

// TestHistoryPathspecIsLiteral pins GIT_LITERAL_PATHSPECS. "[id].go" is a real
// filename here and also a glob matching the sibling i.go, so a globbing pathspec
// folds the sibling's commit into the file's own history and mislabels the shared
// root commit with the sibling's name.
func TestHistoryPathspecIsLiteral(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "[id].go", "package a\n")
	historyWrite(t, dir, "i.go", "package a\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c1: add both")
	historyWrite(t, dir, "[id].go", "package a\n\nfunc B() {}\n")
	historyGit(t, dir, "commit", "-qam", "c2: bracket only")
	historyWrite(t, dir, "i.go", "package a\n\nfunc C() {}\n")
	historyGit(t, dir, "commit", "-qam", "c3: sibling only")

	got, err := logCommits(context.Background(), render.Dir(dir), "[id].go", 10)
	if err != nil {
		t.Fatalf("logCommits: %v", err)
	}
	want := []historyCommit{
		{historyShow(t, dir, "HEAD~1", "%h"), historyShow(t, dir, "HEAD~1", "%ad"), "c2: bracket only", "[id].go"},
		{historyShow(t, dir, "HEAD~2", "%h"), historyShow(t, dir, "HEAD~2", "%ad"), "c1: add both", "[id].go"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("logCommits(%q) = %+v, want %+v", "[id].go", got, want)
	}
}

// TestHistoryFollowsARenameToTheOldName drives the whole report across a rename:
// the pre-rename commit's counts are only reachable when its diff is scoped by the
// name the file carried then, so a report built from the newest name reads (+0/-0)
// there.
func TestHistoryFollowsARenameToTheOldName(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "old.txt", "one\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c1: add")
	historyWrite(t, dir, "old.txt", "one\ntwo\nthree\nfour\n")
	historyGit(t, dir, "commit", "-qam", "c2: extend")
	historyGit(t, dir, "mv", "old.txt", "new.txt")
	historyGit(t, dir, "commit", "-qm", "c3: rename")

	want := historyBlock(t, dir, "HEAD", "c3: rename", "(+4/-0)") +
		historyBlock(t, dir, "HEAD~1", "c2: extend", "(+3/-0)") +
		historyBlock(t, dir, "HEAD~2", "c1: add", "(added)")
	if got := historyRun(t, "new.txt"); got != want {
		t.Fatalf("history new.txt =\n%q\nwant\n%q", got, want)
	}
}

// TestHistoryReportsCountsForAnUnquotableName pins the -z discipline end to end.
// Under default core.quotePath git renders a zero-width joiner and a quote as C
// escapes inside a quoted name, and a report that carried the quoted spelling back
// into the per-commit diff would scope it to a path that does not exist and
// degrade every commit to (+0/-0).
func TestHistoryReportsCountsForAnUnquotableName(t *testing.T) {
	dir := historyRepo(t)
	const name = "zwj\u200dq\"uote.txt"
	historyWrite(t, dir, name, "one\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c1: add")
	historyWrite(t, dir, name, "one\ntwo\nthree\n")
	historyGit(t, dir, "commit", "-qam", "c2: extend")

	quoted := historyGitOut(t, dir, "show", "--name-only", "--format=", "HEAD")
	if !strings.Contains(quoted, `\342\200\215q\"uote.txt`) {
		t.Fatalf("git rendered the name as %q; the quoting this test survives is not happening", quoted)
	}

	want := historyBlock(t, dir, "HEAD", "c2: extend", "(+2/-0)") +
		historyBlock(t, dir, "HEAD~1", "c1: add", "(added)")
	if got := historyRun(t, name); got != want {
		t.Fatalf("history %q =\n%q\nwant\n%q", name, got, want)
	}
}

func TestResolveHistoryPath(t *testing.T) {
	t.Chdir(t.TempDir())
	historyWrite(t, ".", "unique.py", "print('x')\n")
	historyWrite(t, ".", "many.go", "package many\n")
	historyWrite(t, ".", "many.py", "print('x')\n")
	tests := []struct {
		name     string
		path     string
		wantPath string
		wantNote string
	}{
		{"unique sibling resolves", "unique", "unique.py", "# note: unique → unique.py\n"},
		{"ambiguity passes original", "many", "many", ""},
		{"miss passes original", "deleted", "deleted", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, note := resolveHistoryPath(tt.path)
			if path != tt.wantPath {
				t.Errorf("resolveHistoryPath(%q) path = %q, want %q", tt.path, path, tt.wantPath)
			}
			if note != tt.wantNote {
				t.Errorf("resolveHistoryPath(%q) note = %q, want %q", tt.path, note, tt.wantNote)
			}
		})
	}
}

func TestHistoryCommandResolvesExtensionSibling(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "source.go", "package source\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "add source")

	wantPrefix := "# note: source → source.go\n"
	if got := historyRun(t, "source"); !strings.HasPrefix(got, wantPrefix) || !strings.Contains(got, "add source\n    (added)\n") {
		t.Errorf("history output = %q, want prefix %q followed by commit summary", got, wantPrefix)
	}
}

// TestHistoryMasksSecretOutput scripts a repo whose commit subject carries a
// secret, then proves the history report masks it with the shared footer and
// that --reveal-secrets prints it raw. (Changed-symbol names flow through the
// same report-level mask.)
func TestHistoryMasksSecretOutput(t *testing.T) {
	dir := historyRepo(t)
	historyWrite(t, dir, "conf.txt", "value = 1\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "leak "+rawAWSKey+" in subject")

	got := historyRun(t, "conf.txt")
	if strings.Contains(got, rawAWSKey) {
		t.Errorf("history output leaked the raw secret:\n%s", got)
	}
	if !strings.Contains(got, "leak AKIA…[masked:aws-access-token] in subject") {
		t.Errorf("history output missing the masked subject:\n%s", got)
	}
	footer := "# 1 secret(s) masked (aws-access-token) — --reveal-secrets prints raw\n" //nolint:gosec // footer text, not a credential
	if !strings.HasSuffix(got, footer) {
		t.Errorf("history output missing the secrets footer:\n%s", got)
	}

	reveal := historyRun(t, "conf.txt", "--reveal-secrets")
	if !strings.Contains(reveal, "leak "+rawAWSKey+" in subject") {
		t.Errorf("history --reveal-secrets output missing the raw secret:\n%s", reveal)
	}
	if strings.Contains(reveal, "[masked:") || strings.Contains(reveal, "secret(s) masked") {
		t.Errorf("history --reveal-secrets output still masked:\n%s", reveal)
	}
}

// TestCommitSummary drives commitSummary against a scripted real git repo: a root
// commit yields "(added)"; a commit with structural edits (Foo's body changed, Bar
// removed, Baz added) yields the sigil-tagged symbols from the native diff; and a
// comment-only commit with no symbol change degrades to the numstat. It needs git
// and ast-grep.
func TestCommitSummary(t *testing.T) {
	if _, err := exec.LookPath("ast-grep"); err != nil {
		t.Skip("ast-grep not on PATH")
	}
	dir := historyRepo(t)

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c1: root")
	rootSha := historyShow(t, dir, "HEAD", "%h")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 11 }\nfunc Baz() int { return 3 }\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c2: rework symbols")
	symSha := historyShow(t, dir, "HEAD", "%h")

	historyWrite(t, dir, "a.go", "package a\n\nfunc Foo() int { return 11 }\nfunc Baz() int { return 3 }\n// trailing note\n")
	historyGit(t, dir, "add", "-A")
	historyGit(t, dir, "commit", "-qm", "c3: comment only")
	commentSha := historyShow(t, dir, "HEAD", "%h")

	if got, err := commitSummary(context.Background(), render.Dir(dir), rootSha, "a.go"); err != nil || got != "(added)" {
		t.Errorf("root commitSummary = %q, err %v, want %q", got, err, "(added)")
	}

	got, err := commitSummary(context.Background(), render.Dir(dir), symSha, "a.go")
	if err != nil {
		t.Fatalf("symbol commitSummary err: %v", err)
	}
	for _, want := range []string{"~Foo", "+Baz", "-Bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("symbol commitSummary = %q, missing %q", got, want)
		}
	}

	if got, err := commitSummary(context.Background(), render.Dir(dir), commentSha, "a.go"); err != nil || got != "(+1/-0)" {
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

	got, err := commitSummary(context.Background(), render.Dir(dir), symSha, "a.go")
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

// historyRepo initializes a real git repository under a temp dir and chdirs into
// it: logCommits and commitStat run git in the process working directory, and the
// native diff resolves its repository from there too. The ambient git config is
// detached on the environment rather than per-command, so the git children ccx
// spawns are as isolated as the fixture's own.
func historyRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	historyGit(t, dir, "init", "-q")
	historyGit(t, dir, "config", "user.email", "t@t.t")
	historyGit(t, dir, "config", "user.name", "t")
	t.Chdir(dir)
	return dir
}

// historyRun executes the history command with args and returns its output.
func historyRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newHistoryCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("history %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// historyBlock renders the report block the command must print for rev, taking
// the hash and date from git rather than restating them.
func historyBlock(t *testing.T, dir, rev, subject, summary string) string {
	t.Helper()
	return historyShow(t, dir, rev, "%h") + " " + historyShow(t, dir, rev, "%ad") + " " + subject + "\n    " + summary + "\n"
}

// historyShow returns the value git's own --format placeholder reports for rev.
func historyShow(t *testing.T, dir, rev, format string) string {
	t.Helper()
	return strings.TrimSuffix(historyGitOut(t, dir, "show", "-s", "--date=short", "--format="+format, rev), "\n")
}

// historyGitOut runs a git command in dir and returns its stdout.
func historyGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git argv; dir is a test TempDir, args are literals
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
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
