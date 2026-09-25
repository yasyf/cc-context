package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStackRebaseRefusesAFlatStateFile(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	dir := filepath.Join(f.Dir, ".git", stackRebaseStateDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	flat := &stackRebaseRun{Trunk: "main", Applied: true, Branches: []stackRebaseBranch{{Name: "base"}, {Name: "feature"}}, dir: dir}
	if err := stackSaveRun(flat); err != nil {
		t.Fatal(err)
	}

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil || !strings.Contains(err.Error(), stackStatePath(dir)) || !strings.Contains(err.Error(), "rm -r "+dir) {
		t.Fatalf("rebase beside a 0.65.x state.json = %v, want a refusal naming %s and the rm to run", err, stackStatePath(dir))
	}
	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), "rm -r "+dir) {
		t.Fatalf("continue beside a 0.65.x state.json = %v, want the same refusal", err)
	}
}

func TestStackAbortDropsARunWhoseWorkspaceIsGone(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	ws := restackSiblingPath(t, "conflict-base")
	stackPlantConflict(t, f, time.Minute, &stackConflict{Branch: "base", Workspace: ws, Brief: filepath.Join(ws, "brief.md")}, "base")

	out, _, err := runStackCmd(t, f, "abort")
	if err != nil || out != "aborted · no branch moved" {
		t.Fatalf("abort of a run whose workspace is gone = %q, %v", out, err)
	}
	if left, _ := os.ReadDir(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); len(left) != 0 {
		t.Errorf("run state left behind: %v", left)
	}
}

func TestStackLastRunClearedRemovesTheLockDir(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, ".git", stackRebaseStateDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s left behind after the last run cleared: the 0.65.x binary claims its lock with Mkdir of that directory, so an empty one refuses every lane still on it (err = %v)", stackRebaseStateDir, err)
	}
}

func TestStackContinueSelectsARunByStack(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	wsA, wsB := t.TempDir(), t.TempDir()
	stackPlantConflict(t, f, time.Minute, &stackConflict{Branch: "a-top", Workspace: wsA, Brief: filepath.Join(wsA, "brief.md")}, "a-top")
	stackPlantConflict(t, f, time.Minute, &stackConflict{Branch: "b-top", Workspace: wsB, Brief: filepath.Join(wsB, "brief.md")}, "b-top")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", "origin/main")

	_, _, err := runStackCmd(t, f, "abort", "--stack", "b-top")
	if err != nil {
		t.Fatalf("abort --stack b-top from a working copy on neither stack = %v, want b-top's run dropped", err)
	}
	runs, err := stackRuns(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Roots[0] != "a-top" {
		t.Fatalf("runs after abort --stack b-top = %+v, want only a-top's", runs)
	}
}
