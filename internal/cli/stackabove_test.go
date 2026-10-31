package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackBottomMovedByGraphite(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c", "d")
	stubOpenPRs(t, f, nil, "a", "b", "c", "d")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c", "d")
	was := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	clone := filepath.Join(t.TempDir(), "graphite")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "config", "user.email", "t@t.t")
	mustRun(t, f.Env(), clone, "git", "config", "user.name", "t")
	mustRun(t, f.Env(), clone, "git", "branch", "a", "origin/a")
	mustRun(t, f.Env(), clone, "git", "switch", "-q", "b")
	mustRun(t, f.Env(), clone, "git", "rebase", "-q", "--update-refs", "--onto", "origin/main", was, "b")
	mustRun(t, f.Env(), clone, "git", "push", "-qf", "origin", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "a", "origin/a")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "b", "origin/b")
	return f
}

func TestStackRebaseAboveMovesOnlyTheBranchesAboveIt(t *testing.T) {
	f := stackBottomMovedByGraphite(t)
	heads := map[string]string{}
	for _, name := range []string{"main", "a", "b"} {
		heads[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
	}
	localMain := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "b")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above b: %v\n%s%s", err, plan, errOut)
	}
	for _, want := range []string{
		"a · kept at its published head " + heads["a"][:12],
		"b · kept at its published head " + heads["b"][:12],
		"c · onto b",
		"d · onto c",
		"pushes c, d",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}

	out, errOut, err := runStackCmd(t, f, "rebase", "--above", "b")
	if err != nil {
		t.Fatalf("stack rebase --above b: %v\n%s%s", err, out, errOut)
	}
	for name, head := range heads {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != head {
			t.Errorf("origin %s = %.12s, want it left at %.12s", name, got, head)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); got != localMain {
		t.Errorf("local main = %.12s, want it untouched at %.12s", got, localMain)
	}
	c := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", c+"^"); got != heads["b"] {
		t.Errorf("origin c sits on %.12s, want b's published head %.12s", got, heads["b"])
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "d^"); got != c {
		t.Errorf("origin d sits on %.12s, want c %.12s", got, c)
	}
}

func TestStackRebaseAboveReadsADivergedBranchBelowItAtItsPublishedHead(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "a", "b", "c", "d")
	stubOpenPRs(t, f, nil, "a", "b", "c", "d")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b", "c", "d")
	stale := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	clone := filepath.Join(t.TempDir(), "other-lane")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "config", "user.email", "t@t.t")
	mustRun(t, f.Env(), clone, "git", "config", "user.name", "t")
	mustRun(t, f.Env(), clone, "git", "switch", "-q", "a")
	writeShipFile(t, clone, "a.txt", "another lane's a\n")
	mustRun(t, f.Env(), clone, "git", "commit", "-qa", "--amend", "--no-edit")
	mustRun(t, f.Env(), clone, "git", "switch", "-q", "b")
	mustRun(t, f.Env(), clone, "git", "rebase", "-q", "--onto", "a", stale, "b")
	mustRun(t, f.Env(), clone, "git", "push", "-qf", "origin", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "b", "origin/b")
	heads := map[string]string{}
	for _, name := range []string{"a", "b"} {
		heads[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
	}

	out, errOut, err := runStackCmd(t, f, "rebase", "--above", "b")
	if err != nil {
		t.Fatalf("stack rebase --above b with a stale local a: %v\n%s%s", err, out, errOut)
	}
	for name, head := range heads {
		if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name); got != head {
			t.Errorf("origin %s = %.12s, want it left at %.12s", name, got, head)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "a"); got != stale {
		t.Errorf("local a = %.12s, want it left at %.12s", got, stale)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c^"); got != heads["b"] {
		t.Errorf("origin c sits on %.12s, want b's published head %.12s", got, heads["b"])
	}
}

func TestStackRebaseAboveRefusesABranchOutsideTheStack(t *testing.T) {
	f := stackBottomMovedByGraphite(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "other", "main")
	writeShipFile(t, f.Dir, "other.txt", "other\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "other.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "other")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "d")

	for above, want := range map[string]string{
		"main":  "--above main is not in d's stack",
		"other": "--above other is not in d's stack",
		"d":     "--above d has no branch stacked on it",
	} {
		_, _, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", above)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("stack rebase --above %s = %v, want %q", above, err, want)
		}
	}
}
