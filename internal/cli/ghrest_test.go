package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

type ghRoute struct {
	argv   []string
	stdout string
}

func ghRouteKey(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\n") + "\n"))
	return hex.EncodeToString(sum[:])[:16]
}

func installGHRoutes(t *testing.T, routes ...ghRoute) string {
	t.Helper()
	dir := t.TempDir()
	for _, r := range routes {
		if err := os.WriteFile(filepath.Join(dir, ghRouteKey(r.argv)+".out"), []byte(r.stdout), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + dir + `/calls"
key=$(printf '%s\n' "$@" | shasum -a 256 | cut -c1-16)
if [ -r "` + dir + `/$key.out" ]; then cat "` + dir + `/$key.out"; exit 0; fi
printf 'gh: Not Found (HTTP 404): %s\n' "$*" >&2
exit 1
`
	writeShipExecutable(t, dir, "gh", script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls")
}

// ghHeadPRsRoute answers the batched head read for branches with one
// repository object per alias, in branches' order; an empty node list is a
// branch with no pull request.
func ghHeadPRsRoute(t *testing.T, branches []string, nodes ...string) ghRoute {
	t.Helper()
	fields := make([]string, len(branches))
	for i := range branches {
		fields[i] = fmt.Sprintf("%q: {\"nodes\": [%s]}", headPRAlias(i), nodes[i])
	}
	return ghRoute{argv: append([]string{"api"}, ghHeadPRsArgv(branches)...), stdout: `{"data": {"repository": {` + strings.Join(fields, ", ") + `}}}`}
}

func TestStackQueryPRsReadsTheStackInOneGraphQLCall(t *testing.T) {
	branches := []string{"merged", "abandoned", "open", "none"}
	calls := installGHRoutes(t, ghHeadPRsRoute(t, branches,
		`{"number": 3, "url": "u3", "state": "MERGED", "mergedAt": "2026-09-25T00:00:00Z", "baseRefName": "main", "headRefOid": "h3", "mergeable": "UNKNOWN", "labels": {"nodes": []}, "timelineItems": {"nodes": []}}`,
		`{"number": 64, "url": "u64", "state": "CLOSED", "mergedAt": null, "baseRefName": "main", "headRefOid": "h64", "mergeable": "UNKNOWN", "labels": {"nodes": []}, "timelineItems": {"nodes": [{"actor": {"login": "someone"}}]}}`,
		`{"number": 13982, "url": "u13982", "title": "open one", "body": "b", "state": "OPEN", "mergedAt": null, "baseRefName": "base", "headRefOid": "h13982", "mergeable": "CONFLICTING", "labels": {"nodes": [{"name": "merge"}]}, "timelineItems": {"nodes": []}}`,
		``,
	))
	prs, err := stackQueryPRs(context.Background(), render.Dir(t.TempDir()), "main", branches)
	if err != nil {
		t.Fatalf("stackQueryPRs: %v", err)
	}
	type want struct {
		number    int
		state     string
		mergeable string
		landed    bool
	}
	for branch, w := range map[string]want{
		"merged":    {3, "MERGED", statusUnknown, true},
		"abandoned": {64, "CLOSED", statusUnknown, false},
		"open":      {13982, "OPEN", "CONFLICTING", false},
	} {
		pr := prs[branch]
		if pr == nil {
			t.Errorf("%s: no pull request", branch)
			continue
		}
		if pr.Number != w.number || pr.State != w.state || pr.Mergeable != w.mergeable || pr.Landed != w.landed {
			t.Errorf("%s = #%d %s %s landed=%v, want #%d %s %s landed=%v",
				branch, pr.Number, pr.State, pr.Mergeable, pr.Landed, w.number, w.state, w.mergeable, w.landed)
		}
	}
	if pr := prs["open"]; pr != nil && (pr.Head != "h13982" || pr.Base != "base" || pr.URL != "u13982" || pr.Title != "open one" || pr.Body != "b" || !slices.Equal(pr.Labels, []string{"merge"})) {
		t.Errorf("open PR = %+v, want every field of its node", pr)
	}
	if pr, ok := prs["none"]; ok {
		t.Errorf("none resolved to %+v", pr)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(data), "api "); calls != 1 {
		t.Errorf("gh ran %d times, want one batched call:\n%s", calls, data)
	}
}

func TestStackQueryPRsResolvesTheQueueClose(t *testing.T) {
	branches := []string{"stack-rebase-per-root"}
	closedByQueue := `{"number": 64, "url": "u64", "state": "CLOSED", "mergedAt": null, "baseRefName": "main", "headRefOid": "h64", "mergeable": "UNKNOWN", "labels": {"nodes": []}, "timelineItems": {"nodes": [{"actor": {"login": "graphite-app"}}]}}`
	for _, tt := range []struct {
		name     string
		comments string
		landed   bool
	}{
		{"queue landed it", `[[{"user":{"login":"graphite-app[bot]"},"body":"### Merge activity\\n\\n* **Sep 25**: Merged by the Graphite merge queue."}]]`, true},
		{"queue dropped it", `[[{"user":{"login":"graphite-app[bot]"},"body":"### Merge activity\\n\\n* **Sep 25**: removed from the merge queue."}]]`, false},
		{"queue left no comment", loadGHGolden(t, "rest-issue-comments").stdout, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installGHRoutes(t, ghHeadPRsRoute(t, branches, closedByQueue),
				ghRoute{argv: []string{"api", "--paginate", "--slurp", "repos/{owner}/{repo}/issues/64/comments?per_page=100"}, stdout: tt.comments},
			)
			prs, err := stackQueryPRs(context.Background(), render.Dir(t.TempDir()), "main", branches)
			if err != nil {
				t.Fatalf("stackQueryPRs: %v", err)
			}
			if got := prs["stack-rebase-per-root"].Landed; got != tt.landed {
				t.Errorf("landed = %v, want %v", got, tt.landed)
			}
		})
	}
}

// ghRateLimitedGH installs a gh whose pull request reads answer GitHub's rate
// limit refusal the first refusals times, then an empty list. Each refusal
// logs, as GH_DEBUG=api does, a response whose X-RateLimit-Remaining is
// remaining, and rate_limit answers a full quota, as it does on the token that
// misreports it.
func ghRateLimitedGH(t *testing.T, f *vcstest.Fixture, refusals int, remaining string) {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	script := "#!/bin/sh\n" + vcstest.RecordArgv("gh") + fmt.Sprintf(`if [ "$2" = rate_limit ]; then echo 5000; exit 0; fi
[ "$GH_DEBUG" = api ] || { echo "gh api ran without GH_DEBUG=api" >&2; exit 2; }
n=$(cat %q 2>/dev/null || echo 0); echo $((n+1)) > %q
if [ "$n" -lt %d ]; then
	printf '* Request at now\n< HTTP/2.0 403 Forbidden\n< X-Ratelimit-Remaining: %s\n\n* Request took 1ms\n' >&2
	echo "gh: API rate limit exceeded for user ID 1. (HTTP 403)" >&2
	exit 1
fi
echo '[]'
`, count, count, refusals, remaining)
	execstub.Write(t, filepath.Join(f.ShimBin, "gh"), script)
	prev := ghRateLimitWait
	ghRateLimitWait = 0
	t.Cleanup(func() { ghRateLimitWait = prev })
}

// TestGHAPIWaitsOutASecondaryRateLimit is the stack submit that failed on a 403
// GitHub answers for request rate while the REST quota still had room.
func TestGHAPIWaitsOutASecondaryRateLimit(t *testing.T) {
	f := shipRepo(t)
	ghRateLimitedGH(t, f, 2, "4990")

	if _, err := ghAPI(f.Context(), render.Dir(f.Dir), "repos/{owner}/{repo}/pulls"); err != nil {
		t.Fatalf("ghAPI = %v, want an answer after waiting out the limit", err)
	}
}

func TestGHAPIFailsAnExhaustedQuotaAtOnce(t *testing.T) {
	f := shipRepo(t)
	ghRateLimitedGH(t, f, 1, "0")

	_, err := ghAPI(f.Context(), render.Dir(f.Dir), "repos/{owner}/{repo}/pulls")
	if err == nil || !strings.HasSuffix(err.Error(), "exit status 1: gh: API rate limit exceeded for user ID 1. (HTTP 403)") {
		t.Fatalf("ghAPI error = %v, want only gh's rate limit refusal", err)
	}
}

func TestGHRateLimitDelay(t *testing.T) {
	t.Parallel()
	const page = "* Request to https://api.github.com/x?page=1\n< HTTP/2.0 200 OK\n< X-Ratelimit-Remaining: 0\n\n[]\n* Request took 9ms\n"
	tests := []struct {
		name   string
		stderr string
		delay  time.Duration
		ok     bool
	}{
		{"retry-after", page + "< HTTP/2.0 403 Forbidden\n< Retry-After: 42\n< X-Ratelimit-Remaining: 4146\n", 42 * time.Second, true},
		{"quota left", page + "< HTTP/2.0 403 Forbidden\n< X-Ratelimit-Remaining: 4146\n", time.Minute, true},
		{"quota exhausted", "< HTTP/2.0 403 Forbidden\n< X-Ratelimit-Remaining: 0\n", 0, false},
		{"no response logged", "gh: API rate limit exceeded", time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			delay, ok := ghRateLimitDelay(ghDebugLastResponse(tt.stderr), time.Minute)
			if delay != tt.delay || ok != tt.ok {
				t.Errorf("ghRateLimitDelay = %v, %v; want %v, %v", delay, ok, tt.delay, tt.ok)
			}
		})
	}
}
