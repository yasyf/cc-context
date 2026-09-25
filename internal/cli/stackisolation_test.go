package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackSubmitLeavesDirtyTrunkAndItsIndexLockUntouched(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	trunk := restackSiblingPath(t, "trunk")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", trunk, "main")
	old := gitAt(t, f.Env(), trunk, "rev-parse", "HEAD")
	writeShipFile(t, trunk, "seed.txt", "uncommitted trunk\n")
	mustRun(t, f.Env(), trunk, "git", "add", "seed.txt")
	before := gitAt(t, f.Env(), trunk, "diff", "--cached")
	index := gitAt(t, f.Env(), trunk, "rev-parse", "--path-format=absolute", "--git-path", "index.lock")
	if err := os.WriteFile(index, []byte("another process"), 0o600); err != nil {
		t.Fatal(err)
	}
	stackAdvanceTrunk(t, f, "upstream.txt", "new trunk\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), trunk, "rev-parse", "HEAD"); got != old {
		t.Fatalf("trunk moved: %s", got)
	}
	if got := gitAt(t, f.Env(), trunk, "diff", "--cached"); got != before {
		t.Fatalf("staging changed: %s", got)
	}
	if got, err := os.ReadFile(index); err != nil || string(got) != "another process" {
		t.Fatalf("lock changed: %q %v", got, err)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Fatal("feature missed fresh remote trunk")
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if state["feature"].NeedsRestack {
		t.Fatal("freshly submitted feature still needs restack")
	}
	if state["main"].Head != gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main") {
		t.Fatal("effective trunk did not use origin")
	}

	if local, remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); local != remote {
		t.Fatalf("not pushed: %s != %s", local, remote)
	}
}

func TestStackSubmitKeepsConflictForContinue(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "ccx vcs stack continue") || strings.Contains(err.Error(), "gt restack") {
		t.Fatalf("conflict: %v", err)
	}
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != before {
		t.Fatal("conflict moved source branch")
	}
	writeShipFile(t, run.Conflict.Workspace, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), run.Conflict.Workspace, "git", "add", "c.txt")
	if _, _, err := runStackCmdIn(t, f, run.Conflict.Workspace, "continue"); err != nil {
		t.Fatal(err)
	}
	if local, remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); local != remote {
		t.Fatalf("continue did not push: %s != %s", local, remote)
	}
}

func TestStackRebaseRefusesDirtyInvokingCheckoutBeforeRefMoves(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	old := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	writeShipFile(t, f.Dir, "feature.txt", "staged\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "feature.txt")
	writeShipFile(t, f.Dir, "feature.txt", "unstaged\n")
	staged := gitAt(t, f.Env(), f.Dir, "diff", "--cached")
	unstaged := gitAt(t, f.Env(), f.Dir, "diff")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), "has uncommitted work; no branches moved") {
		t.Fatalf("got %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got != old {
		t.Fatal("source moved")
	}
	if got := gitAt(t, f.Env(), f.Dir, "diff", "--cached"); got != staged {
		t.Fatal("staged work changed")
	}
	if got := gitAt(t, f.Env(), f.Dir, "diff"); got != unstaged {
		t.Fatal("unstaged work changed")
	}
}

func TestStackShipIntentSurvivesRemovedBodyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("the exact body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	intent, err := stackShipOptions(shipOpts{noWatch: true, reviews: true, budget: 42}, map[string]prMeta{"feature": {title: "the title", bodyPath: file}}, "owner/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	saved := &stackRebaseRun{Ship: intent, Roots: []string{"feature"}}
	if err := stackClaim(dir, saved); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	run, err := stackOnlyTestRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := run.Ship.Meta["feature"]
	if m.Title != "the title" || m.Body == nil || *m.Body != "the exact body\n" || !run.Ship.NoWatch || !run.Ship.Reviews || run.Ship.Budget != 42 {
		t.Fatalf("intent lost: %#v %#v", run.Ship, m)
	}
}

func TestStackPublishedHeadsRejectsConcurrentCommit(t *testing.T) {
	run := &stackRebaseRun{Branches: []stackRebaseBranch{{Name: "feature", NewHead: "reviewed"}}}
	if err := stackCheckPublishedHeads(gtState{"feature": {Head: "concurrent"}}, run); err == nil {
		t.Fatal("accepted a concurrent commit")
	}
	if err := stackCheckPublishedHeads(gtState{"feature": {Head: "reviewed"}}, run); err != nil {
		t.Fatal(err)
	}
}

func stackOnlyTestRun(commonDir string) (*stackRebaseRun, error) {
	runs, err := stackRuns(commonDir)
	if err != nil {
		return nil, err
	}
	if len(runs) != 1 {
		return nil, fmt.Errorf("expected one test run, got %d", len(runs))
	}
	return runs[0], nil
}

func TestShipPreservesAnotherStacksConflictRun(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stackConflicting(t, f)
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil {
		t.Fatal("expected first stack conflict")
	}
	common := filepath.Join(f.Dir, ".git")
	first, err := stackOnlyTestRun(common)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stackStatePath(first.dir))
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "other")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--no-pr"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(stackStatePath(first.dir))
	if err != nil || string(after) != string(before) {
		t.Fatalf("other run changed: %v", err)
	}
	if _, err := os.Stat(first.Conflict.Workspace); err != nil {
		t.Fatal(err)
	}
	if local, remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "other"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "other"); local != remote {
		t.Fatal("second stack was not pushed")
	}
}
