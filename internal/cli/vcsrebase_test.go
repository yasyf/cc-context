package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func runVcsRebaseCmd(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsRebaseCmd() //nolint:contextcheck // ExecuteContext(ctx) below is what sets cmd's context; contextcheck cannot see through cobra's two-step wiring
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestRebaseParseReadsTheOperands(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
		want rebaseRange
	}{
		{name: "upstream", args: []string{"origin/main"}, want: rebaseRange{upstream: "origin/main"}},
		{name: "upstream and branch", args: []string{"-q", "origin/main", "feature"}, want: rebaseRange{upstream: "origin/main", branch: "feature"}},
		{name: "onto apart", args: []string{"--onto", "origin/main", "main~3"}, want: rebaseRange{onto: "origin/main", upstream: "main~3"}},
		{name: "onto inline", args: []string{"--onto=origin/main", "main~3", "feature"}, want: rebaseRange{onto: "origin/main", upstream: "main~3", branch: "feature"}},
		{
			name: "values apart",
			args: []string{"-x", "make test", "--strategy", "ort", "-X", "theirs", "--empty", "drop", "--trailer", "Signed-off-by: t", "origin/main"},
			want: rebaseRange{upstream: "origin/main"},
		},
		{name: "values attached", args: []string{"-xmake", "-sort", "-Xtheirs", "--whitespace=fix", "origin/main"}, want: rebaseRange{upstream: "origin/main"}},
		{name: "bundled shorts ending in a value", args: []string{"-iqx", "true", "origin/main"}, want: rebaseRange{upstream: "origin/main"}},
		{name: "optional values stay attached", args: []string{"-S", "-r", "--gpg-sign", "--rebase-merges", "origin/main"}, want: rebaseRange{upstream: "origin/main"}},
		{name: "previous branch", args: []string{"-"}, want: rebaseRange{upstream: "@{-1}"}},
		{name: "keep base", args: []string{"--keep-base", "origin/main"}, want: rebaseRange{keepBase: true, upstream: "origin/main"}},
		{name: "root", args: []string{"--root", "feature"}, want: rebaseRange{root: true, branch: "feature"}},
		{name: "continue", args: []string{"--continue"}, want: rebaseRange{resume: true}},
		{name: "skip", args: []string{"--skip"}, want: rebaseRange{resume: true}},
		{name: "abort", args: []string{"--abort"}, want: rebaseRange{idle: true}},
		{name: "end of options", args: []string{"-q", "--", "origin/main"}, want: rebaseRange{upstream: "origin/main"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := rebaseParse(tt.args); got != tt.want {
				t.Errorf("rebaseParse(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRebaseTodoPicksReadsEveryReplayedCommit(t *testing.T) {
	t.Parallel()
	todo := "pick 1111 one\np 2222 two\n# pick 9999 commented out\nexec make\nfixup -C 3333 three\nf 4444\n" +
		"reword 5555 five\nlabel onto\ndrop 6666 six\nbreak\nsquash 7777 seven\nedit 8888 eight\n\n"
	if got, want := rebaseTodoPicks(todo), []string{"1111", "2222", "3333", "4444", "5555", "7777", "8888"}; !slices.Equal(got, want) {
		t.Errorf("rebaseTodoPicks = %q, want %q", got, want)
	}
}

func TestVcsRebaseFetchesTheRebaseInOneBatch(t *testing.T) {
	t.Parallel()
	f, s := replayPartialClone(t)
	control := replayClone(t, f)
	baseline := map[string]int{}
	for _, dir := range []string{s.dir, control} {
		mustRun(t, f.Env(), dir, "git", "switch", "-q", "--detach", "origin/main")
		mustRun(t, f.Env(), dir, "git", "switch", "-q", "-c", "work", "origin/feature")
		baseline[dir] = replayPromisorPacks(t, dir)
	}

	mustRun(t, f.Env(), control, "git", "-c", "core.abbrev=40", "rebase", "-q", "origin/main")
	if got := replayPromisorPacks(t, control); got < baseline[control]+2 {
		t.Fatalf("raw git rebase wrote %d promisor packs, want a fixture whose rebase fetches per commit", got-baseline[control])
	}

	out, err := runVcsRebaseCmd(f.ContextIn(f.Dir), t, "-C", s.dir, "-c", "rerere.enabled=false", "--", "-q", "origin/main")
	if err != nil {
		t.Fatalf("vcs rebase: %v\n%s", err, out)
	}
	if got := replayPromisorPacks(t, s.dir); got != baseline[s.dir]+1 {
		t.Errorf("vcs rebase wrote %d promisor packs, want 1: one batch for the whole rebase", got-baseline[s.dir])
	}
	if subjects := gitAt(t, f.Env(), s.dir, "log", "--format=%s", "origin/main..HEAD"); subjects != "feature 3\nfeature 2\nfeature 1" {
		t.Errorf("rebased subjects = %q, want the three feature commits", subjects)
	}
	if lines := gitAt(t, f.Env(), s.dir, "show", "HEAD:lines.txt"); lines != "A\nb\nc\nd\ne\nf\ng\nh3" {
		t.Errorf("rebased lines.txt = %q, want both sides' edits", lines)
	}
}

func TestVcsRebaseContinueFetchesTheRemainingPicks(t *testing.T) {
	t.Parallel()
	f, s := replayPartialClone(t)
	mustRun(t, f.Env(), s.dir, "git", "switch", "-q", "-c", "work", "origin/feature")
	mustRun(t, append(f.Env(), `GIT_SEQUENCE_EDITOR=perl -pi -e 'print "break\n" if $. == 1'`), s.dir, "git", "rebase", "-q", "-i", "origin/main")
	if head := gitAt(t, f.Env(), s.dir, "rev-parse", "HEAD"); head != s.onto {
		t.Fatalf("HEAD = %s at the break, want the onto commit %s", head, s.onto)
	}
	packs := replayPromisorPacks(t, s.dir)

	out, err := runVcsRebaseCmd(f.ContextIn(s.dir), t, "--", "--continue")
	if err != nil {
		t.Fatalf("vcs rebase --continue: %v\n%s", err, out)
	}
	if got := replayPromisorPacks(t, s.dir); got != packs+1 {
		t.Errorf("vcs rebase --continue wrote %d promisor packs, want 1: one batch for the remaining picks", got-packs)
	}
	if subjects := gitAt(t, f.Env(), s.dir, "log", "--format=%s", "origin/main..HEAD"); subjects != "feature 3\nfeature 2\nfeature 1" {
		t.Errorf("rebased subjects = %q, want the three feature commits", subjects)
	}
}

func TestVcsRebaseDeepensAShallowCloneToTheMergeBase(t *testing.T) {
	t.Parallel()
	f := shipRepo(t, vcstest.Remote())
	var stream strings.Builder
	commit := func(ref, head, from, path, body, subject string, at int) {
		fmt.Fprintf(&stream, "commit %s\n%scommitter t <t@t.t> %d +0000\ndata %d\n%s\n%sM 100644 inline %s\ndata %d\n%s\n", ref, head, 1700000000+at, len(subject), subject, from, path, len(body), body)
	}
	for i := range 300 {
		head, from := "", ""
		if i == 0 {
			from = "from " + gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main") + "\n"
		}
		if i == 299 {
			head = "mark :1\n"
		}
		commit("refs/heads/main", head, from, "trunk.txt", fmt.Sprintf("trunk %d\n", i), fmt.Sprintf("trunk %d", i), i)
	}
	commit("refs/heads/feature", "", "from :1\n", "feature.txt", "feature 1\n", "feature 1", 300)
	commit("refs/heads/feature", "", "", "feature.txt", "feature 2\n", "feature 2", 301)
	for i := 300; i < 320; i++ {
		commit("refs/heads/main", "", "", "trunk.txt", fmt.Sprintf("trunk %d\n", i), fmt.Sprintf("trunk %d", i), i+2)
	}
	cmd := exec.Command("git", "fast-import", "--quiet")
	cmd.Dir = f.RemoteDir
	cmd.Env = append(os.Environ(), f.Env()...)
	cmd.Stdin = strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git fast-import: %v\n%s", err, out)
	}
	fork := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature~2")
	shallow := filepath.Join(t.TempDir(), "shallow")
	mustRun(t, f.Env(), filepath.Dir(shallow), "git", "clone", "-q", "--no-local", "--depth", "1", "--branch", "feature", f.RemoteDir, shallow)
	for _, kv := range [][2]string{{"user.email", "t@t.t"}, {"user.name", "t"}, {"commit.gpgsign", "false"}} {
		mustRun(t, f.Env(), shallow, "git", "config", kv[0], kv[1])
	}
	mustRun(t, f.Env(), shallow, "git", "fetch", "-q", "--depth", "1", "origin", "+refs/heads/main:refs/remotes/origin/main")

	out, err := runVcsRebaseCmd(f.ContextIn(shallow), t, "--", "-q", "origin/main")
	if err != nil {
		t.Fatalf("vcs rebase in a depth-1 clone: %v\n%s", err, out)
	}
	if got := gitAt(t, f.Env(), shallow, "merge-base", "origin/feature", "origin/main"); got != fork {
		t.Errorf("merge base = %s, want the fork %s below the depth-1 boundary", got, fork)
	}
	if subjects := gitAt(t, f.Env(), shallow, "log", "--format=%s", "origin/main..HEAD"); subjects != "feature 2\nfeature 1" {
		t.Errorf("replayed subjects = %q, want only the branch's two commits", subjects)
	}
	if got, want := gitAt(t, f.Env(), shallow, "rev-parse", "HEAD~2"), gitAt(t, f.Env(), shallow, "rev-parse", "origin/main"); got != want {
		t.Errorf("HEAD~2 = %s, want origin/main %s", got, want)
	}
	if got := gitAt(t, f.Env(), shallow, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Errorf("is-shallow-repository = %s, want the clone still shallow: deepened to the fork, not unshallowed", got)
	}
}

func TestVcsRebaseExitsWithGitsCode(t *testing.T) {
	t.Parallel()
	f := shipRepo(t)
	_, err := runVcsRebaseCmd(f.Context(), t, "--", "--no-such-option")
	if got := ExitCode(err); got != 129 {
		t.Errorf("ExitCode(%v) = %d, want git's usage exit 129", err, got)
	}
	if _, err := runVcsRebaseCmd(f.Context(), t, "origin/main"); err == nil || !strings.Contains(err.Error(), "after --") {
		t.Errorf("vcs rebase without -- = %v, want the arguments named after --", err)
	}
}
