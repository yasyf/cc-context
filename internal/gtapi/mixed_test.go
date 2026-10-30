package gtapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/nativeapi"
)

func TestMixedReaderUsesEachPRProviderAndQueue(t *testing.T) {
	var mu sync.Mutex
	var legacyMetadata, legacyQueue []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			var body struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			repository := map[string]any{}
			for _, match := range regexp.MustCompile(`(p\d+):pullRequest\(number:(\d+)\)`).FindAllStringSubmatch(body.Query, -1) {
				number, _ := strconv.Atoi(match[2])
				branch := map[int]string{1: "local-native", 2: "legacy", 3: "server-native", 4: "unknown"}[number]
				pull := map[string]any{"number": number, "id": fmt.Sprintf("PR_%d", number), "headRefName": branch, "headRefOid": "native-head", "baseRefName": "main", "mergeQueueEntry": map[string]any{"state": "AWAITING_CHECKS", "headCommit": map[string]any{"oid": "native-admitted"}}}
				if number == 3 {
					pull["stack"] = map[string]any{"number": 9, "baseRefName": "main"}
				}
				repository[match[1]] = pull
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": repository}}); err != nil {
				t.Error(err)
			}
			return
		}
		var req struct {
			PRNumbers []int `json:"prNumbers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/graphite/mergeability-status" {
			legacyQueue = append([]int(nil), req.PRNumbers...)
			if err := json.NewEncoder(w).Encode(map[string]any{"mergeabilityStatuses": []any{map[string]any{"prNumber": 2, "mergeabilityStatus": MergeabilityWaiting}}}); err != nil {
				t.Error(err)
			}
			return
		}
		legacyMetadata = append([]int(nil), req.PRNumbers...)
		var prs []PullRequestInfo
		for _, number := range req.PRNumbers {
			prs = append(prs, PullRequestInfo{PRNumber: number, HeadRefName: "legacy", Versions: []PRVersion{{HeadSha: "legacy-published", BaseSha: "legacy-fork"}}, MergeQueueStatus: &MergeQueueStatus{IsInGraphiteMq: true, EnqueuedCommit: "legacy-admitted"}})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"status": "ok", "prs": prs}}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	base := NewWithToken(srv.URL, "legacy-token")
	base.native = nativeapi.NewWithToken(srv.URL, "native-token")
	providers := map[string]string{"local-native": "github", "legacy": "graphite", "server-native": "graphite"}
	client := base.WithBranchProviders(providers)
	providers["legacy"] = "github"
	req := PullRequestInfoRequest{RepoOwner: "org", RepoName: "repo", PRNumbers: []int{1, 2, 3, 4}}
	infos, err := client.PullRequestInfo(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 4 || infos[1].Newest().HeadSha != "legacy-published" || infos[1].MergeQueueStatus.EnqueuedCommit != "legacy-admitted" {
		t.Fatalf("legacy history/admission lost: %+v", infos)
	}
	for _, i := range []int{0, 2, 3} {
		if infos[i].Newest().HeadSha != "native-head" || infos[i].MergeQueueStatus.EnqueuedCommit != "native-admitted" {
			t.Fatalf("native record %d lost: %+v", i, infos[i])
		}
	}
	states, err := client.MergeabilityFor(context.Background(), "org", "repo", infos, req.PRNumbers)
	if err != nil {
		t.Fatal(err)
	}
	if states[2] != MergeabilityWaiting || states[1] != MergeabilityQueued || states[3] != MergeabilityQueued || states[4] != MergeabilityQueued {
		t.Fatalf("states=%v", states)
	}
	mu.Lock()
	metadataBatch, queueBatch := legacyMetadata, legacyQueue
	mu.Unlock()
	if !reflect.DeepEqual(metadataBatch, []int{2}) || !reflect.DeepEqual(queueBatch, []int{2}) {
		t.Fatalf("legacy batches metadata=%v queue=%v", metadataBatch, queueBatch)
	}
	if !strings.HasPrefix(client.CacheNamespace(), "mixed-stack--mixed") || !strings.HasPrefix(client.WithQueueProvider("none").CacheNamespace(), "mixed-stack--none--") {
		t.Fatal("mixed cache namespace collision")
	}
	for _, provider := range []string{"github", "graphite", "none"} {
		overridden, err := client.WithQueueProvider(provider).PullRequestInfo(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if overridden[1].Newest().HeadSha != "legacy-published" || overridden[2].StackNumber != 9 {
			t.Fatalf("queue override %s changed stack metadata: %+v", provider, overridden)
		}
		for _, info := range overridden {
			if provider == "none" {
				if info.MergeQueueStatus != nil {
					t.Fatal("disabled queue leaked admission")
				}
				continue
			}
			expected := "native-admitted"
			if provider == "graphite" {
				expected = "legacy-admitted"
			}
			if info.MergeQueueStatus == nil || info.MergeQueueStatus.EnqueuedCommit != expected {
				t.Fatalf("queue override %s admission=%+v", provider, info.MergeQueueStatus)
			}
		}
	}
	if infos[2].StackNumber != 9 {
		t.Fatal("server stack ownership was replaced by stale local legacy ownership")
	}
}

func TestMixedCacheTracksOwnershipAndIgnoresMapOrder(t *testing.T) {
	base := New("https://example.invalid")
	first := base.WithBranchProviders(map[string]string{"a": "graphite", "b": "github"})
	reordered := base.WithBranchProviders(map[string]string{"b": "github", "a": "graphite"})
	converted := base.WithBranchProviders(map[string]string{"a": "github", "b": "github"})
	for _, queue := range []string{"", "github", "graphite", "trunk", "none"} {
		before := first.WithQueueProvider(queue).CacheNamespace()
		if before != reordered.WithQueueProvider(queue).CacheNamespace() {
			t.Fatalf("map order changed namespace for %q", queue)
		}
		if before == converted.WithQueueProvider(queue).CacheNamespace() {
			t.Fatalf("conversion reused namespace for %q", queue)
		}
	}
}
