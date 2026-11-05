package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/prstate"
)

const (
	watchHeadA  = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	watchHeadB  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	watchSquash = "815d915c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c"
)

func watchEvents(t *testing.T, events []prWatchEvent) []string {
	t.Helper()
	lines := make([]string, 0, len(events))
	for _, e := range events {
		lines = append(lines, e.String())
	}
	return lines
}

func TestPRWatchStep(t *testing.T) {
	t.Parallel()
	open := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "MERGEABLE"}
	with := func(base prWatchSnapshot, edit func(*prWatchSnapshot)) prWatchSnapshot {
		edit(&base)
		base.Failing = append([]string(nil), base.Failing...)
		return base
	}
	queued := with(open, func(s *prWatchSnapshot) { s.Queued, s.Green, s.Approved = true, true, true })
	tests := []struct {
		name       string
		prev, next prWatchSnapshot
		want       []string
	}{
		{
			"the first poll reports the standing state",
			prWatchSnapshot{},
			with(open, func(s *prWatchSnapshot) { s.Queued, s.Green, s.Approved, s.Mergeable = true, true, true, "CONFLICTING" }),
			[]string{"#7 queued", "#7 conflicting", "#7 green", "#7 approved"},
		},
		{
			"queued then ejected on a merge conflict",
			queued,
			with(queued, func(s *prWatchSnapshot) {
				s.Queued, s.Evicted, s.Mergeable = false, "it had merge conflicts", "CONFLICTING"
			}),
			[]string{"#7 ejected (it had merge conflicts)", "#7 conflicting"},
		},
		{
			"queued then landed is a landing, never an ejection",
			queued,
			with(queued, func(s *prWatchSnapshot) { s.State, s.Queued, s.Squash = "CLOSED", false, watchSquash }),
			[]string{"#7 landed 815d915c0"},
		},
		{
			"closed with no squash on the trunk",
			queued,
			with(queued, func(s *prWatchSnapshot) { s.State, s.Queued = "CLOSED", false }),
			[]string{"#7 closed-without-squash"},
		},
		{
			"a new head re-reports a check that fails again",
			with(open, func(s *prWatchSnapshot) { s.Failing = []string{"buildkite/tests"} }),
			with(open, func(s *prWatchSnapshot) { s.Head, s.Failing = watchHeadB, []string{"buildkite/tests"} }),
			[]string{"#7 new-head b2b2b2b2b", "#7 red buildkite/tests"},
		},
		{
			"only a check that newly failed is red",
			with(open, func(s *prWatchSnapshot) { s.Failing = []string{"a"} }),
			with(open, func(s *prWatchSnapshot) { s.Failing = []string{"a", "b"} }),
			[]string{"#7 red b"},
		},
		{
			"green after red",
			with(open, func(s *prWatchSnapshot) { s.Failing = []string{"a"} }),
			with(open, func(s *prWatchSnapshot) { s.Green = true }),
			[]string{"#7 green"},
		},
		{
			"a dismissed approval",
			with(open, func(s *prWatchSnapshot) { s.Approved = true }),
			open,
			[]string{"#7 approval-dismissed"},
		},
		{
			"an unchanged pull request says nothing",
			queued,
			queued,
			nil,
		},
		{
			"mergeability GitHub has not recomputed keeps the last verdict",
			with(open, func(s *prWatchSnapshot) { s.Mergeable = "CONFLICTING" }),
			with(open, func(s *prWatchSnapshot) { s.Mergeable = "UNKNOWN" }),
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, events := prWatchStep(7, tt.prev, tt.next)
			if got := watchEvents(t, events); !slices.Equal(got, tt.want) {
				t.Errorf("events = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRWatchStepCarriesMergeabilityAcrossUnknown(t *testing.T) {
	t.Parallel()
	prev := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "CONFLICTING"}
	settled, _ := prWatchStep(7, prev, prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "UNKNOWN"})
	if settled.Mergeable != "CONFLICTING" {
		t.Fatalf("settled mergeable = %q, want CONFLICTING", settled.Mergeable)
	}
	settled, _ = prWatchStep(7, settled, prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "MERGEABLE"})
	_, events := prWatchStep(7, settled, prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "CONFLICTING"})
	if got := watchEvents(t, events); !reflect.DeepEqual(got, []string{"#7 conflicting"}) {
		t.Fatalf("events = %q, want a fresh conflict", got)
	}
}

type scriptedPoller struct {
	ticks []prWatchTick
	errs  []error
	asked [][]int
}

func (p *scriptedPoller) poll(_ context.Context, numbers []int, _ map[int]prWatchSnapshot) (prWatchTick, error) {
	p.asked = append(p.asked, append([]int(nil), numbers...))
	i := len(p.asked) - 1
	if i < len(p.errs) && p.errs[i] != nil {
		return prWatchTick{}, p.errs[i]
	}
	return p.ticks[i], nil
}

func watchTick(snaps map[int]prWatchSnapshot) prWatchTick {
	return prWatchTick{snapshots: snaps}
}

func newTestWatchRun(poller prWatchPoller, o prWatchOpts, out io.Writer, slept *[]time.Duration) prWatchRun {
	now := time.Date(2026, 9, 30, 6, 0, 0, 0, time.UTC)
	return prWatchRun{
		poller: poller,
		opts:   o,
		out:    out,
		warn:   io.Discard,
		now:    func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
	}
}

func TestPRWatchRunReportsAnEjectionThenExitsWhenEveryPRLanded(t *testing.T) {
	t.Parallel()
	queued := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "MERGEABLE", Queued: true}
	ejected := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "CONFLICTING", Evicted: "it had merge conflicts"}
	landed := prWatchSnapshot{State: "CLOSED", Head: watchHeadB, Squash: watchSquash}
	poller := &scriptedPoller{ticks: []prWatchTick{
		watchTick(map[int]prWatchSnapshot{27949: queued}),
		watchTick(map[int]prWatchSnapshot{27949: queued}),
		watchTick(map[int]prWatchSnapshot{27949: ejected}),
		watchTick(map[int]prWatchSnapshot{27949: landed}),
	}}
	var out bytes.Buffer
	var slept []time.Duration
	state := filepath.Join(t.TempDir(), "watch.json")
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded, state: state}, &out, &slept)

	if err := run.watch(context.Background(), []int{27949}, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("watch: %v", err)
	}

	want := "#27949 queued\n" +
		"#27949 ejected (it had merge conflicts)\n#27949 conflicting\n" +
		"#27949 new-head b2b2b2b2b\n#27949 landed 815d915c0\n"
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
	if len(slept) != 3 || slept[0] != time.Minute {
		t.Errorf("slept = %v, want three one-minute sleeps", slept)
	}
	saved, err := loadPRWatchState(state)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !saved[27949].landed() {
		t.Errorf("saved state = %+v, want the landing", saved[27949])
	}
}

func TestPRWatchRunResumesFromStateWithoutReplaying(t *testing.T) {
	t.Parallel()
	queued := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Queued: true, Green: true}
	poller := &scriptedPoller{ticks: []prWatchTick{watchTick(map[int]prWatchSnapshot{27949: queued})}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilNever, once: true}, &out, &slept)

	if err := run.watch(context.Background(), []int{27949}, map[int]prWatchSnapshot{27949: queued}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing on an unchanged resume", out.String())
	}
}

func TestPRWatchRunUntilLandedFailsOnAClosedPR(t *testing.T) {
	t.Parallel()
	poller := &scriptedPoller{ticks: []prWatchTick{
		watchTick(map[int]prWatchSnapshot{1: {State: "CLOSED", Head: watchHeadA}, 2: {State: "CLOSED", Head: watchHeadA, Squash: watchSquash}}),
	}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded}, &out, &slept)

	err := run.watch(context.Background(), []int{1, 2}, map[int]prWatchSnapshot{})
	if err == nil || !strings.Contains(err.Error(), "#1") {
		t.Fatalf("err = %v, want #1 named as closed without landing", err)
	}

	run.opts.until = prWatchUntilClosed
	poller.asked = nil
	if err := run.watch(context.Background(), []int{1, 2}, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("until closed: %v", err)
	}
}

func TestPRWatchRunSleepsToTheNextProbeWhileRateLimited(t *testing.T) {
	t.Parallel()
	probeAt := time.Date(2026, 9, 30, 6, 2, 0, 0, time.UTC)
	limited := &prstate.LimitedError{Backoff: prstate.Backoff{Since: probeAt.Add(-2 * time.Minute), ProbeAt: probeAt, Reason: "rate limit"}}
	done := watchTick(map[int]prWatchSnapshot{5: {State: "CLOSED", Head: watchHeadA, Squash: watchSquash}})
	poller := &scriptedPoller{ticks: []prWatchTick{{}, done}, errs: []error{limited}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded}, &out, &slept)

	if err := run.watch(context.Background(), []int{5}, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if !strings.Contains(out.String(), "rate-limited until 2026-09-30T06:02:00Z\n") {
		t.Errorf("output = %q, want one rate-limited line naming the probe", out.String())
	}
	if len(slept) != 1 || slept[0] != 2*time.Minute {
		t.Errorf("slept = %v, want the two minutes to the probe", slept)
	}
}

func TestPRWatchRunStopsOnAMissingPR(t *testing.T) {
	t.Parallel()
	poller := &scriptedPoller{errs: []error{&prstate.MissingError{Repo: "o/r", PRs: []int{99999}}}}
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded}, io.Discard, &slept)

	err := run.watch(context.Background(), []int{99999}, map[int]prWatchSnapshot{})
	if err == nil || !strings.Contains(err.Error(), "o/r has no pull request #99999") || len(poller.asked) != 1 {
		t.Fatalf("err = %v after %d polls, want the missing PR named at once", err, len(poller.asked))
	}
}

func TestPRWatchRunJoinsPRsTheLanePrefixDiscovers(t *testing.T) {
	t.Parallel()
	discover := watchTick(map[int]prWatchSnapshot{})
	discover.discovered = []int{9}
	poller := &scriptedPoller{ticks: []prWatchTick{
		discover,
		watchTick(map[int]prWatchSnapshot{9: {State: "OPEN", Head: watchHeadA, Queued: true}}),
	}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilNever, once: true, prefix: "yasyf/v3-x/"}, &out, &slept)

	if err := run.watch(context.Background(), nil, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if out.String() != "#9 queued\n" {
		t.Errorf("output = %q, want the discovered PR's state in the same run", out.String())
	}
	if fmt.Sprint(poller.asked) != "[[] [9]]" {
		t.Errorf("asked = %v", poller.asked)
	}
}

func TestPRWatchRunEmitsJSON(t *testing.T) {
	t.Parallel()
	poller := &scriptedPoller{ticks: []prWatchTick{watchTick(map[int]prWatchSnapshot{3: {State: "OPEN", Head: watchHeadA, Queued: true}})}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilNever, once: true, json: true}, &out, &slept)

	if err := run.watch(context.Background(), []int{3}, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	var event prWatchEvent
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if event.PR != 3 || event.Event != "queued" || event.Head != watchHeadA {
		t.Errorf("event = %+v", event)
	}
}

func TestPRWatchRunGivesUpAfterRepeatedFailures(t *testing.T) {
	t.Parallel()
	errs := make([]error, prWatchMaxFails)
	for i := range errs {
		errs[i] = errors.New("boom")
	}
	poller := &scriptedPoller{errs: errs}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilNever}, &out, &slept)

	err := run.watch(context.Background(), []int{3}, map[int]prWatchSnapshot{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the poll failure", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want failures kept off stdout", out.String())
	}
}

type watchGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	responses []string
	vars      []map[string]any
	queries   []string
	pulls     map[string][]string
	gone      []string
	reads     []string
	overlap   time.Duration
	limitREST bool
	flight    sync.Mutex
	inFlight  int
	peak      int
}

func (g *watchGitHub) peakReads() int {
	g.flight.Lock()
	defer g.flight.Unlock()
	return g.peak
}

func (g *watchGitHub) fly(delta int) {
	g.flight.Lock()
	defer g.flight.Unlock()
	g.inFlight += delta
	g.peak = max(g.peak, g.inFlight)
}

// pull makes the REST read of path answer the nth time with the nth body,
// repeating the last.
func (g *watchGitHub) pull(path string, bodies ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pulls == nil {
		g.pulls = map[string][]string{}
	}
	g.pulls[path] = bodies
}

func (g *watchGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.overlap > 0 && r.Method == http.MethodGet {
		g.fly(1)
		defer g.fly(-1)
		time.Sleep(g.overlap)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if slices.Contains(g.gone, r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if bodies, ok := g.pulls[r.URL.Path]; ok && r.Method == http.MethodGet {
		g.reads = append(g.reads, r.URL.Path)
		n := 0
		for _, read := range g.reads {
			if read == r.URL.Path {
				n++
			}
		}
		_, _ = io.WriteString(w, bodies[min(n, len(bodies))-1])
		return
	}
	if g.limitREST && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	if r.URL.Path != "/graphql" {
		g.t.Errorf("path = %s, want /graphql", r.URL.Path)
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		g.t.Errorf("decode: %v", err)
	}
	g.queries = append(g.queries, req.Query)
	g.vars = append(g.vars, req.Variables)
	_, _ = io.WriteString(w, g.responses[min(len(g.queries), len(g.responses))-1])
}

// stubPRState points the shared pull request cache at a scratch directory and
// its GitHub reads at a server answering the nth query with the nth response,
// repeating the last.
func stubPRState(t *testing.T, responses ...string) *watchGitHub {
	t.Helper()
	github := &watchGitHub{t: t, responses: responses}
	ts := httptest.NewServer(github)
	t.Cleanup(ts.Close)
	root := t.TempDir()
	priorAPI, priorRoot := reviewsAPI, prStateRoot
	reviewsAPI = func() *ghapi.Client { return ghapi.New(ts.URL) }
	prStateRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { reviewsAPI, prStateRoot = priorAPI, priorRoot })
	t.Setenv("GH_TOKEN", "pr-state-test-token")
	return github
}

func TestPRWatchHoldsGreenOnAFreshHeadUntilItsWorkflowsFinish(t *testing.T) {
	pollAt := func(suite string) string {
		return fmt.Sprintf(`{"data":{
			"rateLimit":{"remaining":4900,"resetAt":"2026-09-30T07:00:00Z"},
			"repository":{
				"trunk":{"name":"dev","target":{"history":{"nodes":[]}}},
				"p0":{"number":25131,"state":"OPEN","headRefOid":"%s","mergeable":"MERGEABLE",
					"checks":{"nodes":[{"commit":{
						"statusCheckRollup":{"state":"SUCCESS","contexts":{"nodes":[
							{"__typename":"CheckRun","name":"Socket Security","conclusion":"SUCCESS","status":"COMPLETED"}]}},
						"checkSuites":{"nodes":[{"status":"COMPLETED"},{"status":%q}]}}}]}}
			}}}`, watchHeadB, suite)
	}
	greenAtOldHead := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Mergeable: "MERGEABLE", Green: true}
	for _, tc := range []struct {
		suite string
		want  []string
	}{
		{"QUEUED", []string{"#25131 new-head b2b2b2b2b"}},
		{"COMPLETED", []string{"#25131 new-head b2b2b2b2b", "#25131 green"}},
	} {
		t.Run(tc.suite, func(t *testing.T) {
			github := stubPRState(t, pollAt(tc.suite))
			_, gt := stubPRInfo(t, prInfoOpen)
			store, err := openPRState(withGTAPI(t.Context(), gt), "Forge-AI/monorepo", io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			tick, err := prWatchSource{store: store}.poll(t.Context(), []int{25131}, map[int]prWatchSnapshot{25131: greenAtOldHead})
			if err != nil {
				t.Fatalf("poll: %v", err)
			}

			_, events := prWatchStep(25131, greenAtOldHead, tick.snapshots[25131])
			if got := watchEvents(t, events); !slices.Equal(got, tc.want) {
				t.Errorf("events = %q, want %q", got, tc.want)
			}
			if !strings.Contains(github.queries[0], "checkSuites(first: 50, filterBy: {appId: 15368})") {
				t.Errorf("query reads no GitHub Actions check suites:\n%s", github.queries[0])
			}
		})
	}
}

func TestPRWatchPollReadsQueueEvictionAndTrunkSquash(t *testing.T) {
	github := stubPRState(t, fmt.Sprintf(`{"data":{
		"rateLimit":{"remaining":4900,"resetAt":"2026-09-30T07:00:00Z"},
		"repository":{
			"trunk":{"name":"dev","target":{"history":{"nodes":[
				{"oid":"1111111111111111111111111111111111111111","messageHeadline":"api: fix (#261180)"},
				{"oid":"%s","messageHeadline":"ci: 🚀 add a deploy pipeline (#25116)"}]}}},
			"p0":{"number":25116,"state":"CLOSED","headRefOid":"%s","mergeable":"UNKNOWN","reviewDecision":"APPROVED",
				"checks":{"nodes":[]}},
			"t0":{"compare":{"status":"BEHIND"}},
			"p1":{"number":26918,"state":"OPEN","headRefOid":"%s","mergeable":"CONFLICTING","reviewDecision":"APPROVED",
				"checks":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[
					{"__typename":"CheckRun","name":"lint","conclusion":"SUCCESS","status":"COMPLETED"},
					{"__typename":"StatusContext","context":"buildkite/tests","state":"FAILURE"}]}}}}]},
				"comments":{"nodes":[{"body":"thanks"},{"body":%s}]}}
		}}}`, watchSquash, watchHeadB, watchHeadA, mustJSON(t, prActivityEvicted)))
	_, gt := stubPRInfo(t, prInfoEvicted, prInfoLanded)
	store, err := openPRState(withGTAPI(t.Context(), gt), "Forge-AI/monorepo", io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	tick, err := prWatchSource{store: store}.poll(t.Context(), []int{25116, 26918}, map[int]prWatchSnapshot{26918: {State: "OPEN", Queued: true}})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	evicted := tick.snapshots[26918]
	if evicted.Queued || evicted.Evicted != "it had merge conflicts" {
		t.Errorf("#26918 queue = %v %q, want evicted for merge conflicts", evicted.Queued, evicted.Evicted)
	}
	if !reflect.DeepEqual(evicted.Failing, []string{"buildkite/tests"}) || evicted.Green || !evicted.Approved {
		t.Errorf("#26918 = %+v", evicted)
	}
	if landed := tick.snapshots[25116]; landed.Squash != watchSquash {
		t.Errorf("#25116 squash = %q, want the trunk commit whose subject ends (#25116)", landed.Squash)
	}
	query := github.queries[0]
	if !strings.Contains(query, "rateLimit { remaining resetAt }") || strings.Count(query, "comments(last: 100)") != 1 {
		t.Errorf("query reads the activity of more than the queued PR, or skips the rate limit:\n%s", query)
	}
	if github.vars[0]["m0"] != "9cc33f055dc4db19da6eb13a210a810297ccdc05" {
		t.Errorf("vars = %v, want Graphite's merge commit compared against the trunk", github.vars[0])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPRWatchSnapshotOfLandingAndQueueEvidence(t *testing.T) {
	t.Parallel()
	trunk := prstate.Trunk{Name: "dev", History: []prstate.Commit{{OID: watchSquash, Headline: "ci: add a deploy pipeline (#27949)"}}}
	merged := decodePRInfo(t, prInfoLanded)
	queued := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Queued: true}

	t.Run("graphite merged before github closed holds the queue", func(t *testing.T) {
		t.Parallel()
		snap := prWatchSnapshotOf(25116, prstate.PR{State: "OPEN", HeadRefOid: watchHeadA, Graphite: &merged}, trunk, queued)
		if !snap.Queued {
			t.Fatalf("snap = %+v, want the queue held so no ejection is reported", snap)
		}
	})
	t.Run("merged into a parent branch is not a landing", func(t *testing.T) {
		t.Parallel()
		pr := prstate.PR{State: "MERGED", BaseRefName: "yasyf/parent", HeadRefOid: watchHeadA, MergeCommit: watchHeadB, SquashOn: []string{"yasyf/parent"}, Graphite: &merged}
		if snap := prWatchSnapshotOf(4, pr, trunk, queued); snap.landed() {
			t.Fatalf("snap = %+v, want no landing from a merge into a parent", snap)
		}
	})
	t.Run("a closed pr whose squash is on the trunk lands", func(t *testing.T) {
		t.Parallel()
		snap := prWatchSnapshotOf(27949, prstate.PR{State: "CLOSED", HeadRefOid: watchHeadA, Graphite: &merged}, trunk, queued)
		if snap.Squash != watchSquash {
			t.Fatalf("squash = %q", snap.Squash)
		}
	})
	t.Run("graphite's squash on the trunk lands", func(t *testing.T) {
		t.Parallel()
		snap := prWatchSnapshotOf(25116, prstate.PR{State: "CLOSED", HeadRefOid: watchHeadA, SquashOn: []string{"dev"}, Graphite: &merged}, trunk, queued)
		if snap.Squash != merged.MergeCommitSha {
			t.Fatalf("squash = %q, want Graphite's", snap.Squash)
		}
	})
	t.Run("a closure graphite cannot answer stays pollable", func(t *testing.T) {
		t.Parallel()
		snap := prWatchSnapshotOf(1, prstate.PR{State: "CLOSED", HeadRefOid: watchHeadA}, trunk, queued)
		if snap.terminal() {
			t.Fatalf("snap = %+v, want it left open until the evidence arrives", snap)
		}
	})
	t.Run("a failing rollup with no failing check read is red", func(t *testing.T) {
		t.Parallel()
		snap := prWatchSnapshotOf(2, prstate.PR{State: "OPEN", HeadRefOid: watchHeadA, Rollup: &prstate.Rollup{State: "FAILURE"}, Graphite: &merged}, trunk, prWatchSnapshot{})
		if !reflect.DeepEqual(snap.Failing, []string{"rollup failure"}) {
			t.Fatalf("failing = %q", snap.Failing)
		}
	})
}

func TestPRWatchRunResumingSettledPRsExitsWithoutPolling(t *testing.T) {
	t.Parallel()
	poller := &scriptedPoller{}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded}, &out, &slept)
	snaps := map[int]prWatchSnapshot{1: {State: "CLOSED", Head: watchHeadA}, 2: {State: "CLOSED", Head: watchHeadA, Squash: watchSquash}}

	err := run.watch(context.Background(), []int{1, 2}, snaps)
	if err == nil || !strings.Contains(err.Error(), "#1") {
		t.Fatalf("err = %v, want #1 named", err)
	}
	if len(poller.asked) != 0 || len(slept) != 0 {
		t.Fatalf("asked %v slept %v, want an immediate exit", poller.asked, slept)
	}
}

func TestPRWatchRunJobExitsOnTheNamedCheckOfTheCurrentHead(t *testing.T) {
	t.Parallel()
	failing := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Job: "FAILURE"}
	running := prWatchSnapshot{State: "OPEN", Head: watchHeadB, Job: "IN_PROGRESS"}
	passed := prWatchSnapshot{State: "OPEN", Head: watchHeadB, Job: "SUCCESS", Failing: []string{"other"}}
	tests := []struct {
		name    string
		ticks   []prWatchSnapshot
		want    string
		wantErr string
	}{
		{
			name:  "a pass on the head exits zero though another check is red",
			ticks: []prWatchSnapshot{running, passed},
			want:  "#7 red other\n#7 job lint success\n",
		},
		{
			name:    "a failure exits nonzero",
			ticks:   []prWatchSnapshot{failing},
			want:    "#7 job lint failure\n",
			wantErr: "pr watch: job lint: #7 failure",
		},
		{
			name:  "a skip settles it",
			ticks: []prWatchSnapshot{{State: "OPEN", Head: watchHeadA, Job: "SKIPPED"}},
			want:  "#7 job lint skipped\n",
		},
		{
			name:    "a pull request closing first ends the wait",
			ticks:   []prWatchSnapshot{{State: "CLOSED", Head: watchHeadA}},
			want:    "#7 closed-without-squash\n",
			wantErr: "#7 closed before lint settled",
		},
		{
			name:    "a finished rollup without the name ends the wait",
			ticks:   []prWatchSnapshot{{State: "OPEN", Head: watchHeadA, Job: prWatchJobAbsent}},
			want:    "#7 job lint absent\n",
			wantErr: "#7 finished its checks without lint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ticks := make([]prWatchTick, 0, len(tt.ticks))
			for _, snap := range tt.ticks {
				ticks = append(ticks, watchTick(map[int]prWatchSnapshot{7: snap}))
			}
			var out bytes.Buffer
			var slept []time.Duration
			run := newTestWatchRun(&scriptedPoller{ticks: ticks}, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded, job: "lint"}, &out, &slept)
			err := run.watch(context.Background(), []int{7}, map[int]prWatchSnapshot{})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("watch: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if out.String() != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", out.String(), tt.want)
			}
		})
	}
}

func TestPRWatchRunNamesItsRepositoryAndTheFailedJobsLogCommand(t *testing.T) {
	t.Parallel()
	failed := prWatchSnapshot{State: "OPEN", Head: watchHeadA, Job: "FAILURE", JobID: 456}
	var out bytes.Buffer
	var slept []time.Duration
	poller := &scriptedPoller{ticks: []prWatchTick{watchTick(map[int]prWatchSnapshot{7: failed})}}
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded, job: "lint", repo: "o/r"}, &out, &slept)
	err := run.watch(context.Background(), []int{7}, map[int]prWatchSnapshot{})
	wantErr := "pr watch: job lint: o/r#7 failure: ccx vcs job-log 456 -R o/r"
	if err == nil || err.Error() != wantErr {
		t.Errorf("err = %v, want %q", err, wantErr)
	}
	if want := "o/r#7 job lint failure: ccx vcs job-log 456 -R o/r\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestPRWatchRunOpensByNamingAnUnreadQueue(t *testing.T) {
	t.Parallel()
	landed := prWatchSnapshot{State: "MERGED", Head: watchHeadA, Squash: watchSquash}
	var out bytes.Buffer
	var slept []time.Duration
	poller := &scriptedPoller{ticks: []prWatchTick{watchTick(map[int]prWatchSnapshot{7: landed})}}
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded, repo: "o/r", queueUnread: true}, &out, &slept)
	if err := run.watch(context.Background(), []int{7}, map[int]prWatchSnapshot{}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	want := "queue-unread o/r: not a checkout here, so no queued or ejected line can appear\n" +
		"o/r#7 landed 815d915c0\n"
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPRRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		args        []string
		repo        string
		wantRepo    string
		wantNumbers []int
		wantErr     string
	}{
		{name: "bare numbers leave the repository open", args: []string{"1", "#2"}, wantNumbers: []int{1, 2}},
		{name: "a flag names it", args: []string{"1"}, repo: "o/r", wantRepo: "o/r", wantNumbers: []int{1}},
		{name: "an argument names it", args: []string{"o/r#3036", "5"}, wantRepo: "o/r", wantNumbers: []int{3036, 5}},
		{name: "an argument agrees with the flag in any case", args: []string{"O/R#1"}, repo: "o/r", wantRepo: "o/r", wantNumbers: []int{1}},
		{name: "an argument disagrees with the flag", args: []string{"o/other#1"}, repo: "o/r", wantErr: "o/other#1 is not in o/r"},
		{name: "two repositories", args: []string{"o/a#1", "o/b#2"}, wantErr: "o/b#2 is not in o/a"},
		{name: "not a number", args: []string{"o/r#x"}, wantErr: "not a pull request number"},
		{name: "no owner", args: []string{"r#1"}, wantErr: "not a pull request number"},
		{name: "zero", args: []string{"#0"}, wantErr: "not a pull request number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, numbers, err := prRefs(tt.args, tt.repo)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if repo != tt.wantRepo || !slices.Equal(numbers, tt.wantNumbers) {
				t.Errorf("prRefs = %q %v, want %q %v", repo, numbers, tt.wantRepo, tt.wantNumbers)
			}
		})
	}
}

func TestPRWatchRunJobFollowsANewHead(t *testing.T) {
	t.Parallel()
	poller := &scriptedPoller{ticks: []prWatchTick{
		watchTick(map[int]prWatchSnapshot{7: {State: "OPEN", Head: watchHeadB, Job: "QUEUED"}}),
		watchTick(map[int]prWatchSnapshot{7: {State: "OPEN", Head: watchHeadB, Job: "SUCCESS"}}),
	}}
	var out bytes.Buffer
	var slept []time.Duration
	run := newTestWatchRun(poller, prWatchOpts{interval: time.Minute, until: prWatchUntilLanded, job: "lint"}, &out, &slept)
	resumed := map[int]prWatchSnapshot{7: {State: "OPEN", Head: watchHeadA, Job: "QUEUED"}}
	if err := run.watch(context.Background(), []int{7}, resumed); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if want := "#7 new-head b2b2b2b2b\n#7 job lint success\n"; out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPRWatchJobStateReadsTheNamedCheck(t *testing.T) {
	t.Parallel()
	rollup := func(state string, contexts ...string) *statusRollup {
		var body strings.Builder
		for i, name := range contexts {
			if i > 0 {
				body.WriteString(",")
			}
			fmt.Fprintf(&body, `{"__typename":"StatusContext","context":%q,"state":%q}`, strings.Split(name, "=")[0], strings.Split(name, "=")[1])
		}
		var out statusRollup
		raw := fmt.Sprintf(`{"state":%q,"contexts":{"nodes":[%s]}}`, state, body.String())
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	withOpenSuite := rollup("SUCCESS", "Socket Security=SUCCESS")
	withOpenSuite.UnfinishedActions = 1
	tests := []struct {
		name   string
		rollup *statusRollup
		want   string
	}{
		{"no rollup yet", nil, ""},
		{"not reported while an Actions run is open though every reported check finished", withOpenSuite, ""},
		{"not reported while others run", rollup("PENDING", "other=PENDING"), ""},
		{"reported", rollup("PENDING", "lint=SUCCESS", "other=PENDING"), "SUCCESS"},
		{"absent from a finished rollup", rollup("SUCCESS", "other=SUCCESS"), prWatchJobAbsent},
		{"a rerun in flight outranks its old result", rollup("PENDING", "lint=FAILURE", "lint=PENDING"), "PENDING"},
		{"a failure outranks a pass of the same name", rollup("FAILURE", "lint=SUCCESS", "lint=FAILURE"), "FAILURE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := prWatchJobState(tt.rollup, "lint"); got != tt.want {
				t.Errorf("state = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("the id comes from the run that sets the state", func(t *testing.T) {
		t.Parallel()
		var checks statusRollup
		raw := `{"state":"FAILURE","contexts":{"nodes":[` +
			`{"__typename":"CheckRun","name":"lint","conclusion":"SUCCESS","databaseId":1},` +
			`{"__typename":"CheckRun","name":"lint","conclusion":"FAILURE","databaseId":2},` +
			`{"__typename":"CheckRun","name":"other","conclusion":"SUCCESS","databaseId":3}]}}`
		if err := json.Unmarshal([]byte(raw), &checks); err != nil {
			t.Fatal(err)
		}
		if state, id := prWatchJobState(&checks, "lint"); state != "FAILURE" || id != 2 {
			t.Errorf("job = %q %d, want FAILURE 2", state, id)
		}
	})
}
