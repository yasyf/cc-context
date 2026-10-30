package gtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativeapi"
)

func TestOriginalConstructorsRetainGraphiteProvider(t *testing.T) {
	for _, client := range []*Client{Default(), New("https://example.invalid"), NewWithToken("https://example.invalid", "token")} {
		if client.IsNative() {
			t.Fatal("original constructor silently selected native provider")
		}
	}
	native := NewNative()
	if !native.IsNative() {
		t.Fatal("native constructor did not select native provider")
	}
	if !native.WithQueueProvider("trunk").IsNative() {
		t.Fatal("queue choice changed stack provider")
	}
	if New("https://example.invalid").WithQueueProvider("trunk").IsNative() {
		t.Fatal("queue choice changed legacy stack provider")
	}
}

func TestNativeFacadeMapsActualStackAndQueueSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer github-token" {
			t.Errorf("unexpected native request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":{"repository":{"p0":{"id":"PR_2","number":2,"state":"OPEN","headRefName":"child","headRefOid":"current-head","baseRefName":"parent","baseRefOid":"parent-head","updatedAt":"2026-10-30T12:00:00Z","mergeQueueEntry":{"state":"AWAITING_CHECKS","headCommit":{"oid":"queued-head"}},"stackEntry":{"position":2},"stack":{"number":7,"baseRefName":"main","entries":{"nodes":[{"position":1,"pullRequest":{"number":1}},{"position":2,"pullRequest":{"number":2}}],"pageInfo":{"hasNextPage":false}}}}}}`)
	}))
	defer srv.Close()
	client := NewNativeWithToken(srv.URL, "github-token")
	infos, err := client.PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].PRID != "PR_2" || infos[0].StackNumber != 7 || infos[0].DependentPRNumber != 1 || infos[0].Newest().HeadSha != "current-head" {
		t.Fatalf("infos=%+v", infos)
	}
	if queue := infos[0].MergeQueueStatus; queue == nil || !queue.InQueue || queue.IsInGraphiteMq || queue.EnqueuedCommit != "queued-head" {
		t.Fatalf("queue=%+v", queue)
	}
	if !InMergeQueue(&infos[0], "") {
		t.Fatal("common queue guard lost native admission")
	}
	infos, err = client.WithQueueProvider("trunk").PullRequestInfo(context.Background(), PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	if infos[0].MergeQueueStatus != nil {
		t.Fatal("GitHub snapshot leaked into explicitly selected Trunk queue")
	}
}

func TestExplicitQueueOverrideClearsLegacyQueueButKeepsVersions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphite/cli/pull-request-info" || r.Header.Get("Authorization") != "token graphite-token" {
			t.Errorf("legacy protocol changed: %s %s", r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"result":{"status":"ok","prs":[{"prNumber":2,"headRefName":"feature","mergeQueueStatus":{"isInGraphiteMq":true,"enqueuedCommit":"old-queue-head"},"versions":[{"headSha":"published","baseSha":"fork-point","baseName":"parent","createdAt":"2026-10-30T12:00:00Z"}]}]}}`)
	}))
	defer srv.Close()
	client := NewWithToken(srv.URL, "graphite-token")
	req := PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{2}}
	legacy, err := client.PullRequestInfo(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if legacy[0].MergeQueueStatus == nil || !legacy[0].MergeQueueStatus.IsInGraphiteMq {
		t.Fatal("legacy default queue behavior changed")
	}
	overridden, err := client.WithQueueProvider("trunk").PullRequestInfo(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if overridden[0].MergeQueueStatus != nil || !reflect.DeepEqual(overridden[0].Versions, legacy[0].Versions) {
		t.Fatalf("override corrupted stack metadata: %+v", overridden)
	}
}

func TestExplicitTrunkQueueFailureNeverCallsGraphite(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"mergeabilityStatuses":[{"prNumber":2,"mergeabilityStatus":"QUEUED_TO_MERGE"}]}`)
	}))
	defer srv.Close()
	_, err := NewWithToken(srv.URL, "graphite-token").WithQueueProvider("trunk").MergeabilityStatuses(context.Background(), "org", "repo", []int{2})
	if err == nil || !strings.Contains(err.Error(), "TRUNK_MERGE_API_KEY") || calls != 0 {
		t.Fatalf("error=%v legacy calls=%d", err, calls)
	}
}

func TestNativeFacadeReturnsTypedPartialSubmitWithoutFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasPrefix(r.URL.Path, "/graphite/") {
			t.Errorf("native operation called Graphite: %s", r.URL)
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()
	_, err := NewNativeWithToken(srv.URL, "github-token").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "feature", HeadSha: "head", Base: "main", Title: "feature"}}})
	var submit *SubmitError
	var status *StatusError
	if !errors.As(err, &submit) || !errors.As(err, &status) || !errors.Is(err, ErrUnauthorized) || calls != 1 || len(submit.Failed) != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestNativeFacadePublishesActualStackIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /repos/org/repo/pulls":
			fmt.Fprint(w, `{"number":2,"node_id":"PR_2","html_url":"https://github.com/org/repo/pull/2","state":"open","head":{"ref":"child","sha":"head"},"base":{"ref":"parent"}}`)
		case "GET /repos/org/repo/pulls":
			fmt.Fprint(w, `[{"number":1,"state":"open","head":{"ref":"parent"},"base":{"ref":"main"}}]`)
		case "POST /repos/org/repo/stacks":
			var body struct {
				PullRequests []int `json:"pull_requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(body.PullRequests, []int{1, 2}) {
				t.Errorf("native membership=%v", body.PullRequests)
			}
			fmt.Fprint(w, `{"number":7}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()
	submitted, err := NewNativeWithToken(srv.URL, "github-token").SubmitPullRequests(context.Background(), SubmitRequest{RepoOwner: "org", RepoName: "repo", TrunkBranchName: "main", PRs: []SubmitPR{{Action: SubmitCreate, Head: "child", HeadSha: "head", Base: "parent", Title: "child"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(submitted) != 1 || submitted[0].PRID != "PR_2" || submitted[0].StackNumber != 7 {
		t.Fatalf("submitted=%+v", submitted)
	}
}

func TestNativeFacadeRejectsLegacyOnlyOperations(t *testing.T) {
	client := NewNative()
	_, err := client.CheckAuth(context.Background(), "org", "repo")
	if !errors.Is(err, ErrGraphiteOnly) {
		t.Fatalf("auth=%v", err)
	}
	_, err = client.IsRepoSynced(context.Background(), "org", "repo")
	if !errors.Is(err, ErrGraphiteOnly) {
		t.Fatalf("sync=%v", err)
	}
	_, err = client.PreSubmitPullRequests(context.Background(), "org", "repo", nil)
	if !errors.Is(err, ErrGraphiteOnly) {
		t.Fatalf("presubmit=%v", err)
	}
}

func TestNativeQueueStatesRetainCommonSafetyDomain(t *testing.T) {
	states, err := commonQueueStatuses(map[int]string{1: "testing", 2: "pendingFailure", 3: "notReady", 4: "queued:newState", 5: "failed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: MergeabilityQueued, 2: MergeabilityFailureHandling, 3: MergeabilityWaiting, 4: MergeabilityQueued, 5: "failed"}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("states=%v want=%v", states, want)
	}
	cause := &nativeapi.SubmitError{Submitted: []nativeapi.SubmittedPR{{Head: "feature", PRID: "PR_1", StackNumber: 7, PRNumber: 1}}, Failed: []nativeapi.BranchSubmitError{{Head: "other", Message: "native stack rejected"}}}
	var converted *SubmitError
	if !errors.As(commonNativeError(cause), &converted) || converted.Submitted[0].PRID != "PR_1" || converted.Submitted[0].StackNumber != 7 {
		t.Fatalf("converted=%+v", converted)
	}
}

func TestProviderSelectionErrorStopsEveryRouteWithoutFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "must not call", 500) }))
	defer srv.Close()
	selection := errors.New("native stack catalog is unreadable")
	original := NewWithToken(srv.URL, "graphite-token")
	client := original.WithProviderError(selection).WithQueueProvider("trunk")
	ctx := context.Background()
	methods := []func() error{
		func() error { _, err := client.CheckAuth(ctx, "org", "repo"); return err },
		func() error { _, err := client.IsRepoSynced(ctx, "org", "repo"); return err },
		func() error { _, err := client.PullRequestInfo(ctx, PullRequestInfoRequest{}); return err },
		func() error { _, err := client.MergeabilityStatuses(ctx, "org", "repo", []int{2}); return err },
		func() error { _, err := client.PreSubmitPullRequests(ctx, "org", "repo", nil); return err },
		func() error { _, err := client.SubmitPullRequests(ctx, SubmitRequest{}); return err },
	}
	for i, call := range methods {
		if err := call(); !errors.Is(err, selection) {
			t.Fatalf("route %d error=%v", i, err)
		}
	}
	if calls != 0 || original.providerErr != nil {
		t.Fatalf("network calls=%d original error=%v", calls, original.providerErr)
	}
}

func TestQueueProviderAndCacheNamespaceUseEffectiveProviderPair(t *testing.T) {
	seen := map[string]string{}
	for _, native := range []bool{false, true} {
		client := NewWithToken("https://example.invalid", "unused")
		stack := "graphite"
		if native {
			client = NewNativeWithToken("https://example.invalid", "unused")
			stack = "github"
		}
		for _, queue := range []string{"", "graphite", "github", "trunk", "none"} {
			selected := client.WithQueueProvider(queue)
			effective := queue
			if effective == "" {
				effective = stack
			}
			if selected.QueueProvider() != effective {
				t.Fatalf("stack=%s queue=%q effective=%s", stack, queue, selected.QueueProvider())
			}
			namespace := selected.CacheNamespace()
			if stack == "graphite" && effective == "graphite" {
				if namespace != "" {
					t.Fatalf("legacy namespace changed: %q", namespace)
				}
				continue
			}
			if namespace == "" || strings.ContainsAny(namespace, "/\\.") {
				t.Fatalf("unsafe namespace %q", namespace)
			}
			pair := stack + "/" + effective
			if prior, ok := seen[namespace]; ok && prior != pair {
				t.Fatalf("cache collision %s and %s in %s", prior, pair, namespace)
			}
			seen[namespace] = pair
		}
	}
}

func TestInvalidQueueProviderFailsBeforeAnyNetworkReadOrMutation(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "must not call", 500) }))
	defer srv.Close()
	original := NewWithToken(srv.URL, "graphite-token")
	selected := original.WithQueueProvider("../../trunk")
	if selected.QueueProvider() != "invalid" || selected.CacheNamespace() != "invalid" {
		t.Fatalf("invalid cache namespace=%q queue=%q", selected.CacheNamespace(), selected.QueueProvider())
	}
	ctx := context.Background()
	if _, err := selected.PullRequestInfo(ctx, PullRequestInfoRequest{}); err == nil || !strings.Contains(err.Error(), "unknown ccx.queue provider") {
		t.Fatalf("read=%v", err)
	}
	if _, err := selected.MergeabilityStatuses(ctx, "org", "repo", nil); err == nil {
		t.Fatal("queue read accepted invalid provider")
	}
	if _, err := selected.SubmitPullRequests(ctx, SubmitRequest{}); err == nil {
		t.Fatal("mutation accepted invalid provider")
	}
	if calls != 0 || original.QueueProvider() != "graphite" || original.CacheNamespace() != "" {
		t.Fatalf("network calls=%d original=%+v", calls, original)
	}
}
