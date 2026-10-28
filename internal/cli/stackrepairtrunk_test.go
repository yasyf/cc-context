package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackChildOfDeletedParent(t *testing.T, base string) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t)
	shipGTStack(t, f, "gone", "child")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "gone", "child")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "gone")
	stubStackPRs(t, f, map[string]*stackPR{"child": {Number: 42803, Title: "child", State: "OPEN", Base: base}})
	commonDir, err := gtCommonDir(t.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	shipResetLog(t, f)
	return f, commonDir
}

func TestStackRepairPublishedChildRecordsTrunkForAPullRequestBasedThere(t *testing.T) {
	f, commonDir := stackChildOfDeletedParent(t, "main")
	head := gitAt(t, f.Env(), f.Dir, "rev-parse", "child")
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "child")

	out, errOut, err := runStackCmd(t, f, "repair-published-child", "--parent", "main")
	if err != nil || !strings.Contains(out, "repaired child onto main") {
		t.Fatalf("repair --parent main = %q, %v\n%s", out, err, errOut)
	}
	if got := stackParent(t, f, "child"); got != "main" {
		t.Errorf("gt parent = %s, want main", got)
	}
	row, err := gtmeta.ReadPublishedChild(f.Context(), commonDir, "child")
	if want := gitAt(t, f.Env(), f.Dir, "merge-base", "child", "origin/main"); err != nil || row.ParentRevision != want || row.Validation != "VALID" {
		t.Errorf("row = %+v, %v; want parent revision %s, VALID", row, err, want)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "child"); got != head {
		t.Errorf("local child moved from %s to %s", head, got)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "child"); got != published {
		t.Errorf("published child moved from %s to %s", published, got)
	}
	for _, argv := range vcstest.Invocations(t, f.ArgvLog) {
		if len(argv) > 1 && (argv[0] == "gt" || argv[0] == "git" && slices.Contains([]string{"push", "rebase", "replay", "reset"}, argv[1])) {
			t.Errorf("repair ran %v", argv)
		}
	}

	if out, _, err := runStackCmd(t, f, "repair-published-child", "--parent", "main"); err != nil || !strings.Contains(out, "already repaired child onto main") {
		t.Errorf("second repair = %q, %v", out, err)
	}
}

func TestStackRepairPublishedChildRefusesTrunkItCannotMatch(t *testing.T) {
	t.Run("pull request based elsewhere", func(t *testing.T) {
		f, _ := stackChildOfDeletedParent(t, "gone")
		_, _, err := runStackCmd(t, f, "repair-published-child", "--parent", "main")
		if err == nil || !strings.Contains(err.Error(), "is based on gone, not main") {
			t.Fatalf("repair = %v, want the base mismatch refused", err)
		}
		if got := stackParent(t, f, "child"); got != "gone" {
			t.Errorf("gt parent = %s, want gone left alone", got)
		}
	})
	t.Run("unpublished commits", func(t *testing.T) {
		f, _ := stackChildOfDeletedParent(t, "main")
		writeShipFile(t, f.Dir, "more.txt", "more\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", "more.txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "more")
		_, _, err := runStackCmd(t, f, "repair-published-child", "--parent", "main")
		if err == nil || !strings.Contains(err.Error(), "is not its published head") {
			t.Fatalf("repair = %v, want the unpublished head refused", err)
		}
	})
}
