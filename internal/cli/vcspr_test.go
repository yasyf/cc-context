package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if want := "#26918  evicted: it had merge conflicts at Sep 28, 3:32 PM UTC\n"; out != want {
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
	if want := "#26918  not queued · open\n"; out != want {
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
	if want := "#26918  evicted: it had merge conflicts at Sep 28, 3:32 PM UTC · conflicting\n"; out != want {
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
	if want := "#25121  queued · enqueued b103a576 into dev\n"; fresh != want {
		t.Errorf("fresh report = %q, want %q", fresh, want)
	}
	prStateBackdate(t, time.Minute)

	out, err := runPRStatusCmd(t, client, "--repo", "Forge-AI/monorepo", "25121")
	if err != nil {
		t.Fatalf("pr status under the quota backoff: %v", err)
	}
	if !strings.HasPrefix(out, "#25121  queued · enqueued b103a576 into dev · stale, polled 20") || len(github.queries) != 1 {
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
