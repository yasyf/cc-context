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
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtmeta"
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
	f := shipGTRepo(t, vcstest.GTStack(names...))
	stubStackPRs(t, nil)
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

func stackRebaseSourceSnapshot(t *testing.T, f *vcstest.Fixture, branches ...string) map[string]stackPublication {
	t.Helper()
	sources := map[string]stackPublication{}
	for _, branch := range branches {
		parent := stackParent(t, f, branch)
		sources[branch] = stackPublication{
			Branch:     branch,
			Source:     gitAt(t, f.Env(), f.Dir, "rev-parse", branch),
			SourceBase: gitAt(t, f.Env(), f.Dir, "merge-base", branch, parent),
			Parent:     parent,
		}
	}
	return sources
}

func stackAssertRebasePublication(t *testing.T, f *vcstest.Fixture, source stackPublication) string {
	t.Helper()
	branch := source.Branch
	if local := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); local != source.Source {
		t.Errorf("local %s = %s, want unchanged %s", branch, local, source.Source)
	}
	want := source
	want.Head = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	want.Base = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", source.Parent)
	want.OID = gitAt(t, f.Env(), f.Dir, "rev-parse", stackPublicationRef(branch, "receipt"))
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), branch)
	if err != nil || receipt == nil {
		t.Fatalf("publication for %s = %+v, %v", branch, receipt, err)
	}
	if *receipt != want {
		t.Errorf("publication for %s = %+v, want %+v", branch, *receipt, want)
	}
	if !stackOnto(t, f, want.Base, want.Head) {
		t.Errorf("published %s is not on published %s", branch, source.Parent)
	}
	return want.Head
}

func TestStackRebasePublishesTheWholeStackWithoutMovingSources(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	sources := stackRebaseSourceSnapshot(t, f, "base", "feature")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	for _, branch := range []string{"base", "feature"} {
		published := stackAssertRebasePublication(t, f, sources[branch])
		if !stackOnto(t, f, "origin/main", published) {
			t.Errorf("published %s is not on the new trunk", branch)
		}
		if !strings.Contains(out, branch+shipSep+"no pull request"+shipSep+"head "+published[:12]) {
			t.Errorf("output = %q, want a verdict line for %s", out, branch)
		}
	}
	if !strings.Contains(out, "published 2 branches · source checkouts unchanged") {
		t.Errorf("output = %q, want the publication summary", out)
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
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
	sources := stackRebaseSourceSnapshot(t, f, "base", "feature")
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
		published := stackAssertRebasePublication(t, f, sources[branch])
		if !stackOnto(t, f, "origin/main", published) {
			t.Errorf("published %s is not on the remote trunk", branch)
		}
		if stackOnto(t, f, localTrunk, published) {
			t.Errorf("published %s carries the local trunk's unpushed commit", branch)
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

// TestStackRebaseRefusesToOverwriteAnIgnoredFile pins the file git status never
// lists: an ignored file at a path the new trunk tracks, which read-tree -u
// would replace without a word once the refs had already moved.
func TestStackRebaseRefusesToOverwriteAnIgnoredFile(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "gen.txt", "upstream\n")
	restackWrite(t, filepath.Join(f.Dir, ".git", "info", "exclude"), "gen.txt\n")
	writeShipFile(t, f.Dir, "gen.txt", "mine\n")
	before := map[string]string{}
	for _, branch := range []string{"base", "feature"} {
		before[branch] = gitAt(t, f.Env(), f.Dir, "rev-parse", branch)
	}

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "gen.txt") {
		t.Fatalf("rebase = %v, want a refusal naming gen.txt", err)
	}
	for branch, head := range before {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != head {
			t.Errorf("%s moved to %s before the refusal", branch, got)
		}
	}
	if got := restackRead(t, filepath.Join(f.Dir, "gen.txt")); got != "mine\n" {
		t.Errorf("gen.txt = %q, want the ignored file left as it was", got)
	}
}

func TestStackRebaseConflictOpensAWorkspaceAndContinues(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
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
	ws := stackWorkspaceOf(t, err)
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

func TestStackContinueReturnsWhileTheWorkspaceIsStillBeingDeleted(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	marker := stackStallRm(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	if _, _, err := runStackCmdIn(t, f, ws, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	stackAssertDiscarded(t, f, ws, marker)
}

func TestStackAbortSucceedsWhenRemovingTheWorkspaceIsKilled(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	marker := stackStallRm(t, f)
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "git"), "#!/bin/sh\ncase \"$*\" in *\"worktree remove\"*) kill -TERM $$;; esac\nPATH=${PATH#"+bin+":} exec git \"$@\"\n")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on feature")
	}
	ws := stackWorkspaceOf(t, err)
	f.PrependPATH(bin)
	if _, _, err := runStackCmd(t, f, "abort"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	stackAssertDiscarded(t, f, ws, marker)
	if slots, _ := os.ReadDir(filepath.Join(f.Dir, ".git", "ccx-stack-rebase")); len(slots) != 0 {
		t.Errorf("abort left %d rebase slots", len(slots))
	}
}

func stackStallRm(t *testing.T, f *vcstest.Fixture) string {
	t.Helper()
	bin := t.TempDir()
	marker, fifo := filepath.Join(bin, "rm-args"), filepath.Join(bin, "rm-gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "rm"), "#!/bin/sh\necho \"$@\" > "+marker+"\ncat "+fifo+"\n")
	t.Cleanup(func() {
		if gate, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			if err := gate.Close(); err != nil {
				t.Errorf("close rm gate: %v", err)
			}
		}
	})
	f.PrependPATH(bin)
	return marker
}

func stackAssertDiscarded(t *testing.T, f *vcstest.Fixture, ws, marker string) {
	t.Helper()
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Errorf("workspace %s still at its path: %v", ws, err)
	}
	if listed := gitAt(t, f.Env(), f.Dir, "worktree", "list", "--porcelain"); strings.Contains(listed, ws) {
		t.Errorf("workspace %s still registered:\n%s", ws, listed)
	}
	var args string
	for deadline := time.Now().Add(10 * time.Second); args == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		raw, _ := os.ReadFile(marker)
		args = strings.TrimSpace(string(raw))
	}
	aside, ok := strings.CutPrefix(args, "-rf ")
	if !ok || filepath.Dir(aside) != filepath.Dir(ws) || !strings.HasPrefix(filepath.Base(aside), "."+filepath.Base(ws)+".") {
		t.Fatalf("rm ran with %q, want -rf on the workspace moved aside next to %s", args, ws)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the command waited for the deletion of %s: %v", aside, err)
	}
}

func stackWorkspaceOf(t *testing.T, err error) string {
	t.Helper()
	_, rest, _ := strings.Cut(err.Error(), "workspace: ")
	ws, _, _ := strings.Cut(rest, " (detached")
	if ws == "" {
		t.Fatalf("no workspace in %v", err)
	}
	return ws
}

func TestStackRebaseRunsTwoStacksSideBySide(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	shipGTStack(t, f, "a-base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "a-top")
	writeShipFile(t, f.Dir, "c.txt", "a\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "a-top")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "b-top")
	writeShipFile(t, f.Dir, "d.txt", "b\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "d.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "b-top")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	restackAdvanceRemote(t, f, "main", "d.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a-top")
	held := restackSiblingPath(t, "held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "b-top")
	shipResetLog(t, f)

	_, _, errA := runStackCmd(t, f, "rebase", "--no-push")
	if errA == nil {
		t.Fatal("stack rebase of a succeeded, want the conflict on a-top")
	}
	_, _, errB := runStackCmdIn(t, f, held, "rebase", "--no-push")
	if errB == nil || !strings.Contains(errB.Error(), "b-top does not rebase onto main cleanly") {
		t.Fatalf("stack rebase of b = %v, want its own conflict, not a refusal", errB)
	}
	wsA, wsB := stackWorkspaceOf(t, errA), stackWorkspaceOf(t, errB)
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil || !strings.Contains(err.Error(), "a stack rebase of a-base is already in progress") {
		t.Fatalf("second rebase of a = %v, want the in-progress refusal", err)
	}

	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), wsA+" still has unresolved files: c.txt") {
		t.Fatalf("continue from a-top = %v, want a's unresolved c.txt", err)
	}
	writeShipFile(t, wsB, "d.txt", "trunk\nb\n")
	mustRun(t, f.Env(), wsB, "git", "add", "d.txt")
	aTop := gitAt(t, f.Env(), f.Dir, "rev-parse", "a-top")
	if out, _, err := runStackCmdIn(t, f, wsB, "continue"); err != nil || !strings.Contains(out, "resolved b-top") {
		t.Fatalf("continue from b's workspace = %q, %v", out, err)
	}
	if !stackOnto(t, f, "origin/main", "b-top") {
		t.Error("b-top is not on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "a-top"); got != aTop {
		t.Errorf("finishing b moved a-top to %s", got)
	}

	writeShipFile(t, wsA, "c.txt", "trunk\na\n")
	mustRun(t, f.Env(), wsA, "git", "add", "c.txt")
	if out, _, err := runStackCmd(t, f, "continue"); err != nil || !strings.Contains(out, "resolved a-top") {
		t.Fatalf("continue from a-top = %q, %v", out, err)
	}
	if !stackOnto(t, f, "a-base", "a-top") || !stackOnto(t, f, "origin/main", "a-base") {
		t.Error("stack a did not land on the new trunk")
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
	}
}

func stackPlantRun(t *testing.T, f *vcstest.Fixture, age time.Duration, roots ...string) int {
	t.Helper()
	return stackPlantConflict(t, f, age, nil, roots...)
}

func stackPlantConflict(t *testing.T, f *vcstest.Fixture, age time.Duration, conflict *stackConflict, roots ...string) int {
	t.Helper()
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	run := &stackRebaseRun{Trunk: "main", Roots: roots, Pid: exited.Process.Pid, Host: host, Conflict: conflict, dir: stackRunDir(filepath.Join(f.Dir, ".git"), roots[0])}
	if err := os.MkdirAll(run.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(stackStatePath(run.dir), then, then); err != nil {
		t.Fatal(err)
	}
	return run.Pid
}

func TestStackRebaseRefusesARecentRunOfADeadProcess(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	pid := stackPlantRun(t, f, time.Minute, "base")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	want := fmt.Sprintf("a stack rebase of base is already in progress (pid %d on ", pid)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), " exited, last saved 1m") {
		t.Fatalf("err = %v, want the refusal naming base and its exited holder", err)
	}
}

func TestStackRebaseKeepsAStaleRunWaitingOnItsWorkspace(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	ws := t.TempDir()
	stackPlantConflict(t, f, stackStaleAfter+time.Minute, &stackConflict{Branch: "feature", Workspace: ws}, "base")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "a stack rebase of base is already in progress (stopped on feature in "+ws) {
		t.Fatalf("err = %v, want the refusal naming the waiting workspace", err)
	}
}

func TestStackRebaseReclaimsAStaleRun(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	stackPlantRun(t, f, stackStaleAfter+time.Minute, "base")

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if !strings.Contains(out, "reclaimed the stale stack rebase of base") {
		t.Errorf("output = %q, want the reclaim line", out)
	}
	if !stackOnto(t, f, "origin/main", "base") {
		t.Error("base is not on the new trunk")
	}
}

func stackPlantLive(t *testing.T, f *vcstest.Fixture, branches ...string) *stackRebaseRun {
	t.Helper()
	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	run := &stackRebaseRun{Trunk: "main", Roots: branches[:1], Pid: live.Process.Pid, Started: stackProcStart(live.Process.Pid), Host: host, dir: stackRunDir(filepath.Join(f.Dir, ".git"), branches[0])}
	for _, b := range branches {
		run.Branches = append(run.Branches, stackRebaseBranch{Name: b})
	}
	if err := os.MkdirAll(run.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestStackContinueRefusesARunAnotherProcessDrives(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	live := stackPlantLive(t, f, "base", "feature")

	for _, verb := range []string{"continue", "abort"} {
		_, _, err := runStackCmd(t, f, verb)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("pid %d on ", live.Pid)) || !strings.Contains(err.Error(), "is still driving the stack rebase of base") {
			t.Fatalf("%s beside a live run = %v, want the refusal naming its pid", verb, err)
		}
	}
	if runs, err := stackRuns(filepath.Join(f.Dir, ".git")); err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v, want the live run kept", runs, err)
	}
}

func TestStackRebaseRefusesALiveRunBeforePlanning(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackPlantLive(t, f, "base", "feature")
	prev := stackPRLookup
	stackPRLookup = func(context.Context, render.Dir, string, []string) (map[string]*stackPR, error) {
		t.Error("the rebase planned beside a live run of its own branch")
		return nil, nil
	}
	t.Cleanup(func() { stackPRLookup = prev })

	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil || !strings.Contains(err.Error(), "a stack rebase of base is already in progress") {
		t.Fatalf("rebase beside a live run = %v, want the in-progress refusal", err)
	}
}

func TestStackPidAliveRejectsAReusedPid(t *testing.T) {
	run := &stackRebaseRun{Pid: os.Getpid(), Started: stackProcStart(os.Getpid())}
	if !stackPidAlive(run) {
		t.Fatalf("stackPidAlive(own pid, own start %q) = false", run.Started)
	}
	run.Started = "Thu Jan  1 00:00:00 1970"
	if stackPidAlive(run) {
		t.Error("stackPidAlive took a live pid with another start time for the run's process")
	}
}

func TestStackWriteRefsTakesARefAlreadyWritten(t *testing.T) {
	f := stackRebaseRepo(t, "base")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	moved := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	run := &stackRebaseRun{Branches: []stackRebaseBranch{{Name: "base", Local: local, NewHead: moved}}}
	for range 2 {
		if err := stackWriteRefs(f.Context(), render.Dir(f.Dir), run); err != nil {
			t.Fatalf("stackWriteRefs: %v", err)
		}
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != moved {
		t.Errorf("base = %s, want %s", got, moved)
	}
}

func TestStackAbortDropsTheRun(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	hooks := t.TempDir()
	postCheckout := "#!/bin/sh\nmkdir -p node_modules\n"
	writeExecutable(t, filepath.Join(hooks, "post-checkout"), postCheckout)
	gitAt(t, f.Env(), f.Dir, "config", "core.hooksPath", hooks)
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	_, _, conflict := runStackCmd(t, f, "rebase")
	if conflict == nil {
		t.Fatal("stack rebase succeeded, want the conflict")
	}
	if _, err := os.Stat(filepath.Join(stackWorkspaceOf(t, conflict), "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("conflict workspace warmed node_modules: %v", err)
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
	stackForeignPush(t, f, "base", "foreign.txt", false)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "base amended")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil || !strings.Contains(err.Error(), "base has diverged from origin/base") {
		t.Fatalf("err = %v, want the divergence refusal", err)
	}
}

func stackRecordSubmitted(t *testing.T, f *vcstest.Fixture, branches ...string) {
	t.Helper()
	versions := map[string]gtmeta.Version{}
	for _, b := range branches {
		versions[b] = gtmeta.Version{HeadSha: gitAt(t, f.Env(), f.Dir, "rev-parse", b)}
	}
	if err := gtmeta.RecordSubmitted(f.Context(), filepath.Join(f.Dir, ".git"), versions); err != nil {
		t.Fatal(err)
	}
}

func TestStackRebasePushesOverItsOwnLastSubmission(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	stackRecordSubmitted(t, f, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase --no-push: %v", err)
	}
	sourceBase := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	sources := stackRebaseSourceSnapshot(t, f, "base", "feature")
	base := sources["base"]
	base.SourceBase = sourceBase
	sources["base"] = base
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase"); err != nil {
		t.Fatalf("stack rebase over its own last submission: %v", err)
	}
	for _, branch := range []string{"base", "feature"} {
		published := stackAssertRebasePublication(t, f, sources[branch])
		if !stackOnto(t, f, "origin/main", published) {
			t.Errorf("published %s is not on the new trunk", branch)
		}
	}
}

func TestStackRebaseRefusesAForeignPushOverItsLastSubmission(t *testing.T) {
	f := stackRebaseRepo(t, "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	stackRecordSubmitted(t, f, "base")
	stackForeignPush(t, f, "base", "foreign.txt", true)
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "base amended locally")
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
	sources := stackRebaseSourceSnapshot(t, f, "base")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "rebase"); err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	published := stackAssertRebasePublication(t, f, sources["base"])
	if got := gitAt(t, f.Env(), f.Dir, "show", published+":more.txt"); got != "more" {
		t.Errorf("base lost the remote's commit: more.txt = %q", got)
	}
	if !stackOnto(t, f, "origin/main", published) {
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
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil {
		t.Fatal("expected conflict")
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
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
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if _, _, err := runStackCmd(t, f, "rebase"); err == nil {
		t.Fatal("expected conflict")
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
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

func TestStackContinueDropsAParentThatLandedMidRun(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	source := stackRebaseSourceSnapshot(t, f, "feature")["feature"]
	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict to stop")
	}
	ws := stackWorkspaceOf(t, err)
	restackAdvanceRemote(t, f, "main", "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "base")
	stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true}})
	writeShipFile(t, ws, "c.txt", "resolved\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")

	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	for _, want := range []string{"base landed while the run was stopped", "replanning without it"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %q", out, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "for-each-ref", "refs/heads/base"); got != "" {
		t.Errorf("origin base = %q, want the landed branch never pushed back", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != landedAt {
		t.Errorf("source base = %s, want unchanged %s", got, landedAt)
	}
	if got := stackParent(t, f, "feature"); got != source.Parent {
		t.Errorf("feature's gt parent = %s, want unchanged %s", got, source.Parent)
	}
	source.Parent = "main"
	published := stackAssertRebasePublication(t, f, source)
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main.."+published); n != "1" {
		t.Errorf("published feature holds %s commits over trunk, want its own 1", n)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", published+":c.txt"); got != "resolved" {
		t.Errorf("published feature's c.txt = %q, want the resolution kept", got)
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
	}
}

func TestStackContinueRefusesAPullRequestClosedMidRun(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 26108, Title: "feature", State: "OPEN"}})
	stackConflicting(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "refs/heads/feature")
	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict to stop")
	}
	ws := stackWorkspaceOf(t, err)
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 26108, Title: "feature", State: "CLOSED"}})
	writeShipFile(t, ws, "c.txt", "resolved\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	shipResetLog(t, f)

	_, _, err = runStackCmdIn(t, f, ws, "continue")
	if err == nil || !strings.Contains(err.Error(), "feature's pull request #26108 closed without landing while the run was stopped") {
		t.Fatalf("continue = %v, want the closed pull request refused", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "refs/heads/feature"); got != remote {
		t.Errorf("origin feature moved to %s, want it left at %s", got, remote)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if slices.Contains(inv, "submit") {
			t.Errorf("continue submitted: %v", inv)
		}
	}
}

func TestStackRebaseDropsALandedBranchReplayedAfterItsLanding(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "base")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
	stackAdvanceTrunk(t, f, "base.txt", "base\n")
	stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true}})
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if !strings.Contains(out, "base"+shipSep+"drop (#5 landed)") {
		t.Errorf("plan = %q, want base dropped", out)
	}
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..feature"); n != "1" {
		t.Errorf("feature holds %s commits over trunk, want its own 1", n)
	}
}

// TestStackContinueFinishesARebaseGTLost pins the way out of a rebase ccx did
// not start: a gt restack that lost its own operation mid-conflict, which gt
// continue then refuses with "No Graphite operation to continue". stack
// continue finishes it with rerere off rather than leaving raw git rebase
// --continue as the only step left.
func TestStackContinueFinishesARebaseGTLost(t *testing.T) {
	f := shipGTRepo(t)
	stackConflicting(t, f)
	runAllowFail(t, f.Env(), f.Dir, "git", "-c", "rerere.enabled=false", "rebase", "main")
	if !stackRebasing(f.Context(), render.Dir(f.Dir)) {
		t.Fatal("fixture: the rebase did not stop on c.txt")
	}
	writeShipFile(t, f.Dir, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if stackRebasing(f.Context(), render.Dir(f.Dir)) {
		t.Error("the rebase is still in progress")
	}
	if !strings.Contains(out, "finished the rebase of feature") {
		t.Errorf("continue output = %q", out)
	}
	if !stackOnto(t, f, "main", "feature") {
		t.Error("feature is not on the new trunk")
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:c.txt"); got != "trunk\nfeature" {
		t.Errorf("feature's c.txt = %q, want the resolution", got)
	}
	var rerereOff bool
	for _, inv := range shipGTInvocations(t, f) {
		if slices.Contains(inv, "--continue") && slices.Contains(inv, "rerere.enabled=false") {
			rerereOff = true
		}
	}
	if !rerereOff {
		t.Error("the continue ran with rerere on")
	}
}

// TestStackContinueNamesAResolutionRerereReplayed pins the warning a stranded
// rebase gets when rerere, on in the user's config, resolved a conflict from a
// recording nobody rechecked: a stale one silently drops a branch's own hunks.
func TestStackContinueNamesAResolutionRerereReplayed(t *testing.T) {
	f := shipGTRepo(t)
	stackConflicting(t, f)
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "config", "rerere.enabled", "true")
	runAllowFail(t, f.Env(), f.Dir, "git", "rebase", "main")
	writeShipFile(t, f.Dir, "c.txt", "stale\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "-c", "core.editor=true", "rebase", "--continue")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", feature)
	runAllowFail(t, f.Env(), f.Dir, "git", "rebase", "main")
	if got, err := os.ReadFile(filepath.Join(f.Dir, "c.txt")); err != nil || string(got) != "stale\n" {
		t.Fatalf("fixture: c.txt = %q (%v), want rerere's replayed resolution", got, err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")

	_, errOut, err := runStackCmd(t, f, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !strings.Contains(errOut, "rerere replayed a recorded resolution into c.txt") {
		t.Errorf("stderr = %q, want c.txt named as rerere's", errOut)
	}
}

// runAllowFail runs name with args in dir under f's environment and tolerates
// a nonzero exit, for a step whose failure is the point — gt restack stopping
// in a conflict it is about to have resolved by hand.
func runAllowFail(t *testing.T, env []string, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // fixed argv; dir is a TempDir, args are literals
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	_ = cmd.Run()
}

// TestStackContinueFinishesAfterARerereForget continues a stranded rebase whose
// replayed resolution was forgotten and resolved by hand: rerere leaves that
// conflict's preimage behind with no postimage beside it.
func TestStackContinueFinishesAfterARerereForget(t *testing.T) {
	f := shipGTRepo(t)
	stackConflicting(t, f)
	feature := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "config", "rerere.enabled", "true")
	runAllowFail(t, f.Env(), f.Dir, "git", "rebase", "main")
	writeShipFile(t, f.Dir, "c.txt", "stale\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "-c", "core.editor=true", "rebase", "--continue")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", feature)
	runAllowFail(t, f.Env(), f.Dir, "git", "rebase", "main")
	mustRun(t, f.Env(), f.Dir, "git", "rerere", "forget", "c.txt")
	writeShipFile(t, f.Dir, "c.txt", "resolved\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "c.txt")

	if _, _, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.Dir, "c.txt")); err != nil || string(got) != "resolved\n" {
		t.Errorf("c.txt = %q (%v), want the hand resolution", got, err)
	}
}

func TestStackContinuePublishesARunSavedWithoutSourceBases(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(fmt.Sprintf("landed=%t", landed), func(t *testing.T) {
			f := shipGTRepo(t, vcstest.GTStack("base"))
			stubStackPRs(t, nil)
			stackConflicting(t, f)
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
			landedAt := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
			sources := stackRebaseSourceSnapshot(t, f, "base", "feature")
			_, _, err := runStackCmd(t, f, "rebase")
			if err == nil {
				t.Fatal("stack rebase succeeded, want the conflict to stop")
			}
			ws := stackWorkspaceOf(t, err)
			run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			for i := range run.Branches {
				run.Branches[i].SourceBase = ""
			}
			if err := stackSaveRun(run); err != nil {
				t.Fatal(err)
			}
			published := []string{"base", "feature"}
			if landed {
				restackAdvanceRemote(t, f, "main", "base.txt", "base\n")
				mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "base")
				stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, Title: "base", State: "CLOSED", Head: landedAt, Landed: true}})
				feature := sources["feature"]
				feature.Parent = "main"
				sources["feature"] = feature
				published = []string{"feature"}
			}
			writeShipFile(t, ws, "c.txt", "resolved\n")
			mustRun(t, f.Env(), ws, "git", "add", "c.txt")

			if _, _, err := runStackCmdIn(t, f, ws, "continue"); err != nil {
				t.Fatalf("continue: %v", err)
			}
			for _, branch := range published {
				stackAssertRebasePublication(t, f, sources[branch])
			}
			if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
				t.Errorf("run state left behind: %v", left)
			}
		})
	}
}
