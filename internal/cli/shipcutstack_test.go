package cli

import (
	"slices"
	"strings"
	"testing"
)

// TestShipTipOnlyTracksABranchCutFromAStackOntoItWhenLocalTrunkMovedOn is
// tenant-parity-retro-2's #30378: a branch cut from a tracked stack branch was
// tracked onto dev, its four downstack commits replayed into its pull request,
// because local dev had moved past the commit the stack was cut from.
func TestShipTipOnlyTracksABranchCutFromAStackOntoItWhenLocalTrunkMovedOn(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	b := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "c", "b")
	stackCommit(t, f, "c.txt")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "main", "origin/main")
	shipResetLog(t, f)

	out, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only")
	if err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(out, "tracked c onto b") {
		t.Errorf("report = %q, want c tracked onto b", out)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"c"}) {
		t.Errorf("pushed refs = %v, want only c", refs)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "log", "--format=%s", b+"..c"); got != "c.txt" {
		t.Errorf("origin c over b = %q, want only its own c.txt", got)
	}
}
