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
