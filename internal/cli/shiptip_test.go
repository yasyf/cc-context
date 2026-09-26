package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackSubmitTakesLanded(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	shipResetLog(t, f)

	out, _, err := runStackCmd(t, f, "submit", "--landed", "a")
	if err != nil {
		t.Fatalf("stack submit --landed a: %v", err)
	}
	if !strings.Contains(out, "a · drop (declared landed)") {
		t.Errorf("report = %q, want a dropped as declared landed", out)
	}
	assertLandedPublished(t, f, api, "1")
}

func TestShipTakesLanded(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "a", "b")
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	shipGTReady(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--landed", "a")
	if err != nil {
		t.Fatalf("ship --landed a = %v (stderr=%q)", err, errStr)
	}
	if !strings.Contains(got, "a · drop (declared landed)") {
		t.Errorf("summary = %q, want a dropped as declared landed", got)
	}
	assertLandedPublished(t, f, api, "2")
}

// stackResetOverPublished is a lane's hard reset: feature's published commit is
// gone from the local head, replaced by a commit of another branch's.
func stackResetOverPublished(t *testing.T) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "main")
	stackCommit(t, f, "other.txt")
	shipResetLog(t, f)
	return f, published
}

func TestStackSubmitRefusesALocalHeadDroppingPublishedCommits(t *testing.T) {
	f, published := stackResetOverPublished(t)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), `drops 1 commit(s) its published head origin/feature`) || !strings.Contains(err.Error(), `"feature"`) {
		t.Fatalf("submit after a hard reset = %v, want a refusal naming the dropped commit", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != published {
		t.Errorf("origin feature moved to %s, want %s left in place", got, published)
	}

	if _, _, err := runStackCmd(t, f, "submit", "--drop-commits"); err != nil {
		t.Fatalf("submit --drop-commits: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%s", "feature"); got != "other.txt" {
		t.Errorf("origin feature tip = %q, want the local head's other.txt", got)
	}
}

// stackMergeableBase is a submitted base, feature stack whose base pull request
// is open and mergeable, with trunk moved on since.
func stackMergeableBase(t *testing.T, mergeable string) (*vcstest.Fixture, *gtAPIStub, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	stubStackPRs(t, map[string]*stackPR{
		"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main", Mergeable: mergeable},
		"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
	})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	shipGTReady(t, f)
	return f, api, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
}

func stackAssertBaseKept(t *testing.T, f *vcstest.Fixture, base string) {
	t.Helper()
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("origin base moved to %s, want its approved head %s kept", got, base)
	}
	if !stackOnto(t, f, base, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")) {
		t.Error("origin feature does not sit on base's published head")
	}
}

func TestShipKeepsAMergeableAncestorOffNewerTrunk(t *testing.T) {
	f, _, base := stackMergeableBase(t, "MERGEABLE")

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	stackAssertBaseKept(t, f, base)
}

func TestStackRebaseMovesATrunkParentedBranchAShipKeptOffNewerTrunk(t *testing.T) {
	for _, tc := range []struct {
		name, mergeable string
		ship            []string
	}{
		{"mergeable ancestor", "MERGEABLE", nil},
		{"tip only", "CONFLICTING", []string{"--tip-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, base := stackMergeableBase(t, tc.mergeable)
			fork := gitAt(t, f.Env(), f.Dir, "merge-base", base, "origin/main")
			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tc.ship...)
			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship = %v (stderr=%q)", err, errStr)
			}
			stackAssertBaseKept(t, f, base)
			receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "base")
			if err != nil || receipt == nil {
				t.Fatalf("base publication = %+v, %v", receipt, err)
			}
			if receipt.Head != base || receipt.Base != fork {
				t.Errorf("base publication head %.12s base %.12s, want its kept head %.12s on its fork %.12s", receipt.Head, receipt.Base, base, fork)
			}

			out, errStr, err := runStackCmd(t, f, "rebase")
			if err != nil {
				t.Fatalf("stack rebase = %v (stderr=%q)", err, errStr)
			}
			if want := "base" + shipSep + "onto main" + shipSep + "from " + fork[:12]; !strings.Contains(out, want) {
				t.Errorf("plan = %q, want %q", out, want)
			}
			published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			if !stackOnto(t, f, "origin/main", published) {
				t.Errorf("origin base %s is not on the new trunk", published)
			}
			if !stackOnto(t, f, published, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")) {
				t.Error("origin feature does not sit on base's new head")
			}
		})
	}
}

func TestShipTipOnlyPushesNoAncestor(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
	}
	stackAssertBaseKept(t, f, base)
}

func TestShipTipOnlyCommitsWithAncestorCheckedOut(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")
	held := f.WorktreePath("held-base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")

	out, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-push", "--tip-only")
	if err != nil {
		t.Fatalf("ship --no-push --tip-only = %v (stderr=%q)", err, errStr)
	}
	if strings.Contains(out, "restacked") || !strings.Contains(out, "not pushed") {
		t.Errorf("summary = %q, want a commit without a restack or push", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != local {
		t.Errorf("local base moved from %s to %s", local, got)
	}
	if got := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); got != local {
		t.Errorf("held checkout moved from %s to %s", local, got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("published base moved from %s to %s", base, got)
	}

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --no-commit --tip-only = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	stackAssertBaseKept(t, f, base)
}

func TestShipTipOnlyPreviewsAndPublishesExplicitChildMeta(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish base: %v", err)
	}
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	api.prs["base"] = 100
	priorSubmits := len(api.submitHeads())
	stubStackPRs(t, map[string]*stackPR{"base": {Number: api.prs["base"], Title: "base", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"}})
	shipGTStack(t, f, "feature")
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Exact body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--no-commit", "--no-watch", "--tip-only", "--pr-title", "Exact child title", "--pr-body-file", body}
	report := dryRunReport(t, f, args...)
	if refs := dryRunBranches(report, "push ref"); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("preview push refs = %v, want only feature\n%s", refs, report)
	}
	if heads := dryRunValues(report, "pr head"); len(heads) != 0 {
		t.Errorf("preview ancestor PR heads = %v, want none", heads)
	}
	if creates := dryRunValues(report, "pr new"); len(creates) != 1 || !strings.Contains(creates[0], `"Exact child title"`) || !strings.Contains(creates[0], "body.md") {
		t.Errorf("preview child PR = %v, want explicit title and body", creates)
	}
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
		t.Fatalf("publish child = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	if heads := api.submitHeads()[priorSubmits:]; !slices.Equal(heads, []string{"feature"}) {
		t.Errorf("Graphite submitted %v, want only feature", heads)
	}
	entry := api.submitEntry("feature")
	if entry.Title == nil || *entry.Title != "Exact child title" || entry.Body == nil || *entry.Body != "Exact body\n" {
		t.Errorf("Graphite create title/body = %v/%v, want explicit values", entry.Title, entry.Body)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
		t.Errorf("published base moved from %s to %s", base, got)
	}
}

// TestShipKeepsAnApprovedParentOnlyRestackedLocally is ci-go's #25915: the parent
// #25742 was restacked locally onto newer trunk without a push, GitHub had not
// computed its mergeability, and each ship from the child force-pushed the
// parent and reset its approvals.
func TestShipKeepsAnApprovedParentOnlyRestackedLocally(t *testing.T) {
	f, _, base := stackMergeableBase(t, statusUnknown)
	mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", "--", "f.txt")
	if _, _, err := runStackCmd(t, f, "restack"); err != nil {
		t.Fatalf("stack restack: %v", err)
	}
	if local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); local == base {
		t.Fatal("fixture: restack left base where it was published")
	}
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	stackAssertBaseKept(t, f, base)
}
