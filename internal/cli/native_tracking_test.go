package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/vcs"
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
				_, _ = fmt.Fprintf(w, `{"data":{"repository":{"p0":%s}}}`, tt.queue)
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

func TestNativeQueueProbeDoesNotLandAReusedBranchByName(t *testing.T) {
	f := shipRepo(t)
	current := f.Out(t, "git", "rev-parse", "HEAD")
	for _, tt := range []struct {
		name, head string
		want       queueVerdict
	}{
		{"same published head", current, queueMerged},
		{"earlier branch history", "earlier-head", queueUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := map[string]any{"data": map[string]any{"repository": map[string]any{
					"h0": map[string]any{"totalCount": 0, "nodes": []any{}},
					"c0": map[string]any{"nodes": []any{map[string]any{"number": 42, "state": "MERGED", "headRefOid": tt.head, "headRefName": "main", "baseRefName": "main"}}},
				}}}
				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client := gtapi.NewNativeWithToken(server.URL, "stub")
			got := nativeQueueProbe(withGTAPI(f.Context(), client), lane{kind: vcs.Git, root: f.Dir, native: true, repo: &vcs.Repo{NameWithOwner: "org/repo"}})
			if got.Verdict != tt.want {
				t.Fatalf("probe=%+v want %s", got, tt.want)
			}
		})
	}
}

func TestStackNewInheritsExplicitLegacyParentFromNativeCheckout(t *testing.T) {
	f := shipGTRepo(t)
	shipGTStack(t, f, "legacy-parent")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-c", "native-current", "main")
	writeShipFile(t, f.Dir, "native.txt", "native change\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "native.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-m", "native change")
	base := f.Out(t, "git", "rev-parse", "main")
	head := f.Out(t, "git", "rev-parse", "HEAD")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.AdoptRoot(f.Context(), common, "native-current", "main", base, head); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(t.TempDir(), "legacy-child")
	if _, _, err := runStackCmd(t, f, "new", "legacy-child", "--parent", "legacy-parent", "--full-history", "--path", child); err != nil {
		t.Fatal(err)
	}
	provider, err := gtmeta.Provider(f.Context(), common, "legacy-child")
	if err != nil || provider != gtmeta.ProviderGraphite {
		t.Fatalf("child provider=%s error=%v", provider, err)
	}
	if native, err := gtmeta.NativeMembership(common, "legacy-child"); err != nil || native {
		t.Fatalf("legacy child adopted as native: %v %v", native, err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "legacy-parent")
	nativeChild := filepath.Join(t.TempDir(), "native-child")
	if _, _, err := runStackCmd(t, f, "new", "native-child", "--parent", "native-current", "--full-history", "--path", nativeChild); err != nil {
		t.Fatal(err)
	}
	if native, err := gtmeta.NativeMembership(common, "native-child"); err != nil || !native {
		t.Fatalf("native parent produced nonnative child: %v %v", native, err)
	}
}
