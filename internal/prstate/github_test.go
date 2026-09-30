package prstate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

func stubGraphite(t *testing.T, status int, prs ...string) *gtapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gtapi.PullRequestInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode graphite request: %v", err)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = fmt.Fprintf(w, `{"result":{"status":"ok","prs":[%s]}}`, strings.Join(prs, ","))
	}))
	t.Cleanup(srv.Close)
	return gtapi.NewWithToken(srv.URL, "gt-stub-token")
}

func TestPollReadsQueueActivityAndWhereTheSquashLanded(t *testing.T) {
	t.Parallel()
	gt := stubGraphite(t, http.StatusOK,
		`{"prNumber":189,"state":"OPEN","baseRefName":"main","headRefName":"yasyf/gh-budget/pr-189","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"b2b2"},"body":"long body","versions":[{"headSha":"b2"}]}`,
		`{"prNumber":190,"state":"MERGED","baseRefName":"main","mergeCommitSha":"6120690000000000000000000000000000000bbb"}`)
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, gt, ok(t, "poll-queue.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}})
	if err != nil {
		t.Fatal(err)
	}
	queued := st.PRs[189]
	if !strings.HasPrefix(queued.Activity, ActivityHeading) || !queued.QueueLabelled() || !inQueue(queued.Graphite) {
		t.Errorf("#189 = %+v, want its merge activity, label, and Graphite queue record", queued)
	}
	if queued.Graphite.Body != "" || queued.Graphite.Versions != nil {
		t.Errorf("#189 graphite = %+v, want body and versions left out of the cache", queued.Graphite)
	}
	if landed := st.PRs[190]; fmt.Sprint(landed.SquashOn) != "[main]" || !landed.settled() {
		t.Errorf("#190 = %+v, want its squash landed on main", landed)
	}
	query := gh.queries[0]
	if strings.Count(query, "comments(last: 100)") != 1 || !strings.Contains(query, "t1: defaultBranchRef { compare(headRef: $m1)") {
		t.Errorf("query reads activity beyond the queued PR or skips the squash compare:\n%s", query)
	}
	if gh.vars[0]["m1"] != "6120690000000000000000000000000000000bbb" || gh.vars[0]["b1"] != "refs/heads/main" {
		t.Errorf("vars = %v", gh.vars[0])
	}
}

func TestAGraphiteFailureLeavesTheQueueRecordOutButStillReadsActivity(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	gt := stubGraphite(t, http.StatusOK, `{"prNumber":190,"state":"OPEN","baseRefName":"main","headRefName":"yasyf/gh-budget/pr-190","mergeQueueStatus":{"isInGraphiteMq":true}}`)
	dir := t.TempDir()
	first, _ := newStore(t, dir, c, gt, ok(t, "poll-190.json"))
	if _, err := first.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(MinInterval)
	second, gh := newStore(t, dir, c, stubGraphite(t, http.StatusBadGateway), ok(t, "poll-190.json"))
	st, err := second.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatal(err)
	}
	if st.PRs[190].Graphite != nil || !strings.Contains(gh.queries[0], "comments(last: 100)") {
		t.Errorf("#190 = %+v, want no stale queue record, and its activity still read off the last one", st.PRs[190])
	}
}

func TestDropNotFoundRefusesAnyOtherError(t *testing.T) {
	t.Parallel()
	chunk := []target{{number: 1, squash: "x"}, {number: 2}}
	tests := []struct {
		name   string
		errors []ghapi.GraphQLMessage
		kept   string
		gone   string
		ok     bool
	}{
		{name: "missing pull request", errors: []ghapi.GraphQLMessage{{Type: "NOT_FOUND", Path: []any{"repository", "p1"}}}, kept: "[{1 false x  }]", gone: "[2]", ok: true},
		{name: "unknown squash", errors: []ghapi.GraphQLMessage{{Type: "NOT_FOUND", Path: []any{"repository", "t0", "compare"}}}, kept: "[{1 false   } {2 false   }]", gone: "[]", ok: true},
		{name: "missing repository", errors: []ghapi.GraphQLMessage{{Type: "NOT_FOUND", Path: []any{"repository"}}}},
		{name: "rate limited", errors: []ghapi.GraphQLMessage{{Type: "RATE_LIMITED"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kept, gone, ok := dropNotFound(chunk, &ghapi.GraphQLError{Messages: tt.errors})
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if tt.ok && (fmt.Sprint(kept) != tt.kept || fmt.Sprint(append([]int{}, gone...)) != tt.gone) {
				t.Errorf("dropNotFound = %v, %v; want %s, %s", kept, gone, tt.kept, tt.gone)
			}
		})
	}
}

func TestNewGitHubRejectsAMalformedRepository(t *testing.T) {
	t.Parallel()
	if _, err := NewGitHub(ghapi.New("http://unused"), nil, "cc-context", io.Discard); err == nil {
		t.Error("NewGitHub accepted a name without an owner")
	}
}

func TestPollDecodesARecordedGitHubResponse(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, stubGraphite(t, http.StatusBadGateway), ok(t, "recorded-cc-context-189-190.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}, Prefixes: []string{"yasyf/gh-budget/"}})
	if err != nil {
		t.Fatal(err)
	}
	pr := st.PRs[190]
	if pr.State != "MERGED" || pr.HeadRefName != "yasyf/v3-pr-subscribe/pr-watch" || pr.MergeCommit != "6120690334dc2ddbb7f9c6e08a668ee300384bb4" ||
		pr.Author != "yasyf" || pr.ChangedFiles != 5 || pr.CreatedAt.IsZero() {
		t.Errorf("#190 = %+v", pr)
	}
	if fmt.Sprint(pr.SquashOn) != "[main]" || pr.settled() {
		t.Errorf("#190 squash on %v, want main from GitHub's own merge, still polled without a Graphite record", pr.SquashOn)
	}
	if pr.Rollup == nil || pr.Rollup.State != "SUCCESS" || len(pr.Rollup.Contexts.Nodes) == 0 || pr.Rollup.Contexts.Nodes[0].Typename != "CheckRun" {
		t.Errorf("#190 rollup = %+v", pr.Rollup)
	}
	if fmt.Sprint(pr.Files) != "[CHANGELOG.md README.md internal/cli/vcspr.go internal/cli/vcsprwatch.go internal/cli/vcsprwatch_test.go]" {
		t.Errorf("#190 files = %v", pr.Files)
	}
	if st.Trunk.Name != "main" || len(st.Trunk.History) != 100 || st.Rate.Remaining == 0 {
		t.Errorf("trunk %q with %d commits, rate %+v", st.Trunk.Name, len(st.Trunk.History), st.Rate)
	}
	if lane, ok := st.Lanes["yasyf/gh-budget/"]; !ok || len(lane.PRs) != 0 {
		t.Errorf("lane = %+v, want an empty lane recorded", lane)
	}
	if gh.requests() != 1 {
		t.Errorf("requests = %d, want one batched query", gh.requests())
	}
}

func TestAStackReadFollowsParentsAndDecodesReviewEvidence(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-stack-191.json"), ok(t, "poll-190.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{191}})
	if err != nil {
		t.Fatal(err)
	}
	tip := st.PRs[191]
	if fmt.Sprint(tip.Parents) != "[190]" || fmt.Sprint(tip.Children) != "[192]" {
		t.Errorf("#191 parents %v children %v, want [190] and [192]", tip.Parents, tip.Children)
	}
	if fmt.Sprint(tip.Files) != "[internal/prstate/prstate.go internal/prstate/github.go]" ||
		fmt.Sprint(tip.Reviews) != "[{poetic-svc APPROVED} {reviewer CHANGES_REQUESTED}]" {
		t.Errorf("#191 files %v reviews %v", tip.Files, tip.Reviews)
	}
	if len(tip.LabelEvents) != 2 || !tip.LabelEvents[0].Added || tip.LabelEvents[1].Added || tip.LabelEvents[1].Actor != "graphite-app" {
		t.Errorf("#191 label events = %+v", tip.LabelEvents)
	}
	contexts := tip.Rollup.Contexts.Nodes
	if contexts[0].DetailsURL == "" || contexts[1].TargetURL != "https://buildkite.com/forge/test/builds/9#job" {
		t.Errorf("#191 contexts = %+v, want their URLs", contexts)
	}
	if gh.requests() != 2 || gh.vars[0]["h0"] != "yasyf/gh-budget/pr-191" || gh.vars[1]["p0"] != float64(190) || st.PRs[190].State != "OPEN" {
		t.Errorf("vars %v, want the parent #190 read in a follow-up of the same Read", gh.vars)
	}
	if _, leased := st.Leases.PRs[190]; !leased {
		t.Error("the parent #190 carries no lease, so the next poll skips it")
	}
	c.now = c.now.Add(time.Second)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{191}}); err != nil || gh.requests() != 2 {
		t.Errorf("re-read = %v after %d requests, want the stack served from the cache", err, gh.requests())
	}
}

func TestAnOpenPRGraphiteCannotNameReadsItsChildrenInAFollowUp(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, stubGraphite(t, http.StatusBadGateway), ok(t, "poll-190.json"), ok(t, "poll-190.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatal(err)
	}
	if _, asked := gh.vars[0]["h0"]; asked || gh.vars[1]["h0"] != "yasyf/gh-budget/pr-190" || st.PRs[190].Children == nil {
		t.Errorf("vars %v children %v, want the children read once the head branch is known", gh.vars, st.PRs[190].Children)
	}
}
