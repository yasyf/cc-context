package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
)

// Graphite's pull-request-info answers for Forge-AI/monorepo on 2026-09-24:
// #25121 enqueued from the Graphite web UI with no merge label, #25131 open and
// never enqueued, #25116 landed by the queue (GitHub reads it CLOSED with a null
// mergedAt, and Graphite keeps isInGraphiteMq set), and #23925 still carrying a
// merge label the queue had dropped.
const (
	prInfoQueued    = `{"prNumber":25121,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"b103a57671412a4e260ecd9763ba764e66053d15"},"mergeCommitSha":null}`
	prInfoOpen      = `{"prNumber":25131,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`
	prInfoLanded    = `{"prNumber":25116,"state":"MERGED","baseRefName":"dev","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"bea3a53bca0f57ff156ff9c1ca60c6831f2c0f69"},"mergeCommitSha":"9cc33f055dc4db19da6eb13a210a810297ccdc05"}`
	prInfoStaleFlag = `{"prNumber":23925,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`
	prInfoAbandoned = `{"prNumber":24001,"state":"CLOSED","baseRefName":"dev","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"aaaa"},"mergeCommitSha":null}`
)

func decodePRInfo(t *testing.T, body string) gtapi.PullRequestInfo {
	t.Helper()
	var info gtapi.PullRequestInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return info
}

func TestClassifyPRQueue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		body   string
		onBase bool
		want   prQueueReport
	}{
		{
			"enqueued from the web UI",
			prInfoQueued, false,
			prQueueReport{Number: 25121, Queue: prQueueQueued, State: "OPEN", Base: "dev", Enqueued: "b103a57671412a4e260ecd9763ba764e66053d15"},
		},
		{"never enqueued", prInfoOpen, false, prQueueReport{Number: 25131, Queue: prQueueNotQueued, State: "OPEN", Base: "dev"}},
		{"a merge label the queue dropped", prInfoStaleFlag, false, prQueueReport{Number: 23925, Queue: prQueueNotQueued, State: "OPEN", Base: "dev"}},
		{
			"landed by the queue",
			prInfoLanded, true,
			prQueueReport{Number: 25116, Queue: prQueueLanded, State: "MERGED", Base: "dev", Squash: "9cc33f055dc4db19da6eb13a210a810297ccdc05"},
		},
		{"merged but the squash is off the base", prInfoLanded, false, prQueueReport{Number: 25116, Queue: prQueueNotQueued, State: "MERGED", Base: "dev"}},
		{"closed with the queue flag still set", prInfoAbandoned, false, prQueueReport{Number: 24001, Queue: prQueueNotQueued, State: "CLOSED", Base: "dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyPRQueue(decodePRInfo(t, tt.body), tt.onBase); got != tt.want {
				t.Errorf("classifyPRQueue = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// stubPRInfo serves pull-request-info from the recorded payloads and records
// each request's numbers and repository.
func stubPRInfo(t *testing.T, payloads ...string) *[]gtapi.PullRequestInfoRequest {
	t.Helper()
	var asked []gtapi.PullRequestInfoRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphite/cli/pull-request-info" {
			t.Errorf("route = %s, want pull-request-info", r.URL.Path)
		}
		var req gtapi.PullRequestInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		asked = append(asked, req)
		_, _ = fmt.Fprintf(w, `{"result":{"status":"ok","prs":[%s]}}`, strings.Join(payloads, ","))
	}))
	prev := gtAPIClient
	gtAPIClient = func() *gtapi.Client { return gtapi.NewWithToken(srv.URL, "gt-stub-token") }
	t.Cleanup(func() {
		gtAPIClient = prev
		srv.Close()
	})
	return &asked
}

func stubCommitOnBase(t *testing.T, onBase map[string]bool) *[]string {
	t.Helper()
	var compared []string
	prev := prCommitOnBase
	prCommitOnBase = func(_ context.Context, repo, base, sha string) (bool, error) {
		compared = append(compared, repo+" "+base+"..."+sha)
		return onBase[sha], nil
	}
	t.Cleanup(func() { prCommitOnBase = prev })
	return &compared
}

func runPRStatusCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsPRCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"status"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// TestPRStatusReportsEachQueueState pins the three answers against the recorded
// Graphite payloads, in the order the numbers were asked, and that only a
// recorded squash costs a compare against the base.
func TestPRStatusReportsEachQueueState(t *testing.T) {
	asked := stubPRInfo(t, prInfoLanded, prInfoOpen, prInfoQueued)
	compared := stubCommitOnBase(t, map[string]bool{"9cc33f055dc4db19da6eb13a210a810297ccdc05": true})

	out, err := runPRStatusCmd(t, "--repo", "Forge-AI/monorepo", "25121", "#25131", "25116")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	want := "#25121  queued · enqueued b103a576 into dev\n" +
		"#25131  not queued · open\n" +
		"#25116  landed · squash 9cc33f05 on dev\n"
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
	if len(*asked) != 1 {
		t.Fatalf("graphite asked %d times, want one batched request", len(*asked))
	}
	req := (*asked)[0]
	if req.RepoOwner != "Forge-AI" || req.RepoName != "monorepo" || !slices.Equal(req.PRNumbers, []int{25121, 25131, 25116}) || !req.Consistent {
		t.Errorf("request = %+v, want Forge-AI/monorepo #25121 #25131 #25116, consistent", req)
	}
	if want := []string{"Forge-AI/monorepo dev...9cc33f055dc4db19da6eb13a210a810297ccdc05"}; !slices.Equal(*compared, want) {
		t.Errorf("compares = %v, want %v", *compared, want)
	}
}

func TestPRStatusJSON(t *testing.T) {
	stubPRInfo(t, prInfoQueued)
	stubCommitOnBase(t, nil)

	out, err := runPRStatusCmd(t, "--repo", "Forge-AI/monorepo", "--json", "25121")
	if err != nil {
		t.Fatalf("pr status --json: %v", err)
	}
	var got []prQueueReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	want := []prQueueReport{{Number: 25121, Queue: prQueueQueued, State: "OPEN", Base: "dev", Enqueued: "b103a57671412a4e260ecd9763ba764e66053d15"}}
	if !slices.Equal(got, want) {
		t.Errorf("report = %+v, want %+v", got, want)
	}
}

func TestPRStatusRefuses(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"not a number", []string{"--repo", "Forge-AI/monorepo", "abc"}, `"abc" is not a pull request number`},
		{"malformed repository", []string{"--repo", "monorepo", "25121"}, `malformed repository name "monorepo"`},
		{"graphite has no record", []string{"--repo", "Forge-AI/monorepo", "25121", "99999"}, "graphite has no record of Forge-AI/monorepo#99999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubPRInfo(t, prInfoQueued)
			stubCommitOnBase(t, nil)
			if _, err := runPRStatusCmd(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
