package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, nil, "base", "feature")
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

// TestStackSubmitBuildsOnARestackOnlyThisClonePushed leaves no publication
// record naming the restacked heads, so only who pushed them tells this
// lane's push from a foreign one.
func TestStackSubmitBuildsOnARestackOnlyThisClonePushed(t *testing.T) {
	for _, tt := range []struct {
		name      string
		elsewhere bool
	}{
		{name: "pushed from this clone"},
		{name: "pushed from another clone", elsewhere: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := heldPushNativeStack(t)
			other := filepath.Join(t.TempDir(), "other")
			mustRun(t, f.Env(), filepath.Dir(other), "git", "clone", "-q", f.RemoteDir, other)
			mustRun(t, f.Env(), other, "git", "config", "user.email", "t@t.t")
			mustRun(t, f.Env(), other, "git", "config", "user.name", "t")
			mustRun(t, f.Env(), other, "git", "config", "commit.gpgsign", "false")
			mustRun(t, f.Env(), other, "git", "switch", "-q", "--detach", "origin/main")
			mustRun(t, f.Env(), other, "git", "cherry-pick", "origin/main..origin/feature")
			pushed := map[string]string{"base": gitAt(t, f.Env(), other, "rev-parse", "HEAD~1"), "feature": gitAt(t, f.Env(), other, "rev-parse", "HEAD")}
			restacked := pushed["feature"]
			pusher := other
			if !tt.elsewhere {
				mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", other, "HEAD")
				pusher = f.Dir
			}
			mustRun(t, f.Env(), pusher, "git", "push", "-q", "--force", "origin", pushed["base"]+":refs/heads/base", restacked+":refs/heads/feature")
			if !tt.elsewhere {
				if err := gitRecordPush(f.Context(), render.Dir(f.Dir), "origin", pushed); err != nil {
					t.Fatal(err)
				}
			}
			writeShipFile(t, f.Dir, "more.txt", "more\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "more.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "more")

			_, errOut, err := runStackCmd(t, f, "submit")
			if tt.elsewhere {
				if err == nil || !strings.Contains(err.Error(), "feature") {
					t.Fatalf("stack submit over another clone's push = %v (stderr=%q), want a refusal naming feature", err, errOut)
				}
				if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != restacked {
					t.Errorf("origin feature = %s, want the other clone's %s left in place", shortOID(got), shortOID(restacked))
				}
				return
			}
			if err != nil {
				t.Fatalf("stack submit over this clone's push = %v (stderr=%q), want it built on as this lane's", err, errOut)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%s", "feature"); got != "more" {
				t.Errorf("origin feature's head commit = %q, want the lane's new commit published", got)
			}
		})
	}
}
