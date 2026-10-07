package ghapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

	"github.com/yasyf/cc-context/internal/render"
)

const testClientID = "Iv-test"

var testAppKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func testAppPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testAppKey())})
}

type fakeGitHub struct {
	t         *testing.T
	installed map[string]int64
	expiresIn time.Duration
	now       func() time.Time
	lookups   atomic.Int32
	mints     atomic.Int32
	mu        sync.Mutex
	seen      []string
	data      func(w http.ResponseWriter, r *http.Request)
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	f := &fakeGitHub{t: t, installed: map[string]int64{"o/r": 7}, expiresIn: time.Hour, now: time.Now}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/installation"):
		f.verifyJWT(r)
		f.lookups.Add(1)
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation")
		id, ok := f.installed[repo]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%d}`, id)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.verifyJWT(r)
		_, _ = fmt.Fprint(w, `{"permissions":{"contents":"write","pull_requests":"write","checks":"read","workflows":"write"}}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.verifyJWT(r)
		var req struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode token request: %v", err)
		}
		want := map[string]string{"contents": "read", "pull_requests": "read", "checks": "read"}
		if !maps.Equal(req.Permissions, want) {
			f.t.Errorf("token request permissions = %v, want %v", req.Permissions, want)
		}
		n := f.mints.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       fmt.Sprintf("app-%d", n),
			"expires_at":  f.now().Add(f.expiresIn).UTC().Format(time.RFC3339),
			"permissions": want,
		})
	default:
		f.mu.Lock()
		f.seen = append(f.seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		if f.data != nil {
			f.data(w, r)
			return
		}
		if r.URL.Path == "/graphql" {
			_, _ = fmt.Fprint(w, `{"data":{}}`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}
}

func (f *fakeGitHub) verifyJWT(r *http.Request) {
	f.t.Helper()
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		f.t.Errorf("%s %s: authorization is not a JWT", r.Method, r.URL.Path)
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Errorf("decode jwt signature: %v", err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testAppKey().PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		f.t.Errorf("jwt signature does not verify: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Errorf("decode jwt claims: %v", err)
		return
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		f.t.Errorf("parse jwt claims: %v", err)
	}
	if claims.Iss != testClientID || claims.Exp-claims.Iat != int64((appJWTLifetime+appJWTBackdate)/time.Second) {
		f.t.Errorf("jwt claims = %+v, want iss %s and a 10m lifetime", claims, testClientID)
	}
}

func (f *fakeGitHub) tokensSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func testAppSource(base, dir string) *appSource {
	src := newAppSource(appConfig{Name: "test-app", ClientID: testClientID}, base, dir)
	src.key = func(context.Context) ([]byte, error) { return testAppPEM(), nil }
	return src
}

func appClient(base string, src *appSource) *Client {
	c := testClient(base, fixedToken("user-token"))
	c.apps = &appLoader{load: func(context.Context) (*appSource, error) { return src, nil }}
	return c
}

func TestReadsUseTheInstallationTokenAndShareItsCache(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	dir := t.TempDir()
	ctx := context.Background()

	for range 2 {
		c := appClient(ts.URL, testAppSource(ts.URL, dir)).ForRepo("O/R")
		if _, err := Paginate[item](ctx, c, "/repos/o/r/pulls"); err != nil {
			t.Fatalf("Paginate: %v", err)
		}
	}
	if got := f.tokensSeen(); len(got) != 2 || got[0] != "app-1" || got[1] != "app-1" {
		t.Errorf("tokens = %v, want [app-1 app-1]", got)
	}
	if f.lookups.Load() != 1 || f.mints.Load() != 1 {
		t.Errorf("lookups, mints = %d, %d, want 1, 1: a second process must read the shared cache", f.lookups.Load(), f.mints.Load())
	}
	info, err := os.Stat(filepath.Join(dir, "7.json"))
	if err != nil {
		t.Fatalf("stat token cache: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token cache mode = %o, want 600", perm)
	}
}

func TestATokenNearExpiryIsMintedAgain(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	src := testAppSource(ts.URL, t.TempDir())
	c := appClient(ts.URL, src).ForRepo("o/r")

	for _, at := range []time.Time{time.Now(), time.Now().Add(time.Hour - AppRefreshMargin + time.Minute)} {
		src.now = func() time.Time { return at }
		f.now = src.now
		if _, err := Paginate[item](context.Background(), c, "/repos/o/r/pulls"); err != nil {
			t.Fatalf("Paginate: %v", err)
		}
	}
	if got := f.tokensSeen(); strings.Join(got, ",") != "app-1,app-2" {
		t.Errorf("tokens = %v, want the second read on a fresh mint", got)
	}
}

func TestAWatchTokenNeedsTheWatchMargin(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	f.expiresIn = 30 * time.Minute
	c := appClient(ts.URL, testAppSource(ts.URL, t.TempDir())).ForRepo("o/r")
	ctx := context.Background()

	short, _, err := c.AppToken(ctx, AppRefreshMargin)
	if err != nil || short != "app-1" {
		t.Fatalf("AppToken(refresh) = %q, %v, want app-1", short, err)
	}
	long, expires, err := c.AppToken(ctx, AppWatchMargin)
	if err != nil || long != "app-2" {
		t.Fatalf("AppToken(watch) = %q, %v, want a fresh app-2", long, err)
	}
	if expires.IsZero() {
		t.Error("AppToken returned no expiry")
	}
}

func TestReadsStayOnTheUserToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		repo  string
		ctx   func(context.Context) context.Context
		key   func(context.Context) ([]byte, error)
		query string
	}{
		{name: "app not installed on the repository", repo: "o/elsewhere"},
		{name: "client bound to no repository"},
		{name: "graphql mutation", repo: "o/r", query: "mutation { addComment { clientMutationId } }"},
		{name: "GH_TOKEN set by the caller", repo: "o/r", ctx: func(ctx context.Context) context.Context {
			return render.WithEnv(ctx, "GH_TOKEN=pinned")
		}},
		{name: "minting fails", repo: "o/r", key: func(context.Context) ([]byte, error) {
			return nil, errors.New("aws sso session expired")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeGitHub(t)
			src := testAppSource(ts.URL, t.TempDir())
			if tt.key != nil {
				src.key = tt.key
			}
			c := appClient(ts.URL, src)
			if tt.repo != "" {
				c = c.ForRepo(tt.repo)
			}
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx(ctx)
			}
			var err error
			if tt.query != "" {
				_, err = GraphQL[struct{}](ctx, c, tt.query, nil)
			} else {
				_, err = Paginate[item](ctx, c, "/repos/o/r/pulls")
			}
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if got := f.tokensSeen(); len(got) != 1 || got[0] != "user-token" {
				t.Errorf("tokens = %v, want [user-token]", got)
			}
			if f.mints.Load() != 0 {
				t.Errorf("mints = %d, want 0", f.mints.Load())
			}
		})
	}
}

func TestANegativeInstallationLookupIsCached(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	dir := t.TempDir()
	for range 2 {
		c := appClient(ts.URL, testAppSource(ts.URL, dir)).ForRepo("o/elsewhere")
		if _, err := Paginate[item](context.Background(), c, "/repos/o/elsewhere/pulls"); err != nil {
			t.Fatalf("Paginate: %v", err)
		}
	}
	if n := f.lookups.Load(); n != 1 {
		t.Errorf("lookups = %d, want 1", n)
	}
}

func TestAnIntegrationDenialFallsBackToTheUser(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	f.data = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer app-") {
			if r.URL.Path == "/graphql" {
				_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		if r.URL.Path == "/graphql" {
			_, _ = fmt.Fprint(w, `{"data":{"ok":true}}`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"number":1}]`)
	}
	c := appClient(ts.URL, testAppSource(ts.URL, t.TempDir())).ForRepo("o/r")
	ctx := context.Background()

	items, err := Paginate[item](ctx, c, "/repos/o/r/code-scanning/alerts")
	if err != nil || len(items) != 1 {
		t.Fatalf("Paginate = %v, %v, want one item read as the user", items, err)
	}
	got, err := GraphQL[struct {
		OK bool `json:"ok"`
	}](ctx, c, "query { ok }", nil)
	if err != nil || !got.OK {
		t.Fatalf("GraphQL = %+v, %v, want ok read as the user", got, err)
	}
	want := []string{"app-1", "user-token", "app-1", "user-token"}
	if seen := f.tokensSeen(); strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("tokens = %v, want %v", seen, want)
	}
}

func TestAnAppTokenDrawingA401IsMintedPast(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	f.data = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer app-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}
	c := appClient(ts.URL, testAppSource(ts.URL, t.TempDir())).ForRepo("o/r")
	if _, err := Paginate[item](context.Background(), c, "/repos/o/r/pulls"); err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if got := f.tokensSeen(); strings.Join(got, ",") != "app-1,app-2" {
		t.Errorf("tokens = %v, want [app-1 app-2]", got)
	}
}

func TestAuthReportsBothIdentities(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	f.data = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer user-token" {
			_, _ = fmt.Fprint(w, `{"data":{"viewer":{"login":"octocat"},"rateLimit":{"limit":5000,"remaining":4000,"resetAt":"2026-10-01T20:00:00Z"}}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"rateLimit":{"limit":10000,"remaining":9999,"resetAt":"2026-10-01T20:30:00Z"}}}`)
	}
	src := testAppSource(ts.URL, t.TempDir())
	ctx := context.Background()

	auth, err := appClient(ts.URL, src).ForRepo("o/r").Auth(ctx)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if auth.Reads.Kind != identityApp || auth.Reads.Name != "test-app" || auth.Reads.Installation != 7 || auth.Reads.Quota.Remaining != 9999 {
		t.Errorf("reads = %+v, want app test-app on installation 7 with 9999 left", auth.Reads)
	}
	if auth.Writes.Kind != identityUser || auth.Writes.Name != "octocat" || auth.Writes.Quota.Remaining != 4000 {
		t.Errorf("writes = %+v, want user octocat with 4000 left", auth.Writes)
	}

	other, err := appClient(ts.URL, src).ForRepo("o/elsewhere").Auth(ctx)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if other.Reads.Kind != identityUser || other.Reason != "test-app is not installed on o/elsewhere" {
		t.Errorf("auth = %+v, want reads on the user with the reason named", other)
	}
}

func TestAuthReportsASpentUserQuota(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	f.data = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer user-token":
			_, _ = fmt.Fprint(w, `{"data":{"rateLimit":{"limit":10000,"remaining":9999,"resetAt":"2026-10-01T20:30:00Z"}}}`)
		case r.URL.Path == "/user":
			_, _ = fmt.Fprint(w, `{"login":"octocat"}`)
		default:
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			_, _ = fmt.Fprint(w, `{"errors":[{"type":"RATE_LIMIT","message":"API rate limit already exceeded for user ID 1."}]}`)
		}
	}

	auth, err := appClient(ts.URL, testAppSource(ts.URL, t.TempDir())).ForRepo("o/r").Auth(context.Background())
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if auth.Writes.Name != "octocat" || auth.Writes.Quota.Limit != 5000 || auth.Writes.Quota.Remaining != 0 {
		t.Errorf("writes = %+v, want user octocat with 0 of 5000 left", auth.Writes)
	}
	if got := auth.Writes.Quota.ResetAt; got.Sub(reset).Abs() > 2*time.Second {
		t.Errorf("resetAt = %v, want about %v", got, reset)
	}
	if auth.Reads.Kind != identityApp || auth.Reads.Quota.Remaining != 9999 {
		t.Errorf("reads = %+v, want the app with 9999 left", auth.Reads)
	}
}

func TestLoadAppConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    *appConfig
		wantErr bool
	}{
		{name: "absent"},
		{
			name: "complete",
			body: "name = \"vulcan\"\nclient_id = \"Iv1\"\nprivate_key_command = [\"cat\", \"key.pem\"]\n",
			want: &appConfig{Name: "vulcan", ClientID: "Iv1", PrivateKeyCommand: []string{"cat", "key.pem"}},
		},
		{
			name: "name defaults to the client id",
			body: "client_id = \"Iv1\"\nprivate_key_command = [\"cat\"]\n",
			want: &appConfig{Name: "Iv1", ClientID: "Iv1", PrivateKeyCommand: []string{"cat"}},
		},
		{name: "missing the key command", body: "client_id = \"Iv1\"\n", wantErr: true},
		{name: "malformed", body: "client_id = [", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			xdg := t.TempDir()
			if tt.body != "" {
				path := filepath.Join(xdg, "ccx", "github-app.toml")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadAppConfig(render.WithEnv(context.Background(), "XDG_CONFIG_HOME="+xdg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("loadAppConfig error = %v, wantErr %v", err, tt.wantErr)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("loadAppConfig = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseRSAKeyAcceptsPKCS8(t *testing.T) {
	t.Parallel()
	der, err := x509.MarshalPKCS8PrivateKey(testAppKey())
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseRSAKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil || !key.Equal(testAppKey()) {
		t.Errorf("parseRSAKey(PKCS8) = %v, want the key back", err)
	}
	if _, err := parseRSAKey([]byte("not a key")); err == nil {
		t.Error("parseRSAKey accepted text with no PEM block")
	}
}

func TestAFailedMintKeepsEveryProcessOnTheUserTokenForAWhile(t *testing.T) {
	t.Parallel()
	f, ts := newFakeGitHub(t)
	dir := t.TempDir()
	start := time.Now()
	var keyRuns atomic.Int32
	read := func(at time.Time) {
		t.Helper()
		src := testAppSource(ts.URL, dir)
		src.now = func() time.Time { return at }
		src.key = func(context.Context) ([]byte, error) {
			keyRuns.Add(1)
			return nil, errors.New("aws sso session expired")
		}
		if _, err := Paginate[item](context.Background(), appClient(ts.URL, src).ForRepo("o/r"), "/repos/o/r/pulls"); err != nil {
			t.Fatalf("Paginate: %v", err)
		}
	}

	read(start)
	read(start.Add(appDownFor - time.Second))
	if n := keyRuns.Load(); n != 1 {
		t.Errorf("key runs inside the outage = %d, want 1: a second process must skip a key command that just failed", n)
	}
	read(start.Add(appDownFor))
	if n := keyRuns.Load(); n != 2 {
		t.Errorf("key runs after the outage = %d, want 2", n)
	}
	if got := f.tokensSeen(); strings.Join(got, ",") != "user-token,user-token,user-token" {
		t.Errorf("tokens = %v, want every read on the user token", got)
	}
}

func TestProcessesWaitingOnAFailingMintSkipTheKeyCommand(t *testing.T) {
	t.Parallel()
	_, ts := newFakeGitHub(t)
	dir := t.TempDir()
	var keyRuns atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			src := testAppSource(ts.URL, dir)
			src.key = func(context.Context) ([]byte, error) {
				keyRuns.Add(1)
				time.Sleep(100 * time.Millisecond)
				return nil, errors.New("aws sso session expired")
			}
			if _, err := Paginate[item](context.Background(), appClient(ts.URL, src).ForRepo("o/r"), "/repos/o/r/pulls"); err != nil {
				t.Errorf("Paginate: %v", err)
			}
		})
	}
	wg.Wait()
	if n := keyRuns.Load(); n != 1 {
		t.Errorf("key runs = %d, want 1: readers queued on the mint lock must see the outage it recorded", n)
	}
}
