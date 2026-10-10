package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// TestStackSubmitRepublishesOverTheQueuesOwnRestack is tenant-parity-retro-2's
// refusal: after the stack bottom landed, the merge queue restacked the child
// onto trunk between the submit's plan and its push, and the push refused on
// a stale lease though the remote held the same patches.
func TestStackSubmitRepublishesOverTheQueuesOwnRestack(t *testing.T) {
	f, marker := stackRacedByRestack(t, false)

	out, errStr, err := runStackCmd(t, f, "submit")
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("fixture: the queue's restack never raced the submit: %v", statErr)
	}
	if err != nil {
		t.Fatalf("stack submit = %v (stdout=%q stderr=%q)", err, out, errStr)
	}
	if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"), gitAt(t, f.Env(), f.Dir, "rev-parse", "feature"); got != want {
		t.Errorf("origin feature = %s, want the submitted head %s", got, want)
	}
	stackAssertOwnsOnTrunk(t, f, "feature")
}

func TestStackSubmitRefusesARacingPushOfOtherPatches(t *testing.T) {
	f, _ := stackRacedByRestack(t, true)

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "-> feature (stale info)") {
		t.Fatalf("stack submit = %v, want the stale-lease refusal", err)
	}
	raced := gitAt(t, f.Env(), f.RemoteDir, "log", "-1", "--format=%s", "feature")
	if raced != "feature amended elsewhere" {
		t.Errorf("origin feature tip = %q, want the other push kept", raced)
	}
}

// stackRacedByRestack is a landed stack bottom whose child another pusher
// restacks onto trunk between the submit's plan and its push, carrying the
// same patches or, when amended, other ones.
func stackRacedByRestack(t *testing.T, amended bool) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	baseHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	restackSquashRemote(t, f, "main", "base (#41)", "base")
	mustRun(t, f.Env(), f.RemoteDir, "git", "branch", "-D", "base")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")

	elsewhere := filepath.Join(t.TempDir(), "graphite")
	mustRun(t, f.Env(), f.Dir, "git", "clone", "-q", "--branch", "main", f.RemoteDir, elsewhere)
	mustRun(t, f.Env(), elsewhere, "git", "config", "user.name", "graphite")
	mustRun(t, f.Env(), elsewhere, "git", "config", "user.email", "graphite@example.com")
	mustRun(t, f.Env(), elsewhere, "git", "fetch", "-q", "origin", "feature")
	mustRun(t, f.Env(), elsewhere, "git", "cherry-pick", "FETCH_HEAD")
	if amended {
		writeShipFile(t, elsewhere, "elsewhere.txt", "elsewhere\n")
		mustRun(t, f.Env(), elsewhere, "git", "add", "elsewhere.txt")
		mustRun(t, f.Env(), elsewhere, "git", "commit", "-q", "--amend", "-m", "feature amended elsewhere")
	}

	marker := filepath.Join(t.TempDir(), "raced")
	hook := "#!/bin/sh\n[ \"$1\" = committed ] || exit 0\ngrep -q refs/ccx/ || exit 0\n[ -e " + marker + " ] && exit 0\ntouch " + marker + "\nunset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE\ngit -C " + elsewhere + " push -qf origin HEAD:feature\n"
	hooks := filepath.Join(gitAt(t, f.Env(), f.Dir, "rev-parse", "--git-common-dir"), "hooks")
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(f.Dir, hooks)
	}
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o700); err != nil { //nolint:gosec // a git hook must be executable
		t.Fatal(err)
	}
	stubStackPRs(t, f, map[string]*stackPR{
		"base":    {Number: 41, Title: "base", State: "MERGED", Landed: true, Head: baseHead},
		"feature": {Number: 42, Title: "feature", State: "OPEN", Base: "main"},
	})
	shipResetLog(t, f)
	return f, marker
}

// TestStackSubmitRepublishesOverGraphitesRestackOfAParkedPullRequest is the
// #34213 refusal: base landed and parked feature on graphite-base/101, and
// after the submit leased both refs Graphite restacked feature onto a newer
// trunk with the same patches and deleted graphite-base/101, so the atomic
// push refused both leases as stale.
func TestStackSubmitRepublishesOverGraphitesRestackOfAParkedPullRequest(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	api.parkOn(f)
	shipGTStack(t, f, "base", "feature")
	api.prs["base"], api.prs["feature"] = 100, 101
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	baseHead := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	restackSquashRemote(t, f, "main", "base (#100)", "base")
	squash := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main")

	elsewhere := filepath.Join(t.TempDir(), "graphite")
	mustRun(t, f.Env(), f.Dir, "git", "clone", "-q", "--branch", "main", f.RemoteDir, elsewhere)
	mustRun(t, f.Env(), elsewhere, "git", "config", "user.name", "graphite")
	mustRun(t, f.Env(), elsewhere, "git", "config", "user.email", "graphite@example.com")
	writeShipFile(t, elsewhere, "later.txt", "later\n")
	mustRun(t, f.Env(), elsewhere, "git", "add", "later.txt")
	mustRun(t, f.Env(), elsewhere, "git", "commit", "-qm", "later")
	trunk := gitAt(t, f.Env(), elsewhere, "rev-parse", "HEAD")
	mustRun(t, f.Env(), elsewhere, "git", "fetch", "-q", "origin", "feature")
	mustRun(t, f.Env(), elsewhere, "git", "cherry-pick", baseHead+"..FETCH_HEAD")
	restacked := gitAt(t, f.Env(), elsewhere, "rev-parse", "HEAD")

	raced := false
	api.mu.Lock()
	delete(api.prs, "base")
	api.merged["base"] = gtStubMerged{number: 100, head: baseHead, state: gtapi.PRClosed}
	api.prs["feature"] = 101
	api.parked["feature"] = "graphite-base/101"
	api.mergeability[101] = "NEEDS_RESTACK__BASE_BRANCH_MERGED"
	api.remote("update-ref", "refs/heads/graphite-base/101", baseHead)
	api.presubmitRace = func() {
		raced = true
		cmd := exec.Command("git", "push", "-qf", "origin", trunk+":refs/heads/main", restacked+":refs/heads/feature", ":refs/heads/graphite-base/101") //nolint:gosec // the test's own shas
		cmd.Dir, cmd.Env = elsewhere, f.Env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("graphite's restack push: %v: %s", err, out)
		}
	}
	api.mu.Unlock()
	shipResetLog(t, f)

	out, errStr, err := runStackCmd(t, f, "submit")
	api.mu.Lock()
	ran := raced
	api.mu.Unlock()
	if !ran {
		t.Fatal("fixture: Graphite's restack never raced the submit")
	}
	if err != nil {
		t.Fatalf("stack submit = %v (stdout=%q stderr=%q)", err, out, errStr)
	}
	if remote := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); remote == restacked || gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature^") != squash {
		t.Errorf("origin feature = %q, want the submit's republish onto %s rather than Graphite's restack %s", gitAt(t, f.Env(), f.RemoteDir, "log", "-3", "--format=%H %s", "feature"), squash, restacked)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "for-each-ref", "refs/heads/graphite-base/"); got != "" {
		t.Errorf("origin graphite-base refs = %q, want the one Graphite deleted left deleted", got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "main"); got != trunk {
		t.Errorf("origin main = %s, want Graphite's trunk %s untouched", got, trunk)
	}
	api.mu.Lock()
	entry, _ := api.lastEntry("feature")
	api.mu.Unlock()
	if entry.Base != "main" {
		t.Errorf("feature last submitted onto %q, want main", entry.Base)
	}
}
