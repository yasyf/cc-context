package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
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

	mustRun(t, f.Env(), control, "git", "rebase", "-q", "origin/main")
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
