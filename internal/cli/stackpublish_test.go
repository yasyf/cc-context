package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func prepareStackPublication(t *testing.T) (*vcstest.Fixture, *stackRebaseRun, []gtSubmitBranch) {
	t.Helper()
	f := stackRebaseRepo(t, "feature")
	source := shipHead(t, f)
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	pin := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main")
	b := stackRebaseBranch{Name: "feature", Parent: "main", WasParent: "main", Local: source, Remote: source, Head: source, OldBase: base, SourceBase: base, NewBase: pin}
	run := &stackRebaseRun{Trunk: "main", Pin: pin, Origin: f.Dir, Roots: []string{"feature"}}
	if err := stackClaim(filepath.Join(f.Dir, ".git"), run); err != nil {
		t.Fatal(err)
	}
	var err error
	b.NewHead, err = stackReplay(f.Context(), render.Dir(f.Dir), run, &b)
	if err != nil {
		t.Fatal(err)
	}
	run.Branches = []stackRebaseBranch{b}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := stackPinPublication(f.Context(), render.Dir(f.Dir), run); err != nil {
		t.Fatal(err)
	}
	plan := []gtSubmitBranch{{name: b.Name, head: b.NewHead, base: b.Parent, baseSha: b.NewBase, lease: b.Remote, leaseSet: true}}
	shipResetLog(t, f)
	return f, run, plan
}

func TestStackPublicationRecoversAfterPushBeforeMetadata(t *testing.T) {
	f, run, plan := prepareStackPublication(t)
	dir := render.Dir(f.Dir)
	common := filepath.Join(f.Dir, ".git")
	sub := gtSubmit{prefix: "test", publication: run}
	if err := stackPushPublication(f.Context(), dir, sub, plan); err != nil {
		t.Fatal(err)
	}
	if receipt, err := stackReadPublication(f.Context(), dir, "feature"); err != nil || receipt != nil {
		t.Fatalf("premature receipt: %#v %v", receipt, err)
	}
	saved, err := stackOnlyTestRun(common)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Pushed || saved.Receipted {
		t.Fatalf("lost recovery phase: %#v", saved)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", stackPublicationPin(saved, "feature")); got != plan[0].head {
		t.Fatal("published replay was not pinned")
	}
	sub.publication = saved
	if err := stackPushPublication(f.Context(), dir, sub, plan); err != nil {
		t.Fatal(err)
	}
	if err := gtmeta.RecordSubmitted(f.Context(), common, map[string]gtmeta.Version{"feature": {HeadSha: plan[0].head, BaseSha: plan[0].baseSha, BaseName: plan[0].base}}); err != nil {
		t.Fatal(err)
	}
	if err := stackRecordPublication(f.Context(), dir, saved, plan); err != nil {
		t.Fatal(err)
	}
	if err := stackRecordPublication(f.Context(), dir, saved, plan); err != nil {
		t.Fatal(err)
	}
	if err := stackCompletePublication(f.Context(), dir, common, saved); err != nil {
		t.Fatal(err)
	}
	if got := shipHead(t, f); got != run.Branches[0].Local {
		t.Fatal("source moved during recovery")
	}
	if got := stackPublicationPushes(t, f); got != 1 {
		t.Fatalf("recovery pushed %d times", got)
	}
	receipt, err := stackReadPublication(f.Context(), dir, "feature")
	if err != nil || receipt == nil || receipt.Head != plan[0].head || receipt.Source != run.Branches[0].Local {
		t.Fatalf("receipt = %#v, %v", receipt, err)
	}
	if present, err := gitRefExists(f.Context(), dir, "test", stackPublicationPin(saved, "feature")); err != nil || present {
		t.Fatalf("run pin retained after receipt: %v %v", present, err)
	}
}

func TestStackPublicationPinsSourceAcrossConcurrentChanges(t *testing.T) {
	for _, duringPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "before push", true: "during push"}[duringPush], func(t *testing.T) {
			f, run, plan := prepareStackPublication(t)
			source := run.Branches[0].Local
			changed := gitAt(t, f.Env(), f.Dir, "commit-tree", source+"^{tree}", "-p", source, "-m", "concurrent source")
			writeShipFile(t, f.Dir, "feature.txt", "concurrent dirty edit\n")
			before := gitAt(t, f.Env(), f.Dir, "diff")
			if duringPush {
				realBin := shipDisplaceShim(t, f, "git")
				writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ -z \"$CCX_SHIM_DEPTH\" ] && [ \"$1\" = push ]; then\n  CCX_SHIM_DEPTH=1 git update-ref refs/heads/feature '"+changed+"' '"+source+"' || exit $?\nfi\nexec '"+realBin+"' \"$@\"\n")
			} else {
				mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/feature", changed, source)
			}
			err := stackPushPublication(f.Context(), render.Dir(f.Dir), gtSubmit{prefix: "test", publication: run}, plan)
			if duringPush {
				if err != nil {
					t.Fatal(err)
				}
				err = stackCheckSources(f.Context(), render.Dir(f.Dir), run)
			}
			if err == nil || !strings.Contains(err.Error(), "source branch moved") {
				t.Fatalf("source race was not reported: %v", err)
			}
			if got := shipHead(t, f); got != changed {
				t.Fatal("concurrent source ref overwritten")
			}
			if got := gitAt(t, f.Env(), f.Dir, "diff"); got != before {
				t.Fatal("concurrent dirty edit changed")
			}
			want := source
			if duringPush {
				want = plan[0].head
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != want {
				t.Fatalf("remote got unpinned commit %s, want %s", got, want)
			}
		})
	}
}

func TestStackPublicationRejectsUnrelatedRemoteDuringRecovery(t *testing.T) {
	f, run, plan := prepareStackPublication(t)
	dir := render.Dir(f.Dir)
	if err := stackPushPublication(f.Context(), dir, gtSubmit{publication: run}, plan); err != nil {
		t.Fatal(err)
	}
	foreign := gitAt(t, f.Env(), f.Dir, "commit-tree", plan[0].head+"^{tree}", "-p", plan[0].head, "-m", "foreign remote")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", foreign+":refs/heads/feature")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "feature")
	shipResetLog(t, f)
	if err := stackPushPublication(f.Context(), dir, gtSubmit{publication: run}, plan); err == nil || !strings.Contains(err.Error(), "remote heads changed") {
		t.Fatalf("accepted foreign remote: %v", err)
	}
	if stackPublicationPushes(t, f) != 0 {
		t.Fatal("retried a changed remote lease")
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != foreign {
		t.Fatal("foreign remote overwritten")
	}
}

func TestStackPublicationNextShipUsesOwnedReceipt(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	source := shipHead(t, f)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if source == published {
		t.Fatal("fixture did not replay source")
	}
	if source = shipHead(t, f); source != published {
		t.Fatalf("source = %s, want it moved onto its publication %s", source, published)
	}
	for _, changed := range []bool{false, true} {
		if changed {
			writeShipFile(t, f.Dir, "next.txt", "next source commit\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "next.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "next source commit")
			source = shipHead(t, f)
		}
		shipResetLog(t, f)
		if _, _, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch"); err != nil {
			t.Fatal(err)
		}
		if got := shipHead(t, f); got != source {
			t.Fatal("next ship moved source")
		}
		next := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
		if !changed && next != published {
			t.Fatal("unchanged source replayed again")
		}
		if changed && next == published {
			t.Fatal("new source was not published")
		}
		receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
		if err != nil || receipt == nil || receipt.Source != source || receipt.Head != next {
			t.Fatalf("next receipt: %#v %v", receipt, err)
		}
		for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
			if len(argv) > 1 && argv[0] == "git" && argv[1] == "push" {
				if !slices.Contains(argv, "--atomic") || !slices.Contains(argv, "--force-with-lease=refs/heads/feature:"+published) || !slices.Contains(argv, next+":refs/heads/feature") {
					t.Fatalf("not exact atomic publication: %v", argv)
				}
			}
		}
		published = next
	}
	foreign := gitAt(t, f.Env(), f.Dir, "commit-tree", published+"^{tree}", "-p", published, "-m", "unrelated remote")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", foreign+":refs/heads/feature")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin", "feature")
	shipResetLog(t, f)
	if _, _, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch"); err == nil {
		t.Fatal("next ship adopted unrelated fetched remote")
	}
	if stackPublicationPushes(t, f) != 0 {
		t.Fatal("pushed over unrelated remote")
	}
}

func stackPublicationPushes(t *testing.T, f *vcstest.Fixture) int {
	t.Helper()
	count := 0
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) > 1 && argv[0] == "git" && argv[1] == "push" {
			count++
		}
	}
	return count
}

func TestStackPublicationReceiptIdentitySurvivesRunSave(t *testing.T) {
	run := &stackRebaseRun{Roots: []string{"feature"}, Branches: []stackRebaseBranch{{Name: "feature", Publication: &stackPublication{OID: strings.Repeat("a", 40)}}}}
	common := t.TempDir()
	if err := stackClaim(common, run); err != nil {
		t.Fatal(err)
	}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	saved, err := stackOnlyTestRun(common)
	if err != nil || saved.Branches[0].Publication.OID != run.Branches[0].Publication.OID {
		t.Fatalf("lost receipt compare-and-swap identity: %#v %v", saved, err)
	}
	if err := os.RemoveAll(run.dir); err != nil {
		t.Fatal(err)
	}
}

func TestStackPublicationDropsItsLandedPublishedParent(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	basePublished := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stackAdvanceTrunk(t, f, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", ":refs/heads/base")
	stubStackPRs(t, f, map[string]*stackPR{"base": {Number: 5, State: "CLOSED", Landed: true, Head: basePublished}})
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != basePublished {
		t.Fatalf("landed base = %s, want it left on its first publication %s", got, basePublished)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil || receipt == nil || receipt.Parent != "main" {
		t.Fatalf("child did not publish over landed parent: %#v %v", receipt, err)
	}
	if got := shipHead(t, f); got != receipt.Head {
		t.Fatalf("child = %s, want it moved onto its publication %s", got, receipt.Head)
	}
	if count := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main.."+receipt.Head); count != "1" {
		t.Fatalf("published child retained %s commits", count)
	}
}

func TestStackPublicationRecoversUnrecordedPushAfterSourceMoves(t *testing.T) {
	for _, remoteMatches := range []bool{true, false} {
		t.Run(map[bool]string{true: "published", false: "different remote"}[remoteMatches], func(t *testing.T) {
			f, run, plan := prepareStackPublication(t)
			dir := render.Dir(f.Dir)
			common := filepath.Join(f.Dir, ".git")
			run.Publishing = true
			run.PushTargets = stackPublicationTargets(plan)
			if err := stackSaveRun(run); err != nil {
				t.Fatal(err)
			}
			if remoteMatches {
				mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", plan[0].head+":refs/heads/feature", "--force-with-lease=refs/heads/feature:"+plan[0].lease, "--atomic")
			}
			source := run.Branches[0].Local
			changed := gitAt(t, f.Env(), f.Dir, "commit-tree", source+"^{tree}", "-p", source, "-m", "concurrent source")
			mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/feature", changed, source)
			shipResetLog(t, f)
			saved, err := stackOnlyTestRun(common)
			if err != nil {
				t.Fatal(err)
			}
			err = stackPinPublication(f.Context(), dir, saved)
			if remoteMatches {
				if err != nil || !saved.Pushed {
					t.Fatalf("exact published targets did not recover: %v", err)
				}
				if err := stackPushPublication(f.Context(), dir, gtSubmit{publication: saved}, plan); err != nil {
					t.Fatal(err)
				}
				if err := gtmeta.RecordSubmitted(f.Context(), common, map[string]gtmeta.Version{"feature": {HeadSha: plan[0].head, BaseSha: plan[0].baseSha, BaseName: plan[0].base}}); err != nil {
					t.Fatal(err)
				}
				if err := stackRecordPublication(f.Context(), dir, saved, plan); err != nil {
					t.Fatal(err)
				}
				receipt, err := stackReadPublication(f.Context(), dir, "feature")
				if err != nil || receipt == nil || receipt.Source != source || receipt.Head != plan[0].head {
					t.Fatalf("recovered receipt lost original identities: %#v %v", receipt, err)
				}
			} else if err == nil || saved.Pushed {
				t.Fatalf("different remote accepted after source changed: %v", err)
			}
			if stackPublicationPushes(t, f) != 0 {
				t.Fatal("recovery made a new push")
			}
			if got := shipHead(t, f); got != changed {
				t.Fatal("recovery overwrote concurrent source")
			}
			if present, err := gitRefExists(f.Context(), dir, "test", stackPublicationPin(saved, "feature")); err != nil || !present {
				t.Fatalf("recovery discarded its pin: %v %v", present, err)
			}
		})
	}
}

func TestVcsPushRepublishesAFixOnThePublishedHead(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	stubStackPRs(t, f, map[string]*stackPR{"a": {Number: 41, State: "MERGED", Landed: true, Head: gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a")}})
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "gt", "track", "b", "--parent", "main", "--no-interactive")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", published)
	fix := pushCommit(t, f, "fix.txt", "fix\n", "fix")
	if _, err := runVcsPushCmd(f.Context(), t); err != nil {
		t.Fatal(err)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "b")
	if err != nil || receipt == nil || receipt.Source != fix || receipt.Head != fix || receipt.Parent != "main" {
		t.Fatalf("pushed receipt = %+v %v, want source and head %s on main", receipt, err, fix)
	}
	for _, field := range []string{"source", "head"} {
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", stackPublicationRef("b", field)); got != fix {
			t.Errorf("published %s = %s, want %s", field, got, fix)
		}
	}
	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run"); err != nil {
		t.Fatalf("rebase dry run after push: %v", err)
	}
	shipResetLog(t, f)
	if _, _, err := runShipCmdFull(f.Context(), t, "--no-commit", "--no-watch", "--restack"); err != nil {
		t.Fatalf("ship after push: %v", err)
	}
	if count := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main..origin/b"); count != "2" {
		t.Fatalf("republished b carries %s commits above trunk, want its own and the fix", count)
	}
}

func TestStackPublicationSurvivesGraphiteForgettingItsSubmit(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if err := gtmeta.RecordSubmitted(f.Context(), filepath.Join(f.Dir, ".git"), map[string]gtmeta.Version{"feature": {}}); err != nil {
		t.Fatal(err)
	}
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	if _, _, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
		t.Fatalf("submit after graphite forgot the last submit: %v", err)
	}
	if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); remote == published || !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("republished feature %s missed fresh trunk", remote)
	}
}

func TestStackPublicationYieldsToTheLanesOwnPush(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	source := pushCommit(t, f, "fix.txt", "fix\n", "fix")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "feature")
	stackAdvanceTrunk(t, f, "later.txt", "later\n")
	if _, _, err := runStackCmd(t, f, "rebase", "--dry-run"); err != nil {
		t.Fatalf("rebase dry run after the lane pushed its own source: %v", err)
	}
	if _, _, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
		t.Fatalf("submit after the lane pushed its own source: %v", err)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if remote == source || !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("republished feature %s missed fresh trunk", remote)
	}
	if got := shipHead(t, f); got != remote {
		t.Fatalf("source = %s, want it moved onto its publication %s", got, remote)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil || receipt == nil || receipt.Source != remote || receipt.Head != remote {
		t.Fatalf("receipt = %+v %v, want source and head %s", receipt, err, remote)
	}
}

func TestStackSubmitForcesOverTheLanesOwnOldHead(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	old := pushCommit(t, f, "next.txt", "next\n", "next")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "HEAD~1")
	rewritten := pushCommit(t, f, "next.txt", "rewritten\n", "next, rewritten")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("submit over the lane's own old head %s: %v", shortOID(old), err)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if got := gitAt(t, f.Env(), f.RemoteDir, "show", remote+":next.txt"); got != "rewritten" {
		t.Fatalf("published next.txt = %q, want the rewrite %s", got, shortOID(rewritten))
	}
}

func TestStackSubmitAdoptsAForeignIdenticalRestack(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	pushCommit(t, f, "next.txt", "next\n", "next")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	source := shipHead(t, f)
	clone := filepath.Join(t.TempDir(), "foreign")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", f.Dir, clone)
	mustRun(t, f.Env(), clone, "git", "checkout", "-q", "feature")
	mustRun(t, f.Env(), clone, "git", "-c", "user.name=other", "-c", "user.email=o@o.o", "rebase", "-q", "--onto", gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/main"), gitAt(t, f.Env(), f.Dir, "rev-parse", "main"))
	mustRun(t, f.Env(), clone, "git", "push", "-qf", f.RemoteDir, "feature")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	if _, _, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
		t.Fatalf("submit after a foreign identical restack: %v", err)
	}
	foreign := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if foreign == source {
		t.Fatal("fixture: the foreign restack did not replay the source")
	}
	if got := shipHead(t, f); got != foreign {
		t.Fatalf("source = %s, want it moved onto the adopted restack %s", got, foreign)
	}
}

func TestStackSubmitAdoptsGraphitesRestackAfterALanding(t *testing.T) {
	f := stackRebaseRepo(t, "a", "b")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	a := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "a")
	stubStackPRs(t, f, map[string]*stackPR{"a": {Number: 41, Title: "a", State: "MERGED", Landed: true, Head: a}})
	restackSquashRemote(t, f, "main", "a (#41)", "a")
	clone := filepath.Join(t.TempDir(), "graphite-app")
	mustRun(t, f.Env(), filepath.Dir(clone), "git", "clone", "-q", "--branch", "b", f.RemoteDir, clone)
	mustRun(t, f.Env(), clone, "git", "-c", "user.name=graphite-app", "-c", "user.email=g@g.g", "rebase", "-q", "--onto", "origin/main", a)
	mustRun(t, f.Env(), clone, "git", "push", "-qf", "origin", "b")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("submit after graphite-app restacked b onto the landing: %v", err)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	if !stackOnto(t, f, "origin/main", remote) {
		t.Fatalf("republished b %s missed the landing", remote)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "show", remote+":b.txt"); got != "b" {
		t.Fatalf("published b.txt = %q, want b", got)
	}
}

func TestStackPublicationFinishesMovingSourcesAfterAnInterruptedMove(t *testing.T) {
	f, run, plan := prepareStackPublication(t)
	dir := render.Dir(f.Dir)
	common := filepath.Join(f.Dir, ".git")
	if err := stackPushPublication(f.Context(), dir, gtSubmit{prefix: "test", publication: run}, plan); err != nil {
		t.Fatal(err)
	}
	if err := stackRecordPublication(f.Context(), dir, run, plan); err != nil {
		t.Fatal(err)
	}
	l, err := resolveLane(f.Context(), "stack", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		saved, err := stackOnlyTestRun(common)
		if err != nil {
			t.Fatal(err)
		}
		report, err := stackMovePublishedSources(f.Context(), l, common, saved)
		if err != nil || report != "moved feature onto the published heads" {
			t.Fatalf("attempt %d = %q, %v, want feature moved", attempt, report, err)
		}
		if got := shipHead(t, f); got != plan[0].head {
			t.Fatalf("attempt %d: feature = %s, want its published head %s", attempt, got, plan[0].head)
		}
		if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain"); status != "" {
			t.Fatalf("attempt %d left the checkout unaligned: %q", attempt, status)
		}
	}
	receipt, err := stackReadPublication(f.Context(), dir, "feature")
	if err != nil || receipt == nil || receipt.Source != plan[0].head || receipt.Head != plan[0].head {
		t.Fatalf("receipt = %+v, %v, want source and head %s", receipt, err, plan[0].head)
	}
	saved, err := stackOnlyTestRun(common)
	if err != nil {
		t.Fatal(err)
	}
	if err := stackCompletePublication(f.Context(), dir, common, saved); err != nil {
		t.Fatal(err)
	}
}

// stackPublishedElsewhere publishes feature, then hands it to a second worktree,
// the per-branch layout ccx vcs stack new cuts.
func stackPublishedElsewhere(t *testing.T) (*vcstest.Fixture, *stackRebaseRun, lane, string, string) {
	t.Helper()
	f, run, plan := prepareStackPublication(t)
	dir := render.Dir(f.Dir)
	if err := stackPushPublication(f.Context(), dir, gtSubmit{prefix: "test", publication: run}, plan); err != nil {
		t.Fatal(err)
	}
	if err := stackRecordPublication(f.Context(), dir, run, plan); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	held := f.WorktreePath("held")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "feature")
	l, err := resolveLane(f.Context(), "stack", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return f, run, l, filepath.Join(f.Dir, ".git"), held
}

// TestStackPublicationRechecksAHolderOnResume stops a run after it chose to move
// feature and before the ref moved; meanwhile the holding worktree gains an
// ignored file the published head tracks. The resumed move refuses rather than
// overwrite it.
func TestStackPublicationRechecksAHolderOnResume(t *testing.T) {
	f, run, l, common, held := stackPublishedElsewhere(t)
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	holders, err := vcs.BranchHolders(f.Context(), l.checkout)
	if err != nil {
		t.Fatal(err)
	}
	if left, err := stackChooseSourceMoves(f.Context(), l, run, holders); err != nil || len(left) != 0 {
		t.Fatalf("choose = %q, %v, want feature moved", left, err)
	}
	run.SourcesMoving = true
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, "info", "exclude"), []byte("upstream.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "upstream.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := stackOnlyTestRun(common)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := stackMovePublishedSources(f.Context(), l, common, saved); err == nil || !strings.Contains(err.Error(), "would overwrite upstream.txt") {
		t.Fatalf("resumed move = %v, want the ignored file refused", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != source {
		t.Errorf("feature = %s, want it left on its source %s", got, source)
	}
	if content, err := os.ReadFile(filepath.Join(held, "upstream.txt")); err != nil || string(content) != "local\n" {
		t.Errorf("held upstream.txt = %q, %v, want it untouched", content, err)
	}
}

// TestStackPublicationAlignsOnlyTheCurrentHolder switches the holding worktree
// to another branch at the moment the refs move: the move re-reads the holders,
// so that worktree is not realigned onto feature's published head.
func TestStackPublicationAlignsOnlyTheCurrentHolder(t *testing.T) {
	f, run, l, common, held := stackPublishedElsewhere(t)
	realGit := shipDisplaceShim(t, f, "git")
	marker := filepath.Join(t.TempDir(), "switched")
	writeShipExecutable(t, f.ShimBin, "git", fmt.Sprintf(`#!/bin/sh
if [ "$1" = update-ref ] && [ "$2" = --stdin ] && [ ! -e %[1]q ]; then
  %[2]q -C %[3]q switch -q -c unrelated || exit $?
  : > %[1]q
fi
exec %[2]q "$@"
`, marker, realGit, held))

	report, err := stackMovePublishedSources(f.Context(), l, common, run)
	if err != nil || report != "moved feature onto the published heads" {
		t.Fatalf("move = %q, %v, want feature moved", report, err)
	}
	if branch := gitAt(t, f.Env(), held, "branch", "--show-current"); branch != "unrelated" {
		t.Errorf("held is on %q, want unrelated", branch)
	}
	if status := gitAt(t, f.Env(), held, "status", "--porcelain"); status != "" {
		t.Errorf("held status = %q, want unrelated left as it was", status)
	}
}

func TestStackContinueOpensNoPullRequestWithoutAPreparedBody(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base")
	api.prs["base"] = 7
	stubOpenPRs(t, f, nil, "base")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish base: %v", err)
	}
	stackConflicting(t, f)
	_, _, err := runStackCmd(t, f, "submit")
	if err == nil {
		t.Fatal("stack submit succeeded, want the conflict on feature")
	}
	ws := stackWorkspaceOf(t, err)
	writeShipFile(t, ws, "c.txt", "trunk\nfeature\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	posted := len(api.submitHeads())

	_, errOut, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v (stderr=%q)", err, errOut)
	}
	if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"base"}) {
		t.Errorf("submit posts = %v, want base alone: feature has no pull request and no prepared body", heads)
	}
	if want := "pushed, not submitted: feature has no pull request"; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
	if got, local := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != local {
		t.Errorf("origin feature = %.12s, want the resolved head %.12s pushed", got, local)
	}
}
