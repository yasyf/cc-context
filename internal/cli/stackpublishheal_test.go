package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func healReceipt(t *testing.T, f *vcstest.Fixture, branch string) *stackPublication {
	t.Helper()
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), branch)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil {
		t.Fatalf("%s has no publication receipt", branch)
	}
	return receipt
}

func healWriteReceipt(t *testing.T, f *vcstest.Fixture, receipt stackPublication) {
	t.Helper()
	dir := render.Dir(f.Dir)
	var tx strings.Builder
	tx.WriteString("start\n")
	if err := stackReceiptTx(f.Context(), dir, &tx, receipt, healReceipt(t, f, receipt.Branch).OID); err != nil {
		t.Fatal(err)
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(f.Context(), dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		t.Fatal(err)
	}
}

// TestShipPublishesOverAGraphiteRecordTheReceiptOutran is the "publication
// metadata changed" refusal release-slack-backlog, release-picture and
// argo-bluegreen hit: Graphite's last submitted version disagreed with the
// receipt while the remote still held exactly what the receipt names.
func TestShipPublishesOverAGraphiteRecordTheReceiptOutran(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stale := gtmeta.Version{HeadSha: gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"), BaseSha: gitAt(t, f.Env(), f.Dir, "rev-parse", "feature~1"), BaseName: "main"}
	if err := gtmeta.RecordSubmitted(f.Context(), filepath.Join(f.Dir, ".git"), map[string]gtmeta.Version{"feature": stale}); err != nil {
		t.Fatal(err)
	}
	shipGTReady(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship over a stale graphite record = %v (stderr=%q)", err, errStr)
	}
	if got, want := healReceipt(t, f, "feature").Head, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != want {
		t.Errorf("receipt head = %s, want the pushed %s", got, want)
	}
}

// TestStackRebaseStartsAtTheForkPointOverAReceiptBaseTrunkHolds is
// release-target-steps and release-picture-llm: a receipt recorded after the
// branch took newer trunk still named the old base, and the rebase refused to
// replay trunk's own commits.
func TestStackRebaseStartsAtTheForkPointOverAReceiptBaseTrunkHolds(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	published := healReceipt(t, f, "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/feature")
	stackAdvanceTrunk(t, f, "second.txt", "second\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "-f", "origin", "feature")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	healWriteReceipt(t, f, stackPublication{Branch: "feature", Source: head, SourceBase: published.Base, Head: head, Base: published.Base, Parent: "main"})

	plan, _, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("stack rebase --dry-run over a stale receipt base: %v", err)
	}
	if want := "from " + gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")[:12]; !strings.Contains(plan, want) {
		t.Errorf("plan = %q, want feature replayed %s", plan, want)
	}
}

func TestVcsPushRecordsTheForkPointAfterTakingNewerTrunk(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/feature")
	stackAdvanceTrunk(t, f, "second.txt", "second\n")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push: %v", err)
	}

	if got, want := healReceipt(t, f, "feature").Base, gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main"); got != want {
		t.Errorf("receipt base = %s, want the trunk it now sits on %s", got, want)
	}
}

// TestVcsPushFindsAChildsOwnCommitsAfterItsParentWasRebased is release-notes-themes
// on release-picture-llm: with the parent force-rebased, the child's published
// base and gt's recorded parent head were both off HEAD, and push refused.
func TestVcsPushFindsAChildsOwnCommitsAfterItsParentWasRebased(t *testing.T) {
	f := stackRebaseRepo(t, "p", "c")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	oldParent := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/p")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/p")
	writeShipFile(t, f.Dir, "p.txt", "rewritten\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qa", "--amend", "-m", "p")
	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push p: %v", err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "origin/c")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "p", oldParent, "c")

	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatalf("vcs push c after p was rebased: %v", err)
	}
	if got, want := healReceipt(t, f, "c").Base, gitAt(t, f.Env(), f.Dir, "rev-parse", "p"); got != want {
		t.Errorf("c's receipt base = %s, want p's new head %s", got, want)
	}
}

func TestStackSubmitKeepsTheSiblingGraphiteRecordsOverAStaleReceipt(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "p", "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "p")
	shipGTStack(t, f, "z")
	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "z=a"); err != nil {
		t.Fatalf("stack rebase --parent z=a: %v", err)
	}
	prior := *healReceipt(t, f, "z")
	pushCommit(t, f, "first.txt", "first\n", "first")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	healWriteReceipt(t, f, prior)
	pushCommit(t, f, "next.txt", "next\n", "next")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit over a stale receipt: %v", err)
	}
	if got := healReceipt(t, f, "z").Parent; got != "a" {
		t.Errorf("z's receipt parent = %s, want the sibling a it is published onto", got)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "z")) {
		t.Error("origin z left the sibling a")
	}
}

func TestStackSubmitKeepsASeparatelyTrackedSiblingGraphiteRecordsOverAStaleReceipt(t *testing.T) {
	f := shipGTRepo(t)
	stubGTAPI(t)
	shipGTStack(t, f, "a")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTStack(t, f, "z")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stale := *healReceipt(t, f, "z")
	if _, _, err := runStackCmd(t, f, "rebase", "--parent", "z=a"); err != nil {
		t.Fatalf("stack rebase --parent z=a: %v", err)
	}
	if got := healReceipt(t, f, "z").Parent; got != "a" {
		t.Fatalf("fixture: z published onto %s, want a", got)
	}
	healWriteReceipt(t, f, stale)
	pushCommit(t, f, "next.txt", "next\n", "next")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit over a stale receipt: %v", err)
	}
	if !stackOnto(t, f, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "z")) {
		t.Error("origin z left the sibling a Graphite last published it onto")
	}
}
