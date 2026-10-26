package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const gtStackCapRefusal = "🥞 Nice stack! Only opening PRs for the first 50 branches in the stack. To open PRs for the upstack branches, please split up the stack (using gt move) or first merge and close some of the downstack PRs."

func gtCapCommonDir(t *testing.T) string {
	t.Helper()
	common, err := gtCommonDir(context.Background(), render.Dir("."), "test")
	if err != nil {
		t.Fatal(err)
	}
	return common
}

type segmentStack struct {
	f   *vcstest.Fixture
	api *gtAPIStub
}

func newSegmentStack(t *testing.T) segmentStack {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "p", "c")
	api.capped["c"] = true
	api.prs["p"], api.prs["c"] = 100, 101
	s := segmentStack{f: f, api: api}
	s.submit(t)
	return s
}

func (s segmentStack) submit(t *testing.T) string {
	t.Helper()
	_, errOut, err := runStackCmd(t, s.f, "submit")
	if err != nil {
		t.Fatalf("stack submit: %v (stderr=%q)", err, errOut)
	}
	return errOut
}

func (s segmentStack) origin(t *testing.T, branch string) string {
	t.Helper()
	return gitAt(t, s.f.Env(), s.f.Dir, "--git-dir="+s.f.RemoteDir, "for-each-ref", "--format=%(objectname)", gtRestackRef(branch))
}

func (s segmentStack) recordedBase(branch string) string {
	s.api.mu.Lock()
	defer s.api.mu.Unlock()
	entry, _ := s.api.lastEntry(branch)
	return entry.Base
}

func (s segmentStack) amendParent(t *testing.T) {
	t.Helper()
	mustRun(t, s.f.Env(), s.f.Dir, "git", "switch", "-q", "p")
	writeShipFile(t, s.f.Dir, "p.txt", "amended\n")
	mustRun(t, s.f.Env(), s.f.Dir, "git", "add", "p.txt")
	mustRun(t, s.f.Env(), s.f.Dir, "git", "commit", "-q", "--amend", "--no-edit")
	shipResetLog(t, s.f)
}

func TestStackSubmitRecordsACappedBranchOnASegmentOfItsParent(t *testing.T) {
	s := newSegmentStack(t)

	if got := s.recordedBase("c"); got != gtSegmentBase("c") {
		t.Errorf("c recorded on %q, want %q", got, gtSegmentBase("c"))
	}
	if segment, parent := s.origin(t, gtSegmentBase("c")), s.origin(t, "p"); segment == "" || segment != parent {
		t.Errorf("origin %s = %q, want p's head %q", gtSegmentBase("c"), segment, parent)
	}
}

func TestStackSubmitMovesTheSegmentWithItsParent(t *testing.T) {
	s := newSegmentStack(t)
	s.amendParent(t)

	s.submit(t)
	if segment, parent := s.origin(t, gtSegmentBase("c")), s.origin(t, "p"); segment != parent {
		t.Errorf("origin %s = %q, want the amended p %q", gtSegmentBase("c"), segment, parent)
	}
	if got := s.recordedBase("c"); got != gtSegmentBase("c") {
		t.Errorf("c recorded on %q, want %q", got, gtSegmentBase("c"))
	}
}

func TestStackSubmitDeletesTheSegmentOnceGraphiteRecordsTheParent(t *testing.T) {
	s := newSegmentStack(t)
	delete(s.api.capped, "c")
	s.amendParent(t)
	writeShipGH(t, s.f)
	s.f.Setenv("GH_PULLS_JSON", ghOpenPullAt(t, 101, s.origin(t, "c"), "p"))

	errOut := s.submit(t)
	if got := s.recordedBase("c"); got != "p" {
		t.Errorf("c recorded on %q, want p", got)
	}
	if segment := s.origin(t, gtSegmentBase("c")); segment != "" {
		t.Errorf("origin %s = %q, want it deleted:\n%s", gtSegmentBase("c"), segment, errOut)
	}
}

func TestShipGTOpensThePullRequestGraphiteRefusesOnItsSegment(t *testing.T) {
	log, api := setupShipGT(t, true)
	api.submitErrors["feature"] = gtStackCapRefusal
	t.Setenv("GH_PULLS_JSON", "[]")
	created := ghOpenPullAt(t, 77, vcstest.GraphiteLeafSHA, gtSegmentBase("feature"))
	t.Setenv("GH_PULL_CREATE_JSON", created[1:len(created)-1])
	body := writePRBody(t, "body.md", "why this change\n")

	_, errStr, err := runShipCmdFull(api.ctx(context.Background()), t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "cli: open past the cap", "--pr-body-file", body)
	if err == nil || !strings.Contains(err.Error(), "graphite refused to record #77 feature") {
		t.Fatalf("ship error = %v (stderr=%q), want a refusal naming #77", err, errStr)
	}
	opened := slices.ContainsFunc(readInvocations(t, log), func(inv []string) bool {
		return len(inv) > 4 && inv[0] == "gh" && inv[3] == "POST" &&
			slices.Contains(inv, "base="+gtSegmentBase("feature")) && slices.Contains(inv, "title=cli: open past the cap") && slices.Contains(inv, "body=why this change\n")
	})
	if !opened {
		t.Errorf("invocations = %v, want one gh POST onto the segment carrying the title and body", readInvocations(t, log))
	}
	if refs := gtPushedRefs(readInvocations(t, log)); !slices.Contains(refs, gtSegmentBase("feature")) {
		t.Errorf("pushed %v, want the segment base pushed", refs)
	}
	if marked, err := gtUnrecorded(gtCapCommonDir(t), "feature"); err != nil || !marked {
		t.Errorf("unrecorded mark = %v, %v, want feature marked for a stack submit", marked, err)
	}
}

func TestShipGTMovesAnOpenPullRequestGraphiteRefusesOntoItsSegment(t *testing.T) {
	log, api := setupShipGT(t, true)
	api.prs["feature"] = 41
	api.submitErrors["feature"] = gtStackCapRefusal
	t.Setenv("GH_PULLS_JSON", ghOpenPullAt(t, 41, vcstest.GraphiteLeafSHA, "landed-parent"))

	_, errStr, err := runShipCmdFull(api.ctx(context.Background()), t, "-m", "fix: frobnicate", "--no-watch")
	if err == nil || !strings.Contains(err.Error(), "graphite refused to record #41 feature") {
		t.Fatalf("ship error = %v (stderr=%q), want a refusal naming #41", err, errStr)
	}
	moved := slices.ContainsFunc(readInvocations(t, log), func(inv []string) bool {
		return len(inv) > 4 && inv[0] == "gh" && inv[3] == "PATCH" && strings.HasSuffix(inv[4], "/pulls/41") && slices.Contains(inv, "base="+gtSegmentBase("feature"))
	})
	if !moved {
		t.Errorf("invocations = %v, want #41 moved onto its segment", readInvocations(t, log))
	}
	for _, inv := range readInvocations(t, log) {
		if len(inv) > 4 && inv[0] == "gh" && inv[3] == "POST" && strings.HasSuffix(inv[4], "/pulls") {
			t.Errorf("opened a second pull request over #41: %v", inv)
		}
	}
}

func TestShipGTClearsTheCapMarkOnceGraphiteRecordsTheBranch(t *testing.T) {
	_, api := setupShipGT(t, true)
	api.prs["feature"] = 41
	common := gtCapCommonDir(t)
	if err := gtMarkUnrecorded(common, "feature"); err != nil {
		t.Fatal(err)
	}

	if _, errStr, err := runShipCmdFull(api.ctx(context.Background()), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if marked, err := gtUnrecorded(common, "feature"); err != nil || marked {
		t.Errorf("unrecorded mark = %v, %v, want it cleared by the recorded submit", marked, err)
	}
}

func TestStackListLineNamesABranchGraphiteHasNoRecordOf(t *testing.T) {
	if got := stackListLine("feature", "", "/root", gtBranchState{}, true); !strings.Contains(got, "needs stack submit") {
		t.Errorf("line = %q, want the stack submit it needs", got)
	}
	if got := stackListLine("feature", "", "/root", gtBranchState{}, false); strings.Contains(got, "needs stack submit") {
		t.Errorf("line = %q, want no mark on a recorded branch", got)
	}
}
