package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackUntrackedRepo(t *testing.T) (*vcstest.Fixture, *gtAPIStub) {
	t.Helper()
	f := shipGTRepo(t)
	stub := stubGTAPI(t)
	f.Decorate(stub.ctx)
	shipGTStack(t, f, "base", "feature")
	stub.prs = map[string]int{"base": 9000, "feature": 9001}
	stubStackPRs(t, f, map[string]*stackPR{
		"base":    {Number: 9000, State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
		"feature": {Number: 9001, State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
	})
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("first stack submit: %v", err)
	}
	wait := stackTrackingWait
	stackTrackingWait = 0
	t.Cleanup(func() { stackTrackingWait = wait })
	return f, stub
}

func TestStackSubmitRepublishesAPullRequestGraphiteHoldsNoMergeabilityRecordFor(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stub.untracked[9000] = gtStubUntracked{base: "landed-parent", head: published}

	out, errStr, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	for _, want := range []string{
		"base" + shipSep + "#9000" + shipSep + "head ",
		"untracked by graphite",
		"repairing" + shipSep + "Graphite holds no mergeability record for:",
		"#9000 base" + shipSep + "parent main" + shipSep + "Graphite tracks no stack for it" + shipSep + "its server-side parent is still landed-parent",
		"repaired" + shipSep + "#9000 republished with a fresh head and tracked by Graphite",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stack submit output = %q, want it to name %q", out, want)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "base"+shipSep+"#9000") && strings.Contains(line, "untracked") == strings.Contains(line, "mergeable") {
			t.Errorf("verdict line %q reports a pull request Graphite does not track as mergeable, or loses the flag", line)
		}
	}
	rewritten := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	if rewritten == published {
		t.Fatalf("origin base is still %.12s, want a fresh head", published)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", rewritten+"^{tree}"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", published+"^{tree}"); got != want {
		t.Errorf("fresh head tree = %.12s, want the published tree %.12s", got, want)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%an%n%ae%n%ad%n%B", rewritten), gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%an%n%ae%n%ad%n%B", published); got != want {
		t.Errorf("fresh head carries %q, want the published author and message %q", got, want)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%P", rewritten), gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%P", published); got != want {
		t.Errorf("fresh head parents = %q, want %q", got, want)
	}
	if feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); !stackOnto(t, f, rewritten, feature) {
		t.Errorf("origin feature %.12s does not sit on the fresh base %.12s", feature, rewritten)
	}
	if len(stub.untracked) != 0 {
		t.Errorf("Graphite still holds %v untracked", stub.untracked)
	}
}

func TestStackSubmitFailsNamingAPullRequestGraphiteStillDoesNotTrack(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	stub.untracked[9000] = gtStubUntracked{base: "landed-parent", head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"), stuck: true}

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatalf("stack submit exited 0 over a pull request Graphite does not track: %q", out)
	}
	for _, want := range []string{
		"still tracks no stack",
		"#9000 base" + shipSep + "parent main" + shipSep + "Graphite tracks no stack for it" + shipSep + "its server-side parent is still landed-parent",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stack submit = %v, want it to name %q", err, want)
		}
	}
	if strings.Contains(out, "repaired") {
		t.Errorf("stack submit output = %q, claims a repair it did not make", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "base"+shipSep+"#9000") && strings.Contains(line, "mergeable") {
			t.Errorf("verdict line %q reports an untracked pull request as mergeable", line)
		}
	}
}

func TestStackSubmitNamesNoHandRepairOnceGraphiteRecordsAPullRequestOffItsSegment(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	stub.untracked[9001] = gtStubUntracked{base: gtSegmentBase("feature"), head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), stuck: true}

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit exited 0 over a pull request Graphite still does not track")
	}
	if want := "repaired" + shipSep + "#9001 recorded by Graphite on base, off its segment base ccx-segment/feature"; !strings.Contains(out, want) {
		t.Errorf("stack submit output = %q, want %q", out, want)
	}
	if entry := stackLastSubmit(stub, "feature"); entry.Base != "base" || entry.HeadSha != gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature") {
		t.Errorf("feature last submitted on %s at %.12s, want base at its published head", entry.Base, entry.HeadSha)
	}
	if want := "#9001 feature" + shipSep + "parent base" + shipSep + "Graphite tracks no stack for it\n"; !strings.Contains(err.Error()+"\n", want) {
		t.Errorf("stack submit = %v, want it to name %q", err, want)
	}
	if strings.Contains(err.Error(), "gt track") {
		t.Errorf("stack submit = %v, sends the caller to a hand repair ccx already made", err)
	}
}

func stackHeldSegmentRepo(t *testing.T) (*vcstest.Fixture, *gtAPIStub, string) {
	t.Helper()
	f := shipGTRepo(t)
	stub := stubGTAPI(t)
	f.Decorate(stub.ctx)
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "main:"+gtRestackRef(gtSegmentBase("base")))
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane"), "base")
	stub.prs = map[string]int{"base": 9000, "feature": 9001}
	stubStackPRs(t, f, map[string]*stackPR{
		"base":    {Number: 9000, State: "OPEN", Base: gtSegmentBase("base"), Mergeable: "MERGEABLE", Segment: true},
		"feature": {Number: 9001, State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
	})
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	stub.untracked[9000] = gtStubUntracked{base: gtSegmentBase("base"), head: head}
	wait := stackTrackingWait
	stackTrackingWait = 0
	t.Cleanup(func() { stackTrackingWait = wait })
	return f, stub, head
}

func stackLastSubmit(stub *gtAPIStub, branch string) gtStubSubmitEntry {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	entry, _ := stub.lastEntry(branch)
	return entry
}

func stackOriginSegment(t *testing.T, f *vcstest.Fixture, branch string) string {
	t.Helper()
	return gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "for-each-ref", "--format=%(objectname)", gtRestackRef(gtSegmentBase(branch)))
}

func TestStackSubmitRecordsAPullRequestAnotherWorkingCopyHoldsOffItsSegment(t *testing.T) {
	f, stub, head := stackHeldSegmentRepo(t)
	writeShipGH(t, f)
	f.Setenv("GH_PULLS_JSON", ghOpenPullAt(t, 9000, head, "main"))

	out, errStr, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	if want := "repaired" + shipSep + "#9000 recorded by Graphite on main, off its segment base ccx-segment/base"; !strings.Contains(out, want) {
		t.Errorf("stack submit output = %q, want %q", out, want)
	}
	if entry := stackLastSubmit(stub, "base"); entry.Base != "main" || entry.HeadSha != head || entry.PRNumber != 9000 {
		t.Errorf("base last submitted as #%d on %s at %.12s, want #9000 on main at its published head %.12s", entry.PRNumber, entry.Base, entry.HeadSha, head)
	}
	if segment := stackOriginSegment(t, f, "base"); segment != "" {
		t.Errorf("origin %s = %.12s, want it deleted (stderr=%q)", gtSegmentBase("base"), segment, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != head {
		t.Errorf("origin base moved from %.12s to %.12s, want another lane's branch left where it was published", head, got)
	}
	if len(stub.untracked) != 0 {
		t.Errorf("Graphite still holds %v untracked", stub.untracked)
	}
}

func TestStackSubmitLeavesAPullRequestOnItsSegmentWhileGraphiteStillCapsIt(t *testing.T) {
	f, stub, _ := stackHeldSegmentRepo(t)
	stub.capped["base"] = true
	segment := stackOriginSegment(t, f, "base")

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatalf("stack submit exited 0 over a pull request Graphite still caps: %q", out)
	}
	if want := "#9000 stays on its segment base ccx-segment/base: Graphite still caps the stack under main"; !strings.Contains(err.Error(), want) {
		t.Errorf("stack submit = %v, want it to name %q", err, want)
	}
	if strings.Contains(err.Error(), "gt track") {
		t.Errorf("stack submit = %v, sends the caller to a hand repair the cap refuses too", err)
	}
	if strings.Contains(out, "repaired"+shipSep+"#9000") {
		t.Errorf("stack submit output = %q, claims a repair Graphite refused", out)
	}
	if got := stackOriginSegment(t, f, "base"); got != segment {
		t.Errorf("origin %s = %.12s, want it left at %.12s", gtSegmentBase("base"), got, segment)
	}
}

func TestStackSubmitLeavesATrackedStackAlone(t *testing.T) {
	f, _ := stackUntrackedRepo(t)
	before := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if strings.Contains(out, "repair") || strings.Contains(out, "untracked") {
		t.Errorf("stack submit output = %q, want no repair of a tracked stack", out)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != before {
		t.Errorf("origin base moved from %.12s to %.12s with every pull request tracked", before, got)
	}
}

func TestStackTrackingWaitsForGraphiteToTakeInAFreshPush(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	stackTrackingWait = time.Minute
	stackTrackingRetry = time.Millisecond
	t.Cleanup(func() { stackTrackingRetry = 2 * time.Second })
	stub.untracked[9000] = gtStubUntracked{head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")}
	go func() {
		time.Sleep(50 * time.Millisecond)
		stub.mu.Lock()
		delete(stub.untracked, 9000)
		stub.mu.Unlock()
	}()

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if strings.Contains(out, "repair") || strings.Contains(out, "untracked") {
		t.Errorf("stack submit output = %q, want Graphite given time to take the push in before any repair", out)
	}
}

func TestStackSubmitWaitsOnGraphiteForAPullRequestItStillStacksOnLandedOnes(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stub.untracked[9000] = gtStubUntracked{base: "main", head: published, closed: []int{8999, 8998}}
	submits := len(stub.submits)

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v, want Graphite left to drop the landed pull requests itself", err)
	}
	for _, want := range []string{
		"waiting on Graphite" + shipSep,
		stackLandedRemedy,
		"#9000 base" + shipSep + "parent main" + shipSep + "Graphite tracks no stack for it" + shipSep + "its server-side stack still holds #8998, #8999, which already landed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stack submit output = %q, want it to name %q", out, want)
		}
	}
	if strings.Contains(out, "server-side parent is still") {
		t.Errorf("stack submit output = %q, names a stale parent where Graphite recorded the right one", out)
	}
	if strings.Contains(out, "repair") {
		t.Errorf("stack submit output = %q, want no republish a fresh head cannot help", out)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published {
		t.Errorf("origin base moved from %.12s to %.12s, want it left alone", published, got)
	}
	if got := len(stub.submits); got != submits {
		t.Errorf("stack submit posted %d more submits, want none", got-submits)
	}
}

func TestStackSubmitRefusesAPullRequestGraphiteStillStacksOnClosedOnes(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stub.untracked[9000] = gtStubUntracked{base: "main", head: published, closed: []int{8999}, abandoned: []int{8997}}

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatalf("stack submit exited 0 over a pull request stacked on a closed one: %q", out)
	}
	for _, want := range []string{
		"#9000 base" + shipSep + "parent main" + shipSep + "Graphite tracks no stack for it" + shipSep + "its server-side stack still holds #8999, which already landed" + shipSep + "its server-side stack still holds #8997, which closed without landing",
		stackStrandedRemedy,
		"ccx vcs pr recreate 9000",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stack submit = %v, want it to name %q", err, want)
		}
	}
	if strings.Contains(out, "waiting on Graphite") {
		t.Errorf("stack submit output = %q, tells the caller to wait on a record a closed pull request keeps", out)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published {
		t.Errorf("origin base moved from %.12s to %.12s, want it left alone", published, got)
	}
}

func TestStackSubmitRepublishesOnlyThePullRequestsAFreshHeadCanRepair(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	stub.untracked[9000] = gtStubUntracked{base: "main", head: base, closed: []int{8999}}
	stub.untracked[9001] = gtStubUntracked{base: "landed-parent", head: feature}

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v, want #9001 repaired and #9000 left to Graphite", err)
	}
	if want := "repaired" + shipSep + "#9001 republished"; !strings.Contains(out, want) {
		t.Errorf("stack submit output = %q, want %q and #9000, which a fresh head cannot repair, left out", out, want)
	}
	_, waiting, found := strings.Cut(out, "waiting on Graphite")
	if !found || !strings.Contains(waiting, "#9000 base") || strings.Contains(waiting, "#9001") {
		t.Errorf("stack submit output = %q, want the wait to name only #9000", out)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("origin base moved from %.12s to %.12s, want it left alone", base, got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got == feature {
		t.Errorf("origin feature is still %.12s, want a fresh head", feature)
	}
}

func TestStackSubmitClaimsNoRepairForAPullRequestAFreshHeadLeavesOnALandedOne(t *testing.T) {
	f, stub := stackUntrackedRepo(t)
	stub.untracked[9000] = gtStubUntracked{base: "landed-parent", head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"), reveals: []int{8999}}

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v, want #9000 left to Graphite", err)
	}
	if strings.Contains(out, "repaired") {
		t.Errorf("stack submit output = %q, claims a repair Graphite does not track", out)
	}
	if want := "its server-side stack still holds #8999, which already landed"; !strings.Contains(out, "waiting on Graphite") || !strings.Contains(out, want) {
		t.Errorf("stack submit output = %q, want the wait to name %q", out, want)
	}
}

func TestStackSubmitNamesTheHeldParentWithNoPullRequestInsteadOfRepublishing(t *testing.T) {
	f := shipGTRepo(t)
	stub := stubGTAPI(t)
	f.Decorate(stub.ctx)
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane"), "base")
	stub.prs = map[string]int{"feature": 9001}
	stubStackPRs(t, f, map[string]*stackPR{
		"feature": {Number: 9001, State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
	})
	stub.untracked[9001] = gtStubUntracked{base: "base", head: gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"), stuck: true}
	wait := stackTrackingWait
	stackTrackingWait = 0
	t.Cleanup(func() { stackTrackingWait = wait })

	out, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatalf("stack submit exited 0 over a pull request Graphite does not track: %q", out)
	}
	for _, want := range []string{
		"ccx vcs stack submit --include base",
		"#9001 feature" + shipSep + "parent base" + shipSep + "Graphite tracks no stack for it" + shipSep + "below it base kept as another lane's with no pull request",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stack submit = %v, want it to name %q", err, want)
		}
	}
	if strings.Contains(out, "repair") {
		t.Errorf("stack submit output = %q, want no republish a fresh head cannot help", out)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err == nil {
		t.Fatal("second stack submit exited 0 over the same untracked pull request")
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != published {
		t.Errorf("second submit moved origin feature from %.12s to %.12s", published, got)
	}
}
