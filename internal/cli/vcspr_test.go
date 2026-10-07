package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/prstate"
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

// #26918 on 2026-09-28: the queue admitted it at 15:29, evicted it for merge
// conflicts at 15:32, and Graphite's record still read it enqueued at 15:45.
// The activity comment is the one GitHub served, posted through the enqueuing
// user's token rather than graphite-app's.
const (
	prInfoEvicted     = `{"prNumber":26918,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"a37cf143f3cf1022fcb1c106059b3fb336e44203"},"mergeCommitSha":null}`
	prActivityEvicted = "### Merge activity\n\n" +
		"* **Sep 28, 3:29 PM UTC**: The merge label 'merge' was detected. This PR will be added to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo) once it meets the requirements.\n" +
		"* **Sep 28, 3:29 PM UTC**: `yasyf` added this pull request to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).\n" +
		"* **Sep 28, 3:32 PM UTC**: The [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo) couldn't merge this PR because **it had merge conflicts**."
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
		name     string
		body     string
		landedOn string
		activity string
		want     prQueueReport
	}{
		{
			"enqueued from the web UI",
			prInfoQueued, "", "",
			prQueueReport{Number: 25121, Queue: prQueueQueued, State: "OPEN", Base: "dev", Enqueued: "b103a57671412a4e260ecd9763ba764e66053d15"},
		},
		{"never enqueued", prInfoOpen, "", "", prQueueReport{Number: 25131, Queue: prQueueNotQueued, State: "OPEN", Base: "dev"}},
		{"a merge label the queue dropped", prInfoStaleFlag, "", "", prQueueReport{Number: 23925, Queue: prQueueNotQueued, State: "OPEN", Base: "dev"}},
		{
			"landed by the queue",
			prInfoLanded, "dev", "",
			prQueueReport{Number: 25116, Queue: prQueueLanded, State: "MERGED", Base: "dev", Squash: "9cc33f055dc4db19da6eb13a210a810297ccdc05"},
		},
		{"merged but the squash is off the base", prInfoLanded, "", "", prQueueReport{Number: 25116, Queue: prQueueNotQueued, State: "MERGED", Base: "dev"}},
		{"closed with the queue flag still set", prInfoAbandoned, "", "", prQueueReport{Number: 24001, Queue: prQueueNotQueued, State: "CLOSED", Base: "dev"}},
		{
			"evicted while graphite still reads it enqueued",
			prInfoEvicted, "", prActivityEvicted,
			prQueueReport{Number: 26918, Queue: prQueueEvicted, State: "OPEN", Base: "dev", Evicted: "it had merge conflicts", EvictedAt: "Sep 28, 3:32 PM UTC"},
		},
		{
			"re-admitted after an eviction",
			prInfoEvicted, "", prActivityEvicted + "\n* **Sep 28, 4:02 PM UTC**: `yasyf` added this pull request to the [Graphite merge queue](https://app.graphite.com/merges?org=Forge-AI&repo=monorepo).",
			prQueueReport{Number: 26918, Queue: prQueueQueued, State: "OPEN", Base: "dev", Enqueued: "a37cf143f3cf1022fcb1c106059b3fb336e44203"},
		},
		{
			"dequeued while graphite still reads it enqueued",
			prInfoEvicted, "", "### Merge activity\n\n* **Sep 15, 1:43 PM UTC**: Removed this pull request from the Graphite merge queue.",
			prQueueReport{Number: 26918, Queue: prQueueNotQueued, State: "OPEN", Base: "dev"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyPRQueue(decodePRInfo(t, tt.body), tt.landedOn, tt.activity); got != tt.want {
				t.Errorf("classifyPRQueue = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func stubPRInfo(t *testing.T, payloads ...string) (*[]gtapi.PullRequestInfoRequest, *gtapi.Client) {
	t.Helper()
	return stubGraphiteMergeability(t, nil, payloads...)
}

func stubGraphiteMergeability(t *testing.T, statuses map[int]string, payloads ...string) (*[]gtapi.PullRequestInfoRequest, *gtapi.Client) {
	t.Helper()
	var asked []gtapi.PullRequestInfoRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphite/mergeability-status" {
			var req struct {
				PRNumbers []int `json:"prNumbers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
			}
			rows := []string{}
			for _, number := range req.PRNumbers {
				if status, ok := statuses[number]; ok {
					rows = append(rows, fmt.Sprintf(`{"prNumber":%d,"forgeSource":"github","mergeabilityStatus":%q}`, number, status))
				}
			}
			_, _ = fmt.Fprintf(w, `{"mergeabilityStatuses":[%s]}`, strings.Join(rows, ","))
			return
		}
		if r.URL.Path != "/graphite/cli/pull-request-info" {
			t.Errorf("route = %s, want pull-request-info or mergeability-status", r.URL.Path)
		}
		var req gtapi.PullRequestInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		asked = append(asked, req)
		_, _ = fmt.Fprintf(w, `{"result":{"status":"ok","prs":[%s]}}`, strings.Join(payloads, ","))
	}))
	t.Cleanup(srv.Close)
	return &asked, gtapi.NewWithToken(srv.URL, "gt-stub-token")
}

// prNode renders one pull request the way the shared poll's GraphQL query
// answers it; extra is spliced in as further fields.
func prNode(number int, state string, extra string) string {
	node := fmt.Sprintf(`{"number":%d,"state":%q,"title":"t","createdAt":"2026-09-28T15:00:00Z","author":{"login":"yasyf"},`+
		`"baseRefName":"dev","headRefName":"yasyf/pr-%d","headRefOid":"a37cf143f3cf1022fcb1c106059b3fb336e44203","mergeable":"MERGEABLE",`+
		`"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","changedFiles":2,"mergeCommit":null,"labels":{"nodes":[]},`+
		`"checks":{"nodes":[]}`, number, state, number)
	if extra != "" {
		node += "," + extra
	}
	return node + "}"
}

func prComments(bodies ...string) string {
	nodes := make([]string, 0, len(bodies))
	for _, body := range bodies {
		encoded, _ := json.Marshal(body)
		nodes = append(nodes, fmt.Sprintf(`{"body":%s}`, encoded))
	}
	return `"comments":{"nodes":[` + strings.Join(nodes, ",") + `]}`
}

func prPoll(fields ...string) string {
	return `{"data":{"rateLimit":{"remaining":4900,"resetAt":"2026-09-30T08:00:00Z"},"repository":{` +
		`"trunk":{"name":"dev","target":{"history":{"nodes":[]}}}` + prefixComma(strings.Join(fields, ",")) + `}}}`
}

func prefixComma(s string) string {
	if s == "" {
		return ""
	}
	return "," + s
}

func runPRStatusCmd(t *testing.T, client *gtapi.Client, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsPRCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"status"}, args...))
	err := cmd.ExecuteContext(withGTAPI(t.Context(), client))
	return out.String(), err
}

func TestPRStatusReportsEachQueueState(t *testing.T) {
	asked, client := stubPRInfo(t, prInfoLanded, prInfoOpen, prInfoQueued)
	github := stubPRState(t, prPoll(
		`"p0":`+prNode(25116, "CLOSED", ""),
		`"t0":{"compare":{"status":"BEHIND"}}`,
		`"b0":{"compare":{"status":"BEHIND"}}`,
		`"p1":`+prNode(25121, "OPEN", prComments()),
		`"p2":`+prNode(25131, "OPEN", prComments("LGTM")),
	))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121", "#25131", "25116")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	want := "#25121  queued · enqueued b103a576 into dev · ci none · approved · queued\n" +
		"#25131  not queued · open · ci none · approved · blocked:no-ci\n" +
		"#25116  landed · squash 9cc33f05 on dev · ci none · approved · landed\n"
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
	if len(*asked) != 1 {
		t.Fatalf("graphite asked %d times, want one batched request", len(*asked))
	}
	req := (*asked)[0]
	if req.RepoOwner != "Forge-AI" || req.RepoName != "monorepo" || !slices.Equal(req.PRNumbers, []int{25116, 25121, 25131}) || !req.Consistent {
		t.Errorf("request = %+v, want Forge-AI/monorepo #25116 #25121 #25131, consistent", req)
	}
	if len(github.queries) != 1 || github.vars[0]["m0"] != "9cc33f055dc4db19da6eb13a210a810297ccdc05" || github.vars[0]["b0"] != "refs/heads/dev" {
		t.Errorf("graphql = %d queries, vars %v; want one batch comparing only the recorded squash", len(github.queries), github.vars)
	}
}

func TestPRStatusSharesOnePollAcrossInvocations(t *testing.T) {
	_, client := stubPRInfo(t, prInfoQueued)
	github := stubPRState(t, prPoll(`"p0":`+prNode(25121, "OPEN", prComments())))

	for range 2 {
		if _, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121"); err != nil {
			t.Fatalf("pr status: %v", err)
		}
	}
	if len(github.queries) != 1 {
		t.Errorf("graphql queries = %d, want the second status served from the shared cache", len(github.queries))
	}
}

func TestPRStatusReadsALabelledPRTheQueueDropped(t *testing.T) {
	_, client := stubPRInfo(t, `{"prNumber":26918,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`)
	stubPRState(t, prPoll(`"p0":`+strings.Replace(prNode(26918, "OPEN", prComments(prActivityEvicted)), `"labels":{"nodes":[]}`, `"labels":{"nodes":[{"name":"merge"}]}`, 1)))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "26918")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	if want := "#26918  evicted: it had merge conflicts at Sep 28, 3:32 PM UTC · ci none · approved · blocked:no-ci\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
}

func TestPRStatusIgnoresActivityOfAnUnlabelledPROutOfTheQueue(t *testing.T) {
	_, client := stubPRInfo(t, `{"prNumber":26918,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`)
	stubPRState(t, prPoll(`"p0":`+prNode(26918, "OPEN", prComments(prActivityEvicted))))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "26918")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	if want := "#26918  not queued · open · ci none · approved · blocked:no-ci\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
}

func TestPRStatusReportsAnEviction(t *testing.T) {
	_, client := stubPRInfo(t, prInfoEvicted)
	stubPRState(t, prPoll(`"p0":`+strings.Replace(prNode(26918, "OPEN", prComments("Stack comment", prActivityEvicted)), `"CLEAN"`, `"DIRTY"`, 1)))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "26918")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	if want := "#26918  evicted: it had merge conflicts at Sep 28, 3:32 PM UTC · conflicting · ci none · approved · blocked:conflict,no-ci\n"; out != want {
		t.Errorf("report = %q, want %q", out, want)
	}
}

func TestPRStatusJSON(t *testing.T) {
	_, client := stubPRInfo(t, prInfoQueued)
	stubPRState(t, prPoll(`"p0":`+prNode(25121, "OPEN", prComments())))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "--json", "25121")
	if err != nil {
		t.Fatalf("pr status --json: %v", err)
	}
	var got []prStatusReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	want := []prStatusReport{{
		prQueueReport: prQueueReport{Number: 25121, Queue: prQueueQueued, State: "OPEN", Base: "dev", Enqueued: "b103a57671412a4e260ecd9763ba764e66053d15"},
		CI:            prCIReport{State: prCINone},
		Approval:      prApprovalReport{State: prApproved},
		Verdict:       "queued",
	}}
	if !reflect.DeepEqual(got, want) {
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
			_, client := stubPRInfo(t, prInfoQueued)
			stubPRState(t, prPoll(`"p0":`+prNode(25121, "OPEN", prComments()), `"p1":`+prNode(99999, "OPEN", prComments())))
			if _, err := runPRStatusCmd(t, client, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestPRStateReportsRecordsAndLanes(t *testing.T) {
	_, client := stubPRInfo(t, prInfoQueued)
	stubPRState(t, prPoll(
		`"l0":{"totalCount":1,"nodes":[{"name":"yasyf/v3-x/a","associatedPullRequests":{"nodes":[{"number":25121}]}}]}`,
		`"p0":`+prNode(25121, "OPEN", prComments()),
	))
	cmd := newVcsPRCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"state", "--repo", "Forge-AI/monorepo", "--lane-prefix", "yasyf/v3-x/", "25121"})
	if err := cmd.ExecuteContext(withGTAPI(t.Context(), client)); err != nil {
		t.Fatalf("pr state: %v", err)
	}
	var got prStateReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	pr := got.PRs[25121]
	if got.Trunk != "dev" || fmt.Sprint(got.Lanes["yasyf/v3-x/"]) != "[25121]" || pr.HeadRefName != "yasyf/pr-25121" || pr.Graphite == nil {
		t.Errorf("report = %+v", got)
	}
}

func prStateBackdate(t *testing.T, by time.Duration) {
	t.Helper()
	root, err := prStateRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "Forge-AI", "monorepo", "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st prstate.State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	st.PolledAt = st.PolledAt.Add(-by)
	for n, pr := range st.PRs {
		pr.PolledAt = pr.PolledAt.Add(-by)
		st.PRs[n] = pr
	}
	if data, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPRStatusServesTheCacheMarkedStaleWhileRateLimited(t *testing.T) {
	_, client := stubPRInfo(t, prInfoQueued)
	drained := strings.Replace(prPoll(`"p0":`+prNode(25121, "OPEN", prComments())), `"remaining":4900,"resetAt":"2026-09-30T08:00:00Z"`, `"remaining":12,"resetAt":"2099-01-01T00:00:00Z"`, 1)
	github := stubPRState(t, drained)

	fresh, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	if want := "#25121  queued · enqueued b103a576 into dev · ci none · approved · queued\n"; fresh != want {
		t.Errorf("fresh report = %q, want %q", fresh, want)
	}
	prStateBackdate(t, time.Minute)

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121")
	if err != nil {
		t.Fatalf("pr status under the quota backoff: %v", err)
	}
	if !strings.HasPrefix(out, "#25121  queued · enqueued b103a576 into dev · ci none · approved · queued · stale, polled 20") || len(github.queries) != 1 {
		t.Errorf("stale report = %q after %d polls, want the cached verdict marked stale with no new poll", out, len(github.queries))
	}
	jsonOut, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "--json", "25121")
	if err != nil {
		t.Fatalf("pr status --json under the quota backoff: %v", err)
	}
	var got []prQueueReport
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", jsonOut, err)
	}
	if len(got) != 1 || got[0].Stale == nil || got[0].Stale.Reason != "quota below the floor" || got[0].Stale.ProbeAt.Year() != 2099 || got[0].Stale.PolledAt.IsZero() {
		t.Errorf("json report = %+v, want a stale marker naming the backoff and the poll time", got)
	}

	_, err = runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121", "99999")
	if err == nil || !strings.Contains(err.Error(), "github quota below the floor") || !strings.Contains(err.Error(), "next probe at 2099-01-01T00:00:00Z") {
		t.Errorf("err = %v, want the refusal for a pull request the cache never polled", err)
	}
}

func TestPRStateMarksABackoffServedReadStale(t *testing.T) {
	_, client := stubPRInfo(t, prInfoQueued)
	drained := strings.Replace(prPoll(`"p0":`+prNode(25121, "OPEN", prComments())), `"remaining":4900,"resetAt":"2026-09-30T08:00:00Z"`, `"remaining":12,"resetAt":"2099-01-01T00:00:00Z"`, 1)
	stubPRState(t, drained)
	run := func() prStateReport {
		t.Helper()
		cmd := newVcsPRCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"state", "--repo", "Forge-AI/monorepo", "25121"})
		if err := cmd.ExecuteContext(withGTAPI(t.Context(), client)); err != nil {
			t.Fatalf("pr state: %v", err)
		}
		var got prStateReport
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal %q: %v", out.String(), err)
		}
		return got
	}

	if got := run(); got.Stale != nil {
		t.Errorf("fresh report carries a stale marker: %+v", got.Stale)
	}
	prStateBackdate(t, time.Minute)
	got := run()
	if got.Stale == nil || got.Stale.Reason != "quota below the floor" || !got.Stale.PolledAt.Equal(got.PolledAt) || got.PRs[25121].HeadRefName != "yasyf/pr-25121" {
		t.Errorf("report = %+v, want the cached record served with a stale marker", got)
	}
}

// GitHub's answer for Forge-AI/monorepo on 2026-10-02, titles and branches
// scrubbed: #29551 green, #29552 and #29558 with buildkite/test failed and
// Graphite's mergeability check running, all approved by a user and the bot.
func TestPRStatusReadsCIAndApprovalFromARealPoll(t *testing.T) {
	poll, err := os.ReadFile(filepath.Join(ghPkgDir, "testdata", "prstatus", "monorepo-29551-29552-29558.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, client := stubPRInfo(t,
		`{"prNumber":29551,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`,
		`{"prNumber":29552,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`,
		`{"prNumber":29558,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`,
	)
	github := stubPRState(t, string(poll))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "29551", "29552", "29558")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	want := "#29551  not queued · open · ci green · approved by poetic-svc, forge-pr-reviewer · landable\n" +
		"#29552  not queued · open · ci red: buildkite/test · approved by poetic-svc, forge-pr-reviewer · blocked:ci-red\n" +
		"#29558  not queued · open · ci red: buildkite/test · approved by poetic-svc, forge-pr-reviewer · blocked:ci-red\n"
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
	if len(github.queries) != 1 || !strings.Contains(github.queries[0], "latestOpinionatedReviews") || !strings.Contains(github.queries[0], "isDraft") {
		t.Errorf("graphql = %d queries, want one batch asking for reviews and draft state", len(github.queries))
	}
}

// Forge-AI/monorepo on 2026-10-07: #31124's parent landed, and Graphite held its
// mergeability check open over otherwise green heads up the stack.
func TestPRStatusReadsGraphitesStackStateOverItsHeldCheck(t *testing.T) {
	_, client := stubGraphiteMergeability(t,
		map[int]string{31124: "NEEDS_RESTACK__BASE_BRANCH_MERGED", 31126: "WAITING_ON_DOWNSTACK", 31129: "READY_TO_MERGE_AS_STACK"},
		`{"prNumber":31124,"state":"OPEN","baseRefName":"dev","mergeQueueStatus":null,"mergeCommitSha":null}`,
		`{"prNumber":31126,"state":"OPEN","baseRefName":"yasyf/iris-memory-v2-s4-store","mergeQueueStatus":null,"mergeCommitSha":null,"dependentPrNumber":31124}`,
		`{"prNumber":31129,"state":"OPEN","baseRefName":"yasyf/iris-memory-v2-s5-flags","mergeQueueStatus":null,"mergeCommitSha":null,"dependentPrNumber":31126}`,
	)
	held := `"checks":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING","contexts":{"nodes":[` +
		`{"__typename":"CheckRun","name":"emergency-approve","conclusion":"SUCCESS","status":"COMPLETED"},` +
		`{"__typename":"CheckRun","name":"Graphite / mergeability_check","conclusion":null,"status":"IN_PROGRESS"},` +
		`{"__typename":"StatusContext","context":"buildkite/test","state":"SUCCESS"}]}}}}]}`
	node := func(number int) string {
		return strings.Replace(prNode(number, "OPEN", prComments()), `"checks":{"nodes":[]}`, held, 1)
	}
	stubPRState(t, prPoll(`"p0":`+node(31124), `"p1":`+node(31126), `"p2":`+node(31129)))

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "31124", "31126", "31129")
	if err != nil {
		t.Fatalf("pr status: %v", err)
	}
	want := "#31124  not queued · open · ci green · approved · blocked:needs-restack (parent landed)\n" +
		"#31126  not queued · open · ci green · approved · blocked:waiting-on-downstack #31124\n" +
		"#31129  not queued · open · ci green · approved · landable\n"
	if out != want {
		t.Errorf("report =\n%s\nwant\n%s", out, want)
	}
}

func rollupOf(state string, contexts ...prstate.Context) *prstate.Rollup {
	r := &prstate.Rollup{State: state}
	r.Contexts.Nodes = contexts
	return r
}

func checkRun(name, conclusion, status string) prstate.Context {
	return prstate.Context{Typename: "CheckRun", Name: name, Conclusion: conclusion, Status: status}
}

func TestPRCIOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		rollup *prstate.Rollup
		want   prCIReport
		line   string
	}{
		{"nothing reported", nil, prCIReport{State: prCINone}, "ci none"},
		{"only skipped", rollupOf("SUCCESS", checkRun("request", "SKIPPED", "COMPLETED")), prCIReport{State: prCINone}, "ci none"},
		{
			"running with nothing failed",
			rollupOf("PENDING", checkRun("lint", "SUCCESS", "COMPLETED"), checkRun("test", "", "IN_PROGRESS")),
			prCIReport{State: prCIPending, Running: 1},
			"ci pending: 1 running",
		},
		{
			"only Graphite's mergeability check held open",
			rollupOf("PENDING", checkRun("lint", "SUCCESS", "COMPLETED"), checkRun("Graphite / mergeability_check", "", "IN_PROGRESS")),
			prCIReport{State: prCIGreen},
			"ci green",
		},
		{
			"a re-run's newest attempt wins",
			rollupOf("SUCCESS", checkRun("test", "FAILURE", "COMPLETED"), checkRun("test", "SUCCESS", "COMPLETED")),
			prCIReport{State: prCIGreen},
			"ci green",
		},
		{
			"a failed rollup with nothing failing under it",
			rollupOf("ERROR", checkRun("lint", "SUCCESS", "COMPLETED")),
			prCIReport{State: prCIRed, Rollup: "ERROR"},
			"ci red: rollup error",
		},
		{
			"failures named and capped",
			rollupOf("FAILURE", checkRun("a", "FAILURE", "COMPLETED"), checkRun("b", "CANCELLED", "COMPLETED"),
				checkRun("c", "TIMED_OUT", "COMPLETED"), prstate.Context{Typename: "StatusContext", Context: "buildkite/test", State: "ERROR"},
				checkRun("e", "", "IN_PROGRESS")),
			prCIReport{State: prCIRed, Failing: []string{"a", "b", "c", "buildkite/test"}, Running: 1},
			"ci red: a, b, c +1 more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prCIOf(tt.rollup)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("prCIOf = %+v, want %+v", got, tt.want)
			}
			if line := prCIValue(got); line != tt.line {
				t.Errorf("prCIValue = %q, want %q", line, tt.line)
			}
		})
	}
}

func TestPRApprovalOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		decision string
		reviews  []prstate.Review
		want     prApprovalState
		line     string
	}{
		{"approved by the bot", "APPROVED", []prstate.Review{{Author: "forge-pr-reviewer", State: "APPROVED"}}, prApproved, "approved by forge-pr-reviewer"},
		{
			"one approval short",
			"REVIEW_REQUIRED",
			[]prstate.Review{{Author: "poetic-svc", State: "APPROVED"}},
			prReviewRequired, "review required, approved by poetic-svc",
		},
		{"changes requested", "CHANGES_REQUESTED", []prstate.Review{{Author: "yasyf", State: "CHANGES_REQUESTED"}}, prChangesRequested, "changes requested by yasyf"},
		{"a base requiring no review", "", []prstate.Review{{Author: "yasyf", State: "APPROVED"}}, prApproved, "approved by yasyf"},
		{"no review at all", "", nil, prNoReview, "no review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prApprovalOf(prstate.PR{ReviewDecision: tt.decision, Reviews: tt.reviews})
			if got.State != tt.want {
				t.Errorf("prApprovalOf = %+v, want %s", got, tt.want)
			}
			if line := prApprovalValue(got); line != tt.line {
				t.Errorf("prApprovalValue = %q, want %q", line, tt.line)
			}
		})
	}
}

func TestPRVerdict(t *testing.T) {
	t.Parallel()
	green := prCIReport{State: prCIGreen}
	approved := prApprovalReport{State: prApproved}
	open := prQueueReport{Queue: prQueueNotQueued, State: "OPEN"}
	tests := []struct {
		name   string
		report prStatusReport
		pr     prstate.PR
		want   string
	}{
		{"green and approved", prStatusReport{prQueueReport: open, CI: green, Approval: approved}, prstate.PR{}, "landable"},
		{"evicted but clear", prStatusReport{prQueueReport: prQueueReport{Queue: prQueueEvicted, State: "OPEN"}, CI: green, Approval: approved}, prstate.PR{}, "landable"},
		{
			"every cause named",
			prStatusReport{prQueueReport: open, CI: prCIReport{State: prCIPending}, Approval: prApprovalReport{State: prReviewRequired}},
			prstate.PR{Draft: true, Mergeable: "CONFLICTING"},
			"blocked:draft,conflict,ci-pending,unapproved",
		},
		{"changes requested", prStatusReport{prQueueReport: open, CI: green, Approval: prApprovalReport{State: prChangesRequested}}, prstate.PR{}, "blocked:changes-requested"},
		{
			"parent landed",
			prStatusReport{prQueueReport: open, CI: green, Approval: approved, Mergeability: "NEEDS_RESTACK__BASE_BRANCH_MERGED"},
			prstate.PR{}, "blocked:needs-restack (parent landed)",
		},
		{"needs a restack", prStatusReport{prQueueReport: open, CI: green, Approval: approved, Mergeability: "NEEDS_RESTACK"}, prstate.PR{}, "blocked:needs-restack"},
		{
			"waiting on the downstack",
			prStatusReport{prQueueReport: open, CI: green, Approval: prApprovalReport{State: prReviewRequired}, Mergeability: "WAITING_ON_DOWNSTACK", Downstack: 31124},
			prstate.PR{}, "blocked:waiting-on-downstack #31124,unapproved",
		},
		{"ready as a stack", prStatusReport{prQueueReport: open, CI: green, Approval: approved, Mergeability: "READY_TO_MERGE_AS_STACK", Downstack: 31124}, prstate.PR{}, "landable"},
		{"closed", prStatusReport{prQueueReport: prQueueReport{Queue: prQueueNotQueued, State: "CLOSED"}, CI: green, Approval: approved}, prstate.PR{}, "blocked:closed"},
		{"queued", prStatusReport{prQueueReport: prQueueReport{Queue: prQueueQueued, State: "OPEN"}, CI: prCIReport{State: prCIPending}}, prstate.PR{}, "queued"},
		{"landed", prStatusReport{prQueueReport: prQueueReport{Queue: prQueueLanded, State: "MERGED"}}, prstate.PR{}, "landed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := prVerdict(tt.report, tt.pr); got != tt.want {
				t.Errorf("prVerdict = %q, want %q", got, tt.want)
			}
		})
	}
}
