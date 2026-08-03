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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// reviewsRepoPath prefixes every REST path the fake GitHub serves, matching the
// repository setupReviews seeds into the lane cache.
const reviewsRepoPath = "/repos/acme/repo/"

func reviewsPRURL(number int) string {
	return fmt.Sprintf("https://github.com/acme/repo/pull/%d", number)
}

// reviewsPR is one pull request the fake GitHub knows: it answers OPEN for
// openAnswers state reads — the resolution read included — and state after.
type reviewsPR struct {
	openAnswers int
	state       string
	merged      bool
	answers     int
}

type reviewsGraphQL struct {
	query string
	vars  map[string]any
}

// reviewsServer is the fake GitHub the reviews tests poll: one GraphQL endpoint
// answering batched state reads and three REST comment feeds per pull request.
type reviewsServer struct {
	t *testing.T

	mu       sync.Mutex
	prs      map[int]*reviewsPR
	branches map[string][]int
	feeds    map[string][]string
	feedCall map[string]int
	failREST map[int]int
	requests []string
	queries  []reviewsGraphQL
}

// pr registers a pull request. openAnswers state reads answer OPEN before the
// transition to state; the resolution read at setup is the first of them.
func (s *reviewsServer) pr(number, openAnswers int, state string, merged bool) {
	s.prs[number] = &reviewsPR{openAnswers: openAnswers, state: state, merged: merged}
}

// branch registers the pull requests headed by name, oldest first, the order
// GitHub's CREATED_AT ordering sorts them by.
func (s *reviewsServer) branch(name string, numbers ...int) {
	s.branches[name] = numbers
}

// feed queues one payload per poll of number's kind feed ("inline", "comment",
// or "review"); the last payload repeats for every later poll.
func (s *reviewsServer) feed(kind string, number int, payloads ...string) {
	s.feeds[fmt.Sprintf("%s:%d", kind, number)] = payloads
}

// fail makes number's next count REST requests answer 500.
func (s *reviewsServer) fail(number, count int) {
	s.failREST[number] = count
}

// requestLog returns every request in order: "POST /graphql" for a batch, the
// full URL for a REST feed.
func (s *reviewsServer) requestLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *reviewsServer) graphQLCalls() []reviewsGraphQL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reviewsGraphQL(nil), s.queries...)
}

func (s *reviewsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/graphql" {
		s.serveGraphQL(w, r)
		return
	}
	s.requests = append(s.requests, r.URL.String())
	number, kind, ok := reviewsFeed(r.URL.Path)
	if !ok {
		http.Error(w, "unmatched "+r.URL.Path, http.StatusNotFound)
		return
	}
	if left := s.failREST[number]; left > 0 {
		s.failREST[number] = left - 1
		http.Error(w, "transient upstream failure", http.StatusInternalServerError)
		return
	}
	key := fmt.Sprintf("%s:%d", kind, number)
	body := "[]"
	if payloads := s.feeds[key]; len(payloads) > 0 {
		body = payloads[min(s.feedCall[key], len(payloads)-1)]
	}
	s.feedCall[key]++
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, body); err != nil {
		s.t.Errorf("write %s: %v", r.URL, err)
	}
}

func (s *reviewsServer) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode graphql request: %v", err)
		http.Error(w, "undecodable request", http.StatusBadRequest)
		return
	}
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.queries = append(s.queries, reviewsGraphQL{query: req.Query, vars: req.Variables})

	fields := map[string]any{}
	var missing []map[string]any
	for name, raw := range req.Variables {
		if name == "owner" || name == "repo" {
			continue
		}
		switch value := raw.(type) {
		case float64:
			number := int(value)
			pr, known := s.prs[number]
			if !known {
				fields[name] = nil
				missing = append(missing, map[string]any{
					"type":    "NOT_FOUND",
					"path":    []any{"repository", name},
					"message": fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number),
				})
				continue
			}
			fields[name] = s.answer(number, pr)
		case string:
			numbers := s.branches[value]
			if len(numbers) == 0 {
				fields[name] = map[string]any{"nodes": []any{}}
				continue
			}
			number := reviewsHeadPR(numbers, req.Query)
			pr, registered := s.prs[number]
			if !registered {
				fields[name] = nil
				missing = append(missing, map[string]any{
					"type":    "NOT_FOUND",
					"path":    []any{"repository", name},
					"message": fmt.Sprintf("Could not resolve to a Repository with the name of %q.", value),
				})
				continue
			}
			fields[name] = map[string]any{"nodes": []any{s.answer(number, pr)}}
		}
	}

	// GitHub answers a partly-unresolvable batch with data still populated and
	// the unresolvable field null beside the errors array, not with a null data —
	// the shape recorded in testdata/gh/api/reviews-graphql-missing.json.
	body := map[string]any{"data": map[string]any{"repository": fields}}
	if len(missing) > 0 {
		body["errors"] = missing
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.t.Errorf("encode graphql response: %v", err)
	}
}

// reviewsHeadPR picks the pull request a first:1 query lands on, reading the
// CREATED_AT direction off the query the way GitHub does: DESC yields the
// newest, ASC the oldest.
func reviewsHeadPR(numbers []int, query string) int {
	if strings.Contains(query, "direction: DESC") {
		return numbers[len(numbers)-1]
	}
	return numbers[0]
}

func (s *reviewsServer) answer(number int, pr *reviewsPR) map[string]any {
	pr.answers++
	state := "OPEN"
	var mergedAt any
	if pr.answers > pr.openAnswers {
		state = pr.state
		if pr.merged {
			mergedAt = "2026-07-20T19:00:00Z"
		}
	}
	return map[string]any{
		"number":   number,
		"url":      reviewsPRURL(number),
		"state":    state,
		"mergedAt": mergedAt,
	}
}

func reviewsFeed(path string) (int, string, bool) {
	rest, ok := strings.CutPrefix(path, reviewsRepoPath)
	if !ok {
		return 0, "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return 0, "", false
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", false
	}
	switch parts[0] + "/" + parts[2] {
	case "pulls/comments":
		return number, "inline", true
	case "issues/comments":
		return number, "comment", true
	case "pulls/reviews":
		return number, "review", true
	}
	return 0, "", false
}

// setupReviews points the reviews command at a fake GitHub, with an empty PATH:
// the token comes from the environment and the repository from the seeded lane
// cache, so a watch that still spawns anything fails outright.
func setupReviews(t *testing.T) *reviewsServer {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o750); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Setenv("PATH", empty)
	return setupReviewsHere(t)
}

// setupReviewsHere is setupReviews for a repository already standing at the
// working directory, leaving PATH as its fixture installed it — the shape a
// watch that has to ask real git which branch is checked out needs.
func setupReviewsHere(t *testing.T) *reviewsServer {
	t.Helper()
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	seedLaneRecords(t, ".", laneSeed{nameWithOwner: "acme/repo", owner: "acme"})
	t.Setenv(envReviewsPollInterval, "1ms")
	return stubReviewsAPI(t)
}

// stubReviewsAPI points every watch this test starts at a fake GitHub and hands
// it a token, so nothing resolves credentials or an endpoint off the machine.
func stubReviewsAPI(t *testing.T) *reviewsServer {
	t.Helper()
	t.Setenv("GH_TOKEN", "reviews-test-token")
	s := &reviewsServer{
		t:        t,
		prs:      map[int]*reviewsPR{},
		branches: map[string][]int{},
		feeds:    map[string][]string{},
		feedCall: map[string]int{},
		failREST: map[int]int{},
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	prior := reviewsAPI
	reviewsAPI = func() *ghapi.Client { return ghapi.New(ts.URL) }
	t.Cleanup(func() { reviewsAPI = prior })
	return s
}

func runReviewsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newReviewsCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestReviewsStreamsNewComment renders cli/cli#13982's recorded issue-comment
// feed: two comments GitHub really served, one of them carrying the CRLF the
// sanitizer has to strip before the body reaches a terminal.
func TestReviewsStreamsNewComment(t *testing.T) {
	comments := loadGHAPIGolden(t, "reviews-issue-comments").body
	srv := setupReviews(t)
	srv.pr(7, 1, "MERGED", true)
	srv.feed("comment", 7, comments)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	for _, want := range []string{
		"watching pr#7 · https://github.com/acme/repo/pull/7 · poll 1ms\n",
		"◆ comment · github-actions[bot] · pr#7 · 2026-07-27T15:06:57Z\n",
		"  Thanks for your pull request! This is a large change (660 lines across 9 files) that doesn't reference a `help wanted` issue.\n",
		"↳ https://github.com/cli/cli/pull/13982#issuecomment-5093042634 · id 5093042634\n",
		"◆ comment · offbyone · pr#7 · 2026-08-02T04:10:21Z\n",
		"  There are a lot of comments on here; can you clarify which ones are blockers versus commentary? \n",
		"↳ https://github.com/cli/cli/pull/13982#issuecomment-5154974588 · id 5154974588\n",
		"◆ pr#7 merged · https://github.com/acme/repo/pull/7\n",
		"watch done · 1 merged · 0 closed\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\r") {
		t.Errorf("carriage return from the recorded body reached the terminal:\n%q", got)
	}
}

func TestReviewsDedupesAcrossPolls(t *testing.T) {
	comments := loadGHAPIGolden(t, "reviews-issue-comments").body
	srv := setupReviews(t)
	srv.pr(7, 2, "MERGED", true)
	srv.feed("comment", 7, comments)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if count := strings.Count(got, "id 5093042634"); count != 1 {
		t.Errorf("event count = %d, want 1\n%s", count, got)
	}
}

// TestReviewsAllKindsSortedAndSuppressed drives all three of cli/cli's recorded
// feeds through one watch: events from the three kinds interleave in timestamp
// order, an inline comment whose line GitHub nulled reads as outdated, a
// CHANGES_REQUESTED review carries the triage pointer, and the empty-bodied
// COMMENTED review — the container GitHub wraps a batch of inline comments in —
// is emitted not at all.
func TestReviewsAllKindsSortedAndSuppressed(t *testing.T) {
	inline := loadGHAPIGolden(t, "reviews-inline-comments-outdated").body
	comments := loadGHAPIGolden(t, "reviews-issue-comments").body
	reviews := loadGHAPIGolden(t, "reviews-reviews").body
	srv := setupReviews(t)
	srv.pr(7, 1, "MERGED", true)
	srv.feed("inline", 7, inline)
	srv.feed("comment", 7, comments)
	srv.feed("review", 7, reviews)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	for _, want := range []string{
		"◆ inline · Copilot · pr#7 · AGENTS.md:146 · 2026-07-28T21:52:43Z\n",
		"◆ inline · Copilot · pr#7 · AGENTS.md (outdated) · 2026-07-28T21:52:43Z\n",
		"◆ review · BagToad · pr#7 · changes_requested · 2026-07-28T05:42:11Z\n",
		"↳ triage: spawn the cc-context:pr-review-triage agent with pr#7 and review id 4794142826\n",
		"◆ review · copilot-pull-request-reviewer[bot] · pr#7 · commented · 2026-07-27T15:11:14Z\n",
		"◆ pr#7 merged · https://github.com/acme/repo/pull/7\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "id 4837136847") {
		t.Errorf("the empty-bodied COMMENTED review was emitted:\n%s", got)
	}
	if want := reviewsEmittedTimestamps(t, got); !slices.IsSortedFunc(want, time.Time.Compare) {
		t.Errorf("emitted timestamps %v are out of order:\n%s", want, got)
	}
}

// reviewsEmittedTimestamps reads the timestamp off every event header in out,
// which is the trailing "· <RFC3339>" segment.
func reviewsEmittedTimestamps(t *testing.T, out string) []time.Time {
	t.Helper()
	var stamps []time.Time
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "◆ ") || strings.Contains(line, " merged"+shipSep) {
			continue
		}
		parts := strings.Split(line, shipSep)
		stamp, err := time.Parse(time.RFC3339, parts[len(parts)-1])
		if err != nil {
			t.Fatalf("parse timestamp off %q: %v", line, err)
		}
		stamps = append(stamps, stamp)
	}
	if len(stamps) == 0 {
		t.Fatalf("no event headers in:\n%s", out)
	}
	return stamps
}

// TestReviewsPendingReviewSuppressed is the one review shape no capture holds:
// GitHub serves a PENDING review only to its own author, and the recorder's
// token authors none. Its bytes are this test's own until
// `gh api -i 'repos/OWNER/REPO/pulls/N/reviews?per_page=100'` runs under a token
// holding an unsubmitted review on that pull request.
func TestReviewsPendingReviewSuppressed(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 1, "MERGED", true)
	srv.feed("review", 7, `[
		{"id":401,"state":"PENDING","body":"draft","user":{"login":"draft"},"html_url":"https://example/401","submitted_at":null}
	]`)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if strings.Contains(got, "id 401") {
		t.Errorf("pending review emitted:\n%s", got)
	}
}

// TestReviewsEditedReemit needs one comment observed twice at two updated_at
// values, which no single capture holds — the corpus records a feed at one
// instant. Recording it means capturing
// `gh api -i 'repos/OWNER/REPO/issues/N/comments?per_page=100'` before and after
// a comment is edited; until then these two payloads are the test's own.
func TestReviewsEditedReemit(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 2, "MERGED", true)
	srv.feed("comment", 7, `[{
		"id":201,
		"body":"first",
		"user":{"login":"commenter"},
		"html_url":"https://example/201",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`, `[{
		"id":201,
		"body":"second",
		"user":{"login":"commenter"},
		"html_url":"https://example/201",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:02:00Z"
	}]`)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.Contains(got, "comment · commenter · pr#7 · edited · 2026-07-20T18:02:00Z") {
		t.Errorf("edited event missing:\n%s", got)
	}
	if count := strings.Count(got, "id 201"); count != 2 {
		t.Errorf("event count = %d, want 2\n%s", count, got)
	}
}

func TestReviewsTerminalExit(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		merged     bool
		terminal   string
		doneCounts string
	}{
		{"merged", "MERGED", true, "merged", "1 merged · 0 closed"},
		{"closed", "CLOSED", false, "closed", "0 merged · 1 closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := setupReviews(t)
			srv.pr(7, 1, tt.state, tt.merged)

			got, err := runReviewsCmd(t, "7", "--since", "all")
			if err != nil {
				t.Fatalf("reviews error = %v", err)
			}
			wantTail := "◆ pr#7 " + tt.terminal + " · https://github.com/acme/repo/pull/7\n\n" +
				"watch done · " + tt.doneCounts + "\n"
			if !strings.HasSuffix(got, wantTail) {
				t.Errorf("output tail = %q, want suffix %q", got, wantTail)
			}
		})
	}
}

func TestReviewsMultiPRWaitsForAll(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(1, 1, "MERGED", true)
	srv.pr(2, 2, "CLOSED", false)

	got, err := runReviewsCmd(t, "1", "2", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.HasSuffix(got, "watch done · 1 merged · 1 closed\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
	restCounts := map[string]int{}
	batches := 0
	for _, request := range srv.requestLog() {
		switch {
		case request == "POST /graphql":
			batches++
		case strings.Contains(request, "/pulls/1/"), strings.Contains(request, "/issues/1/"):
			restCounts["1"]++
		case strings.Contains(request, "/pulls/2/"), strings.Contains(request, "/issues/2/"):
			restCounts["2"]++
		}
	}
	want := map[string]int{"1": 3, "2": 6}
	if !reflect.DeepEqual(restCounts, want) {
		t.Errorf("REST counts = %v, want %v", restCounts, want)
	}
	if batches != 3 {
		t.Errorf("graphql batches = %d, want 3 (1 resolution + 2 cycles)", batches)
	}
}

// TestReviewsPerCycleRequestBudget pins the poll cycle's cost: one batched
// GraphQL state read plus three REST feeds per open target, holding flat as
// cycles accumulate rather than growing with them.
func TestReviewsPerCycleRequestBudget(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(1, 3, "MERGED", true)
	srv.pr(2, 3, "MERGED", true)

	if _, err := runReviewsCmd(t, "1", "2", "--since", "all"); err != nil {
		t.Fatalf("reviews error = %v", err)
	}

	var cycles []int
	for _, request := range srv.requestLog() {
		if request == "POST /graphql" {
			cycles = append(cycles, 0)
			continue
		}
		if len(cycles) == 0 {
			t.Fatalf("REST request %q preceded every graphql batch", request)
		}
		cycles[len(cycles)-1]++
	}
	want := []int{0, 6, 6, 6}
	if !reflect.DeepEqual(cycles, want) {
		t.Errorf("REST requests per batch = %v, want %v", cycles, want)
	}
}

func TestReviewsTransientFailureTolerance(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 1, "MERGED", true)
	srv.fail(7, 3)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.HasSuffix(got, "watch done · 1 merged · 0 closed\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
}

func TestReviewsAbortsAfterMaxFailures(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 0, "OPEN", false)
	srv.fail(7, 100)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err == nil || !strings.Contains(err.Error(), "1 of 1 target(s) aborted") {
		t.Fatalf("reviews error = %v, want a 1-of-1 aborted error", err)
	}
	if !strings.Contains(got, "◆ pr#7 watch aborted") {
		t.Errorf("aborted line missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "watch done · 0 merged · 0 closed · 1 aborted\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
}

// TestReviewsMultiPRPartialFailureIsolation drives one healthy target (pr#1,
// merges normally) alongside one persistently-failing target (pr#2, whose feeds
// answer 500 every poll): pr#1's events must still be delivered, and only pr#2
// aborts.
func TestReviewsMultiPRPartialFailureIsolation(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(1, 1, "MERGED", true)
	srv.feed("comment", 1, `[{
		"id":101,
		"body":"healthy event",
		"user":{"login":"alice"},
		"html_url":"https://github.com/acme/repo/pull/1#issuecomment-101",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)
	srv.pr(2, 0, "OPEN", false)
	srv.fail(2, 100)

	got, err := runReviewsCmd(t, "1", "2", "--since", "all")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 target(s) aborted") {
		t.Fatalf("reviews error = %v, want a 1-of-2 aborted error", err)
	}
	if !strings.Contains(got, "id 101") {
		t.Errorf("healthy target's event missing:\n%s", got)
	}
	if !strings.Contains(got, "◆ pr#2 watch aborted") {
		t.Errorf("failing target's abort line missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "watch done · 1 merged · 0 closed · 1 aborted\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
}

// TestReviewsBudgetCapFooterIndented caps a recorded three-line comment at a
// budget that fits its first line, so render.Cap's omission footer lands inside
// the event body and has to carry the body's own indentation.
func TestReviewsBudgetCapFooterIndented(t *testing.T) {
	comments := loadGHAPIGolden(t, "reviews-issue-comments").body
	srv := setupReviews(t)
	srv.pr(7, 1, "MERGED", true)
	srv.feed("comment", 7, comments)

	got, err := runReviewsCmd(t, "7", "--since", "all", "--budget", "26")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	wantBody := "  There are a lot of comments on here; can you clarify which ones are blockers versus commentary? \n" +
		"  \n  … +1 lines, ~"
	if !strings.Contains(got, wantBody) {
		t.Errorf("capped body missing or mis-indented:\n%s", got)
	}
	if !strings.Contains(got, "tokens omitted — re-run with a larger --budget") {
		t.Errorf("omission footer missing:\n%s", got)
	}
}

// TestReviewsResolution proves each operand form reaches the batch that can
// answer it: a number resolves through pullRequest(number:), a branch — and the
// checked-out branch an empty operand list stands for — through
// pullRequests(headRefName:). The empty-operand case stands on a real
// repository, so the branch name is git's answer rather than a script's.
func TestReviewsResolution(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		gitBranch string
		wantField string
		wantVar   any
	}{
		{"number", []string{"7"}, "", "pullRequest(number: $p0)", float64(7)},
		{"branch", []string{"feature/reviews"}, "", "pullRequests(headRefName: $p0", "feature/reviews"},
		{"current branch", nil, "feature/reviews", "pullRequests(headRefName: $p0", "feature/reviews"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var srv *reviewsServer
			if tt.gitBranch != "" {
				vcstest.Repo(t, vcstest.Branch(tt.gitBranch))
				srv = setupReviewsHere(t)
			} else {
				srv = setupReviews(t)
			}
			srv.pr(7, 1, "MERGED", true)
			srv.branch("feature/reviews", 7)

			args := append([]string{}, tt.args...)
			args = append(args, "--since", "all")
			if _, err := runReviewsCmd(t, args...); err != nil {
				t.Fatalf("reviews error = %v", err)
			}
			calls := srv.graphQLCalls()
			if len(calls) == 0 {
				t.Fatal("no graphql batch reached the server")
			}
			if !strings.Contains(calls[0].query, tt.wantField) {
				t.Errorf("resolution query = %q, want it to select %q", calls[0].query, tt.wantField)
			}
			if got := calls[0].vars["p0"]; got != tt.wantVar {
				t.Errorf("resolution variable p0 = %#v, want %#v", got, tt.wantVar)
			}
		})
	}
}

// TestReviewsBranchResolvesNewestPR proves a branch resubmitted after its first
// pull request merged watches the live one, not the corpse. Verified against
// github.com/yasyf/cc-context, whose yasyf/transcript-ccx-issues head carries
// PRs #1 and #2: gh pr view resolves #2, the newest, which is the answer this
// resolution must reproduce.
func TestReviewsBranchResolvesNewestPR(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(1, 0, "MERGED", true)
	srv.pr(2, 1, "MERGED", true)
	srv.branch("feature/resubmitted", 1, 2)

	out, err := runReviewsCmd(t, "feature/resubmitted", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.Contains(out, "watching pr#2") {
		t.Errorf("output = %q, want it to watch pr#2, the newest pull request of the branch", out)
	}
	if strings.Contains(out, "watching pr#1") {
		t.Errorf("output = %q, want it to leave pr#1, the merged predecessor, alone", out)
	}
}

// serveGHAPIGolden answers every request with one recorded GitHub response and
// returns a client pointed at it.
func serveGHAPIGolden(t *testing.T, g ghAPIGolden) *ghapi.Client {
	t.Helper()
	t.Setenv("GH_TOKEN", "reviews-test-token")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		if _, err := io.WriteString(w, g.body); err != nil {
			t.Errorf("write %s: %v", g.name, err)
		}
	}))
	t.Cleanup(ts.Close)
	return ghapi.New(ts.URL)
}

// TestReviewsBatchParsesRecordedGraphQL drives GitHub's own recorded batch
// answers through the resolver a poll cycle uses: the success envelope, whose
// merged pull request carries the non-null mergedAt the terminal classifier
// reads, and the unresolvable one, which keeps data populated and nulls only the
// field it could not resolve.
func TestReviewsBatchParsesRecordedGraphQL(t *testing.T) {
	numbers := loadGHAPIGolden(t, "reviews-graphql-numbers")
	missing := loadGHAPIGolden(t, "reviews-graphql-missing")

	client := reviewsClient{api: serveGHAPIGolden(t, numbers), owner: "cli", repo: "cli"}
	byNumber, err := client.pullRequestsByNumber(context.Background(), []int{13982, 13084})
	if err != nil {
		t.Fatalf("pullRequestsByNumber error = %v", err)
	}
	open := byNumber[13982]
	if open.State != "OPEN" || open.URL != "https://github.com/cli/cli/pull/13982" {
		t.Errorf("pr#13982 = %+v, want the recorded open pull request", open)
	}
	if terminal, err := reviewTerminalState(open); err != nil || terminal != "" {
		t.Errorf("reviewTerminalState(open) = (%q, %v), want the watch to stay attached", terminal, err)
	}
	merged := byNumber[13084]
	if merged.MergedAt == nil {
		t.Fatalf("pr#13084 = %+v, want the recorded mergedAt", merged)
	}
	if terminal, err := reviewTerminalState(merged); err != nil || terminal != "merged" {
		t.Errorf("reviewTerminalState(merged) = (%q, %v), want merged", terminal, err)
	}

	client = reviewsClient{api: serveGHAPIGolden(t, missing), owner: "cli", repo: "cli"}
	if _, err := client.pullRequestsByNumber(context.Background(), []int{99999999}); !errors.Is(err, ghapi.ErrNotFound) {
		t.Fatalf("pullRequestsByNumber error = %v, want ErrNotFound", err)
	}
}

func TestReviewsNotFoundExitCode(t *testing.T) {
	tests := []struct {
		name    string
		operand string
	}{
		{"branch with no pull request", "missing"},
		{"pull request number that does not exist", "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupReviews(t)

			_, err := runReviewsCmd(t, tt.operand)
			if err == nil {
				t.Fatal("reviews error = nil, want not found")
			}
			if code := ExitCode(err); code != 3 {
				t.Errorf("ExitCode(error) = %d, want 3; error=%v", code, err)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error = %v, want wrapped ErrNotFound", err)
			}
		})
	}
}

// TestResolveBranchTargetsSkipsBranchWithoutPR proves a branch the batch
// resolves to nothing is a note and a skip rather than a failure — the
// distinction that keeps one PR-less branch from sinking a whole stack watch —
// and that both branches cost one query, not one each.
func TestResolveBranchTargetsSkipsBranchWithoutPR(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 0, "OPEN", false)
	srv.branch("feature", 7)

	var out bytes.Buffer
	client, targets, err := resolveBranchTargets(context.Background(), &out, []string{"feature", "orphan"}, time.Time{})
	if err != nil {
		t.Fatalf("resolveBranchTargets error = %v", err)
	}
	if client.owner != "acme" || client.repo != "repo" {
		t.Errorf("client scoped to %s/%s, want acme/repo", client.owner, client.repo)
	}
	if len(targets) != 1 || targets[0].Number != 7 || targets[0].URL != reviewsPRURL(7) {
		t.Fatalf("targets = %+v, want just pr#7", targets)
	}
	if got := out.String(); got != "reviews: no open PR for orphan\n" {
		t.Errorf("note = %q, want the orphan branch skipped with a note", got)
	}
	calls := srv.graphQLCalls()
	if len(calls) != 1 {
		t.Fatalf("graphql batches = %d, want 1 for both branches", len(calls))
	}
	if calls[0].vars["p0"] != "feature" || calls[0].vars["p1"] != "orphan" {
		t.Errorf("batch variables = %#v, want p0=feature p1=orphan", calls[0].vars)
	}
}

func TestReviewsSincePropagationAndWatermark(t *testing.T) {
	srv := setupReviews(t)
	srv.pr(7, 2, "MERGED", true)
	srv.feed("comment", 7, `[{
		"id":201,
		"body":"new",
		"user":{"login":"commenter"},
		"html_url":"https://example/201",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)

	if _, err := runReviewsCmd(t, "7", "--since", "2026-07-20T18:00:00Z"); err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	var inlinePaths []string
	for _, request := range srv.requestLog() {
		if strings.HasPrefix(request, "/repos/acme/repo/pulls/7/comments") {
			inlinePaths = append(inlinePaths, request)
		}
	}
	want := []string{
		"/repos/acme/repo/pulls/7/comments?per_page=100&since=2026-07-20T18:00:00Z",
		"/repos/acme/repo/pulls/7/comments?per_page=100&since=2026-07-20T18:01:00Z",
	}
	if !reflect.DeepEqual(inlinePaths, want) {
		t.Errorf("inline REST paths = %v, want %v", inlinePaths, want)
	}
}

func TestReviewsCancelSummary(t *testing.T) {
	t.Setenv("GH_TOKEN", "reviews-test-token")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := reviewsClient{api: ghapi.New("http://127.0.0.1:1"), owner: "acme", repo: "repo"}
	targets := []*prTarget{
		{
			Number:    7,
			URL:       "https://github.com/acme/repo/pull/7",
			watermark: time.Date(2026, 7, 20, 18, 1, 0, 0, time.UTC),
			seen:      map[string]time.Time{},
		},
	}
	var out bytes.Buffer

	err := watchReviews(ctx, &out, client, targets, reviewsOpts{interval: time.Hour, all: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watchReviews error = %v, want context.Canceled", err)
	}
	want := "" +
		"watching pr#7 · https://github.com/acme/repo/pull/7 · poll 1h0m0s\n" +
		"watch cancelled · 1 open · 0 merged · 0 closed · " +
		"resume: ccx vcs reviews 7 --since 2026-07-20T18:01:00Z\n"
	if got := out.String(); got != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestReviewsBadEnvInterval(t *testing.T) {
	setupReviews(t)
	t.Setenv(envReviewsPollInterval, "garbage")

	_, err := runReviewsCmd(t, "7")
	if !errors.Is(err, errBadReviewsPollInterval) {
		t.Fatalf("reviews error = %v, want errBadReviewsPollInterval", err)
	}
}

func TestReviewsPollIntervalFloor(t *testing.T) {
	tests := []struct {
		name    string
		flag    time.Duration
		changed bool
		env     string
	}{
		{"flag zero", 0, true, ""},
		{"flag negative", -time.Second, true, ""},
		{"env zero", 0, false, "0s"},
		{"env negative", 0, false, "-1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv(envReviewsPollInterval, tt.env)
			}
			_, err := reviewsPollInterval(tt.flag, tt.changed)
			if !errors.Is(err, errReviewsIntervalNotPositive) {
				t.Errorf("reviewsPollInterval(%v, %v) error = %v, want errReviewsIntervalNotPositive", tt.flag, tt.changed, err)
			}
		})
	}
}

// TestReviewsStackNoTargets drives --stack from trunk, where the real gt state
// names main and nothing else, so stackBranches returns no branches — which
// must refuse rather than silently reporting a 0/0 watch.
func TestReviewsStackNoTargets(t *testing.T) {
	f := shipGTRepo(t)
	shipGTReady(t, f)
	head := shipHead(t, f)

	_, err := runReviewsCmd(t, "--stack")
	wantErr := "reviews: --stack found no stacked branches — run it from a stacked branch, not trunk"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("reviews --stack error = %v, want %q", err, wantErr)
	}
	assertShipRefusedClean(t, f, head)
}

// pathWithoutGit points PATH at a directory holding gt alone, so the first git
// call in reviews' stack resolution cannot resolve. The lane gate ahead of it
// reads the checkout off disk and only looks gt up, so it still reaches the
// graphite lane.
func pathWithoutGit(t *testing.T, f *vcstest.Fixture) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(f.ShimBin, "gt"), filepath.Join(dir, "gt")); err != nil {
		t.Fatalf("link gt: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestReviewsStackFailuresCarryReviewsPrefix(t *testing.T) {
	tests := []struct {
		name  string
		opts  []vcstest.Opt
		noGit bool
		want  string
	}{
		{name: "detached HEAD", opts: []vcstest.Opt{vcstest.Detached()}, want: "reviews: detached HEAD; no stack to resolve"},
		{name: "git cannot name the branch", noGit: true, want: "reviews: git branch --show-current:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipGTRepo(t, tt.opts...)
			shipGTReady(t, f)
			head := shipHead(t, f)
			if tt.noGit {
				pathWithoutGit(t, f)
			}

			_, err := runReviewsCmd(t, "--stack")
			if err == nil {
				t.Fatal("reviews --stack succeeded, want failure")
			}
			if !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to lead with %q", err, tt.want)
			}
			f.OnlyShimPATH(t)
			assertShipRefusedClean(t, f, head)
		})
	}
}

// TestWriteReviewEventSanitizesBody proves an ANSI escape, a \r spoof
// attempt, and a control character are all stripped before the body reaches
// the terminal.
func TestWriteReviewEventSanitizesBody(t *testing.T) {
	event := reviewEvent{
		target:    &prTarget{Number: 7},
		kind:      "comment",
		author:    "alice",
		body:      "\x1b[31mred\x1b[0m\r\nline2\x07bell",
		htmlURL:   "https://example/1",
		id:        1,
		timestamp: time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC),
	}
	var out bytes.Buffer
	if err := writeReviewEvent(&out, event, 0); err != nil {
		t.Fatalf("writeReviewEvent error = %v", err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("ANSI escape leaked into output:\n%q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("carriage return leaked into output:\n%q", got)
	}
	if strings.Contains(got, "\x07") {
		t.Errorf("control character leaked into output:\n%q", got)
	}
	if !strings.Contains(got, "red") || !strings.Contains(got, "line2bell") {
		t.Errorf("sanitized body lost content:\n%q", got)
	}
}

func TestParseSince(t *testing.T) {
	rfc := "2026-07-20T18:01:00Z"
	tests := []struct {
		name    string
		input   string
		want    time.Time
		wantAll bool
		wantErr bool
	}{
		{"all", "all", time.Time{}, true, false},
		{"RFC3339", rfc, time.Date(2026, 7, 20, 18, 1, 0, 0, time.UTC), false, false},
		{"invalid", "yesterday", time.Time{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, all, err := parseSince(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSince(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want || all != tt.wantAll {
				t.Errorf("parseSince(%q) = (%v, %v), want (%v, %v)", tt.input, got, all, tt.want, tt.wantAll)
			}
		})
	}

	before := time.Now().Add(-90 * time.Minute)
	got, all, err := parseSince("90m")
	after := time.Now().Add(-90 * time.Minute)
	if err != nil {
		t.Fatalf("parseSince duration error = %v", err)
	}
	if all || got.Before(before) || got.After(after) {
		t.Errorf("parseSince duration = (%v, %v), want between %v and %v", got, all, before, after)
	}
}
