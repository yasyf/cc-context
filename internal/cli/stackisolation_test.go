package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if local, remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); local != remote {
		t.Fatalf("not pushed: %s != %s", local, remote)
	}
}

func TestStackSubmitKeepsConflictForContinue(t *testing.T) {
	f := shipGTRepo(t)
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "ccx vcs stack continue") || strings.Contains(err.Error(), "gt restack") {
		t.Fatalf("conflict: %v", err)
	}
	run, err := stackLoadRun(filepath.Join(f.Dir, ".git"))
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
	if err := stackSaveRun(dir, &stackRebaseRun{Ship: intent}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	run, err := stackLoadRun(dir)
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
