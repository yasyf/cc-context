package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestNativeStackTrackingRequiresCurrentIdentity(t *testing.T) {
	branch := &stackRebaseBranch{Name: "feature", Parent: "base", NewHead: "current-head"}
	parent := &stackPR{Number: 41, State: "OPEN"}
	matching := gtapi.PullRequestInfo{PRNumber: 42, HeadRefName: "feature", BaseRefName: "base", State: gtapi.PROpen, StackNumber: 50, DependentPRNumber: 41, Versions: []gtapi.PRVersion{{HeadSha: "current-head"}}}
	for _, tt := range []struct {
		name string
		edit func(*gtapi.PullRequestInfo)
		want bool
	}{
		{"matches", func(*gtapi.PullRequestInfo) {}, true},
		{"old head", func(p *gtapi.PullRequestInfo) { p.Versions = []gtapi.PRVersion{{HeadSha: "previous-head"}} }, false},
		{"wrong branch", func(p *gtapi.PullRequestInfo) { p.HeadRefName = "other" }, false},
		{"wrong parent branch", func(p *gtapi.PullRequestInfo) { p.BaseRefName = "main" }, false},
		{"missing native stack", func(p *gtapi.PullRequestInfo) { p.StackNumber = 0 }, false},
		{"wrong native parent PR", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber = 99 }, false},
		{"closed PR", func(p *gtapi.PullRequestInfo) { p.State = gtapi.PRClosed }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := matching
			tt.edit(&p)
			if got := nativeStackTracksBranch(p, branch, "main", parent); got != tt.want {
				t.Fatalf("tracks branch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNativeTrackingAllowsAnOrdinaryRootPR(t *testing.T) {
	branch := &stackRebaseBranch{Name: "feature", Parent: "main", NewHead: "current-head"}
	info := gtapi.PullRequestInfo{PRNumber: 42, HeadRefName: "feature", BaseRefName: "main", State: gtapi.PROpen, Versions: []gtapi.PRVersion{{HeadSha: "current-head"}}}
	if !nativeStackTracksBranch(info, branch, "main", nil) {
		t.Fatal("ordinary root PR should not require a multibranch native stack")
	}
}

func TestForeignRepositoryDoesNotInheritCheckoutQueue(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", "git@github.com:org/local.git")
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.queue", "trunk")
	client := gtapi.NewNativeWithToken("https://example.invalid", "unused")
	previous := gtAPINativeDefault
	gtAPINativeDefault = func() *gtapi.Client { return client }
	t.Cleanup(func() { gtAPINativeDefault = previous })
	ctx := withStackProvider(f.Context(), true)
	if got := gtAPIForRepo(ctx, "org/local").QueueProvider(); got != "trunk" {
		t.Fatalf("local queue=%s", got)
	}
	if got := gtAPIForRepo(ctx, "org/foreign").QueueProvider(); got != "github" {
		t.Fatalf("foreign queue inherited local setting: %s", got)
	}
}

func TestNativePublicationRequiresKnownUnqueuedStateForChanges(t *testing.T) {
	for _, tt := range []struct{ name, queue, want string }{
		{"unqueued", `{"mergeQueueEntry":null}`, ""},
		{"queued", `{"mergeQueueEntry":{"state":"AWAITING_CHECKS"}}`, "dequeue"},
		{"missing PR", `null`, "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprintf(w, `{"data":{"repository":{"p0":%s}}}`, tt.queue)
			}))
			defer server.Close()
			client := gtapi.NewNativeWithToken(server.URL, "stub")
			known := map[string]gtapi.PullRequestInfo{"feature": {PRNumber: 42, BaseRefName: "main", Versions: []gtapi.PRVersion{{HeadSha: "published"}}}}
			plan := []gtSubmitBranch{{name: "feature", head: "new-head", base: "main"}}
			err := nativeRefuseQueuedChanges(context.Background(), client, "org", "repo", "ship", plan, known)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error=%v, want %q", err, tt.want)
			}
			if calls != 1 {
				t.Fatalf("queue reads=%d", calls)
			}
			plan[0].head = "published"
			if err := nativeRefuseQueuedChanges(context.Background(), client, "org", "repo", "ship", plan, known); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("unchanged published head polled queue: %d", calls)
			}
		})
	}
}
