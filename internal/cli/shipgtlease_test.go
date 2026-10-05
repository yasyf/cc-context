package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShipGTLeasesARemoteHeadNoTrackingRefRecords(t *testing.T) {
	for _, scenario := range []string{"no tracking ref", "stale tracking ref", "rebased after a raw push", "foreign push"} {
		t.Run(scenario, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			writeShipGH(t, f)
			api.prs["feature"] = 7
			seedPRViews(t, map[string]string{"feature": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`})
			mustRun(t, f.Env(), f.Dir, "git", "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
			shipGTStack(t, f, "feature")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
			published := shipHead(t, f)
			wantError := false
			switch scenario {
			case "stale tracking ref":
				mustRun(t, f.Env(), f.Dir, "git", "update-ref", "refs/remotes/origin/feature", "main")
			case "rebased after a raw push":
				stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
				mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "origin/main")
				if shipHead(t, f) == published {
					t.Fatal("fixture did not rewrite the published commit")
				}
			case "foreign push":
				other := filepath.Join(t.TempDir(), "other")
				mustRun(t, f.Env(), f.Dir, "git", "clone", "-q", "-b", "feature", f.RemoteDir, other)
				mustRun(t, f.Env(), other, "git", "config", "user.email", "other@example.invalid")
				mustRun(t, f.Env(), other, "git", "config", "user.name", "Other")
				writeShipFile(t, other, "foreign.txt", "preserve\n")
				mustRun(t, f.Env(), other, "git", "add", "foreign.txt")
				mustRun(t, f.Env(), other, "git", "commit", "-qm", "foreign")
				mustRun(t, f.Env(), other, "git", "push", "-q", "origin", "feature")
				published = gitAt(t, f.Env(), other, "rev-parse", "HEAD")
				wantError = true
			}
			shipGTReady(t, f)

			out, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
			remote := gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "rev-parse", "feature")
			if wantError {
				want := "remote feature changed since last submit, by a push this repository did not make"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("ship error = %v, want the remote-changed refusal; output = %s", err, out)
				}
				if remote != published {
					t.Errorf("remote feature = %s, want the foreign head %s kept", remote, published)
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v; output = %s", err, out)
			}
			if head := shipHead(t, f); remote != head {
				t.Errorf("remote feature = %s, want the shipped head %s", remote, head)
			}
		})
	}
}

func TestShipGTResubmitsUntilGraphiteRecordsThePushedHead(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		unrecorded int
		posts      int
		refused    bool
	}{
		{name: "recorded on the resubmit", unrecorded: 1, posts: 2},
		{name: "never recorded", unrecorded: 10, posts: gtVersionAttempts, refused: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			writeShipGH(t, f)
			api.prs["feature"] = 7
			seedPRViews(t, map[string]string{"feature": `{"number":7,"url":"https://github.com/x/pull/7","body":""}`})
			shipGTStack(t, f, "feature")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "feature")
			shipGTReady(t, f)
			api.unrecorded["feature"] = scenario.unrecorded

			out, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
			head := shipHead(t, f)
			if got := gitAt(t, f.Env(), f.Dir, "--git-dir="+f.RemoteDir, "rev-parse", "feature"); got != head {
				t.Errorf("remote feature = %s, want the pushed %s", got, head)
			}
			heads := api.submitHeads()
			if len(heads) != scenario.posts {
				t.Errorf("submit posts = %v, want %d for feature", heads, scenario.posts)
			}
			for _, b := range api.submitBodies() {
				if !strings.Contains(string(b), head) {
					t.Errorf("submit %s does not carry the pushed head %s", b, head)
				}
			}
			if scenario.refused {
				for _, want := range []string{"#7", shortOID(head), "ccx vcs stack submit"} {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("ship error = %v, want it to name %q; output = %s", err, want, out)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ship error = %v; output = %s", err, out)
			}
		})
	}
}
