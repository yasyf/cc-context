package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

func stubPRGraphQL(t *testing.T, response string) string {
	t.Helper()
	dir := t.TempDir()
	responsePath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(responsePath, []byte(response), 0o600); err != nil {
		t.Fatal(err)
	}
	program := "#!/bin/sh\nprintf 'call\\n' >> \"$GH_CALLS\"\nprintf '%s\\n' \"$@\" > \"$GH_ARGS\"\ncat \"$GH_RESPONSE\"\n"
	writeShipExecutable(t, dir, "gh", program)
	argsPath := filepath.Join(dir, "args")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CALLS", filepath.Join(dir, "calls"))
	t.Setenv("GH_ARGS", argsPath)
	t.Setenv("GH_RESPONSE", responsePath)
	return dir
}

func TestPRCommitsOnBaseBatchesGraphQL(t *testing.T) {
	dir := stubPRGraphQL(t, `{"data":{"repository":{"b0":{"c0":{"status":"BEHIND"},"c1":{"status":"IDENTICAL"},"c2":{"status":"AHEAD"}},"b1":{"c0":{"status":"DIVERGED"}}}}}`)
	candidates := []prCommitCandidate{
		{number: 1, base: "dev", sha: "a"},
		{number: 2, base: "dev", sha: "b"},
		{number: 3, base: "feature/work", sha: "c"},
		{number: 4, base: "dev", sha: "d"},
	}
	got, err := prCommitsOnBase(context.Background(), "Forge-AI/monorepo", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]bool{1: true, 2: true, 3: false, 4: false}; !maps.Equal(got, want) {
		t.Errorf("reachability = %v, want %v", got, want)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "call\n" {
		t.Errorf("gh calls = %q, want one", calls)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api\ngraphql\n", "b0=refs/heads/dev", "b1=refs/heads/feature/work", "s0_0=a", "s0_1=b", "s0_2=d", "s1_0=c", "c0: compare(headRef: $s0_0)", "c1: compare(headRef: $s0_1)", "c2: compare(headRef: $s0_2)"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("gh args missing %q: %s", want, args)
		}
	}
}

func TestPRCommitsOnBaseRefusesUnverified(t *testing.T) {
	tests := []struct {
		name, response, want string
	}{
		{"missing branch", `{"data":{"repository":{"b0":null}}}`, `base branch "dev" not found`},
		{"missing comparison", `{"data":{"repository":{"b0":{"c0":null}}}}`, "compare a with dev: no result"},
		{"unknown status", `{"data":{"repository":{"b0":{"c0":{"status":"UNKNOWN"}}}}}`, `unknown status "UNKNOWN"`},
		{"GraphQL error", `{"errors":[{"message":"query failed"}]}`, "query failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubPRGraphQL(t, tt.response)
			_, err := prCommitsOnBase(context.Background(), "Forge-AI/monorepo", []prCommitCandidate{{number: 1, base: "dev", sha: "a"}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
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

func stubCommitsOnBase(t *testing.T, onBase map[string]bool) *[][]prCommitCandidate {
	t.Helper()
	var compared [][]prCommitCandidate
	prev := prCommitsOnBase
	prCommitsOnBase = func(_ context.Context, _ string, candidates []prCommitCandidate) (map[int]bool, error) {
		compared = append(compared, slices.Clone(candidates))
		got := make(map[int]bool, len(candidates))
		for _, candidate := range candidates {
			got[candidate.number] = onBase[candidate.sha]
		}
		return got, nil
	}
	t.Cleanup(func() { prCommitsOnBase = prev })
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
	compared := stubCommitsOnBase(t, map[string]bool{"9cc33f055dc4db19da6eb13a210a810297ccdc05": true})

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
	if want := [][]prCommitCandidate{{{number: 25116, base: "dev", sha: "9cc33f055dc4db19da6eb13a210a810297ccdc05"}}}; !reflect.DeepEqual(*compared, want) {
		t.Errorf("compares = %v, want %v", *compared, want)
	}
}

func TestPRStatusJSON(t *testing.T) {
	stubPRInfo(t, prInfoQueued)
	compared := stubCommitsOnBase(t, nil)

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
	if len(*compared) != 0 {
		t.Errorf("compares = %v, want none", *compared)
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
			stubCommitsOnBase(t, nil)
			if _, err := runPRStatusCmd(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
