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
	"strconv"
	"strings"
	"testing"
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
		{name: "kept merged bottom", bottom: `"state":"closed","merged_at":"2026-10-30T12:00:00Z"`, unstacked: `{"number":7,"pull_requests":[{"number":1,"state":"closed","merged_at":"2026-10-30T12:00:00Z"}]}`, want: "POST /repos/org/repo/stacks/7/add", relinked: []int{2, 4}},
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
		if _, err := fmt.Fprint(w, `[]`); err != nil {
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
	if err == nil || token != "" || !strings.Contains(err.Error(), "security exited 36") {
		t.Fatalf("trunkToken = %q, %v; want the Keychain failure", token, err)
	}
}

func TestTrunkRepositorySSMPrecedesImplicitKeychain(t *testing.T) {
	for _, tc := range []struct {
		name, script, env, want, failure string
		invalid, aws                     bool
	}{
		{name: "repository credential", script: "printf 'repository-fixture-key\\n'", want: "repository-fixture-key", aws: true},
		{name: "invalid repository config", script: "exit 99", invalid: true, failure: "requires a valid aws_profile"},
		{name: "repository lookup failure", script: "printf 'fixture-output\\n'; printf 'fixture-error\\n' >&2; exit 42", failure: "exit code 42", aws: true},
		{name: "environment overrides both", script: "exit 99", env: "explicit-fixture-key", want: "explicit-fixture-key"},
		{name: "environment overrides invalid repository config", script: "exit 99", env: "explicit-fixture-key", want: "explicit-fixture-key", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, calls := fakeTrunkAWS(t, tc.script)
			if tc.invalid {
				config.AWSProfile = ""
			}
			t.Setenv("TRUNK_MERGE_API_KEY", tc.env)
			keychain := keychainTrunkKey
			t.Cleanup(func() { keychainTrunkKey = keychain })
			keychainCalls := 0
			keychainTrunkKey = func() (string, error) {
				keychainCalls++
				return "implicit-keychain-fixture-key", nil
			}
			token, err := NewWithToken("https://unused.invalid", "github-fixture-key").WithTrunkSSM(config).trunkToken(context.Background())
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) || token != "" {
					t.Fatalf("expected repository failure %q, got token=%q error=%v", tc.failure, token, err)
				}
				if strings.Contains(err.Error(), "fixture-output") || strings.Contains(err.Error(), "fixture-error") {
					t.Fatalf("repository failure exposed command output: %v", err)
				}
			} else if err != nil || token != tc.want {
				t.Fatalf("token=%q error=%v want=%q", token, err, tc.want)
			}
			if keychainCalls != 0 {
				t.Fatalf("repository configuration unexpectedly queried Keychain %d times", keychainCalls)
			}
			_, statErr := os.Stat(calls)
			if tc.aws && statErr != nil || !tc.aws && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("AWS execution=%v, invocation receipt error=%v", tc.aws, statErr)
			}
		})
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
