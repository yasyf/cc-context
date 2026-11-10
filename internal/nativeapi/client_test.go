package nativeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

func TestPullRequestInfoBatchesGitHubReadsInInputOrder(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer github-key" {
			t.Errorf("unexpected request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		var body struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		queries = append(queries, body.Query)
		response := map[string]any{"data": map[string]any{"repository": map[string]any{
			"p0": map[string]any{"number": 3, "state": "OPEN", "headRefName": "feature", "headRefOid": "new-head", "baseRefName": "parent", "baseRefOid": "base-head", "updatedAt": "2026-10-30T12:00:00Z", "author": map[string]any{"login": "author"}, "stackEntry": map[string]any{"position": 2}, "stack": map[string]any{"number": 8, "baseRefName": "dev", "entries": map[string]any{"nodes": []map[string]any{{"position": 1, "pullRequest": map[string]any{"number": 2}}, {"position": 2, "pullRequest": map[string]any{"number": 3}}}, "pageInfo": map[string]any{"hasNextPage": false}}}},
			"p1": map[string]any{"number": 1, "state": "MERGED", "mergeCommit": map[string]any{"oid": "landed"}, "updatedAt": "2026-10-29T12:00:00Z"},
		}}}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	infos, err := NewWithToken(srv.URL, "github-key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{3, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || len(infos) != 2 || infos[0].PRNumber != 3 || infos[1].PRNumber != 1 {
		t.Fatalf("queries=%d infos=%+v", len(queries), infos)
	}
	if infos[0].DependentPRNumber != 2 || infos[0].CliTrunk != "dev" || infos[0].Versions[0].HeadSha != "new-head" || infos[1].MergeCommitSha != "landed" {
		t.Fatalf("infos=%+v", infos)
	}
	if strings.Contains(queries[0], "graphite") {
		t.Fatal(queries[0])
	}
}

func TestPullRequestInfoNamesOnlyTheOpenPullRequestsAboveInTheStack(t *testing.T) {
	entry := func(position, number int, state, head string) map[string]any {
		return map[string]any{"position": position, "pullRequest": map[string]any{"number": number, "state": state, "headRefName": head}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response := map[string]any{"data": map[string]any{"repository": map[string]any{
			"p0": map[string]any{"number": 2, "state": "OPEN", "headRefName": "card", "stackEntry": map[string]any{"position": 2}, "stack": map[string]any{"number": 8, "baseRefName": "dev", "entries": map[string]any{"nodes": []any{
				entry(1, 1, "MERGED", "landed"),
				entry(2, 2, "OPEN", "card"),
				entry(3, 3, "OPEN", "claim"),
				entry(4, 4, "CLOSED", "abandoned"),
				entry(5, 5, "OPEN", "claim-2"),
			}}}},
		}}}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	infos, err := NewWithToken(srv.URL, "github-key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	want := []StackPull{{Number: 3, Head: "claim"}, {Number: 5, Head: "claim-2"}}
	if len(infos) != 1 || !reflect.DeepEqual(infos[0].OpenAbove, want) {
		t.Fatalf("infos=%+v, want OpenAbove %+v", infos, want)
	}
}

func TestPullRequestInfoLeavesOutANumberGitHubCannotResolveAndReadsTheRest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"p0":{"number":3,"state":"OPEN","headRefName":"feature"},"p1":null}},"errors":[{"type":"NOT_FOUND","path":["repository","p1"],"message":"Could not resolve to a PullRequest with the number of 45976."}]}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	infos, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{3, 45976}})
	if err != nil || len(infos) != 1 || infos[0].PRNumber != 3 {
		t.Fatalf("infos=%+v err=%v, want #3 read and #45976 left out", infos, err)
	}
}

func TestGraphQLStillFailsWhenTheRepositoryItselfIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'org/repo'."}]}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{3}})
	var result *ResultError
	if !errors.As(err, &result) || !strings.Contains(result.Message, "Could not resolve to a Repository") {
		t.Fatalf("err=%v, want the repository's NOT_FOUND", err)
	}
}

func TestPullRequestInfoSelectsOpenBeforeNewestClosedHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"h0":{"nodes":[{"number":3,"state":"OPEN","headRefName":"feature"}]},"c0":{"nodes":[{"number":4,"state":"CLOSED","headRefName":"feature"}]}}}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	infos, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRHeadRefNames: []string{"feature"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].PRNumber != 3 {
		t.Fatalf("infos=%+v", infos)
	}
}

func TestHeadPullRequestsReadsOnlyNumberStateAndHeadPerBranch(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		query = body.Query
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"o0":{"nodes":[{"number":3,"state":"OPEN","headRefOid":"aaa"}]},"c0":{"nodes":[{"number":2,"state":"MERGED","headRefOid":"old"}]},"o1":{"nodes":[]},"c1":{"nodes":[{"number":9,"state":"MERGED","headRefOid":"bbb"}]},"o2":{"nodes":[]},"c2":{"nodes":[]}}}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	infos, err := NewWithToken(srv.URL, "key").HeadPullRequests(context.Background(), "org", "repo", []string{"open", "merged", "none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].PRNumber != 3 || infos[0].State != "OPEN" || infos[1].PRNumber != 9 || infos[1].HeadRefName != "merged" || infos[1].Versions[0].HeadSha != "bbb" {
		t.Fatalf("infos=%+v, want the open #3 before the merged one it supersedes and #9, with nothing for a branch without a pull request", infos)
	}
	if strings.Contains(query, "body") || strings.Contains(query, "stack") {
		t.Fatalf("query reads fields a landing check does not need: %s", query)
	}
}

func TestGraphQLFailureDoesNotBecomeMissingPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"errors":[{"message":"rate limited"}],"data":null}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{1}})
	var result *ResultError
	if !errors.As(err, &result) || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err=%v", err)
	}
}

func TestSubmitCreatesNativeGitHubStackFromPublishedParent(t *testing.T) {
	var paths []string
	var stack []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "child-head") {
			return
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/org/repo/pulls":
			if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR2","html_url":"https://github.com/org/repo/pull/2","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"child","sha":"child-head"},"base":{"ref":"parent"}}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "GET /repos/org/repo/pulls":
			if r.URL.Query().Get("head") != "org:parent" {
				t.Errorf("head=%s", r.URL.Query().Get("head"))
			}
			if _, err := fmt.Fprint(w, `[{"number":1,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"parent"},"base":{"ref":"dev"}}]`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "POST /repos/org/repo/stacks":
			var body struct {
				PullRequests []int `json:"pull_requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			stack = body.PullRequests
			if _, err := fmt.Fprint(w, `{"number":7}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitCreate, Head: "child", HeadSha: "child-head", Base: "parent", Title: "child"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PRNumber != 2 || !reflect.DeepEqual(stack, []int{1, 2}) {
		t.Fatalf("got=%+v stack=%v paths=%v", got, stack, paths)
	}
}

func TestSubmitStacksOnAParentWhoseBaseHasNoPullRequest(t *testing.T) {
	var stack []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "child-head") {
			return
		}
		parent := `{"number":1,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"parent"},"base":{"ref":"main"}}`
		var body string
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/org/repo/pulls":
			body = `{"number":2,"node_id":"PR2","html_url":"https://github.com/org/repo/pull/2","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"child","sha":"child-head"},"base":{"ref":"parent"}}`
		case "GET /repos/org/repo/pulls":
			body = map[string]string{"org:parent": "[" + parent + "]", "org:main": "[]"}[r.URL.Query().Get("head")]
		case "GET /repos/org/repo/pulls/1":
			body = parent
		case "GET /repos/org/repo":
			body = `{"default_branch":"main"}`
		case "POST /repos/org/repo/stacks":
			var req struct {
				PullRequests []int `json:"pull_requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			stack, body = req.PullRequests, `{"number":7}`
		}
		if body == "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "landed", PRs: []SubmitPR{{Action: SubmitCreate, Head: "child", HeadSha: "child-head", Base: "parent", Title: "child"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PRNumber != 2 || !reflect.DeepEqual(stack, []int{1, 2}) {
		t.Fatalf("got=%+v stack=%v", got, stack)
	}
}

func TestSubmitLinksAnUnstackedGraphiteChainIntoOneNativeStack(t *testing.T) {
	var created, added []int
	stacked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "tip-head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path + "?" + r.URL.Query().Get("head") {
		case "GET /repos/org/repo/pulls/3?":
			reply(`{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"tip-head"},"base":{"ref":"middle"}}`)
		case "GET /repos/org/repo/pulls?org:middle":
			reply(`[{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"middle"},"base":{"ref":"bottom"}}]`)
		case "GET /repos/org/repo/pulls?org:bottom":
			reply(`[{"number":1,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"bottom"},"base":{"ref":"dev"}}]`)
		case "POST /repos/org/repo/stacks?":
			if err := json.NewDecoder(r.Body).Decode(&struct {
				PullRequests *[]int `json:"pull_requests"`
			}{&created}); err != nil {
				t.Error(err)
			}
			stacked = true
			reply(`{"number":7}`)
		case "GET /repos/org/repo/pulls/2?":
			if !stacked {
				t.Error("read the middle PR's stack before creating it")
			}
			reply(`{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"middle"},"base":{"ref":"bottom"},"stack":{"number":7,"position":2,"size":2,"base":{"ref":"dev"}}}`)
		case "POST /repos/org/repo/stacks/7/add?":
			if err := json.NewDecoder(r.Body).Decode(&struct {
				PullRequests *[]int `json:"pull_requests"`
			}{&added}); err != nil {
				t.Error(err)
			}
			reply(`{"number":7}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 3, Head: "tip", HeadSha: "tip-head", Base: "middle"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StackNumber != 7 || !reflect.DeepEqual(created, []int{1, 2}) || !reflect.DeepEqual(added, []int{3}) {
		t.Fatalf("got=%+v created=%v added=%v", got, created, added)
	}
}

func TestSubmitWarnsAndSucceedsWhenAParentBaseHasNoOpenPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "tip-head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path + "?" + r.URL.Query().Get("head") {
		case "GET /repos/org/repo/pulls/3?":
			reply(`{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"tip-head"},"base":{"ref":"middle"}}`)
		case "GET /repos/org/repo/pulls?org:middle":
			reply(`[{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"middle"},"base":{"ref":"gone"}}]`)
		case "GET /repos/org/repo/pulls?org:gone":
			reply(`[]`)
		case "GET /repos/org/repo?":
			reply(`{"default_branch":"dev"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 3, Head: "tip", HeadSha: "tip-head", Base: "middle"}}})
	if err != nil {
		t.Fatalf("submit over a parent based on a branch with no open PR = %v, want a warning and no failure", err)
	}
	if len(got) != 1 || got[0].PRNumber != 3 || got[0].StackNumber != 0 || len(got[0].Warnings) != 1 {
		t.Fatalf("got=%+v, want PR #3 outside any stack with one warning", got)
	}
	for _, want := range []string{"PR #3 stays out of any native stack", "ccx vcs stack submit --parent middle=dev"} {
		if !strings.Contains(got[0].Warnings[0], want) {
			t.Errorf("warning %q lacks %q", got[0].Warnings[0], want)
		}
	}
}

func TestSubmitRefusesAStackedPullRequestWhoseParentBaseHasNoOpenPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "tip-head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path + "?" + r.URL.Query().Get("head") {
		case "GET /repos/org/repo/pulls/3?":
			reply(`{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"tip-head"},"base":{"ref":"gone"},"stack":{"number":7,"position":2,"size":2,"base":{"ref":"dev"}}}`)
		case "GET /repos/org/repo/pulls?org:gone":
			reply(`[]`)
		case "GET /repos/org/repo?":
			reply(`{"default_branch":"dev"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 3, Head: "tip", HeadSha: "tip-head", Base: "gone"}}})
	if err == nil || !strings.Contains(err.Error(), "ccx vcs stack submit --parent tip=dev --move tip") {
		t.Fatalf("submit of a stacked PR over a base with no open PR = %v, want the --move remedy", err)
	}
}

func TestSubmitExtendsExistingNativeStack(t *testing.T) {
	var added []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/org/repo/pulls":
			if _, err := fmt.Fprint(w, `{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"child","sha":"head"},"base":{"ref":"parent"}}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "GET /repos/org/repo/pulls":
			if _, err := fmt.Fprint(w, `[{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"parent"},"base":{"ref":"bottom"},"stack":{"number":7,"position":2,"size":2,"base":{"ref":"dev"}}}]`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "POST /repos/org/repo/stacks/7/add":
			var body struct {
				PullRequests []int `json:"pull_requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			added = body.PullRequests
			if _, err := fmt.Fprint(w, `{"number":7}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitCreate, Head: "child", HeadSha: "head", Base: "parent"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []int{3}) {
		t.Fatalf("added=%v", added)
	}
}

func TestSubmitRelinksANativeStackPastAClosedPullRequestAboveTheParent(t *testing.T) {
	for _, tc := range []struct {
		name, bottom, unstacked, want string
		relinked                      []int
	}{
		{name: "dissolved", bottom: `"state":"open","merged_at":null`, unstacked: "", want: "POST /repos/org/repo/stacks", relinked: []int{1, 2, 4}},
		{name: "merged bottom dropped", bottom: `"state":"closed","merged_at":"2026-10-30T12:00:00Z"`, unstacked: `{"number":7,"pull_requests":[{"number":1,"state":"closed","merged_at":"2026-10-30T12:00:00Z"}]}`, want: "POST /repos/org/repo/stacks", relinked: []int{2, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var relinked []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if publishedHeadFixture(t, w, r, "head") {
					return
				}
				reply := func(body string) {
					if _, err := fmt.Fprint(w, body); err != nil {
						t.Errorf("write fixture response: %v", err)
					}
				}
				call := r.Method + " " + r.URL.Path
				calls = append(calls, call)
				switch call {
				case "POST /repos/org/repo/pulls":
					reply(`{"number":4,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"d","sha":"head"},"base":{"ref":"c"}}`)
				case "GET /repos/org/repo/pulls":
					reply(`[{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"c"},"base":{"ref":"b"},"stack":{"number":7,"position":2,"size":3,"base":{"ref":"dev"}}}]`)
				case "GET /repos/org/repo/stacks/7":
					reply(fmt.Sprintf(`{"number":7,"pull_requests":[{"number":1,%s},{"number":2,"state":"open","merged_at":null},{"number":3,"state":"closed","merged_at":null}]}`, tc.bottom))
				case "POST /repos/org/repo/stacks/7/unstack":
					if tc.unstacked == "" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					reply(tc.unstacked)
				case tc.want:
					if err := json.NewDecoder(r.Body).Decode(&struct {
						PullRequests *[]int `json:"pull_requests"`
					}{&relinked}); err != nil {
						t.Error(err)
					}
					reply(`{"number":9}`)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer srv.Close()
			got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitCreate, Head: "d", HeadSha: "head", Base: "c", Title: "d"}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].StackNumber != 9 || !reflect.DeepEqual(relinked, tc.relinked) {
				t.Fatalf("got=%+v relinked=%v calls=%v", got, relinked, calls)
			}
		})
	}
}

func TestSubmitOpensASecondChildChainAsItsOwnNativeStackOnTheSharedParent(t *testing.T) {
	var created []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "tip-head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path + "?" + r.URL.Query().Get("head") {
		case "POST /repos/org/repo/pulls?":
			reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"fork-tip","sha":"tip-head"},"base":{"ref":"fork"}}`)
		case "GET /repos/org/repo/pulls?org:fork":
			reply(`[{"number":4,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"fork"},"base":{"ref":"parent"}}]`)
		case "GET /repos/org/repo/pulls?org:parent":
			reply(`[{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"parent"},"base":{"ref":"dev"},"stack":{"number":7,"position":1,"size":2,"base":{"ref":"dev"}}}]`)
		case "GET /repos/org/repo/pulls/4?":
			reply(`{"number":4,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"fork"},"base":{"ref":"parent"}}`)
		case "GET /repos/org/repo/stacks/7?":
			reply(`{"number":7,"pull_requests":[{"number":2,"state":"open"},{"number":3,"state":"open"}]}`)
		case "POST /repos/org/repo/stacks?":
			if err := json.NewDecoder(r.Body).Decode(&struct {
				PullRequests *[]int `json:"pull_requests"`
			}{&created}); err != nil {
				t.Error(err)
			}
			reply(`{"number":9}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitCreate, Head: "fork-tip", HeadSha: "tip-head", Base: "fork", Title: "fork tip"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StackNumber != 9 || !reflect.DeepEqual(created, []int{4, 5}) {
		t.Fatalf("got=%+v created=%v", got, created)
	}
}

func TestSubmitRefusesChangedRemoteHeadBeforePatch(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "other-head") {
			return
		}
		methods = append(methods, r.Method)
		if _, err := fmt.Fprint(w, `{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"child","sha":"other-head"},"base":{"ref":"dev"}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 3, Head: "child", HeadSha: "published-head", Base: "dev"}}})
	if err == nil || !strings.Contains(err.Error(), "reports commit other-head") || !reflect.DeepEqual(methods, []string{"GET"}) {
		t.Fatalf("err=%v methods=%v", err, methods)
	}
}

func TestNativeStackRegistrationFailureReportsCreatedPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		if r.Method == http.MethodPost {
			if _, err := fmt.Fprint(w, `{"number":2,"head":{"repo":{"full_name":"org/repo"},"ref":"child","sha":"head"},"base":{"ref":"parent"},"state":"open"}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `[{"number":1},{"number":3}]`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitCreate, Head: "child", HeadSha: "head", Base: "parent"}}})
	var partial *SubmitError
	if !errors.As(err, &partial) || len(partial.Submitted) != 1 || partial.Submitted[0].PRNumber != 2 || len(partial.Failed) != 1 {
		t.Fatalf("err=%v", err)
	}
}

func TestTrunkQueueReadsBatchAndPaginate(t *testing.T) {
	var batches []string
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer trunk-key" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/v2/repositories" {
			if _, err := fmt.Fprint(w, `{"data":[{"id":"repo-id"}],"hasMore":false,"nextCursor":null}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
			return
		}
		if r.URL.Path != "/v2/pull-requests" {
			t.Errorf("path=%s", r.URL.Path)
		}
		numbers := r.URL.Query().Get("number")
		cursor := r.URL.Query().Get("cursor")
		batches = append(batches, numbers)
		pages = append(pages, cursor)
		first, _ := strconv.Atoi(strings.Split(numbers, ",")[0])
		if first == 1 && cursor == "" {
			if _, err := fmt.Fprint(w, `{"data":[{"number":1,"isCurrentlySubmittedToQueue":true,"queueEntry":{"state":"testing"}}],"hasMore":true,"nextCursor":"more"}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
			return
		}
		if cursor == "more" {
			if _, err := fmt.Fprint(w, `{"data":[{"number":2,"isCurrentlySubmittedToQueue":true,"queueEntry":{"state":"futureActiveState"}}],"hasMore":false,"nextCursor":null}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprint(w, `{"data":[{"number":51,"isCurrentlySubmittedToQueue":false,"queueEntry":{"state":"failed"}}],"hasMore":false,"nextCursor":null}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	client := NewWithToken(srv.URL, "github-key").WithQueueProvider("trunk")
	client.queueBase = srv.URL
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-key")
	numbers := make([]int, 51)
	for i := range numbers {
		numbers[i] = i + 1
	}
	states, err := client.MergeabilityStatuses(context.Background(), "org", "repo", numbers)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 || len(strings.Split(batches[0], ",")) != 50 || pages[1] != "more" || batches[2] != "51" {
		t.Fatalf("batches=%v pages=%v", batches, pages)
	}
	if states[1] != "testing" || !QueuedMergeability(states[2]) || states[51] != "failed" {
		t.Fatalf("states=%v", states)
	}
}

func TestMissingTrunkCredentialFailsQueueRead(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	_, err := NewWithToken("http://invalid.local", "github-key").WithQueueProvider("trunk").MergeabilityStatuses(context.Background(), "org", "repo", []int{1})
	if err == nil || !strings.Contains(err.Error(), "TRUNK_MERGE_API_KEY") {
		t.Fatalf("err=%v", err)
	}
}

func TestGitHubTokenEnvironmentPrecedence(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	token, err := resolveToken()
	if err != nil || token != "gh-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestStatusErrorPreservesUnauthorizedAndDiagnostic(t *testing.T) {
	err := statusError("GET", "https://api.github.com/repos/org/repo", http.StatusUnauthorized, []byte(`{"message":"Bad credentials"}`))
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatal(err)
	}
	if got := statusMessage([]byte(strings.Repeat("x", 1000))); len(got) > statusBodyCap+3 {
		t.Fatalf("unbounded diagnostic %d", len(got))
	}
}

func TestQueueProviderDoesNotFallBackFromTrunkCredentialError(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"p0":{"mergeQueueEntry":{"state":"LOCKED"}}}}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	client := NewWithToken(srv.URL, "github-key")
	states, err := client.MergeabilityStatuses(context.Background(), "org", "repo", []int{9})
	if err != nil || !QueuedMergeability(states[9]) || calls != 1 {
		t.Fatalf("states=%v calls=%d err=%v", states, calls, err)
	}
	_, err = client.WithQueueProvider("trunk").MergeabilityStatuses(context.Background(), "org", "repo", []int{9})
	if err == nil || calls != 1 {
		t.Fatalf("Trunk credential error fell back: calls=%d err=%v", calls, err)
	}
	_, err = client.WithQueueProvider("invalid").MergeabilityStatuses(context.Background(), "org", "repo", []int{9})
	if err == nil {
		t.Fatal("invalid provider accepted")
	}
}

func TestGitHubQueueSnapshotIsNotCurrentPullRequestHead(t *testing.T) {
	pull := githubPull{Number: 7, HeadRefOid: "new-head"}
	pull.MergeQueueEntry = &struct {
		State      string
		HeadCommit *struct{ Oid string }
	}{State: "AWAITING_CHECKS", HeadCommit: &struct{ Oid string }{Oid: "queued-head"}}
	info, err := pull.info()
	if err != nil {
		t.Fatal(err)
	}
	if info.MergeQueueStatus == nil || !info.MergeQueueStatus.InQueue || info.MergeQueueStatus.EnqueuedCommit != "queued-head" || info.Versions[0].HeadSha != "new-head" {
		t.Fatalf("info=%+v", info)
	}
	pull.MergeQueueEntry.HeadCommit = nil
	info, err = pull.info()
	if err != nil {
		t.Fatal(err)
	}
	if info.MergeQueueStatus.EnqueuedCommit != "" {
		t.Fatal("unknown snapshot replaced with current head")
	}
}

func TestSubmitReportsCreatedPullWhenReviewerRequestFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		switch r.URL.Path {
		case "/repos/org/repo/pulls":
			if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","html_url":"https://github.com/org/repo/pull/2","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"head"},"base":{"ref":"main"}}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "/repos/org/repo/pulls/2/requested_reviewers":
			w.WriteHeader(http.StatusUnprocessableEntity)
			if _, err := fmt.Fprint(w, `{"message":"reviewer does not have access"}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "feature", HeadSha: "head", Base: "main", Reviewers: []string{"reviewer"}}}})
	var partial *SubmitError
	var status *StatusError
	if !errors.As(err, &partial) || len(partial.Submitted) != 1 || partial.Submitted[0].PRID != "PR_2" || partial.Submitted[0].PRNumber != 2 || len(partial.Failed) != 1 || !errors.As(err, &status) || status.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err=%v partial=%+v", err, partial)
	}
}

func TestStaleCreateSnapshotUsesPublishedRefFence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "published-head") {
			return
		}
		if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"stale-head"},"base":{"ref":"main"},"state":"open"}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	submitted, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "feature", HeadSha: "published-head", Base: "main"}}})
	if err != nil || len(submitted) != 1 || submitted[0].PRID != "PR_2" {
		t.Fatalf("err=%v submitted=%+v", err, submitted)
	}
}

func TestHeadLookupRefusesAmbiguousPullRequestsInsteadOfChoosingFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"h0":{"totalCount":2,"nodes":[{"number":2,"state":"OPEN","headRefName":"feature"}]},"c0":{"nodes":[]}}}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRHeadRefNames: []string{"feature"}})
	if err == nil || !strings.Contains(err.Error(), "2 open pull requests") {
		t.Fatalf("err=%v", err)
	}
}

func TestHeadLookupDoesNotTreatForkBranchAsLocalStackBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"data":{"repository":{"h0":{"totalCount":1,"nodes":[{"number":2,"state":"OPEN","headRefName":"feature","isCrossRepository":true}]},"c0":{"nodes":[]}}}}`); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRHeadRefNames: []string{"feature"}})
	if err == nil || !strings.Contains(err.Error(), "cross-repository PR #2") {
		t.Fatalf("err=%v", err)
	}
}

func publishedHeadFixture(t *testing.T, w http.ResponseWriter, r *http.Request, sha string) bool {
	t.Helper()
	if !strings.Contains(r.URL.Path, "/git/ref/heads/") {
		return false
	}
	if r.Method != http.MethodGet {
		t.Errorf("git ref method=%s", r.Method)
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": sha}}); err != nil {
		t.Errorf("write fixture ref: %v", err)
	}
	return true
}

func TestStaleUpdateSnapshotUsesPublishedRefFence(t *testing.T) {
	patches := 0
	refs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "published-head") {
			refs++
			return
		}
		if r.Method == http.MethodPatch {
			patches++
		}
		if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"stale-head"},"base":{"ref":"main"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	submitted, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 2, Head: "feature", HeadSha: "published-head", Base: "main"}}})
	if err != nil || len(submitted) != 1 || patches != 0 || refs != 2 {
		t.Fatalf("err=%v submitted=%+v patches=%d refs=%d", err, submitted, patches, refs)
	}
}

func TestRefChangeDuringCreateReturnsPartialFailure(t *testing.T) {
	refs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/ref/heads/") {
			refs++
			sha := "published-head"
			if refs > 1 {
				sha = "concurrent-head"
			}
			publishedHeadFixture(t, w, r, sha)
			return
		}
		if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"published-head"},"base":{"ref":"main"},"state":"open"}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "feature", HeadSha: "published-head", Base: "main"}}})
	var partial *SubmitError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "reports commit concurrent-head") || len(partial.Submitted) != 1 || partial.Submitted[0].PRID != "PR_2" || refs != 2 {
		t.Fatalf("err=%v partial=%+v refs=%d", err, partial, refs)
	}
}

func TestClosedMutationResponseIsPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "published-head") {
			return
		}
		if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","state":"closed","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"published-head"},"base":{"ref":"main"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "feature", HeadSha: "published-head", Base: "main"}}})
	var partial *SubmitError
	if !errors.As(err, &partial) || len(partial.Submitted) != 1 || partial.Submitted[0].PRID != "PR_2" || !strings.Contains(err.Error(), "is closed after metadata update") {
		t.Fatalf("err=%v partial=%+v", err, partial)
	}
}

func TestSubmitUpdatesOnlyChangedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		body  string
		want  map[string]string
	}{
		{name: "identical", title: "current title", body: "current body"},
		{name: "empty preserves current"},
		{name: "changed title", title: "new title", body: "current body", want: map[string]string{"title": "new title"}},
		{name: "changed body", title: "current title", body: "new body", want: map[string]string{"body": "new body"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, patches := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if publishedHeadFixture(t, w, r, "published-head") {
					refs++
					return
				}
				if r.Method == http.MethodPatch {
					patches++
					var values map[string]string
					if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
						t.Error(err)
					}
					if _, exists := values["base"]; exists {
						http.Error(w, `{"message":"Validation Failed","errors":[{"field":"base","code":"unchanged"}]}`, http.StatusUnprocessableEntity)
						return
					}
					if !reflect.DeepEqual(values, tc.want) {
						t.Errorf("patch=%v want=%v", values, tc.want)
					}
				}
				if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","state":"open","title":"current title","body":"current body","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"stale-head"},"base":{"ref":"main"}}`); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			submitted, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 2, Head: "feature", HeadSha: "published-head", Base: "main", Title: tc.title, Body: tc.body}}})
			wantPatches := 0
			if tc.want != nil {
				wantPatches = 1
			}
			if err != nil || len(submitted) != 1 || patches != wantPatches || refs != 2 {
				t.Fatalf("err=%v submitted=%+v patches=%d refs=%d", err, submitted, patches, refs)
			}
		})
	}
}

func TestChangedBasePreservesValidationDetails(t *testing.T) {
	refs, patches := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "published-head") {
			refs++
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			var values map[string]string
			if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(values, map[string]string{"base": "new-parent"}) {
				t.Errorf("patch=%v", values)
			}
			http.Error(w, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","field":"base","code":"invalid"}]}`, http.StatusUnprocessableEntity)
			return
		}
		if _, err := fmt.Fprint(w, `{"number":2,"node_id":"PR_2","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"feature","sha":"published-head"},"base":{"ref":"main"}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 2, Head: "feature", HeadSha: "published-head", Base: "new-parent"}}})
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusUnprocessableEntity || !strings.Contains(status.Message, "Validation Failed") || !strings.Contains(status.Message, `"field":"base"`) || !strings.Contains(status.Message, `"code":"invalid"`) || patches != 1 || refs != 1 {
		t.Fatalf("err=%v status=%+v patches=%d refs=%d", err, status, patches, refs)
	}
}

func TestSubmitRetargetsAPullRequestOutOfADeadNativeStack(t *testing.T) {
	for _, tc := range []struct {
		name, sibling string
		dead          bool
	}{
		{name: "every other member closed", sibling: `"state":"closed","merged_at":"2026-10-31T10:00:00Z"`, dead: true},
		{name: "another member open", sibling: `"state":"open","merged_at":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			stacked := true
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if publishedHeadFixture(t, w, r, "head") {
					return
				}
				reply := func(body string) {
					if _, err := fmt.Fprint(w, body); err != nil {
						t.Errorf("write fixture response: %v", err)
					}
				}
				call := r.Method + " " + r.URL.Path
				calls = append(calls, call)
				switch call {
				case "GET /repos/org/repo/pulls/5":
					stack := ""
					if stacked {
						stack = `,"stack":{"number":8,"position":2,"size":2,"base":{"ref":"dev"}}`
					}
					reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"lower"}` + stack + `}`)
				case "GET /repos/org/repo/stacks/8":
					reply(`{"number":8,"pull_requests":[{"number":4,` + tc.sibling + `},{"number":5,"state":"open","merged_at":null}]}`)
				case "POST /repos/org/repo/stacks/8/unstack":
					stacked = false
					w.WriteHeader(http.StatusNoContent)
				case "PATCH /repos/org/repo/pulls/5":
					if stacked {
						http.Error(w, `{"message":"Validation Failed: Cannot change the base branch because the pull request is part of a stack."}`, http.StatusUnprocessableEntity)
						return
					}
					reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"dev"}}`)
				case "GET /repos/org/repo/pulls":
					reply(`[{"number":4,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"lower"},"base":{"ref":"dev"},"stack":{"number":8,"position":1,"size":2,"base":{"ref":"dev"}}}]`)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer srv.Close()
			got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 5, Head: "tip", HeadSha: "head", Base: "dev"}}})
			if err != nil || len(got) != 1 {
				t.Fatalf("got=%+v err=%v calls=%v", got, err, calls)
			}
			unstacked := slices.Contains(calls, "POST /repos/org/repo/stacks/8/unstack")
			if tc.dead {
				if !unstacked || got[0].StackNumber != 0 || len(got[0].Warnings) != 0 {
					t.Fatalf("got=%+v calls=%v, want the dead stack unstacked and the pull request moved onto dev", got, calls)
				}
				return
			}
			if unstacked || slices.Contains(calls, "PATCH /repos/org/repo/pulls/5") || got[0].StackNumber != 8 || len(got[0].Warnings) != 1 || !strings.Contains(got[0].Warnings[0], "kept PR #5 on lower, not dev") || !strings.Contains(got[0].Warnings[0], "ccx vcs stack submit --move tip") {
				t.Fatalf("got=%+v calls=%v, want the live stack kept, its base left alone, and a warning naming it", got, calls)
			}
		})
	}
}

// TestSubmitRebuildsANativeStackWithoutItsMergedBase is capt-hook-prompt-latency:
// once the bottom of native stack #8 merged, a submit moving the next pull
// request onto trunk kept it on the merged branch, failed its link, and told the
// caller to rerun the --parent command it had just run.
func TestSubmitRebuildsANativeStackWithoutItsMergedBase(t *testing.T) {
	var calls []string
	stack := 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		member := func(number int) string {
			bottom := map[int]int{8: 4, 9: 5}[stack]
			if bottom == 0 {
				return ""
			}
			return fmt.Sprintf(`,"stack":{"number":%d,"position":%d,"size":%d,"base":{"ref":"dev"}}`, stack, number-bottom+1, 7-bottom)
		}
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		switch call {
		case "GET /repos/org/repo/pulls/5":
			base := "lower"
			if slices.Contains(calls, "PATCH /repos/org/repo/pulls/5") {
				base = "dev"
			}
			reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid","sha":"head"},"base":{"ref":"` + base + `"}` + member(5) + `}`)
		case "GET /repos/org/repo/pulls/6":
			reply(`{"number":6,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"mid"}` + member(6) + `}`)
		case "GET /repos/org/repo/stacks/8":
			reply(`{"number":8,"pull_requests":[{"number":4,"state":"closed","merged_at":"2026-11-02T12:26:46Z"},{"number":5,"state":"open","merged_at":null},{"number":6,"state":"open","merged_at":null}]}`)
		case "POST /repos/org/repo/stacks/8/unstack":
			stack = 0
			w.WriteHeader(http.StatusNoContent)
		case "PATCH /repos/org/repo/pulls/5":
			if stack != 0 {
				http.Error(w, `{"message":"Validation Failed: Cannot change the base branch because the pull request is part of a stack."}`, http.StatusUnprocessableEntity)
				return
			}
			reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid","sha":"head"},"base":{"ref":"dev"}}`)
		case "POST /repos/org/repo/stacks":
			var body struct {
				PullRequests []int `json:"pull_requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !slices.Equal(body.PullRequests, []int{5, 6}) {
				t.Errorf("relink body = %+v (%v), want [5 6]", body, err)
			}
			stack = 9
			reply(`{"number":9}`)
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:mid" {
				t.Errorf("parent lookup head = %q, want org:mid", got)
			}
			reply(`[{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid"},"base":{"ref":"dev"}` + member(5) + `}]`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{
		{Action: SubmitUpdate, PRNumber: 5, Head: "mid", HeadSha: "head", Base: "dev"},
		{Action: SubmitUpdate, PRNumber: 6, Head: "tip", HeadSha: "head", Base: "mid"},
	}})
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%+v err=%v calls=%v", got, err, calls)
	}
	for _, pr := range got {
		if pr.StackNumber != 9 || len(pr.Warnings) != 0 {
			t.Fatalf("got=%+v calls=%v, want #5 on dev and #6 above it in rebuilt stack #9 without warnings", got, calls)
		}
	}
}

func TestSubmitMoveOpensANewPullRequestWhenALiveNativeStackHoldsTheOldOne(t *testing.T) {
	var calls []string
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		if r.Method != http.MethodGet {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("%s: decode body: %v", call, err)
			}
			bodies[call] = body
		}
		switch call {
		case "GET /repos/org/repo/pulls/5":
			reply(`{"number":5,"title":"Tip","body":"tip body","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"lower"},"stack":{"number":8,"position":2,"size":2,"base":{"ref":"dev"}}}`)
		case "GET /repos/org/repo/stacks/8":
			reply(`{"number":8,"pull_requests":[{"number":4,"state":"open","merged_at":null},{"number":5,"state":"open","merged_at":null}]}`)
		case "POST /repos/org/repo/pulls":
			reply(`{"number":6,"html_url":"https://github.com/org/repo/pull/6","title":"Tip","body":"tip body","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"mid"}}`)
		case "POST /repos/org/repo/issues/5/comments":
			reply(`{"id":1}`)
		case "PATCH /repos/org/repo/pulls/5":
			reply(`{"number":5,"state":"closed"}`)
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:mid" {
				t.Errorf("parent lookup head = %q, want org:mid", got)
			}
			reply(`[{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid"},"base":{"ref":"dev"},"stack":{"number":9,"position":1,"size":1,"base":{"ref":"dev"}}}]`)
		case "POST /repos/org/repo/stacks/9/add":
			reply(`{"number":9}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 5, Head: "tip", HeadSha: "head", Base: "mid", Move: true}}})
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%+v err=%v calls=%v", got, err, calls)
	}
	if got[0].PRNumber != 6 || got[0].Status != "moved" || got[0].StackNumber != 9 || len(got[0].Warnings) != 0 {
		t.Fatalf("got=%+v, want PR #6 moved into stack #9 without a warning", got[0])
	}
	if slices.Contains(calls, "POST /repos/org/repo/stacks/8/unstack") {
		t.Errorf("calls=%v, want stack #8, which holds another open pull request, left alone", calls)
	}
	opened := bodies["POST /repos/org/repo/pulls"]
	if opened["head"] != "tip" || opened["base"] != "mid" || opened["title"] != "Tip" || opened["body"] != "tip body" || opened["draft"] != false {
		t.Errorf("new pull request = %v, want tip onto mid with #5's title, body and draft state", opened)
	}
	if note, _ := bodies["POST /repos/org/repo/issues/5/comments"]["body"].(string); !strings.Contains(note, "Moved to #6") {
		t.Errorf("comment on #5 = %q, want it to name #6", note)
	}
	if closed := bodies["PATCH /repos/org/repo/pulls/5"]; closed["state"] != "closed" || len(closed) != 1 {
		t.Errorf("PATCH #5 = %v, want it closed and nothing else changed", closed)
	}
	comment, closing := slices.Index(calls, "POST /repos/org/repo/issues/5/comments"), slices.Index(calls, "PATCH /repos/org/repo/pulls/5")
	if opening := slices.Index(calls, "POST /repos/org/repo/pulls"); opening < 0 || comment < opening || closing < comment {
		t.Errorf("calls=%v, want #6 opened, then #5 commented on, then closed", calls)
	}
}

func TestSubmitMoveInPlaceRetargetsAPullRequestOutOfALiveNativeStackAndRelinksTheRest(t *testing.T) {
	var calls []string
	bodies := map[string]map[string]any{}
	stacked := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		call := r.Method + " " + r.URL.Path
		calls = append(calls, call)
		if r.Method != http.MethodGet {
			var body map[string]any
			if r.ContentLength > 0 {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("%s: decode body: %v", call, err)
				}
			}
			bodies[call] = body
		}
		switch call {
		case "GET /repos/org/repo/pulls/5":
			stack := ""
			if stacked {
				stack = `,"stack":{"number":8,"position":3,"size":3,"base":{"ref":"dev"}}`
			}
			reply(`{"number":5,"title":"Tip","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"lower"}` + stack + `}`)
		case "GET /repos/org/repo/stacks/8":
			reply(`{"number":8,"pull_requests":[{"number":3,"state":"open","merged_at":null},{"number":4,"state":"open","merged_at":null},{"number":5,"state":"open","merged_at":null}]}`)
		case "POST /repos/org/repo/stacks/8/unstack":
			stacked = false
			w.WriteHeader(http.StatusNoContent)
		case "POST /repos/org/repo/stacks":
			reply(`{"number":10}`)
		case "PATCH /repos/org/repo/pulls/5":
			if stacked {
				http.Error(w, `{"message":"Validation Failed: Cannot change the base branch because the pull request is part of a stack."}`, http.StatusUnprocessableEntity)
				return
			}
			reply(`{"number":5,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"tip","sha":"head"},"base":{"ref":"mid"}}`)
		case "GET /repos/org/repo/pulls":
			if got := r.URL.Query().Get("head"); got != "org:mid" {
				t.Errorf("parent lookup head = %q, want org:mid", got)
			}
			reply(`[{"number":6,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid"},"base":{"ref":"dev"},"stack":{"number":9,"position":1,"size":1,"base":{"ref":"dev"}}}]`)
		case "POST /repos/org/repo/stacks/9/add":
			reply(`{"number":9}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{{Action: SubmitUpdate, PRNumber: 5, Head: "tip", HeadSha: "head", Base: "mid", Move: true, InPlace: true}}})
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%+v err=%v calls=%v", got, err, calls)
	}
	if got[0].PRNumber != 5 || got[0].Status != "updated" || got[0].StackNumber != 9 || len(got[0].Warnings) != 0 {
		t.Fatalf("got=%+v, want PR #5 kept and linked into stack #9 without a warning", got[0])
	}
	if slices.Contains(calls, "POST /repos/org/repo/pulls") {
		t.Errorf("calls=%v, want no new pull request", calls)
	}
	if retarget := bodies["PATCH /repos/org/repo/pulls/5"]; retarget["base"] != "mid" {
		t.Errorf("PATCH #5 = %v, want base mid", retarget)
	}
	if relinked, _ := bodies["POST /repos/org/repo/stacks"]["pull_requests"].([]any); !reflect.DeepEqual(relinked, []any{float64(3), float64(4)}) {
		t.Errorf("relinked stack = %v, want the pull requests below #5, #3 and #4", relinked)
	}
	unstack, retarget, relink, add := slices.Index(calls, "POST /repos/org/repo/stacks/8/unstack"), slices.Index(calls, "PATCH /repos/org/repo/pulls/5"), slices.Index(calls, "POST /repos/org/repo/stacks"), slices.Index(calls, "POST /repos/org/repo/stacks/9/add")
	if unstack < 0 || retarget < unstack || relink < unstack || add < retarget {
		t.Errorf("calls=%v, want stack #8 unstacked before #5 is retargeted and linked into stack #9", calls)
	}
}

func TestSubmitInsertsANewPullRequestBetweenTwoOfALiveNativeStack(t *testing.T) {
	var calls []string
	bodies := map[string]map[string]any{}
	stacked, relinked := true, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publishedHeadFixture(t, w, r, "head") {
			return
		}
		reply := func(body string) {
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		}
		call := r.Method + " " + r.URL.Path
		if r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls" {
			call += "?head=" + r.URL.Query().Get("head")
		}
		calls = append(calls, call)
		if r.Method != http.MethodGet {
			var body map[string]any
			if r.ContentLength > 0 {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("%s: decode body: %v", call, err)
				}
			}
			bodies[call] = body
		}
		switch call {
		case "POST /repos/org/repo/pulls":
			reply(`{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid","sha":"head"},"base":{"ref":"lower"}}`)
		case "GET /repos/org/repo/pulls?head=org:lower":
			stack := ""
			if stacked {
				stack = `,"stack":{"number":8,"position":1,"size":2,"base":{"ref":"dev"}}`
			}
			reply(`[{"number":1,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"lower"},"base":{"ref":"dev"}` + stack + `}]`)
		case "GET /repos/org/repo/stacks/8":
			reply(`{"number":8,"pull_requests":[{"number":1,"state":"open","merged_at":null},{"number":2,"state":"open","merged_at":null}]}`)
		case "GET /repos/org/repo/pulls/2":
			stack := ""
			if stacked {
				stack = `,"stack":{"number":8,"position":2,"size":2,"base":{"ref":"dev"}}`
			}
			reply(`{"number":2,"title":"Upper","state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"upper","sha":"head"},"base":{"ref":"lower"}` + stack + `}`)
		case "POST /repos/org/repo/stacks/8/unstack":
			stacked = false
			w.WriteHeader(http.StatusNoContent)
		case "PATCH /repos/org/repo/pulls/2":
			if stacked {
				http.Error(w, `{"message":"Validation Failed: Cannot change the base branch because the pull request is part of a stack."}`, http.StatusUnprocessableEntity)
				return
			}
			reply(`{"number":2,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"upper","sha":"head"},"base":{"ref":"mid"}}`)
		case "GET /repos/org/repo/pulls?head=org:mid":
			reply(`[{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid"},"base":{"ref":"lower"}}]`)
		case "POST /repos/org/repo/stacks":
			relinked = true
			reply(`{"number":10}`)
		case "GET /repos/org/repo/pulls/3":
			stack := ""
			if relinked {
				stack = `,"stack":{"number":10,"position":2,"size":2,"base":{"ref":"dev"}}`
			}
			reply(`{"number":3,"state":"open","head":{"repo":{"full_name":"org/repo"},"ref":"mid","sha":"head"},"base":{"ref":"lower"}` + stack + `}`)
		case "POST /repos/org/repo/stacks/10/add":
			reply(`{"number":10}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	got, err := NewWithToken(srv.URL, "key").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "dev", PRs: []SubmitPR{
		{Action: SubmitCreate, Head: "mid", HeadSha: "head", Base: "lower", Title: "Mid", Body: "Between lower and upper."},
		{Action: SubmitUpdate, PRNumber: 2, Head: "upper", HeadSha: "head", Base: "mid", Move: true, InPlace: true},
	}})
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%+v err=%v calls=%v", got, err, calls)
	}
	if got[1].PRNumber != 2 || got[1].Status != "updated" || got[1].StackNumber != 10 || len(got[1].Warnings) != 0 {
		t.Fatalf("got=%+v, want PR #2 kept and linked into stack #10 above #3 without a warning", got[1])
	}
	if linked, _ := bodies["POST /repos/org/repo/stacks"]["pull_requests"].([]any); !reflect.DeepEqual(linked, []any{float64(1), float64(3)}) {
		t.Errorf("new stack = %v, want #1 and the inserted #3", linked)
	}
	if added, _ := bodies["POST /repos/org/repo/stacks/10/add"]["pull_requests"].([]any); !reflect.DeepEqual(added, []any{float64(2)}) {
		t.Errorf("added to stack #10 = %v, want #2", added)
	}
	unstack, retarget, relink, add := slices.Index(calls, "POST /repos/org/repo/stacks/8/unstack"), slices.Index(calls, "PATCH /repos/org/repo/pulls/2"), slices.Index(calls, "POST /repos/org/repo/stacks"), slices.Index(calls, "POST /repos/org/repo/stacks/10/add")
	if unstack < 0 || retarget < unstack || relink < retarget || add < relink {
		t.Errorf("calls=%v, want stack #8 unstacked, #2 retargeted onto mid, #1 and #3 linked, then #2 added", calls)
	}
}

func TestTrunkTokenReadsTheKeychainItemWhenTheVariableIsUnset(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	keychain := keychainTrunkKey
	t.Cleanup(func() { keychainTrunkKey = keychain })
	keychainTrunkKey = func() (string, error) { return "keychain-key", nil }
	if token, err := NewWithToken("http://invalid.local", "github-key").trunkToken(context.Background()); err != nil || token != "keychain-key" {
		t.Fatalf("trunkToken = %q, %v; want the Keychain item", token, err)
	}
}

func TestTrunkTokenReportsAKeychainFailureInsteadOfAMissingKey(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	keychain := keychainTrunkKey
	t.Cleanup(func() { keychainTrunkKey = keychain })
	keychainTrunkKey = func() (string, error) {
		return "", errors.New("read the trunk-merge-api-key Keychain item: security exited 36: user interaction is not allowed")
	}
	token, err := NewWithToken("http://invalid.local", "github-key").trunkToken(context.Background())
	if !errors.Is(err, ErrNoTrunkKey) || token != "" || !strings.Contains(err.Error(), "security exited 36") {
		t.Fatalf("trunkToken = %q, %v; want the Keychain failure", token, err)
	}
}

func TestTrunkKeyResolvesEnvironmentThenKeychainThenRepositorySSM(t *testing.T) {
	for _, tc := range []struct {
		name, script, env, keychain, want, failure string
		invalid, aws, keychainRead                 bool
	}{
		{name: "environment overrides both", script: "exit 99", env: "explicit-fixture-key", keychain: "keychain-fixture-key", want: "explicit-fixture-key"},
		{name: "keychain precedes repository credential", script: "exit 99", keychain: "keychain-fixture-key", want: "keychain-fixture-key", keychainRead: true},
		{name: "repository credential without a keychain item", script: "printf 'repository-fixture-key\\n'", want: "repository-fixture-key", aws: true, keychainRead: true},
		{name: "repository lookup without a live AWS session", script: "printf 'fixture-output\\n'; printf 'fixture-error\\n' >&2; exit 255", failure: "exit code 255", aws: true, keychainRead: true},
		{name: "invalid repository config", script: "exit 99", invalid: true, failure: "requires a valid aws_profile", keychainRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, calls := fakeTrunkAWS(t, tc.script)
			if tc.invalid {
				config.AWSProfile = ""
			}
			t.Setenv("TRUNK_MERGE_API_KEY", tc.env)
			keychainCalls := 0
			keychainTrunkKey = func() (string, error) {
				keychainCalls++
				return tc.keychain, nil
			}
			token, err := NewWithToken("https://unused.invalid", "github-fixture-key").WithTrunkSSM(config).trunkToken(context.Background())
			if tc.failure != "" {
				if !errors.Is(err, ErrNoTrunkKey) || !strings.Contains(err.Error(), tc.failure) || token != "" {
					t.Fatalf("token=%q error=%v, want ErrNoTrunkKey naming %q", token, err, tc.failure)
				}
				if strings.Contains(err.Error(), "fixture-output") || strings.Contains(err.Error(), "fixture-error") {
					t.Fatalf("repository failure exposed command output: %v", err)
				}
			} else if err != nil || token != tc.want {
				t.Fatalf("token=%q error=%v want=%q", token, err, tc.want)
			}
			if (keychainCalls > 0) != tc.keychainRead {
				t.Fatalf("Keychain reads=%d, want read=%v", keychainCalls, tc.keychainRead)
			}
			_, statErr := os.Stat(calls)
			if tc.aws && statErr != nil || !tc.aws && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("AWS execution=%v, invocation receipt error=%v", tc.aws, statErr)
			}
		})
	}
}

func TestTrunkKeyAbsentEverywhereIsErrNoTrunkKey(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	keychain := keychainTrunkKey
	t.Cleanup(func() { keychainTrunkKey = keychain })
	keychainTrunkKey = func() (string, error) { return "", nil }
	_, err := NewWithToken("https://unused.invalid", "github-fixture-key").WithQueueProvider("trunk").MergeabilityStatuses(context.Background(), "org", "repo", []int{1})
	if !errors.Is(err, ErrNoTrunkKey) {
		t.Fatalf("err=%v, want ErrNoTrunkKey", err)
	}
}

func TestTrunkEmptyRepositoryConfigurationStillUsesKeychain(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	keychain := keychainTrunkKey
	t.Cleanup(func() { keychainTrunkKey = keychain })
	keychainTrunkKey = func() (string, error) { return "keychain-fixture-key", nil }
	token, err := NewWithToken("https://unused.invalid", "github-fixture-key").WithTrunkSSM(TrunkSSMConfig{}).trunkToken(context.Background())
	if err != nil || token != "keychain-fixture-key" {
		t.Fatalf("token=%q error=%v", token, err)
	}
}

func TestTrunkEmptyQueueReadDoesNotResolveCredentials(t *testing.T) {
	config, calls := fakeTrunkAWS(t, "exit 99")
	keychain := keychainTrunkKey
	t.Cleanup(func() { keychainTrunkKey = keychain })
	keychainCalls := 0
	keychainTrunkKey = func() (string, error) {
		keychainCalls++
		return "keychain-fixture-key", nil
	}
	states, err := NewWithToken("https://unused.invalid", "github-fixture-key").WithQueueProvider("trunk").WithTrunkSSM(config).MergeabilityStatuses(context.Background(), "org", "repo", nil)
	if err != nil || len(states) != 0 || keychainCalls != 0 {
		t.Fatalf("states=%v error=%v Keychain calls=%d", states, err, keychainCalls)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty queue read executed AWS: %v", err)
	}
}

func TestBranchHistoryReadsOriginPresenceAndPullRequestHeads(t *testing.T) {
	transientWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { transientWaits = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo", "/repos/org/repo/branches/release", "/repos/org/repo/branches/feature":
			if _, err := fmt.Fprint(w, `{"name":"x"}`); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "/repos/org/repo/pulls":
			if r.URL.Query().Get("state") != "all" {
				t.Errorf("state=%s; want all", r.URL.Query().Get("state"))
			}
			body := `[]`
			if r.URL.Query().Get("head") == "org:feature" {
				body = `[{"number":4}]`
			}
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write fixture response: %v", err)
			}
		case "/repos/org/repo/branches/gone":
			http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	client := NewWithToken(srv.URL, "key")
	for branch, want := range map[string][2]bool{"release": {true, false}, "feature": {true, true}, "gone": {false, false}} {
		onOrigin, headed, err := client.BranchHistory(context.Background(), "org", "repo", branch)
		if err != nil || onOrigin != want[0] || headed != want[1] {
			t.Fatalf("BranchHistory(%s) = %v, %v, %v; want %v", branch, onOrigin, headed, err, want)
		}
	}
	var status *StatusError
	if _, _, err := client.BranchHistory(context.Background(), "org", "repo", "flaky"); !errors.As(err, &status) || status.Status != http.StatusBadGateway {
		t.Fatalf("BranchHistory(flaky) error = %v; want the 502", err)
	}
	if _, _, err := client.BranchHistory(context.Background(), "org", "hidden", "release"); !errors.As(err, &status) || status.Status != http.StatusBadGateway {
		t.Fatalf("BranchHistory on an unreadable repository = %v; want its error, never a missing branch", err)
	}
}

func TestARateLimitNamingAShortWaitOrNoneIsWaitedOutAndALongResetOrAPermissionRefusalIsNot(t *testing.T) {
	secondaryLimitWaits = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { secondaryLimitWaits = []time.Duration{time.Minute, 2 * time.Minute} })
	limitedMessage := "API rate limit exceeded for user ID 709645. If you reach out to GitHub Support for help, please include the request ID E0F4:127C7B:253205:2798AC:6AECE65F and timestamp 2026-11-05 20:26:07 UTC."
	cases := []struct {
		name      string
		remaining string
		reset     time.Duration
		message   string
		calls     int
		created   bool
	}{
		{"a secondary limit with quota left", "3297", 0, limitedMessage, 2, true},
		{"a spent quota that resets within the patience", "0", time.Second, limitedMessage, 2, true},
		{"a primary limit that resets in an hour", "0", time.Hour, limitedMessage, 1, false},
		{"a permission refusal", "4000", 0, "Resource not accessible by integration", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("X-RateLimit-Remaining", tc.remaining)
					if tc.reset > 0 {
						w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(tc.reset).Unix(), 10))
					}
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"message":"` + tc.message + `"}`))
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"number":46512}`))
			}))
			defer srv.Close()
			var notice strings.Builder
			ctx := render.WithSlowWriter(context.Background(), &notice)
			_, err := NewWithToken(srv.URL, "github-key").post(ctx, "/repos/org/repo/pulls", map[string]any{"head": "feature"})
			if calls != tc.calls || (err == nil) != tc.created {
				t.Fatalf("calls=%d err=%v, want calls=%d created=%v", calls, err, tc.calls, tc.created)
			}
			if announced := strings.Contains(notice.String(), "GitHub rate-limited POST /repos/org/repo/pulls, retrying in"); announced != tc.created {
				t.Fatalf("notice = %q, want a wait announced = %v", notice.String(), tc.created)
			}
		})
	}
}

func TestAServerErrorIsRetriedOnAnIdempotentRequestAndNeverOnAPostOrAClientError(t *testing.T) {
	transientWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { transientWaits = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} })
	cases := []struct {
		name       string
		method     string
		status     int
		fails      int
		calls      int
		wantStatus int
		wantTries  string
	}{
		{"a GET retries a 502 and succeeds", http.MethodGet, http.StatusBadGateway, 1, 2, 0, ""},
		{"a PUT retries a 503 twice and succeeds", http.MethodPut, http.StatusServiceUnavailable, 2, 3, 0, ""},
		{"a POST is never retried on a 502", http.MethodPost, http.StatusBadGateway, 1, 1, http.StatusBadGateway, ""},
		{"a 404 is never retried", http.MethodGet, http.StatusNotFound, 1, 1, http.StatusNotFound, ""},
		{"an exhausted retry names its tries and the last error", http.MethodPatch, http.StatusInternalServerError, 9, 4, http.StatusInternalServerError, "(after 4 tries)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls <= tc.fails {
					http.Error(w, `{"message":"boom"}`, tc.status)
					return
				}
				_, _ = w.Write([]byte(`{"number":46512}`))
			}))
			defer srv.Close()
			_, err := NewWithToken(srv.URL, "github-key").send(context.Background(), tc.method, "/repos/org/repo/pulls/46512", map[string]any{"base": "main"})
			if calls != tc.calls {
				t.Fatalf("calls = %d, want %d (err = %v)", calls, tc.calls, err)
			}
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("err = %v, want the retried request to succeed", err)
				}
				return
			}
			var status *StatusError
			if !errors.As(err, &status) || status.Status != tc.wantStatus || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err = %v, want the %d naming boom", err, tc.wantStatus)
			}
			if got := strings.HasSuffix(err.Error(), " (after 4 tries)"); got != (tc.wantTries != "") {
				t.Errorf("err = %q, want the try count only on an exhausted retry", err)
			}
		})
	}
}

func TestPullRequestInfoReadsOnTheAppTokenWhenTheAppIsInstalled(t *testing.T) {
	for _, tc := range []struct {
		name, app, auth, identity string
	}{
		{name: "app installed", app: "app-key", auth: "Bearer app-key", identity: "app"},
		{name: "no app", auth: "Bearer github-key", identity: "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"data":{"repository":{"p0":{"number":7,"state":"OPEN","headRefName":"b","baseRefName":"main"}}}}`))
			}))
			defer srv.Close()
			c := NewWithToken(srv.URL, "github-key")
			c.appToken = func(context.Context, string) (string, error) { return tc.app, nil }
			var charged string
			c.charge = func(_ context.Context, identity, _ string, _ http.Header) error {
				charged = identity
				return nil
			}
			if _, err := c.PullRequestInfo(t.Context(), PullRequestInfoRequest{RepoOwner: "o", RepoName: "r", PRNumbers: []int{7}}); err != nil {
				t.Fatal(err)
			}
			if auth != tc.auth || charged != tc.identity {
				t.Errorf("read on %q charged to %q, want %q charged to %q", auth, charged, tc.auth, tc.identity)
			}
		})
	}
}
