package prstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	rest    map[string]reply
	reads   []string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, r.URL.Path)
		next, ok := f.rest[r.URL.Path]
		if !ok {
			f.t.Errorf("unexpected REST read %s", r.URL.Path)
			next.status = http.StatusNotFound
		}
		if next.status != 0 {
			w.WriteHeader(next.status)
		}
		_, _ = io.WriteString(w, next.body)
		return
	}
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

func newStore(t *testing.T, dir string, c *clock, gt *gtapi.Client, replies ...reply) (*Store, *fakeGitHub) {
	t.Helper()
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

func TestAReadWithAMaxAgeAnswersFromAnOlderPollWithoutWaiting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	first, firstGH := newStore(t, dir, c, nil, ok(t, "poll-189-190.json"))
	second, secondGH := newStore(t, dir, c, nil)

	if _, err := first.Read(testCtx(t), Want{PRs: []int{189, 190}}); err != nil {
		t.Fatalf("first read: %v", err)
	}
	c.now = c.now.Add(3 * MinInterval)
	st, err := second.Read(testCtx(t), Want{PRs: []int{190}, MaxAge: 4 * MinInterval})
	if err != nil || firstGH.requests() != 1 || secondGH.requests() != 0 || len(c.slept) != 0 || st.PRs[190].HeadRefOid == "" {
		t.Errorf("read = %v with %d, %d requests, slept %v, #190 = %+v; want the older poll's record at once", err, firstGH.requests(), secondGH.requests(), c.slept, st.PRs[190])
	}
}

func TestAPollReadsOnlyWhatWasAskedForSinceTheLastPoll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	store, gh := newStore(t, dir, c, nil, ok(t, "poll-189-190.json"), ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{189, 190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(MinInterval)
	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 2 {
		t.Fatalf("requests = %d, want 2", gh.requests())
	}
	if _, also := gh.vars[1]["p1"]; also || gh.vars[1]["p0"] != float64(190) {
		t.Errorf("second poll vars = %v, want #190 alone, leaving #189's lease to a reader that asks again", gh.vars[1])
	}
	if _, leased := st.Leases.PRs[189]; !leased || !st.PRs[189].PolledAt.Equal(epoch) {
		t.Errorf("#189 lease %v, polled %s; want the lease and the record kept as polled at %s", st.Leases.PRs, st.PRs[189].PolledAt, epoch)
	}
}

func TestReadPollsAnUnreadPRAtOnceWithoutMovingTheSharedPoll(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-189-190.json"), ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(10 * time.Second)
	st, err := store.Read(testCtx(t), Want{PRs: []int{189}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.slept) != 0 || gh.requests() != 2 || st.PRs[189].State != "OPEN" {
		t.Fatalf("slept %v with %d requests, #189 = %+v; want #189 read at once", c.slept, gh.requests(), st.PRs[189])
	}
	if _, also := gh.vars[1]["p1"]; also || gh.vars[1]["p0"] != float64(189) || !st.AttemptedAt.Equal(epoch) {
		t.Errorf("vars %v, attempted %s; want #189 alone with the shared poll still due at %s", gh.vars[1], st.AttemptedAt, epoch.Add(MinInterval))
	}
	c.now = epoch.Add(MinInterval)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	if _, also := gh.vars[2]["p1"]; also || gh.requests() != 3 || gh.vars[2]["p0"] != float64(190) {
		t.Errorf("shared poll vars = %v, want #190 read on the original schedule without #189, still fresh from its own read", gh.vars[2])
	}
}

func TestReadPollsAnUnreadPRAloneWhenTheSharedPollIsDue(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-189-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(MinInterval + 5*time.Second)
	st, err := store.Read(testCtx(t), Want{PRs: []int{189}})
	if err != nil {
		t.Fatal(err)
	}
	if _, also := gh.vars[1]["p1"]; also || gh.requests() != 2 || gh.vars[1]["p0"] != float64(189) || !st.AttemptedAt.Equal(epoch) {
		t.Errorf("vars %v over %d requests, attempted %s; want #189 alone, leaving #190 to the next reader that needs it", gh.vars[1], gh.requests(), st.AttemptedAt)
	}
}

func TestReadPollsAPushedPRAtOnce(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(5 * time.Second)
	if err := store.Pushed(testCtx(t), map[int]string{190: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.slept) != 0 || gh.requests() != 2 || !st.PRs[190].PolledAt.Equal(c.now) {
		t.Errorf("slept %v with %d requests, polled %s; want the pushed PR read at once", c.slept, gh.requests(), st.PRs[190].PolledAt)
	}
}

func TestReadWaitsOutTheMinimumIntervalForANewLane(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-lane.json"), ok(t, "poll-190.json"))

	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(10 * time.Second)
	if _, err := store.Read(testCtx(t), Want{Prefixes: []string{"yasyf/gh-budget/"}}); err != nil {
		t.Fatal(err)
	}
	if len(c.slept) != 1 || c.slept[0] != 20*time.Second || gh.requests() != 3 || gh.vars[2]["p0"] != float64(190) {
		t.Errorf("slept %v with %d requests, vars %v; want 20s, then the lane's poll and a read of the member it found", c.slept, gh.requests(), gh.vars)
	}
}

func TestPeekAnswersFromTheCacheWhereReadWaitsOutTheInterval(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		peek     bool
		replies  int
		slept    string
		requests int
	}{
		{name: "read", replies: 1, slept: "[15s]", requests: 1},
		{name: "peek", peek: true, slept: "[]", requests: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
			if err != nil {
				t.Fatal(err)
			}
			cached := State{AttemptedAt: epoch.Add(20 * time.Second), PolledAt: epoch, PRs: map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch}}}
			if err := seed.save(cached); err != nil {
				t.Fatal(err)
			}
			replies := slices.Repeat([]reply{ok(t, "poll-190.json")}, tt.replies)
			c := &clock{now: epoch.Add(MinInterval + 5*time.Second)}
			store, gh := newStore(t, dir, c, nil, replies...)
			read := store.Read
			if tt.peek {
				read = store.Peek
			}
			st, err := read(testCtx(t), Want{PRs: []int{190}})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(c.slept); got != tt.slept || gh.requests() != tt.requests || st.PRs[190].State != "OPEN" {
				t.Errorf("slept %s with %d requests, #190 = %+v; want slept %s with %d requests", got, gh.requests(), st.PRs[190], tt.slept, tt.requests)
			}
		})
	}
}

func TestReadersWaitingOutTheIntervalShareTheNextPoll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	first, firstGH := newStore(t, dir, c, nil, ok(t, "poll-190.json"), ok(t, "poll-lane.json"))
	second, secondGH := newStore(t, dir, c, nil)
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()

	if _, err := first.Read(ctx, Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = epoch.Add(10 * time.Second)
	second.sleep = func(ctx context.Context, d time.Duration) error {
		c.now = c.now.Add(d)
		_, err := first.Read(ctx, Want{PRs: []int{190}})
		return err
	}
	st, err := second.Read(ctx, Want{Prefixes: []string{"yasyf/gh-budget/"}})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if firstGH.requests() != 2 || secondGH.requests() != 0 || fmt.Sprint(st.Lanes["yasyf/gh-budget/"].PRs) != "[190]" {
		t.Fatalf("requests = %d, %d; lanes = %v; want the sleeping reader's lease read by the other store's poll", firstGH.requests(), secondGH.requests(), st.Lanes)
	}
	if got := firstGH.vars[1]; got["l0"] != "yasyf/gh-budget/" || got["p0"] != float64(190) {
		t.Errorf("second poll vars = %v, want the lane's lease beside #190", got)
	}
}

func TestAReaderThatWaitedReturnsTheFailureOfThePollItWaitedFor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	first, firstGH := newStore(t, dir, c, nil, ok(t, "poll-190.json"), reply{status: http.StatusBadGateway})
	firstGH.rest = map[string]reply{"/repos/yasyf/cc-context/pulls/190": {status: http.StatusServiceUnavailable}}
	second, secondGH := newStore(t, dir, c, nil)
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()

	if _, err := first.Read(ctx, Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	c.now = epoch.Add(10 * time.Second)
	second.sleep = func(ctx context.Context, d time.Duration) error {
		c.now = c.now.Add(d)
		if _, err := first.Read(ctx, Want{PRs: []int{190}}); err == nil {
			t.Error("read through a 502 succeeded")
		}
		return nil
	}
	_, err := second.Read(ctx, Want{Prefixes: []string{"yasyf/gh-budget/"}})
	if err == nil || !strings.Contains(err.Error(), "the poll at 2026-09-30T07:00:30Z failed") || !strings.Contains(err.Error(), "502") {
		t.Errorf("read = %v, want the failure of the poll it waited for", err)
	}
	if secondGH.requests() != 0 {
		t.Errorf("requests = %d, want none before the interval ends", secondGH.requests())
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
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-189-190.json"), ok(t, "poll-190.json"), ok(t, "poll-190.json"))

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
	gh.rest = map[string]reply{"/repos/yasyf/cc-context/pulls/99999": {status: http.StatusNotFound, body: `{"message":"Not Found"}`}}

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

// TestAPRGraphQLCannotResolveIsReadOverRESTBeforeItIsDropped is a landing
// pass over 88 pull requests that failed on 25 open ones the app's GraphQL
// answered NOT_FOUND for: the poll took the answer as the pull request being
// gone and dropped its record.
func TestAPRGraphQLCannotResolveIsReadOverRESTBeforeItIsDropped(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	notFound := reply{body: `{"data":null,"errors":[` +
		`{"type":"NOT_FOUND","path":["repository","p0"],"message":"Could not resolve to a PullRequest with the number of 190."},` +
		`{"type":"NOT_FOUND","path":["repository","p1"],"message":"Could not resolve to a PullRequest with the number of 99999."}]}`}
	store, gh := newStore(t, t.TempDir(), c, nil, notFound, ok(t, "poll-190.json"))
	gh.rest = rest190()
	gh.rest["/repos/yasyf/cc-context/pulls/99999"] = reply{status: http.StatusNotFound, body: `{"message":"Not Found"}`}

	st, err := store.Read(testCtx(t), Want{PRs: []int{190, 99999}})
	var missing *MissingError
	if !errors.As(err, &missing) || fmt.Sprint(missing.PRs) != "[99999]" {
		t.Fatalf("read = %v, want only #99999, which REST also has no record of, reported missing", err)
	}
	if pr := st.PRs[190]; pr.Source != SourceAppRefused || pr.State != "OPEN" || pr.HeadRefOid != restHead {
		t.Errorf("#190 = %+v, want the user's REST record of the open pull request GraphQL could not resolve", pr)
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

const restHead = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"

func rest190() map[string]reply {
	return map[string]reply{
		"/repos/yasyf/cc-context/pulls/190": {body: `{"number":190,"state":"open","merged":false,"title":"title of #190","created_at":"2026-09-29T20:00:00Z",` +
			`"draft":false,"changed_files":3,"mergeable":false,"mergeable_state":"dirty","merge_commit_sha":"d4d4","user":{"login":"yasyf"},` +
			`"base":{"ref":"main"},"head":{"ref":"yasyf/gh-budget/pr-190","sha":"` + restHead + `"},"labels":[{"name":"merge"}]}`},
		"/repos/yasyf/cc-context/pulls/190/reviews": {body: `[{"state":"CHANGES_REQUESTED","user":{"login":"ann"}},{"state":"COMMENTED","user":{"login":"bob[bot]"}},` +
			`{"state":"APPROVED","user":{"login":"bob[bot]"}},{"state":"APPROVED","user":{"login":"ann"}},{"state":"APPROVED","user":{"login":"cat"}},{"state":"DISMISSED","user":{"login":"cat"}}]`},
		"/repos/yasyf/cc-context/commits/" + restHead + "/check-runs": {body: `{"total_count":2,"check_runs":[{"name":"test","status":"completed","conclusion":"success"},` +
			`{"name":"lint","status":"in_progress","conclusion":null}]}`},
		"/repos/yasyf/cc-context/commits/" + restHead + "/status": {body: `{"state":"success","total_count":1,"statuses":[{"context":"buildkite/test","state":"success"}]}`},
		"/repos/yasyf/cc-context/issues/190/comments":             {body: `[{"body":"looks good"},{"body":"### Merge activity\n\n* queued"}]`},
	}
}

func TestAFailedPollReadsThroughRESTAndSpacesTheNextPoll(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusBadGateway}, ok(t, "poll-190.json"))
	gh.rest = rest190()

	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil {
		t.Fatalf("read through a 502: %v", err)
	}
	pr := st.PRs[190]
	if pr.Source != SourceREST || pr.State != "OPEN" || pr.HeadRefOid != restHead || pr.Mergeable != "CONFLICTING" || pr.MergeStateStatus != "DIRTY" ||
		pr.MergeCommit != "" || pr.Author != "yasyf" || !pr.QueueLabelled() || !strings.HasPrefix(pr.Activity, ActivityHeading) {
		t.Errorf("#190 = %+v, want the REST record of the open pull request", pr)
	}
	if fmt.Sprint(pr.Reviews) != "[{ann APPROVED} {bob APPROVED}]" {
		t.Errorf("reviews = %v, want each reviewer's latest opinion with the dismissed one dropped", pr.Reviews)
	}
	if pr.Status != "SUCCESS" || pr.Rollup == nil || pr.Rollup.State != "PENDING" || len(pr.Rollup.Contexts.Nodes) != 3 ||
		pr.Rollup.Contexts.Nodes[1] != (Context{Typename: "CheckRun", Name: "lint", Status: "IN_PROGRESS"}) {
		t.Errorf("status %q, rollup %+v; want the check runs and statuses rolled up pending", pr.Status, pr.Rollup)
	}
	if st.Failure == nil || gh.requests() != 1 {
		t.Errorf("failure %v after %d polls, want the failed poll recorded", st.Failure, gh.requests())
	}

	c.now = epoch.Add(5 * time.Second)
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil || gh.requests() != 1 || len(gh.reads) != 5 {
		t.Fatalf("read inside the interval = %v after %d polls and %d REST reads, want the REST record served", err, gh.requests(), len(gh.reads))
	}
	c.now = epoch.Add(MinInterval)
	st, err = store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil || gh.requests() != 2 || st.PRs[190].Source != "" || st.Failure != nil {
		t.Errorf("read after the interval = %v after %d polls, #190 from %q; want GraphQL polled again", err, gh.requests(), st.PRs[190].Source)
	}
}

func TestAReaderFindingAFailedPollReadsThroughRESTWithoutPolling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	failed := State{
		AttemptedAt: epoch,
		PolledAt:    epoch.Add(-MinInterval),
		Failure:     &Failure{At: epoch, Reason: "ghapi: POST /graphql: 504"},
		PRs:         map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch.Add(-MinInterval)}},
	}
	if err := seed.save(failed); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: epoch.Add(5 * time.Second)}
	store, gh := newStore(t, dir, c, nil)
	gh.rest = rest190()

	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil || gh.requests() != 0 || len(c.slept) != 0 || st.PRs[190].Source != SourceREST {
		t.Errorf("read = %v after %d polls, slept %v, #190 from %q; want REST at once while the failure stands", err, gh.requests(), c.slept, st.PRs[190].Source)
	}
}

func TestRESTReviewDecisionCarriesTheLastDecisionOfTheSameHead(t *testing.T) {
	t.Parallel()
	required := PR{HeadRefOid: restHead, ReviewDecision: "REVIEW_REQUIRED"}
	tests := []struct {
		name    string
		reviews []Review
		last    PR
		want    string
	}{
		{"same head", []Review{{Author: "ann", State: "APPROVED"}}, required, "REVIEW_REQUIRED"},
		{"new head", nil, PR{HeadRefOid: "old", ReviewDecision: "APPROVED"}, ""},
		{"changes requested now", []Review{{Author: "ann", State: "CHANGES_REQUESTED"}}, required, "CHANGES_REQUESTED"},
		{"changes requested before", []Review{{Author: "ann", State: "APPROVED"}}, PR{HeadRefOid: restHead, ReviewDecision: "CHANGES_REQUESTED"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := restReviewDecision(tt.reviews, tt.last, restHead); got != tt.want {
				t.Errorf("decision = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAFailedRESTFallbackNamesBothFailures(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, reply{status: http.StatusBadGateway})
	gh.rest = map[string]reply{"/repos/yasyf/cc-context/pulls/190": {status: http.StatusServiceUnavailable}}

	_, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "the REST fallback failed too") || !strings.Contains(err.Error(), "503") {
		t.Errorf("read = %v, want the poll's failure and the fallback's", err)
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
	st.PRs[190] = PR{Number: 190, State: "OPEN", PolledAt: epoch}
	if !st.fresh(Want{Prefixes: []string{"yasyf/gh-budget/"}}, epoch.Add(time.Second)) {
		t.Error("a lane with every member just read reads stale")
	}
}

func TestAnotherReadersPollReadsWhatALaneAskedForSinceTheLastPollDiscovers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &clock{now: epoch}
	laneReader, laneGH := newStore(t, dir, c, nil, ok(t, "poll-lane.json"), ok(t, "poll-190.json"))
	lane := Want{Prefixes: []string{"yasyf/gh-budget/"}}
	if _, err := laneReader.Read(testCtx(t), lane); err != nil {
		t.Fatal(err)
	}
	c.now = epoch.Add(10 * time.Second)
	if _, err := laneReader.Read(testCtx(t), lane); err != nil || laneGH.requests() != 2 {
		t.Fatalf("fresh lane read = %v after %d requests, want the cached lane with its lease renewed", err, laneGH.requests())
	}
	c.now = epoch.Add(MinInterval)
	other, gh := newStore(t, dir, c, nil, ok(t, "poll-lane-189.json"), ok(t, "poll-191.json"))
	st, err := other.Read(testCtx(t), Want{PRs: []int{189, 190}})
	if err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 2 || gh.vars[1]["p0"] != float64(191) || st.PRs[191].State != "OPEN" {
		t.Errorf("vars %v, #191 %+v; want the lane's new #191 read in the same Read", gh.vars, st.PRs[191])
	}
}

func TestAPushedHeadIsKeptBesideGitHubsUntilGitHubShowsIt(t *testing.T) {
	t.Parallel()
	const old, pushed = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	c := &clock{now: epoch}
	caughtUp := reply{body: strings.ReplaceAll(fixture(t, "poll-190.json"), old, pushed)}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-190.json"), ok(t, "poll-190.json"), caughtUp)
	want := Want{PRs: []int{190}}

	if _, err := store.Read(testCtx(t), want); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(time.Second)
	if err := store.Pushed(testCtx(t), map[int]string{190: pushed}); err != nil {
		t.Fatal(err)
	}
	for polls := 2; polls <= 3; polls++ {
		st, err := store.Read(testCtx(t), want)
		if err != nil {
			t.Fatal(err)
		}
		if pr := st.PRs[190]; pr.HeadRefOid != old || pr.PushedHead != pushed || !pr.PushedAt.Equal(epoch.Add(time.Second)) || gh.requests() != polls {
			t.Errorf("lagging poll %d = head %s pushed %s at %s, want GitHub's head with the push kept beside it", polls, pr.HeadRefOid, pr.PushedHead, pr.PushedAt)
		}
		c.now = c.now.Add(MinInterval)
	}

	st, err := store.Read(testCtx(t), want)
	if err != nil {
		t.Fatal(err)
	}
	if pr := st.PRs[190]; pr.HeadRefOid != pushed || pr.PushedHead != "" || !pr.PushedAt.IsZero() || gh.requests() != 4 {
		t.Errorf("once GitHub shows the push = head %s pushed %s with %d requests, want the push marker cleared", pr.HeadRefOid, pr.PushedHead, gh.requests())
	}
}

func TestAPushedHeadGitHubNeverShowsExpires(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, _ := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"), ok(t, "poll-190.json"))
	want := Want{PRs: []int{190}}

	if err := store.Pushed(testCtx(t), map[int]string{190: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(testCtx(t), want)
	if err != nil {
		t.Fatal(err)
	}
	if pr := st.PRs[190]; pr.HeadRefOid != "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1" || pr.PushedHead != "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2" || pr.State != "OPEN" {
		t.Errorf("first read = %+v, want GitHub's record with the push beside it", pr)
	}
	c.now = c.now.Add(pushLag)
	if st, err = store.Read(testCtx(t), want); err != nil {
		t.Fatal(err)
	}
	if pr := st.PRs[190]; pr.PushedHead != "" || !pr.PushedAt.IsZero() {
		t.Errorf("read past the push window = %+v, want the push marker dropped", pr)
	}
}

func TestAPushRenewsTheLeaseSoAnotherPollKeepsIt(t *testing.T) {
	t.Parallel()
	c := &clock{now: epoch}
	store, gh := newStore(t, t.TempDir(), c, nil, ok(t, "poll-190.json"))
	gh.rest = map[string]reply{"/repos/yasyf/cc-context/pulls/191": {status: http.StatusNotFound, body: `{"message":"Not Found"}`}}
	if err := store.Pushed(testCtx(t), map[int]string{191: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(testCtx(t), Want{PRs: []int{190}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(slices.Collect(maps.Values(gh.vars[0])), any(float64(191))) {
		t.Errorf("poll vars %v, want the pushed PR leased into the poll", gh.vars[0])
	}
	if (State{PRs: map[int]PR{191: {Number: 191, PushedHead: "b2"}}}).Covers(Want{PRs: []int{191}}) {
		t.Error("a push placeholder no poll filled in covers a stale read")
	}
}

func TestCoversNeedsEveryRecordAndLane(t *testing.T) {
	t.Parallel()
	st := State{PRs: map[int]PR{1: {PolledAt: epoch}, 2: {PolledAt: epoch}}, Lanes: map[string]Lane{"a/": {PRs: []int{1}}, "b/": {PRs: []int{3}}}}
	for _, tt := range []struct {
		want Want
		ok   bool
	}{
		{Want{PRs: []int{1, 2}}, true},
		{Want{PRs: []int{1, 3}}, false},
		{Want{Prefixes: []string{"a/"}}, true},
		{Want{Prefixes: []string{"b/"}}, false},
		{Want{Prefixes: []string{"c/"}}, false},
	} {
		if got := st.Covers(tt.want); got != tt.ok {
			t.Errorf("Covers(%+v) = %t, want %t", tt.want, got, tt.ok)
		}
	}
}

func blockedPoller(t *testing.T, dir string, now time.Time) (*Store, <-chan struct{}, func()) {
	t.Helper()
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	body := fixture(t, "poll-190.json")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-release
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(unblock)
	src, err := NewGitHub(ghapi.New(ts.URL), nil, "yasyf/cc-context", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	return store, inFlight, unblock
}

func awaitInFlight(ctx context.Context, t *testing.T, inFlight <-chan struct{}) {
	t.Helper()
	select {
	case <-inFlight:
	case <-ctx.Done():
		t.Fatal("the poll never reached GitHub")
	}
}

func TestAReaderIsNotBlockedWhileAPollIsInFlight(t *testing.T) {
	t.Parallel()
	const pushed = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.save(State{AttemptedAt: epoch, PolledAt: epoch, PRs: map[int]PR{189: {Number: 189, State: "OPEN", PolledAt: epoch}}}); err != nil {
		t.Fatal(err)
	}
	poller, inFlight, release := blockedPoller(t, dir, epoch.Add(time.Second))
	polled := make(chan State, 1)
	go func() {
		st, err := poller.Read(ctx, Want{PRs: []int{190}})
		if err != nil {
			t.Errorf("poll: %v", err)
		}
		polled <- st
	}()
	awaitInFlight(ctx, t, inFlight)

	reader, gh := newStore(t, dir, &clock{now: epoch.Add(time.Second)}, nil)
	st, err := reader.Read(ctx, Want{PRs: []int{189}})
	if err != nil || st.PRs[189].State != "OPEN" || gh.requests() != 0 {
		t.Fatalf("read during the poll = %v, %+v after %d requests; want the cached record at once", err, st.PRs[189], gh.requests())
	}
	if err := reader.Pushed(ctx, map[int]string{190: pushed}); err != nil {
		t.Fatalf("push recorded during the poll: %v", err)
	}
	release()
	if pr := (<-polled).PRs[190]; pr.State != "OPEN" || pr.PushedHead != pushed {
		t.Errorf("#190 after the poll = %+v, want the poll merged beside the push recorded while it ran", pr)
	}
}

func TestAReaderSharesAPollAnotherStoreHasInFlight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.save(State{AttemptedAt: epoch, PolledAt: epoch, PRs: map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch}}}); err != nil {
		t.Fatal(err)
	}
	poller, inFlight, release := blockedPoller(t, dir, epoch.Add(MinInterval))
	polled := make(chan error, 1)
	go func() {
		_, err := poller.Read(ctx, Want{PRs: []int{190}})
		polled <- err
	}()
	awaitInFlight(ctx, t, inFlight)

	c := &clock{now: epoch.Add(MinInterval + time.Second)}
	reader, gh := newStore(t, dir, c, nil)
	reader.sleep = func(_ context.Context, d time.Duration) error {
		c.slept = append(c.slept, d)
		release()
		return <-polled
	}
	st, err := reader.Read(ctx, Want{PRs: []int{190}})
	if err != nil || gh.requests() != 0 || len(c.slept) != 1 || !st.PRs[190].PolledAt.Equal(epoch.Add(MinInterval)) {
		t.Errorf("read = %v after %d requests, slept %v, #190 polled %s; want the in-flight poll's result with no poll of its own",
			err, gh.requests(), c.slept, st.PRs[190].PolledAt)
	}
}

func TestAReaderTakesTheInFlightPollThoughItLandsPastTheInterval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(testCtx(t), 10*time.Second)
	defer cancel()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.save(State{AttemptedAt: epoch, PolledAt: epoch, PRs: map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch}}}); err != nil {
		t.Fatal(err)
	}
	poller, inFlight, release := blockedPoller(t, dir, epoch.Add(MinInterval))
	polled := make(chan error, 1)
	go func() {
		_, err := poller.Read(ctx, Want{PRs: []int{190}})
		polled <- err
	}()
	awaitInFlight(ctx, t, inFlight)

	c := &clock{now: epoch.Add(MinInterval + time.Second)}
	reader, gh := newStore(t, dir, c, nil)
	reader.sleep = func(_ context.Context, d time.Duration) error {
		c.slept = append(c.slept, d)
		c.now = epoch.Add(3 * MinInterval)
		release()
		return <-polled
	}
	st, err := reader.Read(ctx, Want{PRs: []int{190}})
	if err != nil || gh.requests() != 0 || len(c.slept) != 1 || !st.PRs[190].PolledAt.Equal(epoch.Add(MinInterval)) {
		t.Errorf("read = %v after %d requests, slept %v, #190 polled %s; want the in-flight poll's result though it landed %s after its claim",
			err, gh.requests(), c.slept, st.PRs[190].PolledAt, 2*MinInterval)
	}
}

func TestAReaderNoPollAnswersWithinTheBudgetReadsThroughREST(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	inFlight := State{
		AttemptedAt: epoch,
		PolledAt:    epoch.Add(-MinInterval),
		PRs:         map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch.Add(-MinInterval)}},
	}
	if err := seed.save(inFlight); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: epoch.Add(5 * time.Second)}
	store, gh := newStore(t, dir, c, nil)
	gh.rest = rest190()
	store.budget = time.Millisecond
	store.sleep = func(ctx context.Context, d time.Duration) error {
		c.slept = append(c.slept, d)
		<-ctx.Done()
		return ctx.Err()
	}

	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil || gh.requests() != 0 || fmt.Sprint(c.slept) != "[25s]" || st.PRs[190].Source != SourceREST || st.Failure != nil {
		t.Errorf("read = %v after %d polls, slept %v, #190 from %q, failure %v; want REST once the budget ran out waiting on the poll in flight",
			err, gh.requests(), c.slept, st.PRs[190].Source, st.Failure)
	}
}

func TestAReaderThatSpentItsBudgetWaitingStillLandsThePollItClaims(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	last := State{
		AttemptedAt: epoch,
		PolledAt:    epoch.Add(-MinInterval),
		PRs:         map[int]PR{190: {Number: 190, State: "OPEN", PolledAt: epoch.Add(-MinInterval)}},
	}
	if err := seed.save(last); err != nil {
		t.Fatal(err)
	}
	c := &clock{now: epoch.Add(5 * time.Second)}
	store, gh := newStore(t, dir, c, nil, ok(t, "poll-190.json"))
	store.budget = 50 * time.Millisecond
	store.sleep = func(ctx context.Context, d time.Duration) error {
		c.slept = append(c.slept, d)
		c.now = c.now.Add(d)
		<-ctx.Done()
		return nil
	}

	st, err := store.Read(testCtx(t), Want{PRs: []int{190}})
	if err != nil || gh.requests() != 1 || st.PRs[190].Source == SourceREST || !st.PolledAt.Equal(epoch.Add(MinInterval)) || st.Failure != nil {
		t.Errorf("read = %v after %d polls, #190 from %q, polled %s, failure %v; want the claimed poll to land though the reader's budget ran out before it began",
			err, gh.requests(), st.PRs[190].Source, st.PolledAt, st.Failure)
	}
}

func inFlightOver(t *testing.T, count int) (string, Want) {
	t.Helper()
	dir := t.TempDir()
	seed, err := Open(dir, &GitHub{owner: "yasyf", name: "cc-context"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{AttemptedAt: epoch, PolledAt: epoch.Add(-MinInterval), PRs: map[int]PR{}}
	var want Want
	for n := 190; n < 190+count; n++ {
		want.PRs = append(want.PRs, n)
		st.PRs[n] = PR{Number: n, State: "OPEN", PolledAt: epoch.Add(-MinInterval)}
	}
	if err := seed.save(st); err != nil {
		t.Fatal(err)
	}
	return dir, want
}

func TestAReaderWaitsOneBudgetPerPageOfPullRequests(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ prs, pages int }{{1, 1}, {chunkSize, 1}, {chunkSize + 1, 2}, {97, 3}} {
		t.Run(fmt.Sprintf("%d pull requests", c.prs), func(t *testing.T) {
			t.Parallel()
			dir, want := inFlightOver(t, c.prs)
			store, _ := newStore(t, dir, &clock{now: epoch.Add(5 * time.Second)}, nil)
			store.budget = time.Minute
			var budget time.Duration
			store.sleep = func(ctx context.Context, _ time.Duration) error {
				deadline, _ := ctx.Deadline()
				budget = time.Until(deadline)
				return context.Canceled
			}

			_, _ = store.Read(testCtx(t), want)
			if budget <= time.Duration(c.pages-1)*time.Minute || budget > time.Duration(c.pages)*time.Minute {
				t.Errorf("a read of %d pull requests waits %s on the poll; want %d one-minute budgets, one per page of %d", c.prs, budget, c.pages, chunkSize)
			}
		})
	}
}
