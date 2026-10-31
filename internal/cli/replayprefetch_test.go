package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

type replaySpan struct{ dir, from, head, side, onto string }

func replayPartialClone(t *testing.T) (*vcstest.Fixture, replaySpan) {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	from := stackReplayCommit(t, f, "lines.txt", "a\nb\nc\nd\ne\nf\ng\nh\n", "lines")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	var head string
	for _, n := range []string{"1", "2", "3"} {
		writeShipFile(t, f.Dir, "new"+n+".txt", n+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", "new"+n+".txt")
		head = stackReplayCommit(t, f, "lines.txt", "a\nb\nc\nd\ne\nf\ng\nh"+n+"\n", "feature "+n)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "side", from)
	side := stackReplayCommit(t, f, "side.txt", "side\n", "side")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "up.txt", "up\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "up.txt")
	onto := stackReplayCommit(t, f, "lines.txt", "A\nb\nc\nd\ne\nf\ng\nh\n", "upstream")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main", "feature", "side")
	mustRun(t, f.Env(), f.RemoteDir, "git", "config", "uploadpack.allowFilter", "true")
	return f, replaySpan{dir: replayClone(t, f), from: from, head: head, side: side, onto: onto}
}

func replayClone(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "partial")
	mustRun(t, f.Env(), filepath.Dir(dir), "git", "clone", "-q", "--no-local", "--no-checkout", "--filter=blob:none", f.RemoteDir, dir)
	for _, kv := range [][2]string{{"user.email", "t@t.t"}, {"user.name", "t"}, {"maintenance.auto", "false"}, {"gc.auto", "0"}} {
		mustRun(t, f.Env(), dir, "git", "config", kv[0], kv[1])
	}
	mustRun(t, f.Env(), dir, "git", "branch", "side", "origin/side")
	return dir
}

func replayNoLazy(t *testing.T, f *vcstest.Fixture, dir string, args ...string) int {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed argv against the fixture's TempDir
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), f.Env()...), "GIT_NO_LAZY_FETCH=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

func replayPromisorPacks(t *testing.T, dir string) int {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.promisor"))
	if err != nil {
		t.Fatal(err)
	}
	return len(packs)
}

func TestReplayFetchBlobsBatchesWhatTheReplayMerges(t *testing.T) {
	t.Parallel()
	f, s := replayPartialClone(t)
	replay := []string{"replay", "--ref-action=print", "--linearize", "--onto=" + s.onto, s.from + ".." + s.head}
	if code := replayNoLazy(t, f, s.dir, replay...); code == 0 {
		t.Fatal("git replay merged with lazy fetch disabled before any prefetch, want a fixture whose replay needs blobs")
	}
	packs := replayPromisorPacks(t, s.dir)

	if err := replayFetchBlobs(f.ContextIn(s.dir), render.Dir(s.dir), s.onto, s.from, s.head); err != nil {
		t.Fatalf("replayFetchBlobs: %v", err)
	}
	if got := replayPromisorPacks(t, s.dir); got != packs+1 {
		t.Errorf("promisor packs = %d after the prefetch, want %d: one batch for the whole span", got, packs+1)
	}
	if code := replayNoLazy(t, f, s.dir, replay...); code != 0 {
		t.Errorf("git replay with lazy fetch disabled exited %d after the prefetch, want 0", code)
	}
	for _, blob := range []string{s.onto + ":up.txt", s.onto + ":f.txt"} {
		if code := replayNoLazy(t, f, s.dir, "cat-file", "-e", blob); code == 0 {
			t.Errorf("the prefetch fetched %s, which no replayed commit touches", blob)
		}
	}
}

func TestReplayPromisorNamesOnlyABlobFilteredPromisor(t *testing.T) {
	t.Parallel()
	f, s := replayPartialClone(t)
	if got, err := replayPromisor(f.Context(), render.Dir(f.Dir)); err != nil || got != "" {
		t.Errorf("replayPromisor(full clone) = %q, %v, want no promisor", got, err)
	}
	for _, tt := range []struct {
		name, promisor, filter, want string
	}{
		{name: "blob:none", promisor: "true", filter: "blob:none", want: "origin"},
		{name: "blob:limit", promisor: "yes", filter: "blob:limit=1k", want: "origin"},
		{name: "tree filter", promisor: "true", filter: "tree:0"},
		{name: "not a promisor", promisor: "false", filter: "blob:none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mustRun(t, f.Env(), s.dir, "git", "config", "remote.origin.promisor", tt.promisor)
			mustRun(t, f.Env(), s.dir, "git", "config", "remote.origin.partialclonefilter", tt.filter)
			if got, err := replayPromisor(f.ContextIn(s.dir), render.Dir(s.dir)); err != nil || got != tt.want {
				t.Errorf("replayPromisor = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestGTReplaySurvivesAFailedPrefetch(t *testing.T) {
	t.Parallel()
	f, s := replayPartialClone(t)
	mustRun(t, f.Env(), s.dir, "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	ctx := f.ContextIn(s.dir)
	if err := replayFetchBlobs(ctx, render.Dir(s.dir), s.onto, s.from, s.side); err == nil {
		t.Fatal("replayFetchBlobs against a missing remote succeeded, want its fetch to fail")
	}

	got, err := gtReplay(ctx, "test", render.Dir(s.dir), s.onto, s.from, "side", gtBranchState{Head: s.side})
	if err != nil {
		t.Fatalf("gtReplay after a failed prefetch: %v", err)
	}
	if parent := gitAt(t, f.Env(), s.dir, "rev-parse", got+"^"); parent != s.onto {
		t.Errorf("replayed side onto %s, want %s", parent, s.onto)
	}
	if subjects := gitAt(t, f.Env(), s.dir, "log", "--format=%s", s.onto+".."+got); subjects != "side" {
		t.Errorf("replayed subjects = %q, want side", subjects)
	}
}
