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
			mustRun(t, f.Env(), f.Dir, "git", "add", "partial.txt")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "partial")
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
			if files := gitAt(t, f.Env(), f.Dir, "show", "--name-only", "--format=", "HEAD"); files != "partial.txt\nstaged.txt" {
				t.Errorf("committed files = %q, want partial.txt and staged.txt", files)
			}
			if body := gitAt(t, f.Env(), f.Dir, "show", "HEAD:partial.txt"); body != "two" {
				t.Errorf("committed partial.txt = %q, want the staged two", body)
			}
			if body, err := os.ReadFile(filepath.Join(f.Dir, "partial.txt")); err != nil || string(body) != "three\n" {
				t.Errorf("working partial.txt = %q (%v), want the unstaged three kept", body, err)
			}
			if status := gitAt(t, f.Env(), f.Dir, "status", "--porcelain", "--", "untracked.txt"); status != "?? untracked.txt" {
				t.Errorf("untracked.txt status = %q, want it left untracked", status)
			}
		})
	}
}
