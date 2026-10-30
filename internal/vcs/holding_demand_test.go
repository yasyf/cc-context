package vcs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestRetiredDiscoveryCannotBeSkippedForRegisteredRefs(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		name := "registered-ref-has-higher-precedence-retired-holder"
		if invalid {
			name = "unrelated-retired-head-error"
		}
		t.Run(name, func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Trunk("trunk"))
			retired := filepath.Join(t.TempDir(), "retired")
			runGit(t, f, f.Dir, "worktree", "add", "-q", "-b", "shared", retired)
			common := canon(t, filepath.Join(f.Dir, ".git"))
			admin := filepath.Join(common, "arbitrary-admin-group", "retired")
			mustMkdir(t, filepath.Dir(admin))
			if err := os.Rename(filepath.Join(common, "worktrees", "retired"), admin); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(retired, ".git"), gitdirPrefix+admin+"\n")
			active := filepath.Join(t.TempDir(), "active")
			runGit(t, f, f.Dir, "worktree", "add", "-q", active, "shared")
			linked, err := linkedHoldings(common)
			if err != nil {
				t.Fatal(err)
			}
			if len(linked) != 1 || linked[0].Branch != "shared" || canonical(linked[0].Path) != canon(t, active) {
				t.Fatalf("registered holders=%+v, want shared at %s", linked, active)
			}
			checkout, err := ResolveCheckout(f.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if invalid {
				if err := os.Remove(filepath.Join(admin, "HEAD")); err != nil {
					t.Fatal(err)
				}
				mustMkdir(t, filepath.Join(admin, "HEAD"))
				if _, err := CaptureHoldings(f.Context(), checkout); err == nil {
					t.Fatal("registered trunk/shared refs hid an unrelated retired HEAD error")
				} else {
					t.Logf("registered refs resolved; unrelated retired error=%v", err)
				}
				return
			}
			census, err := CaptureHoldings(f.Context(), checkout)
			if err != nil {
				t.Fatal(err)
			}
			holders, err := census.BranchHolders(f.Context(), checkout)
			if err != nil {
				t.Fatal(err)
			}
			if holders["shared"] != canon(t, retired) {
				t.Fatalf("shared holder=%q, want higher-precedence retired %s", holders["shared"], retired)
			}
			t.Logf("registered holder=%s, actual holder=%s", active, holders["shared"])
		})
	}
}
