package prstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
)

var epoch = time.Date(2026, 9, 30, 7, 0, 0, 0, time.UTC)

type reply struct {
	status int
	header map[string]string
	body   string
}

type fakeGitHub struct {
	t       *testing.T
	mu      sync.Mutex
	replies []reply
	queries []string
	vars    []map[string]any
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	f.queries = append(f.queries, req.Query)
	f.vars = append(f.vars, req.Variables)
	if len(f.replies) == 0 {
		f.t.Errorf("unexpected request %d:\n%s", len(f.queries), req.Query)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	next := f.replies[0]
	f.replies = f.replies[1:]
	for k, v := range next.header {
		w.Header().Set(k, v)
	}
	if next.status != 0 {
		w.WriteHeader(next.status)
	}
	_, _ = io.WriteString(w, next.body)
}

func (f *fakeGitHub) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

type clock struct {
	now   time.Time
	slept []time.Duration
}

func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func ok(t *testing.T, name string) reply { return reply{body: fixture(t, name)} }

func testCtx(t *testing.T) context.Context {
	return render.WithEnv(t.Context(), "GH_TOKEN=prstate-test", "GITHUB_TOKEN=")
}

// echoGraphite answers every pull-request-info request with an open record
// naming each pull request's head branch the way the fixtures do, as Graphite
// does for a repository it tracks.
func echoGraphite(t *testing.T) *gtapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gtapi.PullRequestInfoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode graphite request: %v", err)
		}
		prs := make([]string, 0, len(req.PRNumbers))
		for _, n := range req.PRNumbers {
			prs = append(prs, fmt.Sprintf(`{"prNumber":%d,"state":"OPEN","baseRefName":"main","headRefName":"yasyf/gh-budget/pr-%d"}`, n, n))
		}
		_, _ = fmt.Fprintf(w, `{"result":{"status":"ok","prs":[%s]}}`, strings.Join(prs, ","))
	}))
	t.Cleanup(srv.Close)
	return gtapi.NewWithToken(srv.URL, "gt-stub-token")
}

func newStore(t *testing.T, dir string, c *clock, gt *gtapi.Client, replies ...reply) (*Store, *fakeGitHub) {
	t.Helper()
	if gt == nil {
		gt = echoGraphite(t)
	}
	fake := &fakeGitHub{t: t, replies: replies}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	src, err := NewGitHub(ghapi.New(ts.URL), gt, "yasyf/cc-context", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return c.now }
	store.sleep = c.sleep
	return store, fake
}

func TestReadSharesOnePollAcrossStoresWithinTheMinimumInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	first, firstGH := newStore(t, dir, c, nil, ok(t, "poll-189-190.json"))
	second, secondGH := newStore(t, dir, c, nil)

	if _, err := first.Read(testCtx(t), Want{PRs: []int{189, 190}}); err != nil {
		t.Fatalf("first read: %v", err)
	}
	c.now = c.now.Add(MinInterval - time.Second)
	st, err := second.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if firstGH.requests() != 1 || secondGH.requests() != 0 {
		t.Errorf("requests = %d, %d; want one poll shared by both stores", firstGH.requests(), secondGH.requests())
	}
	if st.PRs[190].HeadRefOid == "" {
		t.Errorf("#190 = %+v, want the first store's poll", st.PRs[190])
	}
}

func TestReadBatchesEveryLeasedPRIntoOneQuery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	store, gh := newStore(t, dir, c, nil, ok(t, "poll-190.json"), ok(t, "poll-189-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(MinInterval)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{189}}); err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 2 {
		t.Fatalf("requests = %d, want 2", gh.requests())
	}
	if got := gh.vars[1]; got["p0"] != float64(189) || got["p1"] != float64(190) {
		t.Errorf("second poll vars = %v, want #190's lease carried beside #189", got)
	}
}

func TestReadWaitsOutTheMinimumIntervalForANewPR(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-189-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(10 * time.Second)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{189}}); err != nil {
		t.Fatal(err)
	}
	if len(c.slept) != 1 || c.slept[0] != 20*time.Second || gh.requests() != 2 {
		t.Errorf("slept %v with %d requests, want 20s then one poll", c.slept, gh.requests())
	}
}

func TestSecondaryLimitProbesEveryTwoMinutesAndResumesOnTheFirst200(t *testing.T) {
	t.Parallel()
	limited := reply{status: http.StatusForbidden, header: map[string]string{"Retry-After": "3600"}, body: `{"message":"You have exceeded a secondary rate limit."}`}
	probed := reply{body: `{"data":{"viewer":{"login":"yasyf"},"rateLimit":{"remaining":4000,"resetAt":"2026-09-30T08:00:00Z"}}}`}
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, limited, limited, probed, ok(t, "poll-190.json"))
	want := Want{PRs: []int{190}}

	_, err := store.Read(testCtx(t), want)
	var refused *LimitedError
	if !errors.As(err, &refused) || !refused.ProbeAt.Equal(epoch.Add(ProbeEvery)) || !refused.Until.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("first read = %v, want a LimitedError probing in 2m, not at the hour Retry-After named", err)
	}

	c.now = epoch.Add(time.Minute)
	if _, err := store.Read(testCtx(t), want); !errors.As(err, &refused) || gh.requests() != 1 {
		t.Fatalf("read before the probe = %v after %d requests, want refused with no request", err, gh.requests())
	}

	c.now = epoch.Add(ProbeEvery)
	if _, err := store.Read(testCtx(t), want); !errors.As(err, &refused) || !refused.ProbeAt.Equal(c.now.Add(ProbeEvery)) || !refused.Since.Equal(epoch) {
		t.Fatalf("still-limited probe = %v, want the next probe 2m out and the backoff's start kept", err)
	}

	c.now = epoch.Add(2 * ProbeEvery)
	st, err := store.Read(testCtx(t), want)
	if err != nil {
		t.Fatalf("read after the probe cleared: %v", err)
	}
	if st.Backoff != nil || st.PRs[190].State != "OPEN" {
		t.Errorf("state = %+v, want the backoff cleared and #190 polled", st)
	}
	if gh.requests() != 4 {
		t.Fatalf("requests = %d, want poll, probe, probe, poll", gh.requests())
	}
	for _, i := range []int{1, 2} {
		if gh.queries[i] != probeQuery {
			t.Errorf("request %d = %q, want the cheap probe", i, gh.queries[i])
		}
	}
}

func TestShortRetryAfterIsHonoredBeforeTheProbe(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, _ := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "20"}})

	_, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	var refused *LimitedError
	if !errors.As(err, &refused) || !refused.ProbeAt.Equal(epoch.Add(20*time.Second)) {
		t.Fatalf("read = %v, want the probe at the 20s Retry-After", err)
	}
}

func TestSecondaryLimitWithoutRetryAfterWaitsAMinute(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, _ := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusForbidden, body: `{"message":"You have exceeded a secondary rate limit"}`})

	_, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	var refused *LimitedError
	if !errors.As(err, &refused) || !refused.ProbeAt.Equal(epoch.Add(time.Minute)) {
		t.Fatalf("read = %v, want the probe a minute out", err)
	}
}

func TestBackoffIsSharedAcrossStores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	first, _ := newStore(t, dir, c, nil, reply{status: http.StatusForbidden, header: map[string]string{"Retry-After": "600"}, body: `{"message":"secondary rate limit"}`})
	second, secondGH := newStore(t, dir, c, nil)

	if _, err := first.Read(testCtx(t), Want{PRs: []int{190}}); err == nil {
		t.Fatal("first read succeeded")
	}
	c.now = c.now.Add(time.Minute)
	_, err := second.Read(testCtx(t), Want{PRs: []int{189}})
	var refused *LimitedError
	if !errors.As(err, &refused) || secondGH.requests() != 0 {
		t.Errorf("second store read = %v after %d requests, want it refused by the first store's backoff", err, secondGH.requests())
	}
}

func TestQuotaBelowTheFloorWaitsForTheReset(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	low := strings.Replace(fixture(t, "poll-190.json"), `"remaining": 4990`, `"remaining": 50`, 1)
	store, gh := newStore(t, t.TempDir(), c, nil, reply{body: low})

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatalf("read that drained the quota: %v", err)
	}
	c.now = c.now.Add(MinInterval)
	_, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	var refused *LimitedError
	reset := time.Date(2026, 9, 30, 7, 40, 0, 0, time.UTC)
	if !errors.As(err, &refused) || !refused.ProbeAt.Equal(reset) || gh.requests() != 1 {
		t.Errorf("read = %v after %d requests, want no request before the quota's reset", err, gh.requests())
	}
}

func TestExpiredLeasesDropOutOfThePoll(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-189-190.json"), ok(t, "poll-189-190.json"), ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(10 * time.Minute)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(6 * time.Minute)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 3 {
		t.Fatalf("requests = %d, want 3", gh.requests())
	}
	if _, asked := gh.vars[2]["p1"]; asked || gh.vars[2]["p0"] != float64(190) {
		t.Errorf("third poll vars = %v, want #189 dropped once its lease lapsed", gh.vars[2])
	}
}

func TestSettledPRsAreNotPolledAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.save(State{
		PolledAt: epoch,
		PRs:      map[int]PR{188: {Number: 188, State: "MERGED", SquashOn: []string{"main"}, Graphite: &gtapi.PullRequestInfo{PRNumber: 188}, PolledAt: epoch}},
		Leases:   Leases{PRs: map[int]time.Time{188: epoch}},
	}); err != nil {
		t.Fatal(err)
	}
	store, gh := newStore(t, dir, c, nil)
	c.now = epoch.Add(time.Hour)

	st, err := store.Read(testCtx(t), Want{PRs: []int{188}})
	if err != nil || fmt.Sprint(st.PRs[188].SquashOn) != "[main]" || gh.requests() != 0 {
		t.Errorf("read = %v, %+v after %d requests; want the landed record served with no poll", err, st.PRs[188], gh.requests())
	}
}

func TestMissingPRIsDroppedFromTheBatchAndReported(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	notFound := reply{body: `{"data":null,"errors":[{"type":"NOT_FOUND","path":["repository","p1"],"message":"Could not resolve to a PullRequest with the number of 99999."}]}`}
	store, gh := newStore(t, t.TempDir(), c, nil, notFound, ok(t, "poll-190.json"))

	st, err := store.Read(testCtx(t), Want{PRs: []int{190, 99999}})
	var missing *MissingError
	if !errors.As(err, &missing) || fmt.Sprint(missing.PRs) != "[99999]" {
		t.Fatalf("read = %v, want #99999 reported missing", err)
	}
	if _, leased := st.Leases.PRs[99999]; leased || st.PRs[190].State != "OPEN" {
		t.Errorf("state = %+v, want #99999's lease dropped and #190 polled", st)
	}
	if _, asked := gh.vars[1]["p1"]; asked {
		t.Errorf("retry vars = %v, want #99999 left out", gh.vars[1])
	}
}

func TestLanePrefixReadsTheRecordsOfPRsItDiscovers(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-lane.json"), ok(t, "poll-190.json"), ok(t, "poll-lane.json"))

	st, err := store.Read(testCtx(t), Want{Prefixes: []string{"yasyf/gh-budget/"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Lanes["yasyf/gh-budget/"].PRs; fmt.Sprint(got) != "[190]" {
		t.Fatalf("lane = %v, want [190], the other branch's PR filtered out by prefix", got)
	}
	if gh.requests() != 2 || gh.vars[0]["l0"] != "yasyf/gh-budget/" || gh.vars[1]["p0"] != float64(190) {
		t.Fatalf("vars = %v, want the lane read, then its new PR's record in a follow-up", gh.vars)
	}
	if st.PRs[190].State != "OPEN" {
		t.Errorf("#190 = %+v, want its record read in the same Read", st.PRs[190])
	}
	c.now = c.now.Add(MinInterval)
	if _, err := store.Read(testCtx(t), Want{Prefixes: []string{"yasyf/gh-budget/"}}); err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 3 || gh.vars[2]["l0"] != "yasyf/gh-budget/" || gh.vars[2]["p0"] != float64(190) {
		t.Errorf("next poll vars = %v, want the lane and its leased PR in one query", gh.vars[2])
	}
}

func TestAProbeThatFindsTheQuotaDrainedWaitsForItsReset(t *testing.T) {
	t.Parallel()
	limited := reply{status: http.StatusForbidden, header: map[string]string{"Retry-After": "60"}, body: `{"message":"secondary rate limit"}`}
	drained := reply{body: `{"data":{"viewer":{"login":"yasyf"},"rateLimit":{"remaining":50,"resetAt":"2026-09-30T07:40:00Z"}}}`}
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, limited, drained)

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err == nil {
		t.Fatal("first read succeeded")
	}
	c.now = epoch.Add(time.Minute)
	_, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	var refused *LimitedError
	if !errors.As(err, &refused) || !refused.ProbeAt.Equal(time.Date(2026, 9, 30, 7, 40, 0, 0, time.UTC)) || gh.requests() != 2 {
		t.Errorf("read = %v after %d requests, want the probe's drained quota to hold polling until its reset", err, gh.requests())
	}
}

func TestAFailedPollStillSpacesTheNextOne(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusBadGateway}, ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err == nil {
		t.Fatal("read through a 502 succeeded")
	}
	c.now = epoch.Add(5 * time.Second)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	if len(c.slept) != 1 || c.slept[0] != 25*time.Second || gh.requests() != 2 {
		t.Errorf("slept %v over %d requests, want the retry held to 30s after the failed attempt", c.slept, gh.requests())
	}
}

func TestALaneIsFreshOnlyWithEveryMemberRead(t *testing.T) {
	t.Parallel()
	st := State{
		Lanes: map[string]Lane{"yasyf/gh-budget/": {PRs: []int{190}, PolledAt: epoch}},
		PRs:   map[int]PR{},
	}
	if st.fresh(Want{Prefixes: []string{"yasyf/gh-budget/"}}, epoch) {
		t.Error("a lane whose member was never read reads fresh")
	}
	st.PRs[190] = PR{Number: 190, State: "OPEN", Children: []int{}, PolledAt: epoch}
	if !st.fresh(Want{Prefixes: []string{"yasyf/gh-budget/"}}, epoch.Add(time.Second)) {
		t.Error("a lane with every member just read reads stale")
	}
}

func TestAnotherReadersPollReadsWhatALeasedLaneDiscovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	laneReader, _ := newStore(t, dir, c, nil, ok(t, "poll-lane.json"), ok(t, "poll-190.json"))
	if _, err := laneReader.Read(testCtx(t), Want{Prefixes: []string{"yasyf/gh-budget/"}}); err != nil {
		t.Fatal(err)
	}
	c.now = epoch.Add(MinInterval)
	other, gh := newStore(t, dir, c, nil, ok(t, "poll-lane-189.json"), ok(t, "poll-191.json"))
	st, err := other.Read(testCtx(t), Want{PRs: []int{189}})
	if err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 2 || gh.vars[1]["p0"] != float64(191) || st.PRs[191].State != "OPEN" {
		t.Errorf("vars %v, #191 %+v; want the lane's new #191 read in the same Read", gh.vars, st.PRs[191])
	}
}
