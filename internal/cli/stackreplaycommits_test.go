package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackReplayCommitsPicksAsRebaseDoesAndStopsBeforeAConflict(t *testing.T) {
	t.Parallel()
	f := shipRepo(t)
	trunk := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	commit := func(subject, file, content string, env ...string) string {
		t.Helper()
		if file == "" {
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--allow-empty", "-m", subject)
		} else {
			writeShipFile(t, f.Dir, file, content)
			mustRun(t, f.Env(), f.Dir, "git", "add", file)
			mustRun(t, slices.Concat(f.Env(), env), f.Dir, "git", "commit", "-q", "-m", subject, "-m", "why "+subject)
		}
		return gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	}
	commit("empty", "", "")
	commit("dup", "dup.txt", "same\n")
	commit("kept", "kept.txt", "kept\n", "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=@1700000000 +0230")
	conflict := commit("conflict", "c.txt", "feature\n")
	commit("after", "after.txt", "after\n")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	writeShipFile(t, f.Dir, "dup.txt", "same\n")
	writeShipFile(t, f.Dir, "c.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "dup.txt", "c.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "upstream")
	onto := shipHead(t, f)
	dir := render.Dir(f.Dir)

	head, stop, err := stackReplayCommits(f.Context(), dir, onto, onto, trunk, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stop != conflict {
		t.Errorf("stopped at %s, want the conflicting commit %s", shortOID(stop), shortOID(conflict))
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", onto+".."+head); got != "kept\nempty" {
		t.Errorf("replayed %q, want the empty commit kept and dup, which trunk already holds, dropped", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "-1", "--date=raw", "--format=%an <%ae> %ad%n%B", head); got != "Ada <ada@example.com> 1700000000 +0230\nkept\n\nwhy kept" {
		t.Errorf("replayed kept as %q, want its author, date and message", got)
	}

	rest, stop, err := stackReplayCommits(f.Context(), dir, onto, head, conflict, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stop != "" {
		t.Errorf("stopped at %s replaying past the conflict, want a clean replay", shortOID(stop))
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", head+".."+rest); got != "after" {
		t.Errorf("replayed %q past the conflict, want after", got)
	}
}

func TestStackRebaseChecksOutOnlyTheCommitsThatConflict(t *testing.T) {
	t.Parallel()
	f := shipGTRepo(t, vcstest.GTStack("base"))
	stubStackPRs(t, f, map[string]*stackPR{"feature": {Number: 7, Title: "feature work", State: "OPEN"}})
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "feature")
	shas := map[string]string{}
	for _, c := range []struct{ subject, file, content string }{
		{"f1", "f1.txt", "f1\n"},
		{"f2", "c.txt", "feature\n"},
		{"f3", "f3.txt", "f3\n"},
		{"f4", "c.txt", "feature2\n"},
	} {
		writeShipFile(t, f.Dir, c.file, c.content)
		mustRun(t, f.Env(), f.Dir, "git", "add", c.file)
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", c.subject)
		shas[c.subject] = gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	}
	mustRun(t, f.Env(), f.Dir, "gt", "track", "-f", "--no-interactive")
	restackAdvanceRemote(t, f, "main", "c.txt", "trunk\n")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "-q", "--ff-only", "origin/main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")

	_, _, err := runStackCmd(t, f, "rebase", "--no-push")
	if err == nil {
		t.Fatal("stack rebase succeeded, want the conflict on f2")
	}
	ws := stackWorkspaceOf(t, err)
	stopped := func(conflict, below string) {
		t.Helper()
		if got := gitAt(t, f.Env(), ws, "rev-parse", "REBASE_HEAD"); got != shas[conflict] {
			t.Errorf("the workspace stopped at %s, want %s", shortOID(got), conflict)
		}
		if got := gitAt(t, f.Env(), ws, "log", "-1", "--format=%s", "HEAD"); got != below {
			t.Errorf("the workspace rebases %s onto %q, want %s replayed in memory", conflict, got, below)
		}
		end := gitAt(t, f.Env(), ws, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge/end")
		if got := strings.TrimSpace(restackRead(t, end)); got != "1" {
			t.Errorf("the workspace rebase picks %s commits, want %s alone", got, conflict)
		}
	}
	stopped("f2", "f1")

	writeShipFile(t, ws, "c.txt", "trunk+feature\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	if _, _, err := runStackCmdIn(t, f, ws, "continue"); err == nil || !strings.Contains(err.Error(), "c.txt") {
		t.Fatalf("continue = %v, want the conflict on f4", err)
	}
	stopped("f4", "f3")

	writeShipFile(t, ws, "c.txt", "trunk+feature2\n")
	mustRun(t, f.Env(), ws, "git", "add", "c.txt")
	out, _, err := runStackCmdIn(t, f, ws, "continue")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !strings.Contains(out, "resolved feature") {
		t.Errorf("continue output = %q", out)
	}
	if got := gitAt(t, f.Env(), f.Dir, "log", "--format=%s", "base..feature"); got != "f4\nf3\nf2\nf1" {
		t.Errorf("feature carries %q, want f1 through f4", got)
	}
	if got := gitAt(t, f.Env(), f.Dir, "show", "feature:c.txt"); got != "trunk+feature2" {
		t.Errorf("feature's c.txt = %q, want the last resolution", got)
	}
}
