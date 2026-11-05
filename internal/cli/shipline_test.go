package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestShipAmendOnAPullRequestLessLineLeavesTheBranchesBelowIt is the cc-skills
// dashboard lanes' amend: a pull request based on dashboard-fold-2b, a line
// with no pull request, above another lane's unpushed dashboard-pages-needs.
// ship --amend replayed that branch onto the moved trunk and stopped in
// conflict.
func TestShipAmendOnAPullRequestLessLineLeavesTheBranchesBelowIt(t *testing.T) {
	f := shipGTRepo(t, vcstest.GTStack("needs", "line", "feature"))
	stubOpenPRs(t, f, map[string]*stackPR{"feature": {Number: 9000, Title: "feature", State: "OPEN", Base: "line"}})
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.prs["feature"] = 9000
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "line", "feature")
	stackAdvanceTrunk(t, f, "needs.txt", "trunk's own needs\n")
	line := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "line")
	needs := gitAt(t, f.Env(), f.Dir, "rev-parse", "needs")
	writeShipFile(t, f.Dir, "feature.txt", "amended\n")

	_, errOut, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "feature.txt")
	if err != nil {
		t.Fatalf("ship --amend = %v (stderr=%q), want feature published on the line", err, errOut)
	}
	if !strings.Contains(errOut, "line has no pull request and the pull request above it is based on it, so the run takes it as trunk") {
		t.Errorf("stderr = %q, want the line named as trunk", errOut)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "needs"); got != needs {
		t.Errorf("needs moved to %s, want it left at %s", got, needs)
	}
	for _, at := range []string{f.Dir, f.RemoteDir} {
		if got := gitAt(t, f.Env(), at, "rev-parse", "line"); got != line {
			t.Errorf("line in %s moved to %s, want it left at %s", at, got, line)
		}
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if n := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", line+".."+published); n != "1" || !stackOnto(t, f, line, published) {
		t.Errorf("origin feature holds %s commits over the line's published head, want its amended 1 on it", n)
	}
	if posts := api.submitHeads(); !slices.Equal(posts, []string{"feature"}) {
		t.Errorf("submit posts = %v, want feature alone", posts)
	}
}
