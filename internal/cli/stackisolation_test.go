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
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
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
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != source {
		t.Fatalf("source feature moved: %s != %s", got, source)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if remote == source || !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("published feature %s missed fresh remote trunk", remote)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil {
		t.Fatal(err)
	}
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
	if receipt == nil || receipt.Source != source || receipt.SourceBase != old || receipt.Head != remote || receipt.Base != base || receipt.Parent != "main" {
		t.Fatalf("publication receipt = %+v, want source %s, source base %s, head %s, base %s on main", receipt, source, old, remote, base)
	}
	if staged, unstaged := gitAt(t, f.Env(), f.Dir, "diff", "--cached"), gitAt(t, f.Env(), f.Dir, "diff"); staged != "" || unstaged != "" {
		t.Fatalf("source checkout changed: staged=%q unstaged=%q", staged, unstaged)
	}
}

func TestStackSubmitKeepsConflictForContinue(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, nil)
	stackConflicting(t, f)
	before := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	sourceBase := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
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
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != before {
		t.Fatalf("continue moved source feature: %s != %s", got, before)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if remote == before || !stackOnto(t, f, parent, remote) || !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("continued publication %s lost its published parent %s or fresh trunk", remote, parent)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.Source != before || receipt.SourceBase != sourceBase || receipt.Head != remote || receipt.Base != parent || receipt.Parent != "base" {
		t.Fatalf("continued publication receipt = %+v", receipt)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "show", "feature:c.txt"); got != "trunk\nfeature" {
		t.Fatalf("published resolution = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(f.Dir, "c.txt")); err != nil || string(got) != "feature\n" {
		t.Fatalf("source conflict file changed: %q %v", got, err)
	}
	if staged, unstaged := gitAt(t, f.Env(), f.Dir, "diff", "--cached"), gitAt(t, f.Env(), f.Dir, "diff"); staged != "" || unstaged != "" {
		t.Fatalf("source checkout changed after continue: staged=%q unstaged=%q", staged, unstaged)
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
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", "other")
	sourceBase := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
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
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "other"); got != source {
		t.Fatalf("second stack source moved: %s != %s", got, source)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "other")
	if remote == source || !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("second stack publication %s missed fresh remote trunk", remote)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "other")
	if err != nil {
		t.Fatal(err)
	}
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
	if receipt == nil || receipt.Source != source || receipt.SourceBase != sourceBase || receipt.Head != remote || receipt.Base != base || receipt.Parent != "main" {
		t.Fatalf("second stack publication receipt = %+v", receipt)
	}
}
