package cli

import (
	"strconv"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestRangeDiffSummaryCountsPairsByMark(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"identical", "1:  aaaaaaa = 1:  bbbbbbb base\n2:  ccccccc = 2:  ddddddd more\n", "patch-identical (2 commits)"},
		{"one identical", "1:  aaaaaaa = 1:  bbbbbbb base\n", "patch-identical (1 commit)"},
		{"changed, added and dropped", "1:  aaaaaaa ! 1:  bbbbbbb base\n    @@ base.txt\n    -a = b\n    +a ! c\n2:  ccccccc < -:  ------- gone\n-:  ------- > 2:  ddddddd new\n3:  eeeeeee = 3:  fffffff kept\n", "1 identical, 1 changed, 1 added, 1 dropped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rangeDiffSummary(tc.out); got != tc.want {
				t.Errorf("rangeDiffSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStackSubmitPostsAHeadRecordPerMovedPullRequestInADrive(t *testing.T) {
	registry := `{"session":"sess-abc","agent":"a-other","drive":"other-drive","lane":"other-lane"}` + "' '" +
		`{"session":"sess-abc","agent":"a-lane","drive":"reg-drive","lane":"reg-lane"}`
	for _, tc := range []struct {
		name        string
		env         map[string]string
		who         string
		drive, lane string
	}{
		{"named by the environment", map[string]string{envCCIDrive: "env-drive", envCCILane: "env-lane"}, "", "env-drive", "env-lane"},
		{"named by the session's registry entry", map[string]string{envClaudeSessionKey: "sess-abc", envCCIAgent: "a-lane"}, registry, "reg-drive", "reg-lane"},
		{"outside a drive", map[string]string{envClaudeSessionKey: "sess-abc", envCCIAgent: "a-unbound"}, registry, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shipGTRepo(t)
			api := newGTAPIStub(t)
			api.openPRs("base", "feature")
			f.Decorate(api.ctx)
			shipGTStack(t, f, "base", "feature")
			if _, errStr, err := runStackCmd(t, f, "submit"); err != nil {
				t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
			}
			was := map[string]string{}
			for _, branch := range []string{"base", "feature"} {
				was[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
			}
			stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
			writeShipExecutable(t, f.ShimBin, "cci", "#!/bin/sh\n"+vcstest.RecordArgv("cci")+"[ \"$2\" = --session ] && printf '%s\\n' '"+tc.who+"'\nexit 0\n")
			f.Setenv(envCodexThreadKey, "")
			for k, v := range tc.env {
				f.Setenv(k, v)
			}
			shipResetLog(t, f)

			if _, errStr, err := runStackCmd(t, f, "submit", "--restack"); err != nil {
				t.Fatalf("stack submit --restack = %v (stderr=%q)", err, errStr)
			}
			var want [][]string
			for _, branch := range []string{"base", "feature"} {
				if tc.drive == "" {
					break
				}
				head := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
				if head == was[branch] {
					t.Fatalf("origin %s did not move, want the restack to push it", branch)
				}
				api.mu.Lock()
				number := strconv.Itoa(api.prs[branch])
				api.mu.Unlock()
				pr := fakePRRepo + "#" + number
				want = append(want, []string{
					"cci", "post", "--drive", tc.drive, "--lane", tc.lane, "--kind", "head", "--pr", pr, "--commit", head, "--id", "head:" + pr + ":" + head,
					"--text", "HEAD #" + number + " " + head + " (was " + was[branch] + "): restack, range-diff patch-identical (1 commit)",
				})
			}
			var posts [][]string
			for _, inv := range vcstest.Invocations(t, f.ArgvLog) {
				if inv[0] == "cci" && inv[1] == "post" {
					posts = append(posts, inv)
				}
			}
			assertInvocations(t, posts, want)
		})
	}
}
