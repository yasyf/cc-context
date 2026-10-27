package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackSubmitTakesLanded(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b")
	api.prs["b"] = 42
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
	f.Decorate(api.ctx)
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
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	stubStackPRs(t, f, map[string]*stackPR{
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

func TestShipTipOnlyReadsAParentAnotherLaneRewroteLocally(t *testing.T) {
	for _, tc := range []struct{ name, parent, tip string }{
		{"one lane", "base", "feature"},
		{"another lane's parent", "yasyf/chat-move-r4", "chat-move-r4c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, tc.parent, tc.tip)
			if _, _, err := runStackCmd(t, f, "submit", "--all-lanes"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			stubStackPRs(t, f, map[string]*stackPR{
				tc.parent: {Number: 100, Title: tc.parent, State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
				tc.tip:    {Number: 101, Title: tc.tip, State: "OPEN", Base: tc.parent, Mergeable: "MERGEABLE"},
			})
			cut := gitAt(t, f.Env(), f.Dir, "rev-parse", tc.parent)
			held := f.WorktreePath("held-parent")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, tc.parent)
			writeShipFile(t, held, tc.parent+".txt", "amended and pushed\n")
			mustRun(t, f.Env(), held, "git", "commit", "-qa", "--amend", "--no-edit")
			mustRun(t, f.Env(), held, "git", "push", "-qf", "origin", tc.parent)
			mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", tc.parent, cut, tc.tip)
			mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", tc.tip)
			parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.parent)
			writeShipFile(t, held, tc.parent+".txt", "amended again and not pushed\n")
			mustRun(t, f.Env(), held, "git", "commit", "-qa", "--amend", "--no-edit")
			local := gitAt(t, f.Env(), f.Dir, "rev-parse", tc.parent)
			shipGTReady(t, f)

			if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only"); err != nil {
				t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{tc.tip}) {
				t.Errorf("pushed refs = %v, want only %s", refs, tc.tip)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", tc.parent); got != local {
				t.Errorf("the parent lane's local rewrite moved from %s to %s", local, got)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.parent); got != parent {
				t.Errorf("origin %s moved to %s, want its published head %s kept", tc.parent, got, parent)
			}
			if !stackOnto(t, f, parent, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.tip)) {
				t.Errorf("origin %s does not sit on %s's published head", tc.tip, tc.parent)
			}
		})
	}
}

func TestShipTipOnlyPreviewsAndPublishesExplicitChildMeta(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish base: %v", err)
	}
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	api.prs["base"] = 100
	priorSubmits := len(api.submitHeads())
	stubStackPRs(t, f, map[string]*stackPR{"base": {Number: api.prs["base"], Title: "base", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"}})
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

func TestShipBranchTipOnlyShipsABranchAnotherWorktreeHolds(t *testing.T) {
	f, _, _ := stackMergeableBase(t, "MERGEABLE")
	stackCommit(t, f, "tip.txt")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	held := f.WorktreePath("held-feature")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--no-checkout", held, "feature")
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--branch", "feature", "--tip-only", "--no-commit", "--no-watch"); err != nil {
		t.Fatalf("ship --branch feature = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != head {
		t.Errorf("origin feature = %s, want its local head %s", got, head)
	}
	if got := gitAt(t, f.Env(), f.Dir, "branch", "--show-current"); got != "main" {
		t.Errorf("this working copy moved to %q, want it left on main", got)
	}
	if files := gitAt(t, f.Env(), held, "ls-files"); files != "" {
		t.Errorf("the holder's empty index now lists %q, want it untouched", files)
	}
}

func TestShipBranchTipOnlyMovesNoOtherWorktreesCheckout(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "other")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	held := f.WorktreePath("held-feature")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")

	_, errStr, err := runShipCmdFull(f.Context(), t, "--branch", "feature", "--tip-only", "--no-commit", "--no-watch", "--parent", "other")
	if err == nil || !strings.Contains(err.Error(), filepath.Base(held)) {
		t.Fatalf("ship --parent other = %v (stderr=%q), want a refusal naming the worktree holding feature", err, errStr)
	}
	if got := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); got != head {
		t.Errorf("the holder's checkout moved to %s, want it left at %s", got, head)
	}
}

func TestShipBranchTipOnlyPushesTheParkedBaseWithTheBranch(t *testing.T) {
	f, api, base := stackMergeableBase(t, "MERGEABLE")
	api.parkOn(f)
	api.mu.Lock()
	api.parked["feature"] = "graphite-base/101"
	api.remote("update-ref", "refs/heads/graphite-base/101", base)
	api.mu.Unlock()
	stackCommit(t, f, "tip.txt")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--branch", "feature", "--tip-only", "--no-commit", "--no-watch"); err != nil {
		t.Fatalf("ship --branch feature = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature", "graphite-base/101"}) {
		t.Errorf("pushed refs = %v, want feature with its parked base graphite-base/101", refs)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != head {
		t.Errorf("origin feature = %s, want its local head %s", got, head)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "graphite-base/101"); got != base {
		t.Errorf("origin graphite-base/101 = %s, want it at feature's base %s so Graphite moves the pull request back", got, base)
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

// TestShipTipOnlyLeavesTheUpstackWhereItIs is release-5m's refusal: a
// --tip-only ship from the bottom of a stack replayed the branch above it onto
// the new commit, and refused on that branch's conflict.
func TestShipTipOnlyLeavesTheUpstackWhereItIs(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "core", "slack")
	slack := gitAt(t, f.Env(), f.Dir, "rev-parse", "slack")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "core")
	writeShipFile(t, f.Dir, "slack.txt", "core\n")
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--tip-only", "--no-watch"); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "slack"); got != slack {
		t.Errorf("slack moved to %s, want it left at %s", got, slack)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "core"), gitAt(t, f.Env(), f.Dir, "rev-parse", "core"); got != want {
		t.Errorf("origin core = %s, want the shipped head %s", got, want)
	}
}

// TestShipTipOnlyShipsOverADivergedGrandparent is platy-smarter's refusal: a
// --tip-only ship refused because a grandparent another lane owns had local
// and published heads that diverged, though the dry run promised to push no
// ancestor.
func TestShipTipOnlyShipsOverADivergedGrandparent(t *testing.T) {
	for _, tc := range []struct{ name, grandparent, parent, tip string }{
		{"one lane", "since", "target", "refusals"},
		{"ancestors of another lane", "yasyf/rth-since", "yasyf/rth-target", "platy-refusals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, tc.grandparent, tc.parent, tc.tip)
			if _, _, err := runStackCmd(t, f, "submit", "--all-lanes"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			mustRun(t, f.Env(), f.Dir, "git", "clone", "-q", "--branch", tc.grandparent, f.RemoteDir, elsewhere)
			mustRun(t, f.Env(), elsewhere, "git", "config", "user.name", "elsewhere")
			mustRun(t, f.Env(), elsewhere, "git", "config", "user.email", "elsewhere@example.com")
			writeShipFile(t, elsewhere, "pushed.txt", "pushed by someone else\n")
			mustRun(t, f.Env(), elsewhere, "git", "add", "pushed.txt")
			mustRun(t, f.Env(), elsewhere, "git", "commit", "-q", "--amend", "-m", "rewritten elsewhere")
			mustRun(t, f.Env(), elsewhere, "git", "push", "-qf", "origin", tc.grandparent)
			held := f.WorktreePath("held-grandparent")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, tc.grandparent)
			writeShipFile(t, held, "local.txt", "kept locally\n")
			mustRun(t, f.Env(), held, "git", "add", "local.txt")
			mustRun(t, f.Env(), held, "git", "commit", "-qm", "local only")
			grandparent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.grandparent)
			local := gitAt(t, f.Env(), f.Dir, "rev-parse", tc.grandparent)
			parent := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.parent)
			stackCommit(t, f, "tip.txt")

			args := []string{"--no-commit", "--no-watch", "--tip-only"}
			if refs := dryRunBranches(dryRunReport(t, f, args...), "push ref"); !slices.Equal(refs, []string{tc.tip}) {
				t.Errorf("preview push refs = %v, want only %s", refs, tc.tip)
			}
			shipResetLog(t, f)
			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship --tip-only = %v (stderr=%q)", err, errStr)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{tc.tip}) {
				t.Errorf("pushed refs = %v, want only %s", refs, tc.tip)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.grandparent); got != grandparent {
				t.Errorf("origin %s moved from %s to %s", tc.grandparent, grandparent, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", tc.grandparent); got != local {
				t.Errorf("local %s moved from %s to %s", tc.grandparent, local, got)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.parent); got != parent {
				t.Errorf("origin %s moved from %s to %s", tc.parent, parent, got)
			}
			if !stackOnto(t, f, parent, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", tc.tip)) {
				t.Errorf("origin %s does not sit on %s's published head", tc.tip, tc.parent)
			}
		})
	}
}

func TestShipTipOnlyTakesNoLockOnAParentAnotherLaneIsWriting(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")
	held := f.WorktreePath("held-base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	lock := filepath.Join(f.Dir, ".git", "refs", "heads", "base.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only beside a lane writing its parent = %v (stderr=%q)", err, errStr)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the parent lane's ref lock: %v, want it left in place", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != local {
		t.Errorf("local base moved from %s to %s", local, got)
	}
	stackAssertBaseKept(t, f, base)
}

func TestShipNamesTipOnlyWhenAHeldParentDiverged(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	held := f.WorktreePath("held-base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	mustRun(t, f.Env(), held, "git", "reset", "-q", "--hard", "main")
	writeShipFile(t, held, "other.txt", "other\n")
	mustRun(t, f.Env(), held, "git", "add", "other.txt")
	mustRun(t, f.Env(), held, "git", "commit", "-qm", "other")
	shipGTReady(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	want := "to ship feature alone onto base's published head, run ccx vcs ship --tip-only"
	if err == nil || !strings.Contains(err.Error(), "drops 1 commit(s)") || !strings.Contains(err.Error(), want) {
		t.Fatalf("ship over a diverged held parent = %v, want the refusal to name %q", err, want)
	}
	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only"); err != nil {
		t.Fatalf("ship --tip-only after the refusal = %v (stderr=%q)", err, errStr)
	}
}

func TestShipTipOnlyPublishesATipCutOnAnEmptyBase(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTLevel(t, f, "base")
	shipGTStack(t, f, "feature")
	stackForeignPush(t, f, "main", "trunk.txt", false)
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Exact body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--pr-title", "feature=Exact title", "--pr-body-file", "feature="+body); err != nil {
		t.Fatalf("ship --tip-only = %v (stderr=%q), want the tip published past its empty base", err, errStr)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"feature"}) {
		t.Errorf("Graphite submitted %v, want only feature", heads)
	}
	if entry := api.submitEntry("feature"); entry.Title == nil || *entry.Title != "Exact title" {
		t.Errorf("Graphite create title = %v, want the explicit title", entry.Title)
	}
	if gitBranchExists(t, f.Env(), f.RemoteDir, "base") {
		t.Error("origin carries the empty base")
	}
	if feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); !stackOnto(t, f, "origin/main", feature) {
		t.Errorf("published feature %s does not sit on the fetched trunk", feature)
	}
}

func TestShipTipOnlyMovesNoAncestorWhenItsParentsPublishedHeadMoves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		rewrite bool
		restack bool
	}{
		{"parent advanced", "f.txt", false, false},
		{"parent rewritten", "g.txt", true, false},
		{"parent advanced, restacked", "g.txt", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			shipGTStack(t, f, "base", "feature")
			if _, _, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit: %v", err)
			}
			stubStackPRs(t, f, map[string]*stackPR{
				"base":    {Number: 100, Title: "base", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
				"feature": {Number: 101, Title: "feature", State: "OPEN", Base: "base", Mergeable: "MERGEABLE"},
			})
			cut := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
			published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			stackForeignPush(t, f, "base", tc.file, tc.rewrite)
			base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
			shipGTReady(t, f)

			args := []string{"-m", "fix: frobnicate", "--no-watch", "--tip-only"}
			if tc.restack {
				args = append(args, "--restack")
			}
			out, errStr, err := runShipCmdFull(f.Context(), t, args...)
			if err != nil {
				t.Fatalf("ship %v = %v (stderr=%q)", args, err, errStr)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got != base {
				t.Errorf("origin base moved from %s to %s", base, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != cut {
				t.Errorf("local base moved from %s to %s", cut, got)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
				t.Errorf("pushed refs = %v, want only feature", refs)
			}
			feature := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
			if feature == published {
				t.Fatalf("origin feature stayed at %s, want the new commit pushed", published)
			}
			if tc.rewrite {
				if !stackOnto(t, f, base, feature) || stackOnto(t, f, cut, feature) {
					t.Errorf("origin feature %s, want it replayed onto base's rewritten head %.12s without the replaced %.12s", feature, base, cut)
				}
				if files := gitAt(t, f.Env(), f.Dir, "diff", "--name-only", base, feature); files != "f.txt\nfeature.txt" {
					t.Errorf("origin feature adds %q over base, want only its own f.txt and feature.txt", files)
				}
				return
			}
			if tc.restack {
				if !stackOnto(t, f, base, feature) {
					t.Errorf("origin feature %s, want it replayed onto base's advanced head %.12s", feature, base)
				}
				return
			}
			if !stackOnto(t, f, cut, feature) || stackOnto(t, f, base, feature) {
				t.Errorf("origin feature %s, want the new commit pushed on %.12s without a replay onto base's moved head %.12s", feature, cut, base)
			}
			if !strings.Contains(out, "kept feature on") || !strings.Contains(out, "--tip-only --restack replays it") {
				t.Errorf("summary = %q, want it to name the kept base and the --tip-only --restack that replays it", out)
			}
		})
	}
}

func TestShipTipOnlyAdoptsOntoAParentsPublishedHeadPastItsUnpushedWork(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stubStackPRs(t, f, map[string]*stackPR{
		"base": {Number: 100, Title: "base", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
	})
	base := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "base.txt", "feature's base\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "feature")
	held := f.WorktreePath("held-base")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "base")
	writeShipFile(t, held, "base.txt", "base's unpushed edit\n")
	mustRun(t, f.Env(), held, "git", "commit", "-qam", "unpushed")
	local := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("Exact body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shipResetLog(t, f)

	got, errStr, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--tip-only", "--parent", "base", "--pr-title", "feature=Exact title", "--pr-body-file", "feature="+body)
	if err != nil {
		t.Fatalf("ship --tip-only --parent base = %v (stderr=%q), want feature adopted onto base's published head", err, errStr)
	}
	if want := "tracked feature onto base's published head " + shortSHA(base); !strings.Contains(got, want) {
		t.Errorf("summary = %q, want it to carry %q", got, want)
	}
	if parent := shipParentOf(t, f, "feature"); parent != "base" {
		t.Errorf("feature's parent = %s, want base", parent)
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
		t.Errorf("pushed refs = %v, want only feature", refs)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != local {
		t.Errorf("the base lane's unpushed head moved from %s to %s", local, got)
	}
	stackAssertBaseKept(t, f, base)
}

func TestShipTipOnlySetsAsideAnExitedRunWritingItsTip(t *testing.T) {
	for _, pushed := range []bool{false, true} {
		t.Run("pushed="+strconv.FormatBool(pushed), func(t *testing.T) {
			f, _, base := stackMergeableBase(t, "CONFLICTING")
			run := stackPlantClaims(t, f, stackExitedPid(t), "", 0, []string{"feature"}, stackRebaseBranch{Name: "base", Kept: true}, stackRebaseBranch{Name: "feature"})
			if pushed {
				run.PushTargets = []stackPublicationTarget{{Name: "feature", Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")}}
				if err := stackSaveRun(run); err != nil {
					t.Fatal(err)
				}
			}

			got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only")
			if err != nil {
				t.Fatalf("ship --tip-only beside an exited run of its tip = %v (stderr=%q)", err, errStr)
			}
			if want := "set aside the exited stack rebase of base in " + run.dir + " (pid " + strconv.Itoa(run.Pid) + " on "; !strings.Contains(got, want) {
				t.Errorf("summary = %q, want %q", got, want)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, []string{"feature"}) {
				t.Errorf("pushed refs = %v, want only feature", refs)
			}
			stackAssertBaseKept(t, f, base)
			stackAssertNoRun(t, f)
		})
	}
}

func TestShipTipOnlyBesideRunsOfOtherBranches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		alive   bool
		claims  string
		refusal string
	}{
		{name: "live run of the tip", alive: true, claims: "feature", refusal: "a stack rebase of base is already in progress (pid "},
		{name: "live run of a sibling", alive: true, claims: "sibling"},
		{name: "exited run of a sibling", claims: "sibling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, base := stackMergeableBase(t, "CONFLICTING")
			pid, started := stackExitedPid(t), ""
			if tc.alive {
				live := exec.Command("sleep", "60")
				if err := live.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
				pid, started = live.Process.Pid, stackProcStart(live.Process.Pid)
			}
			stackPlantClaims(t, f, pid, started, 0, []string{tc.claims}, stackRebaseBranch{Name: "base", Kept: true}, stackRebaseBranch{Name: tc.claims})
			remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")

			got, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--tip-only")
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("ship --tip-only beside a live run of its tip = %q, %v, want %q", got, err, tc.refusal)
				}
				if now := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); now != remote {
					t.Errorf("origin feature moved to %s, want it untouched at %s", now, remote)
				}
			} else {
				if err != nil {
					t.Fatalf("ship --tip-only beside a run of %s = %v (stderr=%q)", tc.claims, err, errStr)
				}
				if strings.Contains(got, "set aside") {
					t.Errorf("summary = %q, want the run of %s left alone", got, tc.claims)
				}
				stackAssertBaseKept(t, f, base)
			}
			runs, err := stackRuns(io.Discard, filepath.Join(f.Dir, ".git"))
			if err != nil || len(runs) != 1 || !slices.Equal(runs[0].Claims, []string{tc.claims}) {
				t.Fatalf("runs = %v, %v, want only the run of %s kept", runs, err, tc.claims)
			}
		})
	}
}

func TestShipTipOnlyClaimsOnlyItsTipPastAnotherLanesStoppedRebase(t *testing.T) {
	t.Parallel()
	stopped := &stackRebaseRun{
		Roots:    []string{"bottom"},
		Claims:   []string{"bottom", "sibling"},
		Conflict: &stackConflict{Branch: "sibling", Workspace: "conflict-sibling"},
	}
	published := func(name, parent string) stackRebaseBranch {
		return stackRebaseBranch{Name: name, Parent: parent, Head: "published-" + name, Local: "published-" + name, Kept: true}
	}
	for _, c := range []struct {
		name     string
		tipOnly  bool
		claims   []string
		overlaps bool
	}{
		{"tip only", true, []string{"feature"}, false},
		{"whole stack", false, []string{"bottom"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ship := &stackRebaseRun{
				Roots:    []string{"bottom"},
				Tip:      "feature",
				TipOnly:  c.tipOnly,
				Branches: []stackRebaseBranch{published("bottom", "main"), published("feature", "bottom")},
			}
			ship.Claims = stackRunClaims(ship)
			if !slices.Equal(ship.Claims, c.claims) || stackOverlaps(ship, stopped) != c.overlaps {
				t.Errorf("a ship publishing nothing claims %v, overlapping the stopped rebase of bottom: %v; want %v, %v", ship.Claims, stackOverlaps(ship, stopped), c.claims, c.overlaps)
			}
		})
	}
}
