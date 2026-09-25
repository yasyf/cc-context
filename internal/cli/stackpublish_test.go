package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
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
	b := stackRebaseBranch{Name: "feature", Parent: "main", WasParent: "main", Local: source, Remote: source, Head: source, HeadRef: stackTempRef("feature"), OldBase: base, SourceBase: base, NewBase: pin}
	var err error
	b.NewHead, err = stackReplay(f.Context(), render.Dir(f.Dir), &b)
	if err != nil {
		t.Fatal(err)
	}
	run := &stackRebaseRun{Trunk: "main", Pin: pin, Origin: f.Dir, Roots: []string{"feature"}, Branches: []stackRebaseBranch{b}}
	if err := stackClaim(filepath.Join(f.Dir, ".git"), run); err != nil {
		t.Fatal(err)
	}
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
	baseSource := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	childSource := shipHead(t, f)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	basePublished := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base")
	stackAdvanceTrunk(t, f, "base.txt", "base\n")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", ":refs/heads/base")
	stubStackPRs(t, map[string]*stackPR{"base": {Number: 5, State: "CLOSED", Landed: true, Head: basePublished}})
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != baseSource {
		t.Fatal("landed source moved")
	}
	if got := shipHead(t, f); got != childSource {
		t.Fatal("child source moved")
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil || receipt == nil || receipt.Parent != "main" {
		t.Fatalf("child did not publish over landed parent: %#v %v", receipt, err)
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
