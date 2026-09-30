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
}

func (g *watchGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
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
