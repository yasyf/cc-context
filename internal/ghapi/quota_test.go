package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

func quotaClient(t *testing.T, handler http.HandlerFunc) (*Client, *ledger) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c := testClient(ts.URL, fixedToken("user-token"))
	c.quota = &ledger{dir: t.TempDir(), now: time.Now}
	return c, c.quota
}

func rateHeader(remaining int, reset time.Time) http.Header {
	return http.Header{
		"X-Ratelimit-Limit":     {"5000"},
		"X-Ratelimit-Remaining": {strconv.Itoa(remaining)},
		"X-Ratelimit-Reset":     {strconv.FormatInt(reset.Unix(), 10)},
	}
}

func rateLimitBody(graphql, core int, reset time.Time) string {
	return fmt.Sprintf(`{"resources":{"core":{"limit":5000,"remaining":%d,"reset":%d},"graphql":{"limit":5000,"remaining":%d,"reset":%d},"integration_manifest":{"limit":5000,"remaining":5000,"reset":%d}}}`,
		core, reset.Unix(), graphql, reset.Unix(), reset.Unix())
}

func TestAdmitRefusesAPoolUnderTheFloorAndNamesItsCallers(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	rateReads := 0
	c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		rateReads++
		_, _ = fmt.Fprint(w, rateLimitBody(Floor-1, Floor, reset))
	})
	ctx := render.WithEnv(t.Context(), "CCX_CALLER=desk-runner.py")
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, CallerName(ctx), "", rateHeader(Floor-1, reset)); err != nil {
		t.Fatal(err)
	}

	err := c.Admit(ctx, IdentityUser, ResourceGraphQL)
	var floor *FloorError
	if !errors.As(err, &floor) {
		t.Fatalf("Admit(user graphql) at %d left = %v, want a *FloorError", Floor-1, err)
	}
	if p := floor.Windows[0]; len(floor.Windows) != 1 || p.Remaining != Floor-1 || !p.ResetAt.Equal(reset) || len(p.Callers) != 1 || p.Callers[0] != (Caller{Caller: "desk-runner.py", Requests: 1}) {
		t.Errorf("refused windows = %+v, want one with %d left until %s and desk-runner.py's one request", floor.Windows, Floor-1, reset)
	}
	if wait, limited := RateLimited(err); !limited || wait <= 0 {
		t.Errorf("RateLimited(floor) = %s, %v; want a wait until the reset", wait, limited)
	}
	if err := c.Admit(ctx, IdentityUser, ResourceCore); err != nil {
		t.Errorf("Admit(user core) at exactly the floor = %v, want it admitted", err)
	}
	if rateReads != 1 {
		t.Errorf("rate_limit read %d times, want once: the refusal reads it, and the core Admit reads the ledger it left", rateReads)
	}
}

func TestAdmitSpillKeepsHalfTheUsersPool(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	for remaining, refused := range map[int]bool{2499: true, 2500: false} {
		c, _ := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, rateLimitBody(remaining, remaining, reset))
		})
		ctx := t.Context()
		if err := c.Admit(ctx, IdentityUser, ResourceCore); err != nil {
			t.Fatalf("Admit at %d left = %v, want it admitted above the floor", remaining, err)
		}
		err := c.AdmitSpill(ctx, ResourceCore)
		var floor *FloorError
		if errors.As(err, &floor) != refused {
			t.Errorf("AdmitSpill at %d left = %v, want refused=%v", remaining, err, refused)
		}
		if refused && floor.Windows[0].Floor != 2500 {
			t.Errorf("refused pool floor = %d, want 2500", floor.Windows[0].Floor)
		}
	}
}

func TestAdmitReadsAPoolPastItsResetAgain(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, rateLimitBody(4999, 5000, time.Now().Add(time.Hour)))
	})
	header := http.Header{}
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Remaining", "3")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(past.Unix(), 10))
	header.Set("X-RateLimit-Resource", ResourceGraphQL)
	if err := l.charge(t.Context(), IdentityUser, ResourceCore, "watch.sh", "", header); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(t.Context(), IdentityUser, ResourceGraphQL); err != nil {
		t.Fatalf("Admit after the pool's reset = %v, want the pool read again and admitted", err)
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pools {
		if p.Resource == ResourceGraphQL && (p.Remaining != 4999 || len(p.Callers) != 0) {
			t.Errorf("graphql pool = %+v, want 4999 left and no callers carried past the reset", p)
		}
	}
}

func TestDoChargesEveryResponseToItsPool(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	c, l := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4200")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set("X-RateLimit-Resource", ResourceCore)
		if r.URL.Path == "/rate_limit" {
			_, _ = fmt.Fprint(w, rateLimitBody(5000, 4200, reset))
			return
		}
		_, _ = fmt.Fprint(w, `{"number":1}`)
	})
	ctx := render.WithEnv(t.Context(), "CCX_CALLER=", "CCI_AGENT_ID=landing-desk")
	for range 2 {
		if _, err := Get[map[string]any](ctx, c, "/repos/o/r/pulls/1"); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pools {
		if p.Resource != ResourceCore {
			continue
		}
		if p.Remaining != 4200 || len(p.Callers) != 1 || p.Callers[0] != (Caller{Caller: "landing-desk", Requests: 2}) {
			t.Errorf("core pool = %+v, want 4200 left and landing-desk charged twice", p)
		}
		return
	}
	t.Errorf("pools = %+v, want a core pool", pools)
}

func TestScriptName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv []string
		want string
	}{
		{argv: []string{"/usr/bin/python3.12", "-u", "/x/desk-runner.py", "--pass"}, want: "desk-runner.py"},
		{argv: []string{"/bin/zsh", "/tmp/watch.sh"}, want: "watch.sh"},
		{argv: []string{"zsh", "-c", "ccx vcs pr status 1"}, want: "zsh"},
		{argv: []string{"uv", "run", "ledger.py"}, want: "ledger.py"},
		{argv: []string{"/Applications/Orca.app/Contents/MacOS/Orca"}, want: "Orca"},
		{want: "unknown"},
	}
	for _, tt := range tests {
		if got := scriptName(tt.argv); got != tt.want {
			t.Errorf("scriptName(%q) = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

func TestResource(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]string{
		"https://api.github.com/graphql":                ResourceGraphQL,
		"/graphql":                                      ResourceGraphQL,
		"https://api.github.com/search/issues?q=x":      ResourceSearch,
		"https://api.github.com/repos/o/r/pulls/1":      ResourceCore,
		"repos/o/r/commits/abc/check-runs?per_page=100": ResourceCore,
	} {
		if got := Resource(target); got != want {
			t.Errorf("Resource(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestAdmitScalesTheFloorToASmallPool(t *testing.T) {
	reset := time.Now().Add(time.Minute).Truncate(time.Second)
	c, _ := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"resources":{"search":{"limit":30,"remaining":%d,"reset":%d}}}`, 30, reset.Unix())
	})
	if err := c.Admit(t.Context(), IdentityUser, ResourceSearch); err != nil {
		t.Errorf("Admit(search) with all 30 left = %v, want it admitted under the pool's own floor of 6", err)
	}
}

func TestNewResponseCanRaiseTheBalanceWithinAWindow(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	l := &ledger{dir: t.TempDir(), now: time.Now}
	for _, remaining := range []string{"4000", "4100"} {
		header := http.Header{}
		header.Set("X-RateLimit-Limit", "5000")
		header.Set("X-RateLimit-Remaining", remaining)
		header.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		if err := l.charge(t.Context(), IdentityUser, ResourceCore, "watch.sh", "", header); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Remaining != 4100 || pools[0].Callers[0].Requests != 2 {
		t.Errorf("pools = %+v, want the latest header balance of 4100 and two requests charged", pools)
	}
}

func TestChargesQueuedBehindAnotherHolderAllLandInOneFlush(t *testing.T) {
	l := &ledger{dir: t.TempDir(), now: time.Now}
	held, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- cache.WithLock(t.Context(), l.dir, "ledger", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	header := http.Header{}
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Remaining", "4000")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const charges = 300
	errs := make(chan error, charges)
	var wg sync.WaitGroup
	for range charges {
		wg.Go(func() { errs <- l.charge(ctx, IdentityUser, ResourceCore, "status.sh", "", header) })
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("charge = %v, want every queued charge recorded", err)
		}
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || len(pools[0].Callers) != 1 || pools[0].Callers[0].Requests != charges {
		t.Errorf("pools = %+v, want %d requests charged", pools, charges)
	}
}

func TestRecordChargesAnotherClientsRequestToItsCallerAndPool(t *testing.T) {
	c, l := quotaClient(t, func(http.ResponseWriter, *http.Request) {})
	t.Setenv("CCX_CALLER", "stack-submit")
	header := http.Header{}
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Remaining", "4000")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if err := c.Record(t.Context(), IdentityUser, "https://api.github.com/graphql", header); err != nil {
		t.Fatal(err)
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Resource != ResourceGraphQL || pools[0].Remaining != 4000 || len(pools[0].Callers) != 1 || pools[0].Callers[0] != (Caller{Caller: "stack-submit", Requests: 1}) {
		t.Errorf("pools = %+v, want one graphql request by stack-submit with 4000 left", pools)
	}
}

func TestLedgerKeepsAFinishedWindowsCallers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	now := start
	l := &ledger{dir: dir, now: func() time.Time { return now }}
	header := func(reset time.Time) http.Header {
		return http.Header{
			"X-Ratelimit-Limit":     {"5000"},
			"X-Ratelimit-Remaining": {"4000"},
			"X-Ratelimit-Reset":     {strconv.FormatInt(reset.Unix(), 10)},
		}
	}
	first, second := start.Add(time.Hour), start.Add(2*time.Hour)
	for _, caller := range []string{"a", "a", "b"} {
		if err := l.charge(t.Context(), IdentityUser, ResourceCore, caller, "", header(first)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryFile)); !os.IsNotExist(err) {
		t.Fatalf("history written before the window closed: %v", err)
	}
	now = first.Add(time.Minute)
	if err := l.charge(t.Context(), IdentityUser, ResourceCore, "c", "", header(second)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, HistoryFile))
	if err != nil {
		t.Fatal(err)
	}
	var window closedWindow
	if err := json.Unmarshal(data, &window); err != nil {
		t.Fatalf("history %q: %v", data, err)
	}
	if window.Pool != poolKey(IdentityUser, ResourceCore) || window.Callers["a"] != 2 || window.Callers["b"] != 1 || !window.ResetAt.Equal(first) {
		t.Errorf("window = %+v", window)
	}
	pools, err := l.load()
	if err != nil {
		t.Fatal(err)
	}
	if live := pools[poolKey(IdentityUser, ResourceCore)].Windows; len(live) != 1 || live[0].Callers["c"] != 1 || live[0].Callers["a"] != 0 {
		t.Errorf("live windows = %+v, want the new window alone with its caller", live)
	}
}

func TestALedgerCountsEachCallersPathsFoldedAcrossPullRequests(t *testing.T) {
	t.Parallel()
	l := &ledger{dir: t.TempDir(), now: time.Now}
	const sha = "a37cf143f3cf1022fcb1c106059b3fb336e44203"
	for _, target := range []string{
		"https://api.github.com/repos/o/r/pulls/1/reviews?per_page=100",
		"/repos/o/r/pulls/2/reviews",
		"/repos/o/r/commits/" + sha + "/status",
		"gh pr view",
	} {
		if err := l.charge(t.Context(), IdentityUser, ResourceCore, "lr-facts", target, rateHeader(4000, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	states, err := l.load()
	if err != nil {
		t.Fatal(err)
	}
	got := states[poolKey(IdentityUser, ResourceCore)].Windows[0].Paths["lr-facts"]
	want := map[string]int{"repos/o/r/pulls/:n/reviews": 2, "repos/o/r/commits/:sha/status": 1, "gh pr view": 1}
	if !maps.Equal(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestFoldPathFoldsRefsSoAPathIsOneRow(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]string{
		"/repos/o/r/compare/dev...yasyf/feature": "repos/o/r/compare/:refs",
		"repos/o/r/branches/yasyf/feature":       "repos/o/r/branches/:ref",
		"graphql":                                "graphql",
	} {
		if got := FoldPath(target); got != want {
			t.Errorf("FoldPath(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestObserveKeepsEachWindowByItsReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 10, 45, 32, 0, time.UTC)
	l := &ledger{dir: t.TempDir(), now: func() time.Time { return now }}
	rateLimit := now.Add(3*time.Minute + 30*time.Second)
	headers := now.Add(8*time.Minute + 12*time.Second)
	for _, reading := range []struct {
		caller    string
		remaining int
		reset     time.Time
	}{
		{"cc-review", 550, headers},
		{"ccx", 3498, rateLimit},
		{"cc-review", 540, headers},
		{"ccx", 3510, rateLimit},
	} {
		if err := l.charge(t.Context(), IdentityUser, ResourceGraphQL, reading.caller, "", rateHeader(reading.remaining, reading.reset)); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 ||
		pools[0].Remaining != 3510 || !pools[0].ResetAt.Equal(rateLimit) || pools[0].Callers[0] != (Caller{Caller: "ccx", Requests: 2}) ||
		pools[1].Remaining != 540 || !pools[1].ResetAt.Equal(headers) || pools[1].Callers[0] != (Caller{Caller: "cc-review", Requests: 2}) {
		t.Errorf("pools = %+v, want latest 3510 left until %s for ccx and 540 left until %s for cc-review", pools, rateLimit, headers)
	}
}

func TestAdmitUsesLatestResponseRatherThanLowestRegionalWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	olderReset, latestReset := now.Add(8*time.Minute), now.Add(3*time.Minute)
	rateReads := 0
	c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		rateReads++
		_, _ = fmt.Fprint(w, rateLimitBody(5000, 5000, olderReset))
	})
	l.now = func() time.Time { return now }
	ctx := t.Context()
	for _, observation := range []struct {
		remaining int
		reset     time.Time
	}{{0, olderReset}, {373, latestReset}} {
		now = now.Add(time.Second)
		if err := l.charge(ctx, IdentityUser, ResourceGraphQL, "watch", "", rateHeader(observation.remaining, observation.reset)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Admit(ctx, IdentityUser, ResourceGraphQL); err != nil {
		t.Fatalf("healthy latest headers refused by an older regional window: %v", err)
	}
	now = now.Add(time.Second)
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, "watch", "", rateHeader(0, latestReset)); err != nil {
		t.Fatal(err)
	}
	if err := c.Sent(ctx, IdentityUser, []string{"https://api.github.com/graphql"}); err != nil {
		t.Fatal(err)
	}
	pools, err := c.Pools(ctx, IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	under := 0
	for _, pool := range pools {
		if pool.Remaining < pool.Floor {
			under++
		}
	}
	if under != 1 {
		t.Fatalf("status reports %d refusing windows, want only the current response", under)
	}
	var floor *FloorError
	if err := c.Admit(ctx, IdentityUser, ResourceGraphQL); !errors.As(err, &floor) || !floor.Until().Equal(latestReset) {
		t.Fatalf("exhausted latest response should hold until %s: %v", latestReset, err)
	}
	if rateReads != 1 {
		t.Fatalf("rate_limit reads = %d, want only the explicit Pools read", rateReads)
	}
	now = latestReset.Add(time.Second)
	if err := c.Admit(ctx, IdentityUser, ResourceGraphQL); err != nil {
		t.Fatalf("expired response should refresh and admit: %v", err)
	}
}

func TestBackgroundReserveLeavesForegroundAndOtherResourcesAvailable(t *testing.T) {
	for _, resource := range []string{ResourceCore, ResourceGraphQL, ResourceSearch} {
		t.Run(resource, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, rateLimitBody(5000, 5000, now.Add(time.Hour)))
			})
			l.now = func() time.Time { return now }
			header := rateHeader(99, now.Add(time.Hour))
			limit, remaining := 5000, 99
			if resource == ResourceSearch {
				limit, remaining = 30, 0
				header.Set("X-RateLimit-Limit", "30")
				header.Set("X-RateLimit-Remaining", "0")
			}
			ctx := t.Context()
			if err := l.charge(ctx, IdentityUser, resource, "watch", "", header); err != nil {
				t.Fatal(err)
			}
			if err := c.Admit(BackgroundQuota(ctx), IdentityUser, resource); !errors.As(err, new(*FloorError)) {
				t.Fatalf("background admitted %d/%d: %v", remaining, limit, err)
			}
			if remaining > 0 {
				if err := c.Admit(ctx, IdentityUser, resource); err != nil {
					t.Fatalf("foreground refused remaining quota: %v", err)
				}
			}
			other := ResourceCore
			if resource == other {
				other = ResourceGraphQL
			}
			if err := c.Admit(ctx, IdentityUser, other); err != nil {
				t.Fatalf("other resource refused: %v", err)
			}
		})
	}
}

func TestRateLimitReplacesABalanceHeaderlessChargesWore(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(10 * time.Minute)
	c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, rateLimitBody(4000, 5000, reset))
	})
	l.now = func() time.Time { return now }
	ctx := t.Context()
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, "ccx", "", rateHeader(Floor, reset)); err != nil {
		t.Fatal(err)
	}
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, "gh-shim", "gh pr view", http.Header{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Admit(ctx, IdentityUser, ResourceGraphQL); err != nil {
		t.Errorf("Admit with rate_limit at 4000 after charges wore the ledger to %d = %v, want rate_limit's balance to admit", Floor-1, err)
	}
}

func TestAHeaderlessChargeLandsOnTheNewestWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 10, 45, 0, 0, time.UTC)
	l := &ledger{dir: t.TempDir(), now: func() time.Time { return now }}
	ctx := t.Context()
	if err := l.charge(ctx, IdentityUser, ResourceCore, "watch.sh", "", http.Header{}); err != nil {
		t.Fatal(err)
	}
	if err := l.charge(ctx, IdentityUser, ResourceCore, "ccx", "", rateHeader(3000, now.Add(4*time.Minute))); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := l.charge(ctx, IdentityUser, ResourceCore, "ccx", "", rateHeader(2000, now.Add(8*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := l.charge(ctx, IdentityUser, ResourceCore, "gh-shim", "gh pr view", http.Header{}); err != nil {
		t.Fatal(err)
	}
	pools, err := l.pools(IdentityUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 || pools[0].Remaining != 3000 || len(pools[0].Callers) != 1 ||
		pools[1].Remaining != 1999 || len(pools[1].Callers) != 2 || pools[1].Callers[1] != (Caller{Caller: "gh-shim", Requests: 1}) {
		t.Errorf("pools = %+v, want gh-shim's request on the window read last, taking it to 1999, and watch.sh's, before any window, on none", pools)
	}
}

func TestAnOpenWindowFromHeadersSparesRateLimitReads(t *testing.T) {
	rateReads := 0
	c, l := quotaClient(t, func(w http.ResponseWriter, _ *http.Request) {
		rateReads++
		_, _ = fmt.Fprint(w, rateLimitBody(5000, 5000, time.Now().Add(time.Hour)))
	})
	ctx := t.Context()
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, "cc-review", "", rateHeader(4000, time.Now().Add(30*time.Minute))); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.Admit(ctx, IdentityUser, ResourceGraphQL); err != nil {
			t.Fatal(err)
		}
	}
	if rateReads != 0 {
		t.Errorf("rate_limit read %d times, want none while a window read under a minute ago has 4000 left", rateReads)
	}
}

func TestHoldDoublesWhileRefusalsFollowItAndKeepsTheLongerNamedWait(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	minute := hold{Until: now.Add(time.Minute), Backoff: time.Minute}
	for _, tt := range []struct {
		name  string
		h     hold
		at    time.Time
		wait  time.Duration
		named bool
		want  hold
	}{
		{name: "a first refusal waits a minute", at: now, want: minute},
		{name: "a refusal inside the hold is one it counts", h: minute, at: now.Add(30 * time.Second), want: minute},
		{name: "a refusal within a backoff of the lift doubles it", h: minute, at: now.Add(90 * time.Second), want: hold{Until: now.Add(210 * time.Second), Backoff: 2 * time.Minute}},
		{name: "a refusal long after the lift waits a minute again", h: hold{Until: now, Backoff: 8 * time.Minute}, at: now.Add(time.Hour), want: hold{Until: now.Add(61 * time.Minute), Backoff: time.Minute}},
		{name: "a named wait holds until it", at: now, wait: 5 * time.Minute, named: true, want: hold{Until: now.Add(5 * time.Minute)}},
		{name: "a named wait never shortens the hold", h: minute, at: now, wait: 10 * time.Second, named: true, want: minute},
	} {
		if got := tt.h.refused(tt.at, tt.wait, tt.named); got != tt.want {
			t.Errorf("%s: refused = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func secondaryLimitServer(t *testing.T, refusals int32, retryAfter string) (*Client, *ledger, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	reset := time.Now().Add(time.Hour)
	c, l := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			_, _ = fmt.Fprint(w, rateLimitBody(5000, 5000, reset))
			return
		}
		if requests.Add(1) <= refusals {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"number":1}`)
	})
	return c, l, &requests
}

func TestDoHoldsTheIdentityOnASecondaryLimitAndSendsAgainOnceItLifts(t *testing.T) {
	t.Parallel()
	c, _, requests := secondaryLimitServer(t, 1, "1")
	var said bytes.Buffer
	start := time.Now()
	got, err := Get[map[string]any](render.WithSlowWriter(t.Context(), &said), c, "/repos/o/r/pulls/1")
	if err != nil {
		t.Fatal(err)
	}
	if got["number"] != float64(1) || requests.Load() != 2 {
		t.Errorf("Get = %v after %d requests, want the second request's answer", got, requests.Load())
	}
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("Get sent again after %s, want after the 1s Retry-After", waited)
	}
	if n := strings.Count(said.String(), HoldNotice); n != 1 {
		t.Errorf("Get said %q, want one hold notice", said.String())
	}
}

func TestUnwaitingHandsASecondaryLimitBackAndLeavesTheHoldForEveryoneElse(t *testing.T) {
	t.Parallel()
	c, l, requests := secondaryLimitServer(t, 2, "600")
	for range 2 {
		if _, err := Get[map[string]any](t.Context(), c.Unwaiting(), "/repos/o/r/pulls/1"); err == nil {
			t.Fatal("Get = nil, want the secondary limit's refusal")
		}
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want each Unwaiting call sent at once", n)
	}
	pools, err := l.load()
	if err != nil {
		t.Fatal(err)
	}
	for key, p := range pools {
		if strings.HasSuffix(key, ":"+ResourceSecondary) && p.Hold != nil && p.Hold.Until.After(time.Now().Add(9*time.Minute)) {
			return
		}
	}
	t.Errorf("ledger = %+v, want a hold until the 600s Retry-After", pools)
}
