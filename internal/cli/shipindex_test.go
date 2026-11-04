package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestShipWithAStagedIndexCommitsExactlyTheIndex(t *testing.T) {
	for _, gt := range []bool{false, true} {
		name := "git"
		if gt {
			name = "graphite"
		}
		t.Run(name, func(t *testing.T) {
			var f *vcstest.Fixture
			if gt {
				f = shipGTRepo(t, vcstest.GTStack("base"))
			} else {
				f = shipRepo(t, vcstest.Remote())
			}
			writeShipFile(t, f.Dir, "partial.txt", "one\n")
			writeShipFile(t, f.Dir, "retired.txt", "retired\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "partial.txt", "retired.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "partial")
			mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "--cached", "retired.txt")
			writeShipFile(t, f.Dir, "pages/[id].txt", "page\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "pages/[id].txt")
			writeShipFile(t, f.Dir, "pages/i.txt", "unrelated\n")
			writeShipFile(t, f.Dir, "partial.txt", "two\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "partial.txt")
			writeShipFile(t, f.Dir, "partial.txt", "three\n")
			writeShipFile(t, f.Dir, "staged.txt", "staged\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "staged.txt")
			writeShipFile(t, f.Dir, "untracked.txt", "wip\n")

			got, err := runShipCmd(f.Context(), t, "-m", "fix: the staged set", "--no-push")
			if err != nil {
				t.Fatalf("ship = %v", err)
			}
			if strings.Contains(got, "swept") {
				t.Errorf("summary = %q, want no sweep over a staged index", got)
			}
			if want := "left uncommitted 4 path(s): pages/i.txt, partial.txt, retired.txt, untracked.txt"; !strings.Contains(got, want) {
				t.Errorf("summary = %q, want it to name what stayed out: %q", got, want)
			}
			if files := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "pages/[id].txt\npartial.txt\nretired.txt\nstaged.txt" {
				t.Errorf("committed files = %q, want the page, partial.txt, the retired.txt removal, and staged.txt", files)
			}
			if body := gitAt(t, f.Env(), f.Dir, "show", "HEAD:partial.txt"); body != "two" {
				t.Errorf("committed partial.txt = %q, want the staged two", body)
			}
			if body, err := os.ReadFile(filepath.Join(f.Dir, "partial.txt")); err != nil || string(body) != "three\n" {
				t.Errorf("working partial.txt = %q (%v), want the unstaged three kept", body, err)
			}
			for _, path := range []string{"untracked.txt", "retired.txt", "pages/i.txt"} {
				if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain", "--untracked-files=all", "--", path); status != "?? "+path {
					t.Errorf("%s status = %q, want it left untracked", path, status)
				}
			}
		})
	}
}

func TestShipRefusesAnIndexThatLeavesATrackedEditUnstaged(t *testing.T) {
	for _, gt := range []bool{false, true} {
		name := "git"
		if gt {
			name = "graphite"
		}
		t.Run(name, func(t *testing.T) {
			var f *vcstest.Fixture
			if gt {
				f = shipGTRepo(t, vcstest.GTStack("base"))
			} else {
				f = shipRepo(t, vcstest.Remote())
			}
			for _, path := range []string{"gone.txt", "edited.txt", "partial.txt"} {
				writeShipFile(t, f.Dir, path, "one\n")
			}
			mustRun(t, f.Env(), f.Dir, "git", "add", "gone.txt", "edited.txt", "partial.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "tracked")
			head := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
			mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "gone.txt")
			writeShipFile(t, f.Dir, "partial.txt", "two\n")
			mustRun(t, f.Env(), f.Dir, "git", "add", "partial.txt")
			writeShipFile(t, f.Dir, "partial.txt", "three\n")
			writeShipFile(t, f.Dir, "edited.txt", "two\n")

			refusals := dryRunValues(dryRunReport(t, f, "-m", "fix: retire gone.txt", "--no-push"), "refuses")
			if len(refusals) != 1 || !strings.Contains(refusals[0], "tracked edited.txt,") {
				t.Errorf("dry run refusals = %q, want one naming edited.txt", refusals)
			}
			_, err := runShipCmd(f.Context(), t, "-m", "fix: retire gone.txt", "--no-push")
			if err == nil || !strings.Contains(err.Error(), "the index stages gone.txt, partial.txt but none of the changes to tracked edited.txt,") {
				t.Fatalf("ship = %v, want a refusal naming edited.txt and nothing else", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD = %s, want %s untouched by the refusal", got, head)
			}
			for path, want := range map[string]string{"gone.txt": "D  gone.txt", "edited.txt": "M edited.txt", "partial.txt": "MM partial.txt"} {
				if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain", "--", path); status != want {
					t.Errorf("%s status = %q, want %q", path, status, want)
				}
			}

			mustRun(t, f.Env(), f.Dir, "git", "add", "edited.txt")
			if _, err := runShipCmd(f.Context(), t, "-m", "fix: retire gone.txt", "--no-push"); err != nil {
				t.Fatalf("ship after staging edited.txt = %v", err)
			}
			if files := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "edited.txt\ngone.txt\npartial.txt" {
				t.Errorf("committed files = %q, want the edit, the removal, and the staged half of partial.txt", files)
			}
		})
	}
}

func TestShipWithASparseStagedMergeCommitsExactlyTheIndex(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	writeShipFile(t, f.Dir, "included/file.txt", "included\n")
	writeShipFile(t, f.Dir, "outside/file.txt", "before\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "included/file.txt", "outside/file.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "sparse base")
	branch := gitAt(t, f.Env(), f.Dir, "branch", "--show-current")
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	mustRun(t, f.Env(), f.Dir, "git", "checkout", "-qb", "sparse-incoming")
	writeShipFile(t, f.Dir, "outside/file.txt", "incoming\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "outside/file.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "sparse incoming")
	incoming := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD")
	mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", branch)
	mustRun(t, f.Env(), f.Dir, "git", "sparse-checkout", "set", "--cone", "included")
	mustRun(t, f.Env(), f.Dir, "git", "merge", "--no-ff", "--no-commit", "sparse-incoming")
	tree := gitAt(t, f.Env(), f.Dir, "write-tree")
	outside := filepath.Join(f.Dir, "outside", "file.txt")
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside sparse entry exists before ship: %v", err)
	}

	if _, err := runShipCmd(f.Context(), t, "-m", "fix: preserve the sparse merge", "--no-push"); err != nil {
		t.Fatalf("ship sparse merge = %v", err)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "HEAD^{tree}"); got != tree {
		t.Errorf("committed tree = %s, want prepared %s", got, tree)
	}
	if got, want := gitAt(t, f.Env(), f.Dir, "show", "-s", "--format=%P", "HEAD"), base+" "+incoming; got != want {
		t.Errorf("merge parents = %s, want %s", got, want)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("outside sparse entry materialized by ship: %v", err)
	}
}
