package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeReviewsFakes(t *testing.T, dir string) {
	t.Helper()
	gh := `#!/bin/sh
{ printf 'gh\0'; for a in "$@"; do printf '%s\0' "$a"; done; printf '\0'; } >> "$SHIP_LOG"

case "$1 $2" in
  "pr view")
    if [ -n "$GH_PR_NOT_FOUND" ]; then
      printf 'no pull requests found for branch "missing"\n' >&2
      exit 1
    fi
    operand=
    if [ "$3" != "--json" ]; then operand=$3; fi
    key=$operand
    if [ -z "$key" ]; then key=DEFAULT; fi
    eval 'open_json=${GH_PR_VIEW_JSON_'"$key"'-}'
    if [ -z "$open_json" ]; then open_json=$GH_PR_VIEW_JSON; fi
    eval 'marker=${GH_PR_VIEW_MARKER_'"$key"'-}'
    if [ -z "$marker" ]; then
      printf '%s' "$open_json"
      exit 0
    fi
    count=0
    if [ -r "$marker" ]; then IFS= read -r count < "$marker" || :; fi
    count=${count:-0}
    count=$((count + 1))
    printf '%s' "$count" > "$marker"
    eval 'open_calls=${GH_PR_VIEW_OPEN_CALLS_'"$key"'-0}'
    if [ "$count" -le "$open_calls" ]; then
      printf '%s' "$open_json"
    else
      eval 'fail_after=${GH_PR_VIEW_FAIL_AFTER_'"$key"'-}'
      if [ -n "$fail_after" ]; then
        printf 'no pull requests found for branch "%s"\n' "$key" >&2
        exit 1
      fi
      eval 'done_json=${GH_PR_VIEW_DONE_JSON_'"$key"'-}'
      printf '%s' "$done_json"
    fi
    ;;
  "api --paginate")
    path=$3
    case "$path" in
      */pulls/*) rest=${path#*/pulls/}; num=${rest%%/*} ;;
      */issues/*) rest=${path#*/issues/}; num=${rest%%/*} ;;
      *) num= ;;
    esac
    eval 'fail_marker=${GH_API_FAIL_MARKER_'"$num"'-}'
    if [ -z "$fail_marker" ]; then fail_marker=$GH_API_FAIL_MARKER; fi
    if [ -n "$fail_marker" ]; then
      count=0
      if [ -r "$fail_marker" ]; then IFS= read -r count < "$fail_marker" || :; fi
      count=${count:-0}
      if [ "$count" -gt 0 ]; then
        count=$((count - 1))
        printf '%s' "$count" > "$fail_marker"
        printf 'gh: transient network timeout\n' >&2
        exit 1
      fi
    fi

    feed=
    case "$path" in
      */pulls/*/comments*) feed=INLINE ;;
      */issues/*/comments*) feed=COMMENTS ;;
      */pulls/*/reviews*) feed=REVIEWS ;;
    esac
    cycle=1
    if [ -n "$GH_API_CYCLE_MARKER" ]; then
      count=0
      if [ -r "$GH_API_CYCLE_MARKER" ]; then IFS= read -r count < "$GH_API_CYCLE_MARKER" || :; fi
      count=${count:-0}
      if [ "$feed" = INLINE ]; then
        count=$((count + 1))
        printf '%s' "$count" > "$GH_API_CYCLE_MARKER"
      fi
      cycle=$count
    fi
    var=GH_${feed}_JSON
    if [ "$cycle" -gt "${GH_API_SWITCH_AFTER:-999999}" ]; then var=${var}_2; fi
    if [ -z "$feed" ]; then var=GH_API_JSON; fi
    eval 'json=${'"$var"'-}'
    printf '%s' "$json"
    ;;
  *)
    printf 'fake gh: unmatched argv: %s\n' "$*" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o700); err != nil { //nolint:gosec // fake executable must be owner-executable
		t.Fatalf("write fake gh: %v", err)
	}
}

func setupReviews(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o750); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeReviewsFakes(t, binDir)
	log := filepath.Join(dir, "reviews.log")
	t.Setenv("PATH", binDir)
	t.Setenv("SHIP_LOG", log)
	t.Setenv(envReviewsPollInterval, "1ms")
	t.Setenv("GH_INLINE_JSON", "[]")
	t.Setenv("GH_COMMENTS_JSON", "[]")
	t.Setenv("GH_REVIEWS_JSON", "[]")
	return log
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

func readReviewsInvocations(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var got [][]string
	for _, record := range strings.Split(string(data), "\x00\x00") {
		record = strings.Trim(record, "\x00")
		if record != "" {
			got = append(got, strings.Split(record, "\x00"))
		}
	}
	return got
}

func reviewsViewJSON(number int, state string, merged bool) string {
	mergedAt := "null"
	if merged {
		mergedAt = `"2026-07-20T19:00:00Z"`
	}
	return fmt.Sprintf(
		`{"number":%d,"state":%q,"url":"https://github.com/acme/repo/pull/%d","mergedAt":%s}`,
		number, state, number, mergedAt,
	)
}

func setReviewsTransition(t *testing.T, number, openCalls int, state string, merged bool) {
	t.Helper()
	key := fmt.Sprintf("%d", number)
	marker := filepath.Join(t.TempDir(), "view.marker")
	t.Setenv("GH_PR_VIEW_JSON_"+key, reviewsViewJSON(number, "OPEN", false))
	t.Setenv("GH_PR_VIEW_MARKER_"+key, marker)
	t.Setenv("GH_PR_VIEW_OPEN_CALLS_"+key, fmt.Sprintf("%d", openCalls))
	t.Setenv("GH_PR_VIEW_DONE_JSON_"+key, reviewsViewJSON(number, state, merged))
}

func TestReviewsStreamsNewComment(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 1, "MERGED", true)
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":101,
		"body":"hello",
		"user":{"login":"alice"},
		"html_url":"https://github.com/acme/repo/pull/7#issuecomment-101",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	want := "" +
		"watching pr#7 · https://github.com/acme/repo/pull/7 · poll 1ms\n" +
		"◆ comment · alice · pr#7 · 2026-07-20T18:01:00Z\n" +
		"  hello\n" +
		"↳ https://github.com/acme/repo/pull/7#issuecomment-101 · id 101\n\n" +
		"◆ pr#7 merged · https://github.com/acme/repo/pull/7\n\n" +
		"watch done · 1 merged · 0 closed\n"
	if got != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestReviewsDedupesAcrossPolls(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 2, "MERGED", true)
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":101,
		"body":"once",
		"user":{"login":"alice"},
		"html_url":"https://github.com/acme/repo/pull/7#issuecomment-101",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if count := strings.Count(got, "id 101"); count != 1 {
		t.Errorf("event count = %d, want 1\n%s", count, got)
	}
}

func TestReviewsAllKindsSortedAndSuppressed(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 1, "MERGED", true)
	t.Setenv("GH_INLINE_JSON", `[{
		"id":301,
		"body":"inline body",
		"user":{"login":"inline-author"},
		"path":"internal/cli/reviews.go",
		"line":null,
		"html_url":"https://github.com/acme/repo/pull/7#discussion_r301",
		"created_at":"2026-07-20T18:03:00Z",
		"updated_at":"2026-07-20T18:03:00Z"
	}]`)
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":201,
		"body":"issue body",
		"user":{"login":"commenter"},
		"html_url":"https://github.com/acme/repo/pull/7#issuecomment-201",
		"created_at":"2026-07-20T18:02:00Z",
		"updated_at":"2026-07-20T18:02:00Z"
	}]`)
	t.Setenv("GH_REVIEWS_JSON", `[
		{"id":401,"state":"PENDING","body":"draft","user":{"login":"draft"},"html_url":"https://example/401","submitted_at":null},
		{"id":402,"state":"COMMENTED","body":"","user":{"login":"container"},"html_url":"https://example/402","submitted_at":"2026-07-20T18:00:00Z"},
		{"id":403,"state":"CHANGES_REQUESTED","body":"please fix","user":{"login":"reviewer"},"html_url":"https://example/403","submitted_at":"2026-07-20T18:01:00Z"},
		{"id":404,"state":"APPROVED","body":"","user":{"login":"approver"},"html_url":"https://example/404","submitted_at":"2026-07-20T18:04:00Z"}
	]`)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	want := "" +
		"watching pr#7 · https://github.com/acme/repo/pull/7 · poll 1ms\n" +
		"◆ review · reviewer · pr#7 · changes_requested · 2026-07-20T18:01:00Z\n" +
		"  please fix\n" +
		"↳ https://example/403 · id 403\n" +
		"↳ triage: spawn the cc-context:pr-review-triage agent with pr#7 and review id 403\n\n" +
		"◆ comment · commenter · pr#7 · 2026-07-20T18:02:00Z\n" +
		"  issue body\n" +
		"↳ https://github.com/acme/repo/pull/7#issuecomment-201 · id 201\n\n" +
		"◆ inline · inline-author · pr#7 · internal/cli/reviews.go (outdated) · 2026-07-20T18:03:00Z\n" +
		"  inline body\n" +
		"↳ https://github.com/acme/repo/pull/7#discussion_r301 · id 301\n\n" +
		"◆ review · approver · pr#7 · approved · 2026-07-20T18:04:00Z\n" +
		"↳ https://example/404 · id 404\n\n" +
		"◆ pr#7 merged · https://github.com/acme/repo/pull/7\n\n" +
		"watch done · 1 merged · 0 closed\n"
	if got != want {
		t.Errorf("output mismatch\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "id 401") || strings.Contains(got, "id 402") {
		t.Errorf("suppressed review emitted:\n%s", got)
	}
}

func TestReviewsEditedReemit(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 2, "MERGED", true)
	cycle := filepath.Join(t.TempDir(), "api.marker")
	t.Setenv("GH_API_CYCLE_MARKER", cycle)
	t.Setenv("GH_API_SWITCH_AFTER", "1")
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":201,
		"body":"first",
		"user":{"login":"commenter"},
		"html_url":"https://example/201",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)
	t.Setenv("GH_COMMENTS_JSON_2", `[{
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
			setupReviews(t)
			setReviewsTransition(t, 7, 1, tt.state, tt.merged)

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
	log := setupReviews(t)
	setReviewsTransition(t, 1, 1, "MERGED", true)
	setReviewsTransition(t, 2, 2, "CLOSED", false)

	got, err := runReviewsCmd(t, "1", "2", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.HasSuffix(got, "watch done · 1 merged · 1 closed\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
	apiCounts := map[string]int{}
	for _, invocation := range readReviewsInvocations(t, log) {
		if len(invocation) == 4 && invocation[1] == "api" {
			path := invocation[3]
			switch {
			case strings.Contains(path, "/pulls/1/"), strings.Contains(path, "/issues/1/"):
				apiCounts["1"]++
			case strings.Contains(path, "/pulls/2/"), strings.Contains(path, "/issues/2/"):
				apiCounts["2"]++
			}
		}
	}
	want := map[string]int{"1": 3, "2": 6}
	if !reflect.DeepEqual(apiCounts, want) {
		t.Errorf("API counts = %v, want %v", apiCounts, want)
	}
}

func TestReviewsTransientFailureTolerance(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 1, "MERGED", true)
	failures := filepath.Join(t.TempDir(), "failures.marker")
	if err := os.WriteFile(failures, []byte("3"), 0o600); err != nil {
		t.Fatalf("write failures marker: %v", err)
	}
	t.Setenv("GH_API_FAIL_MARKER", failures)

	got, err := runReviewsCmd(t, "7", "--since", "all")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	if !strings.HasSuffix(got, "watch done · 1 merged · 0 closed\n") {
		t.Errorf("completion summary mismatch:\n%s", got)
	}
}

func TestReviewsAbortsAfterMaxFailures(t *testing.T) {
	setupReviews(t)
	t.Setenv("GH_PR_VIEW_JSON_7", reviewsViewJSON(7, "OPEN", false))
	failures := filepath.Join(t.TempDir(), "failures.marker")
	if err := os.WriteFile(failures, []byte("5"), 0o600); err != nil {
		t.Fatalf("write failures marker: %v", err)
	}
	t.Setenv("GH_API_FAIL_MARKER", failures)

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
// merges normally) alongside one persistently-failing target (pr#2, fails
// every poll via its own GH_API_FAIL_MARKER_2): pr#1's events must still be
// delivered, and only pr#2 aborts.
func TestReviewsMultiPRPartialFailureIsolation(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 1, 1, "MERGED", true)
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":101,
		"body":"healthy event",
		"user":{"login":"alice"},
		"html_url":"https://github.com/acme/repo/pull/1#issuecomment-101",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)
	t.Setenv("GH_PR_VIEW_JSON_2", reviewsViewJSON(2, "OPEN", false))
	failures := filepath.Join(t.TempDir(), "pr2-failures.marker")
	if err := os.WriteFile(failures, []byte("5"), 0o600); err != nil {
		t.Fatalf("write failures marker: %v", err)
	}
	t.Setenv("GH_API_FAIL_MARKER_2", failures)

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

func TestReviewsBudgetCapFooterIndented(t *testing.T) {
	setupReviews(t)
	setReviewsTransition(t, 7, 1, "MERGED", true)
	t.Setenv("GH_COMMENTS_JSON", `[{
		"id":201,
		"body":"1234\n5678\n90",
		"user":{"login":"commenter"},
		"html_url":"https://example/201",
		"created_at":"2026-07-20T18:00:00Z",
		"updated_at":"2026-07-20T18:01:00Z"
	}]`)

	got, err := runReviewsCmd(t, "7", "--since", "all", "--budget", "2")
	if err != nil {
		t.Fatalf("reviews error = %v", err)
	}
	wantBody := "  1234\n  … +2 lines, ~2 tokens omitted — re-run with a larger --budget\n"
	if !strings.Contains(got, wantBody) {
		t.Errorf("capped body missing or mis-indented:\n%s", got)
	}
}

func TestReviewsResolution(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantArg string
	}{
		{"number", []string{"7"}, "7"},
		{"branch", []string{"feature/reviews"}, "feature/reviews"},
		{"current branch", nil, "--json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := setupReviews(t)
			t.Setenv("GH_PR_VIEW_JSON", reviewsViewJSON(7, "OPEN", false))
			if tt.wantArg == "7" {
				setReviewsTransition(t, 7, 1, "MERGED", true)
			} else {
				setReviewsTransition(t, 7, 0, "MERGED", true)
			}

			args := append([]string{}, tt.args...)
			args = append(args, "--since", "all")
			if _, err := runReviewsCmd(t, args...); err != nil {
				t.Fatalf("reviews error = %v", err)
			}
			invocations := readReviewsInvocations(t, log)
			if len(invocations) == 0 || len(invocations[0]) < 4 {
				t.Fatalf("first invocation = %v", invocations)
			}
			if got := invocations[0][3]; got != tt.wantArg {
				t.Errorf("resolution operand = %q, want %q; invocation=%v", got, tt.wantArg, invocations[0])
			}
		})
	}
}

func TestReviewsNotFoundExitCode(t *testing.T) {
	setupReviews(t)
	t.Setenv("GH_PR_NOT_FOUND", "1")

	_, err := runReviewsCmd(t, "missing")
	if err == nil {
		t.Fatal("reviews error = nil, want not found")
	}
	if code := ExitCode(err); code != 3 {
		t.Errorf("ExitCode(error) = %d, want 3; error=%v", code, err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want wrapped ErrNotFound", err)
	}
}

func TestReviewsSincePropagationAndWatermark(t *testing.T) {
	log := setupReviews(t)
	setReviewsTransition(t, 7, 2, "MERGED", true)
	t.Setenv("GH_COMMENTS_JSON", `[{
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
	for _, invocation := range readReviewsInvocations(t, log) {
		if len(invocation) == 4 && invocation[1] == "api" &&
			strings.Contains(invocation[3], "/pulls/7/comments") {
			inlinePaths = append(inlinePaths, invocation[3])
		}
	}
	want := []string{
		"repos/{owner}/{repo}/pulls/7/comments?per_page=100&since=2026-07-20T18:00:00Z",
		"repos/{owner}/{repo}/pulls/7/comments?per_page=100&since=2026-07-20T18:01:00Z",
	}
	if !reflect.DeepEqual(inlinePaths, want) {
		t.Errorf("inline API paths = %v, want %v", inlinePaths, want)
	}
}

func TestReviewsCancelSummary(t *testing.T) {
	setupReviews(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	targets := []*prTarget{
		{
			Number:    7,
			URL:       "https://github.com/acme/repo/pull/7",
			watermark: time.Date(2026, 7, 20, 18, 1, 0, 0, time.UTC),
			seen:      map[string]time.Time{},
		},
	}
	var out bytes.Buffer

	err := watchReviews(ctx, &out, targets, reviewsOpts{interval: time.Hour, all: true})
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

// TestReviewsStackNoTargets drives --stack from trunk (stackBranches returns
// no branches), which must refuse rather than silently reporting a 0/0 watch.
func TestReviewsStackNoTargets(t *testing.T) {
	setupShipGT(t, false)
	t.Setenv("GIT_BRANCH", "main")
	t.Setenv("GT_STATE_JSON", `{"main":{"trunk":true}}`)

	_, err := runReviewsCmd(t, "--stack")
	wantErr := "reviews: --stack found no stacked branches — run it from a stacked branch, not trunk"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("reviews --stack error = %v, want %q", err, wantErr)
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

func TestGhPagesConcatenatedArrays(t *testing.T) {
	setupReviews(t)
	t.Setenv("GH_API_JSON", `[{"id":1,"body":"one"}][{"id":2,"body":"two"}]`)

	got, err := ghPages[ghPRComment](context.Background(), "pages")
	if err != nil {
		t.Fatalf("ghPages error = %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("ghPages = %#v, want ids 1,2", got)
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
