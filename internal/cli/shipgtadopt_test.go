package cli

import (
	"strings"
	"testing"
)

func TestShipGTAdoptsAnUntrackedParentFromGraphitesRecord(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base")
	shipGTUntracked(t, f, "parent")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "parent")
	api.prs["parent"] = 50
	api.recorded["parent"] = "base"
	shipGTUntracked(t, f, "feature")
	shipGTReady(t, f)

	out, errOut, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	if err != nil {
		t.Fatalf("ship error = %v; stdout = %s; stderr = %s", err, out, errOut)
	}
	if got := thinGTParent(t, f, f.Dir, "parent"); got != "base" {
		t.Errorf("parent's gt parent = %q, want base, the parent Graphite records for #50", got)
	}
	if got := thinGTParent(t, f, f.Dir, "feature"); got != "parent" {
		t.Errorf("feature's gt parent = %q, want parent", got)
	}
	if !strings.Contains(out, "tracked parent onto base from Graphite's record of #50") {
		t.Errorf("stdout = %q, want it to name the adoption", out)
	}
}

func TestShipGTRefusesAnUntrackedParentGraphiteHasNoRecordOf(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTUntracked(t, f, "parent")
	shipGTUntracked(t, f, "feature")
	shipGTReady(t, f)
	head := shipHead(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--parent", "parent")
	want := "ship: parent is a branch graphite does not track here, and Graphite records no open pull request for it — track it with gt track parent --parent <its parent>, or pass --no-gt"
	if err == nil || err.Error() != want {
		t.Fatalf("ship error = %v, want %q", err, want)
	}
	assertShipRefusedClean(t, f, head)
}
