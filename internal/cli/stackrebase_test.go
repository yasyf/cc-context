package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stubStackPRs(t *testing.T, prs map[string]*stackPR) {
	t.Helper()
	prev := stackPRLookup
	stackPRLookup = func(_ context.Context, _ render.Dir, _ string, branches []string) (map[string]*stackPR, error) {
		out := map[string]*stackPR{}
		for _, b := range branches {
			if pr, ok := prs[b]; ok {
				out[b] = pr
			}
		}
		return out, nil
	}
	t.Cleanup(func() { stackPRLookup = prev })
}

func runStackCmdIn(t *testing.T, f *vcstest.Fixture, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newStackCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(f.ContextIn(dir))
	return strings.TrimSpace(out.String()), errOut.String(), err
}

func stackRebaseRepo(t *testing.T, names ...string) *vcstest.Fixture {
	t.Helper()
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	shipGTStack(t, f, names...)
	return f
}

func stackAdvanceTrunk(t *testing.T, f *vcstest.Fixture, file, content string) {
	t.Helper()
	restackAdvanceRemote(t, f, "main", file, content)
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
}

func stackOnto(t *testing.T, f *vcstest.Fixture, ancestor, branch string) bool {
	t.Helper()
	ok, err := gitIsAncestor(f.Context(), render.Dir(f.Dir), "test", ancestor, branch)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func stackParent(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	return state[branch].Parents[0].Ref
}

func TestStackRebaseMovesAndPushesTheWholeStack(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	for _, branch := range []string{"base", "feature"} {
		if !stackOnto(t, f, "origin/main", branch) {
			t.Errorf("%s is not on the new trunk", branch)
		}
		local := gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
		if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); remote != local {
			t.Errorf("origin %s = %s, want the rebased %s", branch, remote, local)
		}
		if !strings.Contains(out, branch+shipSep+"no pull request"+shipSep+"head "+local[:12]) {
			t.Errorf("output = %q, want a verdict line for %s", out, branch)
		}
	}
	if !strings.Contains(out, "rebased 2 branches onto main@") {
		t.Errorf("output = %q, want the summary", out)
	}
	if _, err := os.Stat(stackStatePath(filepath.Join(f.Dir, ".git"))); !os.IsNotExist(err) {
		t.Errorf("run state left behind: %v", err)
	}
}

func TestStackRebaseDropsASquashLandedParent(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	baseCommit := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	stackAdvanceTrunk(t, f, "base.txt", "base\n")
	stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, Title: "base", State: "CLOSED", Landed: true}})
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if !strings.Contains(out, "base"+shipSep+"drop (#5 landed)") {
		t.Errorf("plan = %q, want base dropped", out)
	}
	if got := stackParent(t, f, "feature"); got != "main" {
		t.Errorf("feature's gt parent = %s, want main", got)
	}
	if stackOnto(t, f, baseCommit, "feature") {
		t.Errorf("feature still carries base's own commit %s", baseCommit)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..feature"); n != "1" {
		t.Errorf("feature holds %s commits over trunk, want its own 1", n)
	}
}

func TestStackRebasePushesWithADivergedLocalTrunk(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "local.txt", "local\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "local.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "local trunk work")
	localTrunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	_, errOut, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if strings.Contains(errOut, "local main holds") {
		t.Errorf("stderr = %q, want no local trunk warning", errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); got != localTrunk {
		t.Errorf("local main = %s, want unchanged %s", got, localTrunk)
	}
	for _, branch := range []string{"base", "feature"} {
		if !stackOnto(t, f, "origin/main", branch) {
			t.Errorf("%s is not on the remote trunk", branch)
		}
		if stackOnto(t, f, localTrunk, branch) {
			t.Errorf("%s carries the local trunk's unpushed commit", branch)
		}
		local := gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
		if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); remote != local {
			t.Errorf("origin %s = %s, want the rebased %s", branch, remote, local)
		}
	}
}

func TestStackRebaseLinearizesSiblings(t *testing.T) {
	f := stackRebaseRepo(t, "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "b")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push", "--linearize", "a,b"); err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if got := stackParent(t, f, "b"); got != "a" {
		t.Errorf("b's gt parent = %s, want a", got)
	}
	if !stackOnto(t, f, "a", "b") {
		t.Error("b does not sit on a")
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "a..b"); n != "1" {
		t.Errorf("b holds %s commits over a, want 1", n)
	}
}

func TestStackRebaseRefusesABranchAnotherWorkingCopyHolds(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	before := map[string]string{}
	for _, branch := range []string{"base", "feature"} {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}
	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "checked out in "+held) {
		t.Fatalf("rebase = %v, want holder refusal", err)
	}
	for branch, head := range before {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != head {
			t.Errorf("%s moved to %s before refusal", branch, got)
		}
	}
	if dirt := gitAt(t, f.Env(), held, "status", "--porcelain"); dirt != "" {
		t.Errorf("held reads dirty: %q", dirt)
	}
	mustRun(t, f.Env(), held, "git", "switch", "--detach", "-q")
	if _, _, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue after detaching holder: %v", err)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature did not reach pinned trunk")
	}
}

func TestStackRebaseRefusesDirtyOriginBeforeMovingRefs(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	writeShipFile(t, f.Dir, "feature.txt", "uncommitted\n")
	before := map[string]string{}
	for _, branch := range []string{"base", "feature"} {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}
	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "uncommitted work") {
		t.Fatalf("rebase = %v, want dirty refusal", err)
	}
	for branch, head := range before {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != head {
			t.Errorf("%s moved to %s before refusal", branch, got)
		}
	}
	if got := restackRead(t, filepath.Join(f.Dir, "feature.txt")); got != "uncommitted\n" {
		t.Errorf("work changed to %q", got)
	}
}

func TestStackRebaseConflictOpensAWorkspaceAndContinues(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 7, Title: "feature work", Body: "adds c.txt", State: "OPEN"}})
	stackConflicting(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	for _, want := range []string{"feature does not rebase onto base cleanly", "c.txt", `#7 "feature work"`, "adds c.txt", "ccx vcs stack continue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("brief = %v, want %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base moved to %s before the conflict was resolved", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved to %s before the conflict was resolved", got)
	}
	_, rest, _ := strings.Cut(err.Error(), "workspace: ")
	ws, _, _ := strings.Cut(rest, " (detached")
	if filepath.Base(ws) != "conflict-feature" {
		t.Fatalf("workspace = %q, want a pool entry named conflict-feature", ws)
	}
	var rerereOff bool
	for _, inv := range shipGTInvocations(t, f) {
		if slices.Contains(inv, "rebase") && slices.Contains(inv, "rerere.enabled=false") {
			rerereOff = true
		}
	}
	if !rerereOff {
		t.Error("the workspace rebase ran with rerere on")
	}

	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), "still has unresolved files: c.txt") {
		t.Fatalf("continue over an unresolved file = %v, want a refusal", err)
	}
	writeShipFile(t, ws, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !strings.Contains(out, "resolved feature") {
		t.Errorf("continue output = %q", out)
	}
	if !stackOnto(t, f, "base", "feature") || !stackOnto(t, f, "main", "base") {
		t.Error("the stack did not land on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:c.txt"); got != "trunk\nfeature" {
		t.Errorf("feature's c.txt = %q, want the resolution", got)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace %s left behind: %v", ws, err)
	}
}

func TestStackAbortDropsTheRun(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	if _, _, err := runStackCmd(t, f, "rebase"); err == nil {
		t.Fatal("stack rebase succeeded, want the conflict")
	}
	if _, _, err := runStackCmd(t, f, "rebase"); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second rebase = %v, want the in-progress refusal", err)
	}
	out, _, err := runStackCmd(t, f, "abort")
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if out != "aborted · no branch moved" {
		t.Errorf("abort output = %q", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature = %s, want %s", got, feature)
	}
	if list := gitAt(t, f.Env(), f.Dir, "worktree", "list"); strings.Contains(list, "conflict-feature") {
		t.Errorf("workspace still registered: %s", list)
	}
}

func TestStackRebaseRefusesADivergedRemote(t *testing.T) {
	f := stackRebaseRepo(t, "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "base amended")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil || !strings.Contains(err.Error(), "base has diverged from origin/base") {
		t.Fatalf("err = %v, want the divergence refusal", err)
	}
}

func TestStackRebaseTakesARemoteThatIsAhead(t *testing.T) {
	f := stackRebaseRepo(t, "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	clone := filepath.Join(t.TempDir(), "other")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "base", f.RemoteDir, clone)
	writeShipFile(t, clone, "more.txt", "more\n")
	mustRun(t, f.Env(), clone, "git", "add", "more.txt")
	mustRun(t, f.Env(), clone, "git", "-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "-qm", "more")
	mustRun(t, f.Env(), clone, "git", "push", "-q", "origin", "base")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase"); err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "base:more.txt"); got != "more" {
		t.Errorf("base lost the remote's commit: more.txt = %q", got)
	}
	if !stackOnto(t, f, "origin/main", "base") {
		t.Error("base is not on the new trunk")
	}
	if refs := gitAt(t, f.Env(), f.Dir, "for-each-ref", "refs/heads/"+stackRebaseStateDir); refs != "" {
		t.Errorf("temporary refs left behind: %s", refs)
	}
}

func TestStackRebaseDryRunMovesNothing(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "feature"+shipSep+"onto base") {
		t.Errorf("plan = %q", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != feature {
		t.Errorf("feature moved on a dry run")
	}
}

func TestStackRebaseRefusesALandedBranchWithCommitsPastItsLanding(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base~0")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	writeShipFile(t, f.Dir, "later.txt", "later\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "later.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "later")
	stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true}})
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "holds commits past it") {
		t.Fatalf("err = %v, want the refusal to drop unlanded commits", err)
	}
}

func TestGTPushArgvPinsAnAbsentRemote(t *testing.T) {
	t.Parallel()
	argv := gtPushArgv(gtSubmit{}, []gtSubmitBranch{{name: "new", head: "abc", leaseSet: true}})
	if !slices.Contains(argv, "--force-with-lease=refs/heads/new:") {
		t.Errorf("argv = %v, want the lease to require the branch absent", argv)
	}
}

func TestStackContinueRefusesConcurrentLocalAdvance(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil {
		t.Fatal("expected conflict")
	}
	run, err := stackLoadRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	ws := run.Conflict.Workspace
	writeShipFile(t, ws, "c.txt", "resolved\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "--allow-empty", "-qm", "concurrent work")
	advanced := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	_, _, err = runStackCmd(t, f, "continue")
	if err == nil || !strings.Contains(err.Error(), "branch moved locally") {
		t.Fatalf("continue = %v, want ref transaction refusal", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != advanced {
		t.Errorf("concurrent work replaced with %s", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
		t.Errorf("base partially moved to %s", got)
	}
}

func TestStackContinueRefusesConcurrentRemoteAdvance(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if _, _, err := runStackCmd(t, f, "rebase"); err == nil {
		t.Fatal("expected conflict")
	}
	run, err := stackLoadRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	ws := run.Conflict.Workspace
	writeShipFile(t, ws, "c.txt", "resolved\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	restackAdvanceRemote(t, f, "feature", "concurrent.txt", "concurrent\n")
	advanced := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	_, _, err = runStackCmd(t, f, "continue")
	if err == nil {
		t.Fatal("continue overwrote a remote advance")
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != advanced {
		t.Errorf("remote work replaced with %s", got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("remote base partially moved to %s", got)
	}
}
