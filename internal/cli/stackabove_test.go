package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
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
	oldB := gitAt(t, f.Env(), f.Dir, "rev-parse", "c^")
	oldC := gitAt(t, f.Env(), f.Dir, "rev-parse", "c")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "b")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above b: %v\n%s%s", err, plan, errOut)
	}
	for _, want := range []string{
		"a · kept at its published head " + heads["a"][:12],
		"b · kept at its published head " + heads["b"][:12],
		"c · onto b · from " + oldB[:12] + " · replays 1 commit",
		"d · onto c · from " + oldC[:12] + " · replays 1 commit",
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
	if !strings.Contains(out, "kept local a at "+stale[:12]) {
		t.Errorf("report lacks a kept local:\n%s", out)
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

func stackNativeChain(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	for _, name := range names {
		shipGTUntracked(t, f, name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--all")
	return f
}

func stackNativePRs(t *testing.T, f *vcstest.Fixture, bottom string, above ...string) {
	t.Helper()
	const stack = 46
	api := stubGTAPI(t)
	api.openPRs(append([]string{bottom}, above...)...)
	prs := map[string]*stackPR{bottom: {Number: api.prs[bottom], Title: bottom, State: "OPEN", Base: "main", NativeStack: stack}}
	api.bases[bottom], api.stacks[bottom] = "main", stack
	parent := bottom
	for _, name := range above {
		api.bases[name], api.stacks[name] = parent, stack
		prs[name] = &stackPR{Number: api.prs[name], Title: name, State: "OPEN", Base: parent, NativeStack: stack}
		parent = name
	}
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, prs)
	stubStackStanding(t, f, nil)
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	base := gitAt(t, f.Env(), f.Dir, "merge-base", bottom, "origin/main")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", bottom)
	if err := nativemeta.AdoptRoot(f.Context(), common, bottom, "main", base, head); err != nil {
		t.Fatal(err)
	}
}

func stackReplayBottomOffLandedParent(t *testing.T, f *vcstest.Fixture, bottom, tip string) {
	t.Helper()
	old := gitAt(t, f.Env(), f.Dir, "rev-parse", bottom)
	restackAdvanceRemote(t, f, "main", "landed-squash.txt", "landed\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", bottom)
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", old)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", bottom)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", tip)
}

func TestStackRebaseAboveReplaysAChildFromItsParentsHeadBeforeTheRewrite(t *testing.T) {
	f := stackNativeChain(t, "landed", "a", "b", "c")
	oldA := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	oldB := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	stackReplayBottomOffLandedParent(t, f, "a", "c")
	stackNativePRs(t, f, "a", "b", "c")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "a")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above a: %v\n%s%s", err, plan, errOut)
	}
	for _, want := range []string{
		"b · onto a · from " + oldA[:12] + " · replays 1 commit",
		"c · onto b · from " + oldB[:12] + " · replays 1 commit",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}

	out, errOut, err := runStackCmd(t, f, "rebase", "--no-push", "--above", "a")
	if err != nil {
		t.Fatalf("stack rebase --no-push --above a: %v\n%s%s", err, out, errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "--reverse", "--format=%s", "origin/main..c"); got != "a\nb\nc" {
		t.Errorf("commits above trunk = %q, want a, b, c, with the landed parent's commit left behind", got)
	}
}

func TestStackRebaseAboveCountsTheCommitsWhenNoReflogKnowsTheParentsOldHead(t *testing.T) {
	f := stackNativeChain(t, "landed", "a", "b", "c")
	fork := gitAt(t, f.Env(), f.Dir, "merge-base", "c", "origin/main")
	stackReplayBottomOffLandedParent(t, f, "a", "c")
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")
	stackNativePRs(t, f, "a", "b", "c")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "a")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above a: %v\n%s%s", err, plan, errOut)
	}
	if want := "b · onto a · from " + fork[:12] + " · replays 3 commits"; !strings.Contains(plan, want) {
		t.Errorf("plan lacks %q:\n%s", want, plan)
	}
}

func TestStackRebaseAboveKeepsTheCommitsAChildsParentWasResetBelow(t *testing.T) {
	f := stackNativeChain(t, "a", "moved", "b", "c")
	a := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "a", "moved")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "a", a)
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "moved")
	stackNativePRs(t, f, "a", "b", "c")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "a")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above a: %v\n%s%s", err, plan, errOut)
	}
	if want := "b · onto a · from " + a[:12] + " · replays 2 commits"; !strings.Contains(plan, want) {
		t.Errorf("plan lacks %q:\n%s", want, plan)
	}
}

func TestStackRebaseAboveReadsAnUnmovedParentAsBefore(t *testing.T) {
	f := stackNativeChain(t, "a", "b", "c")
	a := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	b := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	stackNativePRs(t, f, "a", "b", "c")

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--above", "a")
	if err != nil {
		t.Fatalf("stack rebase --dry-run --above a: %v\n%s%s", err, plan, errOut)
	}
	for _, want := range []string{
		"b · onto a · from " + a[:12] + " · replays 1 commit",
		"c · onto b · from " + b[:12] + " · replays 1 commit",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
}
