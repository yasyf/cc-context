package cli

import (
	"strings"
	"testing"
)

// TestStackRebaseTakesGTsParentOverAPublishedOneOutsideTheRun is
// ci-legacy-orphans' panic: c was published onto b, b's pull request closed
// without landing and b left gt, and c was re-recorded onto a.
func TestStackRebaseTakesGTsParentOverAPublishedOneOutsideTheRun(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "gt", "untrack", "b", "--force", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	if parent := dropGTParent(t, f, "c"); parent != "a" {
		t.Fatalf("gt parent of c = %s, want a", parent)
	}

	out, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("stack rebase --dry-run: %v", err)
	}
	if !strings.Contains(out, "c · onto a") {
		t.Errorf("plan = %q, want c onto a, where gt now records it", out)
	}
}

func TestStackRebaseStopsAtAConflictBelowAPublishedParentOutsideTheRun(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "c", "--parent", "a", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "gt", "untrack", "b", "--force", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	stackAdvanceTrunk(t, f, "c.txt", "trunk\n")

	_, _, err := runStackCmd(t, f, "rebase")
	if err == nil {
		t.Fatal("stack rebase replayed c over trunk's c.txt without a conflict")
	}
	if ws := stackWorkspaceOf(t, err); !strings.Contains(ws, "conflict-c") {
		t.Errorf("conflict workspace = %q, want c's", ws)
	}
}

// TestShipKeepsABranchOnTheSiblingItWasPublishedOnto is release-targeting's
// panic: record-zod was published onto its sibling approve-then-override, gt
// still recorded both on drop-record-schema, which landed, and a ship from
// record-zod read a parent its downstack never held.
func TestShipKeepsABranchOnTheSiblingItWasPublishedOnto(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "p", "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	shipGTStack(t, f, "z")
	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "z=a"); err != nil {
		t.Fatalf("stack rebase --parent z=a: %v", err)
	}
	if parent := dropGTParent(t, f, "z"); parent != "p" {
		t.Fatalf("fixture: gt parent of z = %s, want p, with the publication alone naming a", parent)
	}
	stubStackPRs(t, map[string]*stackPR{"p": {Number: 41, Title: "p", State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.Dir, "rev-parse", "p")}})
	restackSquashRemote(t, f, "main", "p (#41)", "p")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "z")) {
		t.Error("origin z left a, the parent it was published onto")
	}
}
