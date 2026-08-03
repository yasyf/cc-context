package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/version"
)

type item struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

// testClient points a client at an httptest server and hands it a token
// directly, so no test needs gh or an env var.
func testClient(baseURL string, resolve func(context.Context) (string, error)) *Client {
	c := New(baseURL)
	c.tokens.resolve = resolve
	return c
}

func fixedToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token, nil }
}

func TestPaginateSendsHeadersAndDecodes(t *testing.T) {
	t.Setenv("GH_TOKEN", "env-token")
	t.Setenv("GITHUB_TOKEN", "")
	stubGH(t, "")

	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/repos/o/r/pulls/12" {
			t.Errorf("path = %q, want /repos/o/r/pulls/12", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer env-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer env-token")
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want application/vnd.github+json", got)
		}
		if want := "ccx-gh/" + version.String(); r.Header.Get("User-Agent") != want {
			t.Errorf("User-Agent = %q, want %q", r.Header.Get("User-Agent"), want)
		}
		_, _ = fmt.Fprint(w, `[{"number":12,"body":"hi"}]`)
	}))
	t.Cleanup(ts.Close)

	got, err := Paginate[item](context.Background(), New(ts.URL), "/repos/o/r/pulls/12")
	if err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 1 || got[0] != (item{Number: 12, Body: "hi"}) {
		t.Errorf("Paginate = %+v, want [{12 hi}]", got)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestPaginateFollowsLinkHeader(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		page := func(n int) string {
			return fmt.Sprintf("http://%s/comments?labels=a,b&page=%d", r.Host, n)
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="last", <%s>; rel="next"`, page(3), page(2)))
			_, _ = fmt.Fprint(w, `[{"number":1,"body":"one"}]`)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, page(3)))
			_, _ = fmt.Fprint(w, `[{"number":2,"body":"two"}]`)
		case "3":
			_, _ = fmt.Fprint(w, `[{"number":3,"body":"three"}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	t.Cleanup(ts.Close)

	got, err := Paginate[item](context.Background(), testClient(ts.URL, fixedToken("tok")), "/comments?labels=a,b")
	if err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	want := []item{{1, "one"}, {2, "two"}, {3, "three"}}
	if len(got) != len(want) {
		t.Fatalf("Paginate = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Paginate[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
}

func TestPaginateRefusesALinkCycle(t *testing.T) {
	tests := []struct {
		name         string
		next         map[string]string
		wantRequests int32
		wantCycle    string
	}{
		{name: "a page naming itself", next: map[string]string{"1": "1"}, wantRequests: 1, wantCycle: "/items?page=1"},
		{name: "a chain looping back to its first page", next: map[string]string{"1": "2", "2": "1"}, wantRequests: 2, wantCycle: "/items?page=1"},
		{name: "a chain looping back to a later page", next: map[string]string{"1": "2", "2": "3", "3": "2"}, wantRequests: 3, wantCycle: "/items?page=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				page := r.URL.Query().Get("page")
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/items?page=%s>; rel="next"`, r.Host, tt.next[page]))
				_, _ = fmt.Fprint(w, `[{"number":1,"body":"page"}]`)
			}))
			t.Cleanup(ts.Close)

			got, err := Paginate[item](context.Background(), testClient(ts.URL, fixedToken("tok")), "/items?page=1")
			if !errors.Is(err, ErrPaginationCycle) {
				t.Fatalf("Paginate error = %v, want ErrPaginationCycle", err)
			}
			if got != nil {
				t.Errorf("Paginate = %+v, want no partial results", got)
			}
			want := fmt.Sprintf("ghapi: paginate %s%s: link chain revisits a page already fetched", ts.URL, tt.wantCycle)
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if n := requests.Load(); n != tt.wantRequests {
				t.Errorf("requests = %d, want %d", n, tt.wantRequests)
			}
		})
	}
}

func TestUnauthorizedReResolvesTokenOnce(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer token-2" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"number":7,"body":"fresh"}]`)
	}))
	t.Cleanup(ts.Close)

	resolutions := 0
	c := testClient(ts.URL, func(context.Context) (string, error) {
		resolutions++
		return fmt.Sprintf("token-%d", resolutions), nil
	})

	got, err := Paginate[item](context.Background(), c, "/repos/o/r/pulls/7")
	if err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 1 || got[0] != (item{Number: 7, Body: "fresh"}) {
		t.Errorf("Paginate = %+v, want [{7 fresh}]", got)
	}
	if resolutions != 2 {
		t.Errorf("token resolutions = %d, want 2", resolutions)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestUnauthorizedSurvivingReResolveIsTyped(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	t.Cleanup(ts.Close)

	resolutions := 0
	c := testClient(ts.URL, func(context.Context) (string, error) {
		resolutions++
		return fmt.Sprintf("token-%d", resolutions), nil
	})

	_, err := Paginate[item](context.Background(), c, "/repos/o/r/pulls/7")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Paginate error = %v, want ErrUnauthorized", err)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusUnauthorized || status.Message != "Bad credentials" {
		t.Errorf("Paginate error = %v, want *StatusError{401, Bad credentials}", err)
	}
	if resolutions != 2 {
		t.Errorf("token resolutions = %d, want 2", resolutions)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	t.Cleanup(ts.Close)

	_, err := Paginate[item](context.Background(), testClient(ts.URL, fixedToken("tok")), "/repos/o/r/pulls/404")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Paginate error = %v, want ErrNotFound", err)
	}
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("Paginate error = %v, want *StatusError", err)
	}
	want := fmt.Sprintf("ghapi: GET %s/repos/o/r/pulls/404: 404 Not Found", ts.URL)
	if status.Error() != want {
		t.Errorf("error = %q, want %q", status.Error(), want)
	}
}

func TestGraphQLRoundTrip(t *testing.T) {
	const query = "query($owner:String!){repository(owner:$owner){name}}"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("request = %s %s, want POST /graphql", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var sent graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if sent.Query != query {
			t.Errorf("query = %q, want %q", sent.Query, query)
		}
		if got := sent.Variables["owner"]; got != "yasyf" {
			t.Errorf("variables[owner] = %v, want yasyf", got)
		}
		_, _ = fmt.Fprint(w, `{"data":{"repository":{"name":"cc-context"}}}`)
	}))
	t.Cleanup(ts.Close)

	type response struct {
		Repository struct {
			Name string `json:"name"`
		} `json:"repository"`
	}
	got, err := GraphQL[response](context.Background(), testClient(ts.URL, fixedToken("tok")), query, map[string]any{"owner": "yasyf"})
	if err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if got.Repository.Name != "cc-context" {
		t.Errorf("repository.name = %q, want cc-context", got.Repository.Name)
	}
}

func TestGraphQLErrorsAreTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`)
	}))
	t.Cleanup(ts.Close)

	_, err := GraphQL[struct{}](context.Background(), testClient(ts.URL, fixedToken("tok")), "query{viewer{login}}", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GraphQL error = %v, want ErrNotFound", err)
	}
	var gqlErr *GraphQLError
	if !errors.As(err, &gqlErr) {
		t.Fatalf("GraphQL error = %v, want *GraphQLError", err)
	}
	if want := "ghapi: graphql: Could not resolve to a Repository"; gqlErr.Error() != want {
		t.Errorf("error = %q, want %q", gqlErr.Error(), want)
	}
}

func TestRateLimitRetryHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, `[{"number":3,"body":"after the wait"}]`)
	}))
	t.Cleanup(ts.Close)

	got, err := Paginate[item](context.Background(), testClient(ts.URL, fixedToken("tok")), "/repos/o/r/pulls/3")
	if err != nil {
		t.Fatalf("Paginate: %v", err)
	}
	if len(got) != 1 || got[0] != (item{Number: 3, Body: "after the wait"}) {
		t.Errorf("Paginate = %+v, want [{3 after the wait}]", got)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestRateLimitBeyondCapFailsImmediately(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	t.Cleanup(ts.Close)

	_, err := Paginate[item](context.Background(), testClient(ts.URL, fixedToken("tok")), "/repos/o/r/pulls/3")
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusForbidden || status.Message != "API rate limit exceeded" {
		t.Fatalf("Paginate error = %v, want *StatusError{403, API rate limit exceeded}", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		name string
		link string
		want string
	}{
		{name: "absent", link: "", want: ""},
		{name: "next only", link: `<https://api.github.com/x?page=2>; rel="next"`, want: "https://api.github.com/x?page=2"},
		{
			name: "next after a comma-bearing prev",
			link: `<https://api.github.com/x?labels=a,b&page=1>; rel="prev", <https://api.github.com/x?labels=a,b&page=3>; rel="next"`,
			want: "https://api.github.com/x?labels=a,b&page=3",
		},
		{name: "last page has no next", link: `<https://api.github.com/x?page=1>; rel="first", <https://api.github.com/x?page=1>; rel="prev"`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			if tt.link != "" {
				header.Set("Link", tt.link)
			}
			if got := nextLink(header); got != tt.want {
				t.Errorf("nextLink = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveRef(t *testing.T) {
	c := New("https://api.example/")
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "rooted path", ref: "/repos/o/r", want: "https://api.example/repos/o/r"},
		{name: "bare path", ref: "repos/o/r", want: "https://api.example/repos/o/r"},
		{name: "absolute url is followed as given", ref: "https://api.github.com/repositories/1/pulls?page=2", want: "https://api.github.com/repositories/1/pulls?page=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.resolveRef(tt.ref); got != tt.want {
				t.Errorf("resolveRef(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
