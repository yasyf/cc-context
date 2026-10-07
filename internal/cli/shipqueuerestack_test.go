package cli

import (
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

func TestShipLeavesAPullRequestTheQueueIsRestackingAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	baseHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	restackSquashRemote(t, f, "main", "base (#100)", "base")
	api.mu.Lock()
	delete(api.prs, "base")
	api.merged["base"] = gtStubMerged{number: 100, head: baseHead, state: gtapi.PRClosed}
	api.prs["feature"] = 101
	api.parked["feature"] = "graphite-base/101"
	api.remote("update-ref", "refs/heads/graphite-base/101", baseHead)
	api.mu.Unlock()

	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "follow", "base")
	stackCommit(t, f, "follow.txt")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "base", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "follow")
	api.mu.Lock()
	api.prs["follow"] = 102
	api.mu.Unlock()
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "follow", "base", "feature")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "--parent", "follow", "--no-interactive")
	remoteFeature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	shipResetLog(t, f)

	_, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only")
	if err == nil {
		t.Fatalf("ship moved a pull request the merge queue is restacking (stderr=%q)", errStr)
	}
	for _, want := range []string{"#101", "graphite-base/101", "base", "landed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "graphite-base/101"); got != baseHead {
		t.Errorf("origin graphite-base/101 = %s, want it left at the landed parent's head %s", got, baseHead)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remoteFeature {
		t.Errorf("origin feature = %s, want it left at %s", got, remoteFeature)
	}
}

// TestStackSubmitWaitsOutTheQueuesRestackOntoTrunk is #31266: base landed and
// the queue parked feature on graphite-base/101 to restack it onto trunk, so a
// push of feature onto trunk would race graphite-app's own force-push.
func TestStackSubmitWaitsOutTheQueuesRestackOntoTrunk(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	baseHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	restackSquashRemote(t, f, "main", "base (#100)", "base")
	api.mu.Lock()
	delete(api.prs, "base")
	api.merged["base"] = gtStubMerged{number: 100, head: baseHead, state: gtapi.PRClosed}
	api.prs["feature"] = 101
	api.parked["feature"] = "graphite-base/101"
	api.remote("update-ref", "refs/heads/graphite-base/101", baseHead)
	api.mu.Unlock()
	remoteFeature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit pushed feature onto trunk while the merge queue restacks it")
	}
	for _, want := range []string{"#101", "graphite-base/101", "onto main", "graphite-app force-pushes its own restack", "wait until #101's base leaves graphite-base/101"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remoteFeature {
		t.Errorf("origin feature = %s, want it left at %s", got, remoteFeature)
	}
}
