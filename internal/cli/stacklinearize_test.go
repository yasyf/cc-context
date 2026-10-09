package cli

import (
	"maps"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func linearizeRemoteHeads(t *testing.T, f *vcstest.Fixture, branches ...string) map[string]string {
	t.Helper()
	heads := map[string]string{}
	for _, branch := range branches {
		heads[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
	}
	return heads
}

func TestStackRebaseLinearizeRefusesAPushThatMergesAnOpenPullRequest(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "s15b", "s14b", "s13b", "i11b")
	api.prs["s15b"], api.prs["s14b"], api.prs["s13b"], api.prs["i11b"] = 28, 29, 30, 31
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stubOpenPRs(t, f, nil, "s15b", "s14b", "s13b", "i11b")
	before := linearizeRemoteHeads(t, f, "s15b", "s14b", "s13b", "i11b")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "rebase", "--parent", "s14b=main", "--linearize", "s14b,s13b,i11b,s15b")
	if err == nil {
		t.Fatal("linearize pushed a stack that makes GitHub merge #29 into s15b")
	}
	if want := "#29 (s14b) into s15b"; !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %q, want it to name %q", err, want)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
		t.Errorf("pushed %v before refusing", refs)
	}
	if after := linearizeRemoteHeads(t, f, "s15b", "s14b", "s13b", "i11b"); !maps.Equal(before, after) {
		t.Errorf("remote heads moved from %v to %v", before, after)
	}

	api.mu.Lock()
	api.bases["s14b"] = "main"
	api.mu.Unlock()
	if _, errOut, err := runStackCmd(t, f, "continue"); err != nil {
		t.Fatalf("continue after retargeting #29 onto main: %v (stderr=%q)", err, errOut)
	}
	if after := linearizeRemoteHeads(t, f, "s15b", "s14b", "s13b", "i11b"); maps.Equal(before, after) {
		t.Error("continue pushed nothing after the retarget")
	}
	if !stackOnto(t, f, "i11b", "s15b") {
		t.Error("s15b does not sit on i11b")
	}
}

func TestStackRebaseLinearizePushesSiblingsOntoEachOther(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "p", "c1")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit c1: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	shipGTStack(t, f, "c2")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit c2: %v", err)
	}
	api.prs["p"], api.prs["c1"], api.prs["c2"] = 100, 101, 102
	stubOpenPRs(t, f, nil, "p", "c1", "c2")
	shipResetLog(t, f)

	if _, errOut, err := runStackCmd(t, f, "rebase", "--linearize", "p,c1,c2"); err != nil {
		t.Fatalf("linearize siblings: %v (stderr=%q)", err, errOut)
	}
	api.mu.Lock()
	entry, _ := api.lastEntry("c2")
	api.mu.Unlock()
	if entry.Base != "c1" {
		t.Errorf("c2 last submitted on %q, want c1", entry.Base)
	}
	if !stackOnto(t, f, "c1", "c2") {
		t.Error("c2 does not sit on c1")
	}
}
