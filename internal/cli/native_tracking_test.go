package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
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
		{"bottom of a fork stack on the parent", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber, p.CliTrunk = 0, "base" }, true},
		{"bottom of a stack on another base", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber, p.CliTrunk = 0, "main" }, false},
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

func TestNativeRefuseFork(t *testing.T) {
	parent := gtapi.PullRequestInfo{PRNumber: 15, HeadRefName: "card", StackNumber: 16, OpenAbove: []gtapi.StackPull{{Number: 27, Head: "claim"}, {Number: 28, Head: "claim-2"}}}
	for _, tt := range []struct {
		name    string
		parents map[string]string
		known   map[string]gtapi.PullRequestInfo
		want    string
	}{
		{
			name:    "an open sibling names both pull requests and the stack's top",
			parents: map[string]string{"card": "dev", "thread": "card"},
			known:   map[string]gtapi.PullRequestInfo{"card": parent, "thread": {PRNumber: 34, HeadRefName: "thread"}},
			want:    "stack rebase: #34 thread and #27 claim both sit on #15 card. Native GitHub stack #16 is a single chain and GitHub has no stack of one pull request for a fork, so #34 thread cannot register. Nothing was pushed. Chain it on the stack's top: ccx vcs stack rebase --parent thread=claim-2. Or take it off #15: ccx vcs stack rebase --parent thread=dev",
		},
		{
			name:    "a branch with no pull request yet is named by branch",
			parents: map[string]string{"thread": "card"},
			known:   map[string]gtapi.PullRequestInfo{"card": parent},
			want:    "stack rebase: thread and #27 claim both sit on #15 card. Native GitHub stack #16 is a single chain and GitHub has no stack of one pull request for a fork, so thread cannot register. Nothing was pushed. Chain it on the stack's top: ccx vcs stack rebase --parent thread=claim-2. Or take it off #15: ccx vcs stack rebase --parent thread=dev",
		},
		{
			name:    "a landed or closed member above the parent leaves no sibling",
			parents: map[string]string{"thread": "card"},
			known:   map[string]gtapi.PullRequestInfo{"card": {PRNumber: 15, HeadRefName: "card", StackNumber: 16}},
		},
		{
			name:    "the sibling already in the stack passes",
			parents: map[string]string{"claim": "card"},
			known:   map[string]gtapi.PullRequestInfo{"card": parent, "claim": {PRNumber: 27, HeadRefName: "claim", StackNumber: 16}},
		},
		{
			name:    "a fork with a child of its own opens its own stack",
			parents: map[string]string{"fork": "card", "fork-tip": "fork"},
			known:   map[string]gtapi.PullRequestInfo{"card": parent},
		},
		{
			name:    "a parent this run cannot read passes to the verdict",
			parents: map[string]string{"thread": "card"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := nativeRefuseFork("stack rebase", "dev", tt.parents, tt.known)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestStackPublishedParentsLeavesOutWhatTheRunWillNotRegister(t *testing.T) {
	run := &stackRebaseRun{Trunk: "main", Branches: []stackRebaseBranch{
		{Name: "bottom", Parent: "main"},
		{Name: "landed", Parent: "bottom", Landed: "#7 landed"},
		{Name: "tip", Parent: "bottom"},
		{Name: "another-lane", Parent: "bottom", Pinned: true},
		{Name: "unpublished", Parent: "bottom", LocalOnly: true},
	}}
	if got, want := stackPublishedParents(run), (map[string]string{"tip": "bottom"}); !maps.Equal(got, want) {
		t.Fatalf("parents = %v, want %v", got, want)
	}
}

func TestNativeTrackingAllowsAnOrdinaryRootPR(t *testing.T) {
	branch := &stackRebaseBranch{Name: "feature", Parent: "main", NewHead: "current-head"}
	info := gtapi.PullRequestInfo{PRNumber: 42, HeadRefName: "feature", BaseRefName: "main", State: gtapi.PROpen, Versions: []gtapi.PRVersion{{HeadSha: "current-head"}}}
	if !nativeStackTracksBranch(info, branch, "main", nil) {
		t.Fatal("ordinary root PR should not require a multibranch native stack")
	}
}

func TestAQueueReadOutsideACheckoutOfItsRepositoryRefusesAndNamesTheFix(t *testing.T) {
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
	if !prQueueUnread(ctx, "org/foreign") || prQueueUnread(ctx, "org/local") {
		t.Fatalf("queue unread: foreign=%v local=%v, want only the foreign repository unread", prQueueUnread(ctx, "org/foreign"), prQueueUnread(ctx, "org/local"))
	}
	if _, err := openPRState(ctx, "org/foreign", io.Discard); err != nil {
		t.Fatalf("read of org/foreign from an org/local checkout = %v, want an answer from GitHub alone", err)
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestShipPublishesWithoutATrunkKeyAndSaysTheQueueStateIsUnknown(t *testing.T) {
	noTrunkKeyAnnounced = sync.Once{}
	var notices strings.Builder
	ctx := render.WithSlowWriter(context.Background(), &notices)
	client := gtapi.NewNativeWithToken("https://unused.invalid", "stub").WithProviderError(fmt.Errorf("read the merge queue: %w", gtapi.ErrNoTrunkKey))
	known := map[string]gtapi.PullRequestInfo{"feature": {PRNumber: 42, BaseRefName: "main", Versions: []gtapi.PRVersion{{HeadSha: "published"}}}}
	plan := []gtSubmitBranch{{name: "feature", head: "new-head", base: "main"}}
	for range 2 {
		if err := nativeRefuseQueuedChanges(ctx, client, "org", "repo", "ship", plan, known); err != nil {
			t.Fatalf("ship refused without a Trunk key: %v", err)
		}
	}
	if got := notices.String(); got != "ccx: Trunk queue state: unknown (no Trunk key)\n" {
		t.Fatalf("notices=%q, want the unknown queue state announced once", got)
	}
}

func TestNativeQueueProbeDoesNotLandAReusedBranchByName(t *testing.T) {
	f := shipRepo(t)
	current := strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
	for _, tt := range []struct {
		name, head string
		want       queueVerdict
	}{
		{"same published head", current, queueMerged},
		{"earlier branch history", "earlier-head", queueUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	base := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
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

func TestStackNewPrefersNativeForGitHubOriginWithoutCatalog(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	seedLaneRecords(f.Context(), t, f.Dir, laneSeed{})
	origin := "https://github.com/yasyf/cc-context.git"
	mustRun(t, f.Env(), f.Dir, "git", "config", "url."+f.RemoteDir+".insteadOf", origin)
	mustRun(t, f.Env(), f.Dir, "git", "remote", "set-url", "origin", origin)
	child := filepath.Join(t.TempDir(), "native-first")
	if _, _, err := runStackCmd(t, f, "new", "native-first", "--full-history", "--path", child); err != nil {
		t.Fatal(err)
	}
	native, err := gtmeta.NativeMembership(filepath.Join(f.Dir, ".git"), "native-first")
	if err != nil || !native {
		t.Fatalf("new GitHub stack native=%v error=%v", native, err)
	}
	assertNoGT(t, vcstest.Invocations(t, f.ArgvLog))
}

// TestStackNewTracksAnUntrackedNativeParentFirst is cc-transcript's
// parallel-grep: a child cut on fix/discovery-index, a plain pull request branch
// no native stack tracked, left its restack with no parent below the child's.
func TestStackNewTracksAnUntrackedNativeParentFirst(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	stubStackPRs(t, f, nil)
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	shipGTUntracked(t, f, "loose")
	loose := gitAt(t, f.Env(), f.Dir, "rev-parse", "loose")
	child := filepath.Join(t.TempDir(), "child")

	out, errOut, err := runStackCmd(t, f, "new", "child", "--full-history", "--path", child)
	if err != nil {
		t.Fatalf("stack new on untracked loose: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, "tracked loose onto main"+shipSep+"cut child onto loose") {
		t.Errorf("stack new = %q, want loose tracked onto main before child is cut onto it", out)
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	for branch, want := range map[string]gtRef{"loose": {Ref: "main", SHA: trunk}, "child": {Ref: "loose", SHA: loose}} {
		if s := state[branch]; len(s.Parents) != 1 || s.Parents[0] != want {
			t.Errorf("%s = %+v, want it recorded on %s at %.12s", branch, s, want.Ref, want.SHA)
		}
	}
}
