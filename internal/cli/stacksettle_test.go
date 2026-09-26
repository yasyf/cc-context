package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackFailGit(t *testing.T, f *vcstest.Fixture, verb string) func() {
	t.Helper()
	realBin := shipDisplaceShim(t, f, "git")
	writeShipExecutable(t, f.ShimBin, "git", "#!/bin/sh\nif [ \"$1\" = "+verb+" ]; then echo 'refused by the test' >&2; exit 1; fi\nexec '"+realBin+"' \"$@\"\n")
	return func() {
		t.Helper()
		if err := os.Rename(realBin, filepath.Join(f.ShimBin, "git")); err != nil {
			t.Fatal(err)
		}
	}
}

func stackOwnRun(t *testing.T, run *stackRebaseRun) {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	run.Pid, run.Host = os.Getpid(), host
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
}

func stackAssertNoRun(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	if runs, err := stackRuns(filepath.Join(f.Dir, ".git")); err != nil || len(runs) != 0 {
		t.Fatalf("runs = %v, %v, want none", runs, err)
	}
	if pins := gitAt(t, f.Env(), f.Dir, "for-each-ref", "refs/ccx/publication-runs", "refs/heads/"+stackRebaseStateDir); pins != "" {
		t.Fatalf("run refs left behind: %s", pins)
	}
}

func TestStackAbortEndsAnAppliedRunWhoseBranchMovedOn(t *testing.T) {
	f := stackRebaseRepo(t, "base", "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	restore := stackFailGit(t, f, "read-tree")
	if _, _, err := runStackCmd(t, f, "rebase", "--no-push"); err == nil || !strings.Contains(err.Error(), "could not be aligned") {
		t.Fatalf("rebase with a failing align = %v, want it stopped after the rewrite", err)
	}
	restore()
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil || !run.Applied {
		t.Fatalf("run = %+v, %v, want an applied run", run, err)
	}
	if _, _, err := runStackCmd(t, f, "abort"); err == nil || !strings.Contains(err.Error(), "ccx vcs stack continue finishes") {
		t.Fatalf("abort while the rewrite holds = %v, want continue named", err)
	}

	rewritten := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	onwards := gitAt(t, f.Env(), f.Dir, "commit-tree", rewritten+"^{tree}", "-p", rewritten, "-m", "onwards")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/feature", onwards, rewritten)
	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), "ccx vcs stack abort") {
		t.Fatalf("continue after the branch moved on = %v, want abort named", err)
	}
	out, _, err := runStackCmd(t, f, "abort")
	if err != nil || out != "aborted · the branches no longer hold the rewrite, so every branch stays where it is" {
		t.Fatalf("abort after continue named it = %q, %v, want the run dropped", out, err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != onwards {
		t.Errorf("feature = %s, want it left at %s", got, onwards)
	}
	stackAssertNoRun(t, f)
}

func TestStackAbortEndsAnUnpublishedRunWhoseSourceMoved(t *testing.T) {
	f, run, plan := prepareStackPublication(t)
	run.Publishing = true
	run.PushTargets = stackPublicationTargets(plan)
	stackOwnRun(t, run)
	source := run.Branches[0].Local
	changed := gitAt(t, f.Env(), f.Dir, "commit-tree", source+"^{tree}", "-p", source, "-m", "onwards")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/heads/feature", changed, source)

	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), "source branch moved") || !strings.Contains(err.Error(), "ccx vcs stack abort") {
		t.Fatalf("continue over a moved source = %v, want abort named", err)
	}
	out, _, err := runStackCmd(t, f, "abort")
	if err != nil || out != "aborted · no branch moved" {
		t.Fatalf("abort of an unpublished run = %q, %v, want it dropped", out, err)
	}
	if got := shipHead(t, f); got != changed {
		t.Errorf("feature = %s, want it left at %s", got, changed)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != source {
		t.Errorf("remote feature = %s, want it untouched at %s", got, source)
	}
	stackAssertNoRun(t, f)
}

func TestStackAbortOfAPushedRunWithoutReceipts(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote holds the push", true: "remote moved on"}[moved], func(t *testing.T) {
			f, run, plan := prepareStackPublication(t)
			if err := stackPushPublication(f.Context(), render.Dir(f.Dir), gtSubmit{prefix: "test", publication: run}, plan); err != nil {
				t.Fatal(err)
			}
			stackOwnRun(t, run)
			if moved {
				foreign := gitAt(t, f.Env(), f.Dir, "commit-tree", plan[0].head+"^{tree}", "-p", plan[0].head, "-m", "foreign remote")
				mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", foreign+":refs/heads/feature")
			}
			out, _, err := runStackCmd(t, f, "abort")
			if !moved {
				if err == nil || !strings.Contains(err.Error(), "ccx vcs stack continue records them") {
					t.Fatalf("abort of an unreceipted push = %q, %v, want continue named", out, err)
				}
				if _, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git")); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil || !strings.HasPrefix(out, "aborted · the remote moved off the pushed stack") {
				t.Fatalf("abort after the remote moved on = %q, %v, want the run dropped", out, err)
			}
			stackAssertNoRun(t, f)
		})
	}
}

func TestShipLeavesNoRunWhenItsRestackIsNotPublished(t *testing.T) {
	for _, stop := range []string{"graphite unsynced", "push refused"} {
		t.Run(stop, func(t *testing.T) {
			f := shipGTRepo(t, vcstest.GTStack("feature"))
			source := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
			restore := func() {}
			if stop == "push refused" {
				restore = stackFailGit(t, f, "push")
			} else {
				stubGTAPI(t).synced = gtapi.RepoNotSyncedAddable
			}
			if _, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--no-pr"); err == nil {
				t.Fatal("ship published through a refused submit")
			}
			stackAssertNoRun(t, f)
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != source {
				t.Fatalf("feature = %s, want the source %s", got, source)
			}

			restore()
			stubGTAPI(t)
			if _, err := runShipCmd(f.Context(), t, "--no-commit", "--no-watch", "--no-pr"); err != nil {
				t.Fatalf("ship after the refusal cleared = %v, want it published", err)
			}
			if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); !stackOnto(t, f, "origin/main", remote) {
				t.Errorf("published feature %s missed the fresh trunk", remote)
			}
			stackAssertNoRun(t, f)
		})
	}
}

func stackPlantLegacyApplied(t *testing.T, f *vcstest.Fixture, age time.Duration) (string, string) {
	t.Helper()
	source := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature")
	rewritten := gitAt(t, f.Env(), f.Dir, "commit-tree", source+"^{tree}", "-p", "origin/main", "-m", "feature rewritten by 0.65.15")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--keep", rewritten)
	stackPlantRun(t, f, age, "feature")
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	run.Applied = true
	run.Branches = []stackRebaseBranch{{Name: "feature", Parent: "main", WasParent: "main", Local: source, NewHead: rewritten, HeadRef: stackTempRef("feature")}}
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(stackStatePath(run.dir), then, then); err != nil {
		t.Fatal(err)
	}
	return source, rewritten
}

func TestStackAbortEndsAnAppliedRunAnOlderShipLeft(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	_, rewritten := stackPlantLegacyApplied(t, f, time.Minute)

	if _, _, err := runStackCmd(t, f, "continue"); err == nil || !strings.Contains(err.Error(), "ccx vcs stack abort") {
		t.Fatalf("continue of a 0.65.15 applied run = %v, want abort named", err)
	}
	out, _, err := runStackCmd(t, f, "abort")
	if err != nil || !strings.HasPrefix(out, "aborted · an older ccx rewrote these branches in place") {
		t.Fatalf("abort of a 0.65.15 applied run = %q, %v, want it dropped", out, err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != rewritten {
		t.Errorf("feature = %s, want it left at %s", got, rewritten)
	}
	stackAssertNoRun(t, f)
}

func TestStackAbortEndsARunWhoseCheckoutWasRemoved(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	_, rewritten := stackPlantLegacyApplied(t, f, time.Minute)
	run, err := stackOnlyTestRun(filepath.Join(f.Dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	run.Origin = filepath.Join(t.TempDir(), "removed-lane")
	if err := stackSaveRun(run); err != nil {
		t.Fatal(err)
	}

	out, _, err := runStackCmd(t, f, "abort", "--stack", "feature")
	if err != nil || !strings.HasPrefix(out, "aborted · an older ccx rewrote these branches in place") {
		t.Fatalf("abort of a run whose checkout is gone = %q, %v, want it dropped", out, err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != rewritten {
		t.Errorf("feature = %s, want it left at %s", got, rewritten)
	}
	stackAssertNoRun(t, f)
}

func TestStackRebaseReclaimsAnAppliedRunAnOlderShipLeft(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	stackPlantLegacyApplied(t, f, stackStaleAfter+time.Minute)
	stackAdvanceTrunk(t, f, "later.txt", "later\n")

	out, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err != nil || !strings.Contains(out, "reclaimed the stale stack rebase of feature") {
		t.Fatalf("rebase beside a dead 0.65.15 applied run = %q, %v, want it reclaimed", out, err)
	}
	if !stackOnto(t, f, "origin/main", "feature") {
		t.Error("feature is not on the new trunk")
	}
	stackAssertNoRun(t, f)
}

func TestStackSubmitAdoptsAPublicationTheBranchWasResetTo(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatal(err)
	}
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--keep", published)
	common := filepath.Join(f.Dir, ".git")
	if err := gtmeta.RecordSubmitted(f.Context(), common, map[string]gtmeta.Version{"feature": {HeadSha: published, BaseSha: gitAt(t, f.Env(), f.Dir, "rev-parse", "main"), BaseName: "main"}}); err != nil {
		t.Fatal(err)
	}
	stackAdvanceTrunk(t, f, "later.txt", "later\n")

	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("submit after adopting the published head = %v, want it published", err)
	}
	remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	if remote == published || !stackOnto(t, f, "origin/main", remote) || gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "origin/main.."+remote) != "1" || gitAt(t, f.Env(), f.Dir, "diff", published, remote, "--", "feature.txt") != "" {
		t.Fatalf("published feature %s is not the adopted %s replayed onto the new trunk", remote, published)
	}
	receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), "feature")
	if err != nil || receipt == nil || receipt.Source != published || receipt.Head != remote {
		t.Fatalf("receipt = %+v, %v, want source %s published as %s", receipt, err, published, remote)
	}
	stackAssertNoRun(t, f)
}
