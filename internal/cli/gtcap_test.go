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

func TestShipGTOpensThePullRequestGraphiteCapsThroughGitHub(t *testing.T) {
	log, api := setupShipGT(t, true)
	api.submitErrors["feature"] = gtStackCapRefusal
	t.Setenv("GH_PULLS_JSON", "[]")
	created := ghOpenPullAt(t, 77, vcstest.GraphiteLeafSHA, "main")
	t.Setenv("GH_PULL_CREATE_JSON", created[1:len(created)-1])
	body := writePRBody(t, "body.md", "why this change\n")

	_, errStr, err := runShipCmdFull(api.ctx(context.Background()), t, "-m", "fix: frobnicate", "--no-watch", "--pr-title", "cli: open past the cap", "--pr-body-file", body)
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "ship: graphite refused feature at its stack cap, so ccx opened PR #77 https://github.com/yasyf/cc-context/pull/77 on main through GitHub alone; Graphite has no record of it, so run ccx vcs stack submit after each parent lands to re-set its base, and once the stack is split"; !strings.Contains(errStr, want) {
		t.Errorf("stderr = %q, want %q", errStr, want)
	}
	opened := slices.ContainsFunc(readInvocations(t, log), func(inv []string) bool {
		return len(inv) > 4 && inv[0] == "gh" && inv[3] == "POST" &&
			slices.Contains(inv, "base=main") && slices.Contains(inv, "title=cli: open past the cap") && slices.Contains(inv, "body=why this change\n")
	})
	if !opened {
		t.Errorf("invocations = %v, want one gh POST carrying the title, body and base", readInvocations(t, log))
	}
	if marked, err := gtUnrecorded(gtCapCommonDir(t), "feature"); err != nil || !marked {
		t.Errorf("unrecorded mark = %v, %v, want feature marked for a stack submit", marked, err)
	}
}

func TestShipGTMovesAnOpenPullRequestGraphiteCapsOntoItsBase(t *testing.T) {
	log, api := setupShipGT(t, true)
	api.prs["feature"] = 41
	api.submitErrors["feature"] = gtStackCapRefusal
	t.Setenv("GH_PULLS_JSON", ghOpenPullAt(t, 41, vcstest.GraphiteLeafSHA, "landed-parent"))

	_, errStr, err := runShipCmdFull(api.ctx(context.Background()), t, "-m", "fix: frobnicate", "--no-watch")
	if err != nil {
		t.Fatalf("ship error = %v (stderr=%q)", err, errStr)
	}
	if want := "so ccx retargeted PR #41"; !strings.Contains(errStr, want) {
		t.Errorf("stderr = %q, want %q", errStr, want)
	}
	moved := slices.ContainsFunc(readInvocations(t, log), func(inv []string) bool {
		return len(inv) > 4 && inv[0] == "gh" && inv[3] == "PATCH" && strings.HasSuffix(inv[4], "/pulls/41") && slices.Contains(inv, "base=main")
	})
	if !moved {
		t.Errorf("invocations = %v, want #41 moved onto main", readInvocations(t, log))
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
