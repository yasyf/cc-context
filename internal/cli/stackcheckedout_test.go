package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackForgetCheckouts(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "HEAD")
}

func TestStackSubmitTakesABranchAnotherLaneOpenedOnceThisWorkingCopyChecksItOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed bool
	}{{name: "named for another lane"}, {name: "claimed by the lane that opened it", claimed: true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			opener := restackSiblingPath(t, "five-options-1")
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", "-b", "options/card", opener, "main")
			writeShipFile(t, opener, "card.txt", "card\n")
			mustRun(t, f.Env(), opener, "git", "add", "card.txt")
			mustRun(t, f.Env(), opener, "git", "commit", "-q", "-m", "card", "-m", "why card")
			mustRun(t, f.Env(), opener, "gt", "track", "-f", "--no-interactive")
			api.prs["options/card"] = 7
			if _, errOut, err := runStackCmdIn(t, f, opener, "submit"); err != nil {
				t.Fatalf("open options/card from its own lane: %v\n%s", err, errOut)
			}
			mustRun(t, f.Env(), f.Dir, "git", "worktree", "remove", "--force", opener)
			if tc.claimed {
				writeShipExecutable(t, f.ShimBin, "cci", "#!/bin/sh\n[ \"$1 $3\" = 'who branch:options/card' ] && echo '{\"session\":\"sess-five\",\"lane\":\"five-options-1\"}'\nexit 0\n")
				f.Setenv(envClaudeSessionKey, "sess-drive")
				f.Setenv(envCodexThreadKey, "")
			}
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "options/card")
			stackCommit(t, f, "card-rework.txt")
			rework := gitAt(t, f.Env(), f.Dir, "rev-parse", "options/card")
			shipGTStack(t, f, "edit-fork")
			api.prs["edit-fork"] = 8
			posted := len(api.submitHeads())

			out, errOut, err := runStackCmd(t, f, "submit")
			if err != nil {
				t.Fatalf("stack submit from edit-fork: %v\n%s", err, errOut)
			}
			if heads := api.submitHeads()[posted:]; !slices.Equal(heads, []string{"options/card", "edit-fork"}) {
				t.Errorf("submit posts = %v, want options/card then edit-fork — this working copy checked options/card out", heads)
			}
			card := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "options/card")
			if card != rework {
				t.Errorf("origin options/card = %.12s, want the rework %.12s committed here", card, rework)
			}
			if fork := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "edit-fork"); !stackOnto(t, f, card, fork) {
				t.Errorf("origin edit-fork %.12s is not on options/card's new head %.12s", fork, card)
			}
			for _, refused := range []string{"another lane", "not pushing options/card", "keeping options/card"} {
				if strings.Contains(out+errOut, refused) {
					t.Errorf("output = %q, want options/card taken as this lane's, not %q", out+errOut, refused)
				}
			}
		})
	}
}
