package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStackSubmitAllLanesTakesAnUnpublishedBranchASiblingWorktreeHolds(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", restackSiblingPath(t, "lane"), "base")
	shipResetLog(t, f)

	if _, _, err := runStackCmd(t, f, "submit", "--all-lanes"); err != nil {
		t.Fatalf("stack submit --all-lanes = %v, want the sibling's base taken into the run", err)
	}
	if heads := api.submitHeads(); !slices.Equal(heads, []string{"base", "feature"}) {
		t.Errorf("submit posts = %v, want base then feature", heads)
	}
	if !gitBranchExists(t, f.Env(), f.RemoteDir, "base") || !gitBranchExists(t, f.Env(), f.RemoteDir, "feature") {
		t.Error("origin lacks a branch --all-lanes submitted")
	}
}

func TestStackSubmitAllLanesPublishesTheFixASiblingWorktreeCommitted(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	mid, _ := stackHeldMidWithFix(t, f, api, true)

	if _, _, err := runStackCmd(t, f, "submit", "--all-lanes"); err != nil {
		t.Fatalf("stack submit --all-lanes: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"); got == mid {
		t.Error("origin mid still at its old head after --all-lanes")
	}
	if !strings.Contains(gitAt(t, f.Env(), f.RemoteDir, "ls-tree", "--name-only", "top"), "fix.txt") {
		t.Error("origin top lacks mid's fix after --all-lanes")
	}
}

func TestStackSubmitRefusesToKeepAHeadTheHoldingLaneAmended(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "mid", "top")
	api.prs["base"], api.prs["mid"], api.prs["top"] = 7, 8, 9
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("publish the stack: %v", err)
	}
	mid, top := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"), gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "top")
	lane := restackSiblingPath(t, "lane")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", lane, "mid")
	writeShipFile(t, lane, "mid.txt", "amended\n")
	mustRun(t, f.Env(), lane, "git", "commit", "-qa", "--amend", "--no-edit")
	amended := gitAt(t, f.Env(), lane, "rev-parse", "HEAD")
	posted := len(api.submitHeads())

	_, _, err := runStackCmd(t, f, "submit")
	if err == nil || !strings.Contains(err.Error(), "mid is another lane's, kept at its published head") || !strings.Contains(err.Error(), "rewrote that head") || !strings.Contains(err.Error(), "--include mid") {
		t.Fatalf("stack submit = %v, want the amended mid refused with --include mid", err)
	}
	if heads := api.submitHeads()[posted:]; len(heads) != 0 {
		t.Errorf("submit posts = %v, want none", heads)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"); got != mid {
		t.Errorf("origin mid = %s, want %s", got, mid)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "top"); got != top {
		t.Errorf("origin top = %s, want %s", got, top)
	}

	if _, _, err := runStackCmd(t, f, "submit", "--include", "mid"); err != nil {
		t.Fatalf("stack submit --include mid: %v", err)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "mid"); got != amended {
		t.Errorf("origin mid = %s, want the amended %s", got, amended)
	}
	if !stackOnto(t, f, "mid", "top") {
		t.Error("top does not sit on the amended mid")
	}
}

func TestShipAmendLeavesAnEmptyChildUnderUncommittedWork(t *testing.T) {
	for _, push := range []bool{false, true} {
		name := "no_push"
		if push {
			name = "push"
		}
		t.Run(name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			shipGTStack(t, f, "p", "c")
			if push {
				if _, _, err := runStackCmd(t, f, "submit"); err != nil {
					t.Fatalf("stack submit: %v", err)
				}
			}
			shipGTLevel(t, f, "g")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "c")
			held := restackSiblingPath(t, "held")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", held, "g")
			writeShipFile(t, held, "scratch.txt", "work in progress\n")
			before := gitAt(t, f.Env(), held, "rev-parse", "HEAD")
			writeShipFile(t, f.Dir, "c.txt", "amended\n")

			args := []string{"--amend", "--no-watch", "c.txt"}
			if !push {
				args = append(args, "--no-push")
			}
			if _, _, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship --amend = %v, want the empty child left where it is", err)
			}
			if head := gitAt(t, f.Env(), held, "rev-parse", "HEAD"); head != before {
				t.Errorf("held HEAD moved to %s under uncommitted work", head)
			}
			if body, err := os.ReadFile(filepath.Join(held, "scratch.txt")); err != nil || string(body) != "work in progress\n" {
				t.Errorf("held scratch.txt = %q (%v), want the uncommitted work kept", body, err)
			}
			if push {
				if got, want := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c"), gitAt(t, f.Env(), f.Dir, "rev-parse", "c"); got != want {
					t.Errorf("origin c = %s, want the amended %s", got, want)
				}
			}
		})
	}
}
