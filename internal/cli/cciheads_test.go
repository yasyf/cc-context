package cli

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestRangeDiffSummaryCountsPairsByMark(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"identical", "1:  aaaaaaa = 1:  bbbbbbb base\n2:  ccccccc = 2:  ddddddd more\n", "patch-identical (2 commits)"},
		{"one identical", "1:  aaaaaaa = 1:  bbbbbbb base\n", "patch-identical (1 commit)"},
		{"changed, added and dropped", "1:  aaaaaaa ! 1:  bbbbbbb base\n    @@ base.txt\n    -a = b\n    +a ! c\n2:  ccccccc < -:  ------- gone\n-:  ------- > 2:  ddddddd new\n3:  eeeeeee = 3:  fffffff kept\n", "1 identical, 1 changed, 1 added (ddddddd), 1 dropped"},
		{"ten or more pairs pad their numbers", paddedRangeDiff(10) + " -:  ------- > 11:  ccccccc new\n", "10 identical, 1 added (ccccccc)"},
		{"ten or more identical pairs", paddedRangeDiff(12), "patch-identical (12 commits)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rangeDiffSummary(tc.out); got != tc.want {
				t.Errorf("rangeDiffSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func paddedRangeDiff(n int) string {
	var out string
	for i := 1; i <= n; i++ {
		index := fmt.Sprintf("%2d", i)
		out += index + ":  aaaaaaa = " + index + ":  bbbbbbb commit " + strconv.Itoa(i) + "\n"
	}
	return out
}

func TestStackSubmitPostsOneHeadRecordCarryingEveryMovedPullRequestInADrive(t *testing.T) {
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
			if tc.drive != "" {
				want = append(want, []string{"cci", "post", "--drive", tc.drive, "--lane", tc.lane, "--kind", "head", "--id", ""})
			}
			var keys []string
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
				keys = append(keys, pr+":"+head)
				want[0] = append(want[0], "--head", pr+"="+was[branch]+".."+head+" restack, range-diff patch-identical (1 commit)")
			}
			if tc.drive != "" {
				want[0][9] = "head:" + strings.Join(keys, ",")
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
