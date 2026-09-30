package prstate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		`{"prNumber":189,"state":"OPEN","baseRefName":"main","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"b2b2"},"body":"long body","versions":[{"headSha":"b2"}]}`,
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

func TestPollHoldsTheLastGraphiteRecordWhenGraphiteFails(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	gt := stubGraphite(t, http.StatusOK, `{"prNumber":190,"state":"OPEN","baseRefName":"main","mergeQueueStatus":{"isInGraphiteMq":true}}`)
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
	if !inQueue(st.PRs[190].Graphite) || !strings.Contains(gh.queries[0], "comments(last: 100)") {
		t.Errorf("#190 = %+v, want the queued record held and its activity still read", st.PRs[190])
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
		{name: "missing pull request", errors: []ghapi.GraphQLMessage{{Type: "NOT_FOUND", Path: []any{"repository", "p1"}}}, kept: "[{1 false x }]", gone: "[2]", ok: true},
		{name: "unknown squash", errors: []ghapi.GraphQLMessage{{Type: "NOT_FOUND", Path: []any{"repository", "t0", "compare"}}}, kept: "[{1 false  } {2 false  }]", gone: "[]", ok: true},
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
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "recorded-cc-context-189-190.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}, Prefixes: []string{"yasyf/gh-budget/"}})
	if err != nil {
		t.Fatal(err)
	}
	pr := st.PRs[190]
	if pr.State != "MERGED" || pr.HeadRefName != "yasyf/v3-pr-subscribe/pr-watch" || pr.MergeCommit != "6120690334dc2ddbb7f9c6e08a668ee300384bb4" ||
		pr.Author != "yasyf" || pr.ChangedFiles != 5 || pr.CreatedAt.IsZero() {
		t.Errorf("#190 = %+v", pr)
	}
	if fmt.Sprint(pr.SquashOn) != "[main]" || !pr.settled() {
		t.Errorf("#190 squash on %v, want main from GitHub's own merge", pr.SquashOn)
	}
	if pr.Rollup == nil || pr.Rollup.State != "SUCCESS" || len(pr.Rollup.Contexts.Nodes) == 0 || pr.Rollup.Contexts.Nodes[0].Typename != "CheckRun" {
		t.Errorf("#190 rollup = %+v", pr.Rollup)
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
