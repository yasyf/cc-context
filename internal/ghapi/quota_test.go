package ghapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

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
	c, l := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		rateReads++
		_, _ = fmt.Fprint(w, rateLimitBody(Floor-1, Floor, reset))
	})
	ctx := render.WithEnv(t.Context(), "CCX_CALLER=desk-runner.py")
	if err := l.charge(ctx, IdentityUser, ResourceGraphQL, CallerName(ctx), http.Header{}); err != nil {
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

func TestAdmitReadsAPoolPastItsResetAgain(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	c, l := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, rateLimitBody(4999, 5000, time.Now().Add(time.Hour)))
	})
	header := http.Header{}
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Remaining", "3")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(past.Unix(), 10))
	header.Set("X-RateLimit-Resource", ResourceGraphQL)
	if err := l.charge(t.Context(), IdentityUser, ResourceCore, "watch.sh", header); err != nil {
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
	c, _ := quotaClient(t, func(w http.ResponseWriter, r *http.Request) {
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
		if err := l.charge(t.Context(), IdentityUser, ResourceCore, "watch.sh", header); err != nil {
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
