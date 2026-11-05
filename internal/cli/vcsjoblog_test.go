package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
)

const jobLogStub = `#!/bin/sh
case "$*" in
"api repos/o/r/actions/jobs/7/logs --allow-escape-sequences")
	printf '\357\273\277setup\n\033[31mred\033[0m \033]8;;http://x\007link\033]8;;\007\033(B\nthe failure\n'
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
		want    string
		wantErr []string
	}{
		{name: "a settled job prints plain text", id: 7, want: "setup\nred link\nthe failure\n"},
		{name: "a budget keeps the failure at the end", id: 7, budget: 3, want: "… +2 lines, ~3 tokens omitted above — re-run with a larger --budget\nthe failure\n"},
		{name: "an unsettled job says so instead of relaying the blob error", id: 8, wantErr: []string{"vcs job-log: job 8 is in_progress; GitHub serves its log once it settles"}},
		{name: "an unknown job relays GitHub's answer", id: 9, wantErr: []string{"vcs job-log: job 9 in o/r", "gh: Not Found (HTTP 404)"}},
		{name: "a settled job whose log is gone relays GitHub's answer", id: 10, wantErr: []string{"vcs job-log: job 10 in o/r", "gh: Log expired (HTTP 410)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out.Reset()
			err := runVcsJobLog(cmd, tt.id, "o/r", tt.budget)
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
