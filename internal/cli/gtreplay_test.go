package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func gtReplayBranch(t *testing.T, f *vcstest.Fixture) (from, head, onto string) {
	t.Helper()
	from = gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
	stackReplayCommit(t, f, "a.txt", "a\n", "a")
	head = stackReplayCommit(t, f, "b.txt", "b\n", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	return from, head, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
}

// TestGTReplayMatchesRebasePolicy pins that ship's in-place restack replays a
// saved span the way the conflict workspace's rebase would, merges linearized,
// without a checkout and without moving the branch it read the span from — even
// where the repository's own config asks git replay to update refs itself.
func TestGTReplayMatchesRebasePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(t *testing.T, f *vcstest.Fixture) (from, head, onto string)
		want  []string
	}{
		{name: "linear", build: gtReplayBranch, want: []string{"b", "a"}},
		{
			name: "merge linearized",
			build: func(t *testing.T, f *vcstest.Fixture) (string, string, string) {
				t.Helper()
				from := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "branch")
				stackReplayCommit(t, f, "feature.txt", "feature\n", "feature")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side", "main")
				stackReplayCommit(t, f, "side.txt", "side\n", "side")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "branch")
				mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--no-ff", "-m", "merge side", "side")
				head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
				mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
				return from, head, stackReplayCommit(t, f, "t.txt", "trunk\n", "trunk")
			},
			want: []string{"side", "feature"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipRepo(t)
			from, head, onto := tt.build(t, f)
			mustRun(t, f.Env(), f.Dir, "git", "config", "replay.refAction", "update")
			stackReplaySettleIndex(t, f)
			index, err := os.ReadFile(filepath.Join(f.Dir, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			refs := stackReplayRefs(t, f)
			worktrees := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain")
			argvLog := filepath.Join(t.TempDir(), "argv")
			bin := stackReplayGitWrapper(t, "printf '%s\\n' \"$*\" >> "+shQuote(argvLog)+"\n")
			ctx := render.WithEnv(f.Context(), "PATH="+bin+string(os.PathListSeparator)+f.PATH())

			got, err := gtReplay(ctx, "test", render.Dir(f.Dir), onto, from, "branch", gtBranchState{Head: head})
			if err != nil {
				t.Fatalf("gtReplay: %v", err)
			}

			ran, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatal(err)
			}
			if want := "--attr-source=" + onto + " replay --ref-action=print --linearize --ref=refs/heads/branch --onto=" + onto + " " + from + ".." + head + "\n"; string(ran) != want {
				t.Errorf("git ran %q, want exactly %q", ran, want)
			}
			if after := stackReplayRefs(t, f); after != refs {
				t.Errorf("gtReplay moved refs:\n%s\nwant:\n%s", after, refs)
			}
			if after, err := os.ReadFile(filepath.Join(f.Dir, ".git", "index")); err != nil || !bytes.Equal(after, index) {
				t.Errorf("source index changed (err %v)", err)
			}
			if after := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); after != worktrees {
				t.Errorf("worktree list changed:\n%s\nwant:\n%s", after, worktrees)
			}
			ref := filepath.Join(t.TempDir(), "reference")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", ref, head)
			mustRun(t, f.Env(), ref, "git", slices.Concat(stackGitRebaseArgs, []string{"rebase"}, stackReplayRebaseArgs, []string{"--onto", onto, from})...)
			want := gitAt(t, f.Env(), ref, "rev-parse", "HEAD")
			if g, w := gitAt(t, f.Env(), f.Dir, "rev-parse", got+"^{tree}"), gitAt(t, f.Env(), f.Dir, "rev-parse", want+"^{tree}"); g != w {
				t.Errorf("replayed tree %s, rebased tree %s", g, w)
			}
			replayed := stackReplaySubjects(t, f, onto, got)
			if rebased := stackReplaySubjects(t, f, onto, want); !slices.Equal(replayed, rebased) {
				t.Errorf("replayed subjects %q, rebased subjects %q", replayed, rebased)
			}
			if !slices.Equal(replayed, tt.want) {
				t.Errorf("replayed subjects %q, want %q", replayed, tt.want)
			}
		})
	}
}

// TestGTReplayRefusesAnyOtherUpdate pins that the one update gtReplay accepts
// names the branch it asked for, moving from the head it saved: a different
// ref, a stale old value, a second line, silence, or a branch that moved since
// the head was read are each refused, and nothing moves.
func TestGTReplayRefusesAnyOtherUpdate(t *testing.T) {
	t.Parallel()
	f := shipRepo(t)
	from, head, onto := gtReplayBranch(t, f)
	refs := stackReplayRefs(t, f)

	tests := []struct {
		name  string
		awk   string
		moved bool
	}{
		{name: "another ref", awk: `{ $2 = "refs/heads/elsewhere"; print }`},
		{name: "a wrong old value", awk: `{ $4 = "` + onto + `"; print }`},
		{name: "two updates", awk: `{ print; print }`},
		{name: "no update", awk: `{ }`},
		{name: "an update split across lines", awk: `{ print $1, $2; print $3, $4 }`},
		{name: "a blank line before the update", awk: `{ print ""; print }`},
		{name: "a branch moved since it was read", moved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := f.Context()
			if tt.awk != "" {
				bin := stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then\n"+
					"  out=$(PATH=${PATH#${0%/git}:} git \"$@\") || exit $?\n"+
					"  printf '%s\\n' \"$out\" | awk "+shQuote(tt.awk)+"\n"+
					"  exit 0\n"+
					"fi\n")
				ctx = render.WithEnv(ctx, "PATH="+bin+string(os.PathListSeparator)+f.PATH())
			}
			want := refs
			if tt.moved {
				mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/branch", from, head)
				t.Cleanup(func() { mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/branch", head, from) })
				want = stackReplayRefs(t, f)
			}

			got, err := gtReplay(ctx, "test", render.Dir(f.Dir), onto, from, "branch", gtBranchState{Head: head})
			if err == nil || !strings.Contains(err.Error(), "want exactly one update of refs/heads/branch from "+head) {
				t.Fatalf("gtReplay = %q, %v, want a refusal naming exactly one update from %s", got, err, head)
			}
			if after := stackReplayRefs(t, f); after != want {
				t.Errorf("gtReplay moved refs:\n%s\nwant:\n%s", after, want)
			}
		})
	}
}

// TestGTReplayConflictIsExitOneAlone pins that only git replay's exit 1 reads as
// a conflict. Every other exit is a failure surfaced with its code and git's own
// words — a silent one included, which the old empty-stderr reading took for a
// conflict and sent the user to resolve a conflict that never happened.
func TestGTReplayConflictIsExitOneAlone(t *testing.T) {
	t.Parallel()
	f := shipRepo(t)
	from, head, onto := gtReplayBranch(t, f)
	refs := stackReplayRefs(t, f)

	tests := []struct {
		name     string
		script   string
		conflict bool
		want     []string
	}{
		{name: "fatal", script: "echo 'fatal: boom' >&2; exit 128", want: []string{"exited 128", "fatal: boom"}},
		{name: "silent usage error", script: "exit 129", want: []string{"exited 129"}},
		{name: "exit 1 with words", script: "echo 'error: could not apply' >&2; exit 1", conflict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then "+tt.script+"; fi\n")
			ctx := render.WithEnv(f.Context(), "PATH="+bin+string(os.PathListSeparator)+f.PATH())

			_, err := gtReplay(ctx, "test", render.Dir(f.Dir), onto, from, "branch", gtBranchState{Head: head})
			if err == nil {
				t.Fatal("gtReplay succeeded")
			}
			if got := errors.Is(err, errReplayConflict); got != tt.conflict {
				t.Errorf("errors.Is(%v, errReplayConflict) = %v, want %v", err, got, tt.conflict)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to carry %q", err, want)
				}
			}
			if after := stackReplayRefs(t, f); after != refs {
				t.Errorf("gtReplay moved refs:\n%s\nwant:\n%s", after, refs)
			}
		})
	}

	t.Run("genuine conflict", func(t *testing.T) {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
		conflicting := stackReplayCommit(t, f, "a.txt", "trunk a\n", "trunk a")
		_, err := gtReplay(f.Context(), "test", render.Dir(f.Dir), conflicting, from, "branch", gtBranchState{Head: head})
		if !errors.Is(err, errReplayConflict) {
			t.Errorf("gtReplay = %v, want errReplayConflict", err)
		}
	})
}

// TestShipGTRestackLinearizesAMergeSpan pins that ship's in-place restack
// carries a branch holding a merge onto its moved parent as a line, where git
// replay without --linearize refuses the merge outright.
func TestShipGTRestackLinearizesAMergeSpan(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t, vcstest.GTStack("base", "feature"))
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side", "base")
	stackReplayCommit(t, f, "side.txt", "side\n", "side")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--no-ff", "-m", "merge side", "side")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "side")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	stackReplayCommit(t, f, "base2.txt", "base2\n", "base2")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	shipGTReady(t, f)

	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if !strings.Contains(got, "restacked 1 branch") {
		t.Errorf("summary = %q, want it to report the restack", got)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "feature..base"); behind != "0" {
		t.Errorf("base holds %s commit(s) feature does not", behind)
	}
	if merges := gitAt(t, f.Env(), f.Dir, "rev-list", "--merges", "base..feature"); merges != "" {
		t.Errorf("feature still carries merges %q above base", merges)
	}
	if subjects := stackReplaySubjects(t, f, "base", "feature"); !slices.Equal(subjects, []string{"fix: frobnicate", "side", "feature"}) {
		t.Errorf("feature above base = %q, want the commit on the linearized span", subjects)
	}
	if dirt := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); dirt != "" {
		t.Errorf("working copy status = %q, want clean on the restacked head", dirt)
	}
}

// TestShipGTRestackReportsANonConflictReplayError pins that a replay failing for
// any reason but a conflict reaches the user as that failure: ship names git's
// exit and words, sends no one to resolve a conflict, and moves no branch.
func TestShipGTRestackReportsANonConflictReplayError(t *testing.T) {
	t.Parallel()
	f := shipGTUnrestacked(t, "base2.txt", "base2\n")
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$2\" = replay ]; then echo 'fatal: boom' >&2; exit 128; fi\n"))

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err == nil {
		t.Fatal("ship succeeded, want the replay error")
	}
	if msg := err.Error(); !strings.Contains(msg, "exited 128") || !strings.Contains(msg, "fatal: boom") {
		t.Errorf("error = %q, want it to carry exited 128 and fatal: boom", msg)
	}
	if msg := err.Error(); strings.Contains(msg, "cleanly") || strings.Contains(msg, "stack rebase") {
		t.Errorf("error = %q, want no conflict advice for a failure that was not one", msg)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base moved to %s, want %s", got, base)
	}
	if parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature^"); parent != feature {
		t.Errorf("feature^ = %s, want the commit on the unmoved %s", parent, feature)
	}
}

// TestShipGTRefusesAnOldGitBeforeAnyMutation pins that the graphite lane's
// ship, whose restack and --parent moves run on git replay --linearize, refuses
// a git older than 2.56 before it commits, tracks, or moves anything — and that
// --dry-run reports the same refusal.
func TestShipGTRefusesAnOldGitBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "run", args: []string{"-m", "fix: frobnicate", "--no-push"}},
		{name: "dry run", args: []string{"-m", "fix: frobnicate", "--no-push", "--dry-run"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := shipGTUnrestacked(t, "base2.txt", "base2\n")
			refs := stackReplayRefs(t, f)
			status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain")
			f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$1\" = version ]; then echo 'git version 2.55.0'; exit 0; fi\n"))

			_, err := runShipCmd(f.Context(), t, tt.args...)
			want := "ship: needs git 2.56 or newer for git replay --ref and --linearize, and found git 2.55.0 — upgrade git, then re-run"
			if err == nil || err.Error() != want {
				t.Fatalf("ship = %v, want %q", err, want)
			}
			if after := stackReplayRefs(t, f); after != refs {
				t.Errorf("refs changed:\n%s\nwant:\n%s", after, refs)
			}
			if after := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); after != status {
				t.Errorf("status = %q, want the uncommitted %q untouched", after, status)
			}
			for _, r := range shipGTRecords(t, f) {
				if len(r.Argv) > 0 && r.Argv[0] == "gt" {
					t.Errorf("ship ran %q before refusing", r.Argv)
				}
			}
		})
	}
}

// TestStackDropRefusesAnOldGitBeforeAnyChange pins the same floor on stack drop,
// whose restack of the dropped branch's children runs on git replay.
func TestStackDropRefusesAnOldGitBeforeAnyChange(t *testing.T) {
	t.Parallel()
	f := stackRebaseRepo(t, "base", "feature")
	refs := stackReplayRefs(t, f)
	f.PrependPATH(stackReplayGitWrapper(t, "if [ \"$1\" = version ]; then echo 'git version 2.55.0'; exit 0; fi\n"))

	_, _, err := runStackCmd(t, f, "drop", "base")
	want := dropPrefix + ": needs git 2.56 or newer for git replay --ref and --linearize, and found git 2.55.0 — upgrade git, then re-run"
	if err == nil || err.Error() != want {
		t.Fatalf("stack drop = %v, want %q", err, want)
	}
	if after := stackReplayRefs(t, f); after != refs {
		t.Errorf("refs changed:\n%s\nwant:\n%s", after, refs)
	}
}
