package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestShipGTPinsTrunkBeforeTheRestack is #24579's other half: ship restacked
// onto whatever the local trunk held, which another checkout kept weeks behind,
// while stack submit fast-forwarded it first. A pushing ship now fetches, pins
// the local trunk to the remote and restacks onto that.
func TestShipGTPinsTrunkBeforeTheRestack(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(got, "restacked 1 branch") {
		t.Errorf("summary = %q, want feature restacked onto the fetched trunk", got)
	}
	remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main")
	if local := gitAt(t, f.Env(), f.Dir, "rev-parse", "main"); local != remote {
		t.Errorf("main = %s, want it pinned to origin/main %s", local, remote)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "feature..refs/remotes/origin/main"); behind != "0" {
		t.Errorf("origin/main holds %s commit(s) feature does not, want feature restacked onto it", behind)
	}
	if entry := api.submitEntry("feature"); entry.BaseSha != remote {
		t.Errorf("submitted base sha = %s, want the pinned trunk %s", entry.BaseSha, remote)
	}
}

// TestShipGTNoPushPinsTheFetchedTrunk is the offline half: a --no-push ship
// fetches nothing, and pins to the remote-tracking trunk it already has.
func TestShipGTNoPushPinsTheFetchedTrunk(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "feature")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	shipGTReady(t, f)

	got, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
	if err != nil {
		t.Fatalf("ship error = %v", err)
	}
	if !strings.Contains(got, "restacked 1 branch") {
		t.Errorf("summary = %q, want feature restacked onto the fetched trunk", got)
	}
	if behind := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "feature..refs/remotes/origin/main"); behind != "0" {
		t.Errorf("origin/main holds %s commit(s) feature does not", behind)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 1 && inv[0] == "git" && inv[1] == "fetch" {
			t.Errorf("a --no-push ship ran %v", inv)
		}
	}
}

// gtLandedStack stacks b on a and lands a on origin's trunk as the merge queue
// does — one squash, so a's head is no ancestor of trunk — with Graphite
// reporting a's pull request as state.
func gtLandedStack(t *testing.T, f *vcstest.Fixture, api *gtAPIStub, state gtapi.PRState) {
	t.Helper()
	shipGTStack(t, f, "a", "b")
	api.landed["a"] = gtStubLanded{number: 41, state: state, head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")}
	restackSquashRemote(t, f, "main", "a (#41)", "a")
}

// assertLandedDropped holds the stack to the shape a landed parent leaves: a
// gone from gt, b on trunk carrying its own commits alone, and only b pushed
// and submitted, based on trunk.
func assertLandedDropped(t *testing.T, f *vcstest.Fixture, api *gtAPIStub, own string) {
	t.Helper()
	if parent := shipParentOf(t, f, "b"); parent != "main" {
		t.Errorf("b's parent = %s, want main", parent)
	}
	state, err := gtStateQuery(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatalf("gt state: %v", err)
	}
	if _, tracked := state["a"]; tracked {
		t.Error("gt still tracks a, whose pull request landed")
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "refs/remotes/origin/main..b"); got != own {
		t.Errorf("b carries %s commit(s) above trunk, want %s — the landed commit must not come along", got, own)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"b"}) {
		t.Errorf("submit posts = %v, want b alone", heads)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"b"}) {
		t.Errorf("pushed %v, want b alone", refs)
	}
	entry := api.submitEntry("b")
	if remote := gitAt(t, f.Env(), f.Dir, "rev-parse", "refs/remotes/origin/main"); entry.Base != "main" || entry.BaseSha != remote {
		t.Errorf("b submitted onto %s at %s, want main at %s", entry.Base, entry.BaseSha, remote)
	}
}

// TestStackSubmitDropsALandedParent is the ten minutes a stack sat after its
// bottom landed through the queue, until deleting the landed branch closed the
// child's pull request: one stack submit now retargets the child.
func TestStackSubmitDropsALandedParent(t *testing.T) {
	for _, state := range []gtapi.PRState{gtapi.PRMerged, gtapi.PRClosed} {
		t.Run(string(state), func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			gtLandedStack(t, f, api, state)
			shipResetLog(t, f)

			out, _, err := runStackCmd(t, f, "submit")
			if err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			if !strings.Contains(out, "dropped landed a (#41)") {
				t.Errorf("report = %q, want it to name the landed branch", out)
			}
			assertLandedDropped(t, f, api, "1")
		})
	}
}

// TestShipGTDropsALandedParent is the same landing met by a plain ship from the
// child's lane.
func TestShipGTDropsALandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	gtLandedStack(t, f, api, gtapi.PRMerged)
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(got, "dropped landed a (#41)") {
		t.Errorf("summary = %q, want it to name the landed branch", got)
	}
	assertLandedDropped(t, f, api, "2")
}

// TestStackSubmitLeavesAClosedUnlandedParent pins the other queue close: a pull
// request the queue closed without landing leaves no squash on trunk, and its
// branch stays in the stack.
func TestStackSubmitLeavesAClosedUnlandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b")
	api.landed["a"] = gtStubLanded{number: 41, state: gtapi.PRClosed, head: gitAt(t, f.Env(), f.Dir, "rev-parse", "a")}

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if strings.Contains(out, "dropped landed") {
		t.Errorf("report = %q, want nothing dropped", out)
	}
	if parent := shipParentOf(t, f, "b"); parent != "a" {
		t.Errorf("b's parent = %s, want a", parent)
	}
}

// TestShipGTLeavesAnUnchangedDownstackBranchAlone is the lane C incident: a
// parent lane's Graphite error — "Pull request not found" on a pull request
// that existed — failed the child's ship, though the child changed nothing of
// the parent's. A branch below the tip that its last submit already carries is
// neither pushed nor submitted again.
func TestShipGTLeavesAnUnchangedDownstackBranchAlone(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.submitErrors["base"] = "GitHub returned an error: Pull request not found"
	posted := len(api.submitHeads())
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "not resubmitting 1 branch, unchanged since its last submit: base"; !strings.Contains(errStr, want) {
		t.Errorf("stderr = %q, want it to carry %q", errStr, want)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"feature"}) {
		t.Errorf("submit posts = %v, want feature alone", heads)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed %v, want feature alone", refs)
	}
	if want := "submitted feature → PR #101 " + gtStubPRURL(101); !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
}

// TestShipGTReportsFromGraphiteAlone pins the report to what the submit already
// knows: the pull request it opened, from Graphite's own answer, with no GitHub
// round trip to spend a rate limit another lane has exhausted.
func TestShipGTReportsFromGraphiteAlone(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "submitted feature → PR #101 " + gtStubPRURL(101); !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	for _, inv := range shipGTInvocations(t, f) {
		if len(inv) > 0 && inv[0] == "gh" {
			t.Errorf("ship ran %v — the submit's report is Graphite's", inv)
		}
	}
}
