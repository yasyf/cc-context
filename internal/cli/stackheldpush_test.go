package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// heldPushNativeStack publishes base and feature as a native stack, checked
// out on feature, then lands a trunk commit beside feature's change, so a
// restack of feature applies cleanly with a different patch.
func heldPushNativeStack(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	writeShipFile(t, f.Dir, "notes.txt", "one\ntwo\nthree\nfour\nfive\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "notes.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "notes")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	trunk := shipHead(t, f)
	shipGTUntracked(t, f, "base")
	base := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	writeShipFile(t, f.Dir, "notes.txt", "one\ntwo\nTHREE\nfour\nfive\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qam", "feature")
	for _, link := range []struct{ branch, parent, fork, head string }{{"base", "main", trunk, base}, {"feature", "base", base, shipHead(t, f)}} {
		if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, link.fork, link.head); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "feature")
	api := stubGTAPI(t)
	api.openPRs("base", "feature")
	api.bases["base"], api.stacks["base"] = "main", 47
	api.bases["feature"], api.stacks["feature"] = "base", 47
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)
	if _, errOut, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errOut)
	}
	restackAdvanceRemote(t, f, "main", "notes.txt", "one\nTWO\nthree\nfour\nfive\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	return f
}

func heldPushOrigin(t *testing.T, f *vcstest.Fixture) map[string]string {
	t.Helper()
	heads := map[string]string{}
	for _, name := range []string{"base", "feature"} {
		heads[name] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", name)
	}
	return heads
}

func TestStackSubmitAfterARestackPushesNothingTheSecondTime(t *testing.T) {
	f := heldPushNativeStack(t)
	if _, errOut, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
		t.Fatalf("stack submit --restack = %v (stderr=%q)", err, errOut)
	}
	restacked := heldPushOrigin(t, f)
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "merge-base", "main", "feature"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"); got != want {
		t.Fatalf("origin feature forks from %s, want the restack onto trunk %s", shortOID(got), shortOID(want))
	}

	if _, errOut, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("second stack submit = %v (stderr=%q), want the heads the first one pushed taken as this lane's", err, errOut)
	}
	if got := heldPushOrigin(t, f); got["base"] != restacked["base"] || got["feature"] != restacked["feature"] {
		t.Errorf("second stack submit moved origin from %v to %v, want nothing pushed", restacked, got)
	}
}

func TestStackSubmitRefusesAnotherLanesPushOverARestackedHead(t *testing.T) {
	f := heldPushNativeStack(t)
	if _, errOut, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
		t.Fatalf("stack submit --restack = %v (stderr=%q)", err, errOut)
	}
	other := filepath.Join(t.TempDir(), "other-lane")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "--detach", other, "origin/feature")
	writeShipFile(t, other, "notes.txt", "one\nTWO\nthree, rewritten elsewhere\nfour\nfive\n")
	mustRun(t, f.Env(), other, "git", "commit", "-qa", "--amend", "-m", "feature, rewritten elsewhere")
	mustRun(t, f.Env(), other, "git", "push", "-qf", "origin", "HEAD:feature")
	foreign := gitAt(t, f.Env(), other, "rev-parse", "HEAD")

	_, errOut, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("stack submit over another lane's push = %v (stderr=%q), want a refusal naming feature", err, errOut)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != foreign {
		t.Errorf("origin feature = %s, want the other lane's %s left in place", shortOID(got), shortOID(foreign))
	}
}

// TestStackSubmitRecordsItsPushBeforePostingTheHead interrupts a restacking
// submit while it posts the cci HEAD record, the first step after its push.
func TestStackSubmitRecordsItsPushBeforePostingTheHead(t *testing.T) {
	f := heldPushNativeStack(t)
	before := heldPushOrigin(t, f)
	posting := filepath.Join(t.TempDir(), "posting")
	writeShipExecutable(t, f.ShimBin, "cci", "#!/bin/sh\ncase \"$*\" in *'--kind head'*) : > '"+posting+"'; exec sleep 30;; esac\nexit 0\n")
	f.Setenv(envCodexThreadKey, "")
	f.Setenv(envCCIDrive, "drive")
	f.Setenv(envCCILane, "lane")
	ctx, cancel := context.WithCancel(f.ContextIn(f.Dir))
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(posting); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	cmd := newStackCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"submit", "--restack"})
	_ = cmd.ExecuteContext(ctx)

	for name, head := range heldPushOrigin(t, f) {
		if head == before[name] {
			t.Fatalf("origin %s did not move, want the restack pushed before the HEAD post", name)
		}
		receipt, err := stackReadPublication(f.Context(), render.Dir(f.Dir), name)
		if err != nil {
			t.Fatal(err)
		}
		if receipt == nil || receipt.Head != head {
			t.Errorf("%s's publication receipt = %+v, want it naming the pushed head %s", name, receipt, shortOID(head))
		}
	}
}
