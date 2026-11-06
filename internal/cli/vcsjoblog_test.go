package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
)

const jobLogStub = `#!/bin/sh
case "$*" in
"api repos/o/r/actions/jobs/7/logs --allow-escape-sequences")
	printf '\357\273\277setup\n\033[31mred\033[0m \033]8;;http://x\007link\033]8;;\007\033(B\nthe failure\n'
	;;
"api repos/o/r/actions/jobs/11/logs --allow-escape-sequences")
	printf '2026-11-06T19:36:17.6245033Z ===== short test summary info =====\n2026-11-06T19:36:17.6246128Z \033[31mFAILED\033[0m tests/test_a.py::test_b[x - y] - AssertionError\n2026-11-06T19:36:17.6248090Z 1 failed, 3 passed in 2.01s\n2026-11-06T19:36:17.6702454Z ##[error]Process completed with exit code 1.\n'
	;;
"api repos/o/r/actions/jobs/8/logs --allow-escape-sequences")
	echo '<Error>BlobNotFound</Error>'
	echo 'gh: HTTP 404' >&2
	exit 1
	;;
"api repos/o/r/actions/jobs/8 --jq .status")
	echo in_progress
	;;
"api repos/o/r/actions/jobs/9/logs --allow-escape-sequences")
	echo 'gh: Not Found (HTTP 404)' >&2
	exit 1
	;;
"api repos/o/r/actions/jobs/9 --jq .status")
	echo 'gh: Not Found (HTTP 404)' >&2
	exit 1
	;;
"api repos/o/r/actions/jobs/10/logs --allow-escape-sequences")
	echo 'gh: Log expired (HTTP 410)' >&2
	exit 1
	;;
"api repos/o/r/actions/jobs/10 --jq .status")
	echo completed
	;;
*)
	echo "unexpected: $*" >&2
	exit 2
	;;
esac
`

func TestRunVcsJobLog(t *testing.T) {
	cmd, out := ghGatewayFixture(t)
	shims := t.TempDir()
	execstub.Write(t, filepath.Join(shims, "gh"), jobLogStub)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name    string
		id      int64
		budget  int
		failed  bool
		want    string
		wantErr []string
	}{
		{name: "a settled job prints plain text", id: 7, want: "setup\nred link\nthe failure\n"},
		{name: "a budget keeps the failure at the end", id: 7, budget: 3, want: "… +2 lines, ~3 tokens omitted above — re-run with a larger --budget\nthe failure\n"},
		{name: "failed tests print only the ids the failing pytest run names", id: 11, failed: true, want: "tests/test_a.py::test_b[x - y]\n"},
		{name: "an unsettled job says so instead of relaying the blob error", id: 8, wantErr: []string{"vcs job-log: job 8 is in_progress; GitHub serves its log once it settles"}},
		{name: "an unknown job relays GitHub's answer", id: 9, wantErr: []string{"vcs job-log: job 9 in o/r", "gh: Not Found (HTTP 404)"}},
		{name: "a settled job whose log is gone relays GitHub's answer", id: 10, wantErr: []string{"vcs job-log: job 10 in o/r", "gh: Log expired (HTTP 410)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out.Reset()
			err := runVcsJobLog(cmd, tt.id, "o/r", tt.budget, tt.failed)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("err = nil, want %q", tt.wantErr)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("out = %q, want %q", got, tt.want)
			}
		})
	}
}

func pytestJobLog(tests ...string) string {
	var log strings.Builder
	log.WriteString("2026-11-06T19:36:17.6245033Z =========================== short test summary info ============================\n")
	for _, test := range tests {
		fmt.Fprintf(&log, "2026-11-06T19:36:17.6246128Z FAILED %s - AssertionError: assert {\n2026-11-06T19:36:17.6247380Z   Extra items in the left set:\n", test)
	}
	fmt.Fprintf(&log, "2026-11-06T19:36:17.6248090Z %d failed, 320 passed in 20.36s\n2026-11-06T19:36:17.6702454Z ##[error]Process completed with exit code 1.\n", len(tests))
	return log.String()
}

func TestFailedTestsReadsThePytestRunThatFailedTheStep(t *testing.T) {
	const kept, added = "tests/test_fail_open.py::test_every_refusal[go_posts_parse - kept]", "tests/test_merge.py::test_refusal"
	for _, tc := range []struct {
		name, log string
		want      []string
	}{
		{name: "every failing test with its whole parameter id", log: pytestJobLog(kept, added), want: []string{kept, added}},
		{
			name: "a collection error",
			log:  "2026-11-06T19:36:17.1Z ===== short test summary info =====\n2026-11-06T19:36:17.2Z ERROR tests/test_c.py - ImportError: boom\n2026-11-06T19:36:17.3Z !!!!! Interrupted: 1 error during collection !!!!!\n2026-11-06T19:36:17.4Z 1 error in 0.50s\n2026-11-06T19:36:17.5Z ##[error]Process completed with exit code 2.\n",
			want: []string{"tests/test_c.py"},
		},
		{name: "an assertion message quoting the summary header", log: strings.Replace(pytestJobLog(added, kept), "Extra items in the left set:", "assert 'short test summary info' in out", 1), want: []string{added, kept}},
		{name: "a FAILED line printed before the summary", log: "2026-11-06T19:36:17.0000000Z FAILED " + added + "\n" + pytestJobLog(kept), want: []string{kept}},
		{name: "a colored summary", log: strings.NewReplacer("FAILED", "\x1b[31mFAILED\x1b[0m", "1 failed", "\x1b[31m1 failed\x1b[0m").Replace(pytestJobLog(kept)), want: []string{kept}},
		{name: "a summary printed before something else failed the step", log: strings.Replace(pytestJobLog(kept), "##[error]", "control reader-validation failed\n2026-11-06T19:36:18.0000000Z ##[error]", 1), want: nil},
		{name: "a job cancelled before its step ended", log: "2026-11-06T19:36:17.0000000Z ##[error]The operation was canceled.\n", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failedTests(tc.log); !slices.Equal(got, tc.want) {
				t.Errorf("failedTests = %q, want %q", got, tc.want)
			}
		})
	}
}
