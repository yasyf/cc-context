package ghapi

import (
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
	"sync"
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
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, CallerName(ctx), "", http.Header{}); err != nil {
		t.Fatal(err)
	}

	err := c.Admit(ctx, IdentityUser, ResourceGraphQL)
	var floor *FloorError
	if !errors.As(err, &floor) {
		t.Fatalf("Admit(user graphql) at %d left = %v, want a *FloorError", Floor-1, err)
	}
	if p := floor.Pool; p.Remaining != Floor-1 || !p.ResetAt.Equal(reset) || len(p.Callers) != 1 || p.Callers[0] != (Caller{Caller: "desk-runner.py", Requests: 1}) {
		t.Errorf("refused pool = %+v, want %d left until %s with desk-runner.py's one request", p, Floor-1, reset)
	}
	if wait, limited := RateLimited(err); !limited || wait <= 0 {
		t.Errorf("RateLimited(floor) = %s, %v; want a wait until the reset", wait, limited)
	}
	if err := c.Admit(ctx, IdentityUser, ResourceCore); err != nil {
		t.Errorf("Admit(user core) at exactly the floor = %v, want it admitted", err)
	}
	if rateReads != 1 {
		t.Errorf("rate_limit read %d times, want once: the second Admit reads the fresh ledger", rateReads)
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
		if refused && floor.Pool.Floor != 2500 {
			t.Errorf("refused pool floor = %d, want 2500", floor.Pool.Floor)
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

func TestALateResponseNeverRaisesTheBalanceWithinAWindow(t *testing.T) {
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
	if len(pools) != 1 || pools[0].Remaining != 4000 || pools[0].Callers[0].Requests != 2 {
		t.Errorf("pools = %+v, want 4000 left after a late 4100 and two requests charged", pools)
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
	if got := pools[poolKey(IdentityUser, ResourceCore)].Callers; got["c"] != 1 || got["a"] != 0 {
		t.Errorf("live callers = %v, want the new window's only", got)
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
		if err := l.charge(t.Context(), IdentityUser, ResourceCore, "lr-facts", target, http.Header{}); err != nil {
			t.Fatal(err)
		}
	}
	states, err := l.load()
	if err != nil {
		t.Fatal(err)
	}
	got := states[poolKey(IdentityUser, ResourceCore)].Paths["lr-facts"]
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
