package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

const stackClaimingLane = `{"session":"sess-drive","agent":"a-go-client","agent_name":"go-client"}`

func stackClaimedBase(t *testing.T) (*vcstest.Fixture, *gtAPIStub) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base")
	api.prs["base"] = 7
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish base: %v", err)
	}
	shipGTStack(t, f, "feature")
	api.prs["feature"] = 8
	writeShipExecutable(t, f.ShimBin, "cci", "#!/bin/sh\n[ \"$1 $3\" = 'who branch:base' ] && echo '"+stackClaimingLane+"'\nexit 0\n")
	f.Setenv(envClaudeSessionKey, "sess-drive")
	f.Setenv(envCodexThreadKey, "")
	f.Setenv(envCCIAgentKey, "a-fault-run")
	return f, api
}

func TestStackSubmitKeepsAParentAnotherLaneClaimsAtItsPublishedHead(t *testing.T) {
	f, api := stackClaimedBase(t)
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	restackAdvanceRemote(t, f, "main", "upstream.txt", "upstream\n")
	posted := len(api.submitHeads())

	out, _, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"feature"}) {
		t.Errorf("submit posts = %v, want feature alone", heads)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != published {
		t.Errorf("origin base = %s, want its published head %s", got, published)
	}
	if feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); !stackOnto(t, f, published, feature) {
		t.Errorf("published feature %s is not on base's published head %s", feature, published)
	}
	if want := "stack submit: keeping base (claimed by lane go-client) at its published head — another lane owns it; 0 commit(s) not on origin; pass --include base to submit it\n"; !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestStackSubmitRefusesABranchOffTheClaimedParentsPublishedHead(t *testing.T) {
	f, _ := stackClaimedBase(t)
	writeShipFile(t, f.Dir, "base.txt", "feature\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "feature edits base")
	restackAdvanceRemote(t, f, "base", "base.txt", "rework\n")
	rework := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	shipResetLog(t, f)

	_, _, err := runStackCmd(t, f, "submit")
	if want := "stack submit: feature no longer applies on base's published head " + rework[:12] + ", which lane go-client owns, so nothing was pushed; publish feature alone with ccx vcs ship --tip-only from its working copy"; err == nil || err.Error() != want {
		t.Fatalf("stack submit error = %v, want %q", err, want)
	}
	if n := pushCount(t, f); n != 0 {
		t.Errorf("pushes = %d, want none", n)
	}
}
