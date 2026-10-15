package prstate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
)

var restPath = regexp.MustCompile(`^/repos/yasyf/cc-context/(pulls/(\d+)(/reviews)?|commits/\w+/(check-runs|status)|issues/(\d+)/comments|compare/.+|commits|pulls|)$`)

type restGitHub struct {
	t *testing.T
}

func (h restGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/graphql" {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	m := restPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		h.t.Errorf("unexpected REST path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	n, _ := strconv.Atoi(m[2])
	switch {
	case r.URL.Path == "/repos/yasyf/cc-context":
		_, _ = io.WriteString(w, `{"default_branch":"main"}`)
	case r.URL.Path == "/repos/yasyf/cc-context/commits":
		_, _ = io.WriteString(w, `[{"sha":"aaa111","commit":{"message":"first line (#190)\n\nbody"}}]`)
	case r.URL.Path == "/repos/yasyf/cc-context/pulls":
		_, _ = io.WriteString(w, `[{"number":191,"head":{"ref":"yasyf/lane/a"}},{"number":192,"head":{"ref":"other"}}]`)
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		_, _ = io.WriteString(w, `[
			{"state":"APPROVED","user":{"login":"alice"}},
			{"state":"COMMENTED","user":{"login":"alice"}},
			{"state":"CHANGES_REQUESTED","user":{"login":"bob"}},
			{"state":"DISMISSED","user":{"login":"bob"}}]`)
	case strings.HasSuffix(r.URL.Path, "/check-runs"):
		_, _ = io.WriteString(w, `{"check_runs":[
			{"name":"build","status":"completed","conclusion":"success"},
			{"name":"test","status":"in_progress","conclusion":null}]}`)
	case strings.HasSuffix(r.URL.Path, "/status"):
		_, _ = io.WriteString(w, `{"state":"pending","statuses":[]}`)
	case strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = io.WriteString(w, `[{"body":"hello"},{"body":`+strconv.Quote(ActivityHeading+"\nenqueued")+`}]`)
	case strings.Contains(r.URL.Path, "/compare/"):
		_, _ = io.WriteString(w, `{"status":"behind"}`)
	case n == 190:
		_, _ = io.WriteString(w, `{"number":190,"state":"closed","title":"landed","merged_at":"2026-09-30T06:00:00Z","merge_commit_sha":"6120690","base":{"ref":"main"},"head":{"ref":"h","sha":"bbb222"},"mergeable":null,"mergeable_state":"unknown"}`)
	case n == 404:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	default:
		_, _ = fmt.Fprintf(w, `{"number":%d,"state":"open","title":"open","draft":true,"changed_files":3,"user":{"login":"yasyf"},"created_at":"2026-09-29T00:00:00Z",`+
			`"merge_commit_sha":"testmerge","mergeable":true,"mergeable_state":"clean","labels":[{"name":"merge"}],"base":{"ref":"main"},"head":{"ref":"yasyf/lane/a","sha":"ccc333"}}`, n)
	}
}

func newRESTStore(t *testing.T, gt *gtapi.Client, warn io.Writer) (*Store, *GitHub) {
	t.Helper()
	ts := httptest.NewServer(restGitHub{t: t})
	t.Cleanup(ts.Close)
	src, err := NewGitHub(ghapi.New(ts.URL), gt, "yasyf/cc-context", warn)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir(), src)
	if err != nil {
		t.Fatal(err)
	}
	return store, src
}

func TestAGraphQLFailureFallsBackToRESTRecordsWithGraphitesQueueState(t *testing.T) {
	t.Parallel()
	gt := stubGraphite(t, http.StatusOK,
		`{"prNumber":189,"state":"OPEN","baseRefName":"main","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"c3"}}`,
		`{"prNumber":190,"state":"MERGED","baseRefName":"main","mergeCommitSha":"6120690"}`)
	var warn bytes.Buffer
	store, _ := newRESTStore(t, gt, &warn)

	st, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}, Prefixes: []string{"yasyf/lane/"}})
	if err != nil {
		t.Fatal(err)
	}

	queued := st.PRs[189]
	if queued.Via != "graphite+rest" || queued.Graphite == nil || queued.GraphiteDown() {
		t.Errorf("#189 via %q graphite %v, want Graphite's record kept beside REST", queued.Via, queued.Graphite)
	}
	if queued.State != "OPEN" || !queued.Draft || queued.ChangedFiles != 3 || queued.Author != "yasyf" || queued.Mergeable != "MERGEABLE" ||
		queued.MergeStateStatus != "CLEAN" || queued.MergeCommit != "" || fmt.Sprint(queued.Labels) != "[merge]" || queued.HeadRefOid != "ccc333" {
		t.Errorf("#189 = %+v", queued)
	}
	if fmt.Sprint(queued.Reviews) != "[{alice APPROVED}]" {
		t.Errorf("#189 reviews = %v, want alice's approval stood and bob's dismissed", queued.Reviews)
	}
	if queued.Rollup == nil || queued.Rollup.State != "PENDING" || len(queued.Rollup.Contexts.Nodes) != 2 || queued.Status != "" {
		t.Errorf("#189 rollup = %+v status %q, want a pending rollup of the two check runs", queued.Rollup, queued.Status)
	}
	if !strings.HasPrefix(queued.Activity, ActivityHeading) {
		t.Errorf("#189 activity = %q, want the merge activity comment of a queued PR", queued.Activity)
	}
	landed := st.PRs[190]
	if landed.State != "MERGED" || landed.MergeCommit != "6120690" || fmt.Sprint(landed.SquashOn) != "[main]" || landed.Mergeable != "UNKNOWN" || landed.Activity != "" {
		t.Errorf("#190 = %+v, want GitHub's own merge", landed)
	}
	if st.Trunk.Name != "main" || len(st.Trunk.History) != 1 || st.Trunk.History[0].Headline != "first line (#190)" {
		t.Errorf("trunk = %+v", st.Trunk)
	}
	lane := st.Lanes["yasyf/lane/"]
	if fmt.Sprint(lane.PRs) != "[191]" {
		t.Fatalf("lane = %+v, want the open PR on the prefix", lane)
	}
	if got := st.PRs[191]; got.Via != "rest" || !got.GraphiteDown() || got.HeadRefName != "yasyf/lane/a" {
		t.Errorf("#191 = %+v, want a REST record Graphite was never asked about", got)
	}
	if !strings.Contains(warn.String(), "answering from github rest") {
		t.Errorf("warn = %q, want the fallback named", warn.String())
	}
}

func TestAGraphiteAndGraphQLFailureLeavesRESTRecordsWithoutAQueueState(t *testing.T) {
	t.Parallel()
	store, _ := newRESTStore(t, stubGraphite(t, http.StatusBadGateway), io.Discard)

	st, err := store.Read(testCtx(t), Want{PRs: []int{189}})
	if err != nil {
		t.Fatal(err)
	}
	if pr := st.PRs[189]; pr.Via != "rest" || pr.Graphite != nil || !pr.GraphiteDown() {
		t.Errorf("#189 = %+v, want a REST record with no Graphite record", pr)
	}
}

func TestARESTFallbackReportsAMissingPull(t *testing.T) {
	t.Parallel()
	store, _ := newRESTStore(t, nil, io.Discard)

	_, err := store.Read(testCtx(t), Want{PRs: []int{189, 404}})
	var missing *MissingError
	if !errors.As(err, &missing) || fmt.Sprint(missing.PRs) != "[404]" {
		t.Errorf("err = %v, want #404 reported missing", err)
	}
}

func TestEachUpstreamIsBoundedByTheDeadline(t *testing.T) {
	t.Parallel()
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	store, src := newRESTStore(t, gtapi.NewWithToken(hang.URL, "gt-stub-token"), io.Discard)
	src.deadline = 100 * time.Millisecond

	started := time.Now()
	st, err := store.Read(testCtx(t), Want{PRs: []int{189}})
	if err != nil {
		t.Fatal(err)
	}
	if pr := st.PRs[189]; pr.Via != "rest" || pr.Graphite != nil {
		t.Errorf("#189 = %+v, want a REST record after Graphite hung", pr)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("read took %s, want each hung upstream cut at the deadline", elapsed)
	}
}

func TestARateLimitedGraphQLDoesNotFallBack(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, _ := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "120"}})

	_, err := store.Read(testCtx(t), Want{PRs: []int{189}})
	var limited *LimitedError
	if !errors.As(err, &limited) {
		t.Errorf("err = %v, want the rate limit left to the backoff", err)
	}
}
