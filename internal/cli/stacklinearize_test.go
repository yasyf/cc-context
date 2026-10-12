package cli

import (
	"maps"
	"slices"
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

func TestStackRebaseLinearizeNeverAsksAboutAnUnrelatedBranch(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	unrelated := map[string]string{}
	for _, name := range []string{"u1", "u2", "u3"} {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
		shipGTStack(t, f, name)
		unrelated[name] = gitAt(t, f.Env(), f.Dir, "rev-parse", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "p", "c1")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	shipGTStack(t, f, "c2")
	shipResetLog(t, f)

	if _, errOut, err := runStackCmd(t, f, "rebase", "--no-push", "--linearize", "p,c1,c2"); err != nil {
		t.Fatalf("linearize siblings: %v (stderr=%q)", err, errOut)
	}
	if !stackOnto(t, f, "c1", "c2") {
		t.Error("c2 does not sit on c1")
	}
	for _, argv := range shipGTInvocations(t, f) {
		for name, head := range unrelated {
			if slices.Contains(argv, head) {
				t.Errorf("%v names %s's head, a branch outside the run", argv, name)
			}
		}
	}
}

func linearizeLocallyMoved(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	for _, name := range []string{"culprits", "grants", "third"} {
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach", "main")
		writeShipFile(t, f.Dir, name+".txt", name+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name+".txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", name)
		mustRun(t, f.Env(), f.Dir, "git", "branch", name)
		mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", name)
		mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--parent", "main", "--no-interactive")
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "culprits", "grants", "third")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "grants")
	if _, errOut, err := runStackCmd(t, f, "rebase", "--linearize", "culprits,grants"); err != nil {
		t.Fatalf("linearize culprits,grants: %v (stderr=%q)", err, errOut)
	}
	if !stackOnto(t, f, "culprits", "grants") {
		t.Fatal("grants does not sit on culprits after the local linearize")
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "third")
}

func TestStackRebaseLinearizesOntoABranchALocalRunMoved(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	linearizeLocallyMoved(t, f)
	before := linearizeRemoteHeads(t, f, "culprits", "grants", "third")

	out, errOut, err := runStackCmd(t, f, "rebase", "--linearize", "grants,third")
	if err != nil {
		t.Fatalf("linearize grants,third: %v (stderr=%q)", err, errOut)
	}
	if want := "no branch of this stack has a pull request"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want %q", out, want)
	}
	if !stackOnto(t, f, "grants", "third") || !stackOnto(t, f, "culprits", "third") {
		t.Error("third does not sit on grants and culprits")
	}
	if after := linearizeRemoteHeads(t, f, "culprits", "grants", "third"); !maps.Equal(before, after) {
		t.Errorf("remote heads moved from %v to %v in a run with no pull request", before, after)
	}
}

func TestStackRebaseRefusesToStackOnAPublishedHeadALocalRunMovedPast(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	linearizeLocallyMoved(t, f)
	stubOpenPRs(t, f, nil, "third")
	api.prs["third"] = 9000
	before := linearizeRemoteHeads(t, f, "culprits", "grants", "third")

	_, _, err := runStackCmd(t, f, "rebase", "--linearize", "grants,third")
	if err == nil || !strings.Contains(err.Error(), "grants stays at its published head") || !strings.Contains(err.Error(), "--parent grants=culprits") {
		t.Fatalf("linearize grants,third = %v, want grants' stale published head named", err)
	}
	if after := linearizeRemoteHeads(t, f, "culprits", "grants", "third"); !maps.Equal(before, after) {
		t.Errorf("remote heads moved from %v to %v before refusing", before, after)
	}
}

func TestStackRebaseLocalOnlyKeepsAnotherLanesUnpushedCommits(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "other/base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "other/base")
	writeShipFile(t, f.Dir, "more.txt", "more\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "more.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "more")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "other/base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "mine/feature")

	if _, errOut, err := runStackCmd(t, f, "rebase", "--parent", "mine/feature=other/base"); err != nil {
		t.Fatalf("rebase --parent: %v (stderr=%q)", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "other/base"); got != local {
		t.Errorf("other/base = %.12s, want its unpushed head %.12s kept", got, local)
	}
	if !stackOnto(t, f, local, "mine/feature") {
		t.Errorf("mine/feature does not sit on other/base's local head %.12s", local)
	}
}
