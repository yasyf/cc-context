package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipLeavesAPullRequestTheQueueIsRestackingAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "base", "feature")
	api.prs["base"], api.prs["feature"] = 100, 101
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
	api.mergeability[101] = gtapi.MergeabilityRebasing
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
	api.prs["base"], api.prs["feature"] = 100, 101
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
	api.mergeability[101] = gtapi.MergeabilityRebasing
	api.remote("update-ref", "refs/heads/graphite-base/101", baseHead)
	api.mu.Unlock()
	remoteFeature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit pushed feature onto trunk while the merge queue restacks it")
	}
	for _, want := range []string{"#101", "graphite-base/101", "onto main", "graphite-app force-pushes its own restack", "wait until #101's base leaves graphite-base/101", "base=main", "run ccx vcs stack continue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remoteFeature {
		t.Errorf("origin feature = %s, want it left at %s", got, remoteFeature)
	}
}

// TestStackSubmitMovesAParkedPullRequestGraphiteNoLongerRestacks is #33251:
// the queue parked it on graphite-base/33251 when its parent landed and never
// restacked it, so Graphite read it as needing a restack an hour later.
func TestStackSubmitMovesAParkedPullRequestGraphiteNoLongerRestacks(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "base", "feature")
	api.prs["base"], api.prs["feature"] = 100, 101
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
	api.mergeability[101] = "NEEDS_RESTACK__BASE_BRANCH_MERGED"
	api.remote("update-ref", "refs/heads/graphite-base/101", baseHead)
	api.mu.Unlock()
	remoteFeature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	shipResetLog(t, f)

	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit = %v (stderr=%q), want a parked pull request no restack is coming for moved onto main", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got == remoteFeature {
		t.Errorf("origin feature is still %s, want it pushed onto main", got)
	}
	api.mu.Lock()
	entry, _ := api.lastEntry("feature")
	api.mu.Unlock()
	if entry.Base != "main" {
		t.Errorf("feature last submitted onto %q, want main", entry.Base)
	}
}

func TestStackSubmitRefusesWhileGraphiteRestacksAnUnparkedMember(t *testing.T) {
	f, api, remoteFeature := rebasingFeature(t)
	posted := len(api.submitHeads())

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit pushed feature while Graphite restacks it")
	}
	for _, want := range []string{"#101 (feature)", "REBASING", "races that restack"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %q", err, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remoteFeature {
		t.Errorf("origin feature = %s, want it left at %s", got, remoteFeature)
	}
	if heads := api.submitHeads()[posted:]; len(heads) > 0 {
		t.Errorf("submit posts = %v, want none", heads)
	}
}

func TestStackSubmitPushesAPullRequestGraphiteHasNoRecordOfThroughItsRebasing(t *testing.T) {
	for name, tc := range map[string]struct {
		markedAt time.Time
		pushes   bool
	}{
		"no record since the mark": {time.Now(), true},
		"recorded since the mark":  {time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, remoteFeature := rebasingFeature(t)
			common, err := gtCommonDir(context.Background(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := gtMarkUnrecorded(common, "feature"); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(gtUnrecordedPath(common, "feature"), tc.markedAt, tc.markedAt); err != nil {
				t.Fatal(err)
			}

			_, errStr, err := runStackCmd(t, f, "submit")
			pushed := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature") != remoteFeature
			if pushed != tc.pushes || (err == nil) != tc.pushes {
				t.Errorf("stack submit = %v (stderr=%q), pushed %v, want pushed %v", err, errStr, pushed, tc.pushes)
			}
		})
	}
}

// TestStackSubmitPushesOverAStaleRebasingOnceGraphitesRestackStands is
// #40214: Graphite kept reporting the stack REBASING for 25 minutes after
// graphite-app had pushed its restack of every pull request in it.
func TestStackSubmitPushesOverAStaleRebasingOnceGraphitesRestackStands(t *testing.T) {
	f, api, remoteFeature := rebasingFeature(t)
	api.mu.Lock()
	api.restacks["feature"] = gtStubRestack{head: remoteFeature, createdAt: time.Now().Add(-gtRestackSettle - time.Minute)}
	api.mu.Unlock()

	if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit = %v (stderr=%q), want graphite-app's settled restack read as a stale REBASING", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got == remoteFeature {
		t.Errorf("origin feature is still %s, want the new commit pushed", got)
	}
}

func TestStackSubmitRefusesWhileGraphitesRestackHasNotSettled(t *testing.T) {
	for name, restack := range map[string]func(remoteFeature string) gtStubRestack{
		"fresh push": func(head string) gtStubRestack { return gtStubRestack{head: head, createdAt: time.Now()} },
		"origin moved on": func(string) gtStubRestack {
			return gtStubRestack{head: strings.Repeat("e", 40), createdAt: time.Now().Add(-time.Hour)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, api, remoteFeature := rebasingFeature(t)
			api.mu.Lock()
			api.restacks["feature"] = restack(remoteFeature)
			api.mu.Unlock()

			_, _, err := runStackCmd(t, f, "submit")
			if err == nil {
				t.Fatal("stack submit pushed feature before Graphite's restack settled on origin")
			}
			for _, want := range []string{"#101 (feature)", "REBASING", "stood on origin for 10m0s"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to name %q", err, want)
				}
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != remoteFeature {
				t.Errorf("origin feature = %s, want it left at %s", got, remoteFeature)
			}
		})
	}
}

func rebasingFeature(t *testing.T) (*vcstest.Fixture, *gtAPIStub, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	api.prs["base"], api.prs["feature"] = 100, 101
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.mu.Lock()
	api.mergeability[101] = gtapi.MergeabilityRebasing
	api.mu.Unlock()
	stackCommit(t, f, "more.txt")
	shipResetLog(t, f)
	return f, api, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
}
