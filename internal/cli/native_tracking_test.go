package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestNativeStackTrackingRequiresCurrentIdentity(t *testing.T) {
	branch := &stackRebaseBranch{Name: "feature", Parent: "base", NewHead: "current-head"}
	parent := &stackPR{Number: 41, State: "OPEN"}
	matching := gtapi.PullRequestInfo{PRNumber: 42, HeadRefName: "feature", BaseRefName: "base", State: gtapi.PROpen, StackNumber: 50, DependentPRNumber: 41, Versions: []gtapi.PRVersion{{HeadSha: "current-head"}}}
	sibling := []gtapi.StackPull{{Number: 43, Head: "sibling"}}
	for _, tt := range []struct {
		name  string
		edit  func(*gtapi.PullRequestInfo)
		above []gtapi.StackPull
		want  bool
	}{
		{"matches", func(*gtapi.PullRequestInfo) {}, nil, true},
		{"old head", func(p *gtapi.PullRequestInfo) { p.Versions = []gtapi.PRVersion{{HeadSha: "previous-head"}} }, nil, false},
		{"wrong branch", func(p *gtapi.PullRequestInfo) { p.HeadRefName = "other" }, nil, false},
		{"wrong parent branch", func(p *gtapi.PullRequestInfo) { p.BaseRefName = "main" }, nil, false},
		{"missing native stack", func(p *gtapi.PullRequestInfo) { p.StackNumber = 0 }, nil, false},
		{"lone fork beside an open pull request above its parent", func(p *gtapi.PullRequestInfo) { p.StackNumber, p.DependentPRNumber = 0, 0 }, sibling, true},
		{"lone fork on an old head", func(p *gtapi.PullRequestInfo) {
			p.StackNumber, p.Versions = 0, []gtapi.PRVersion{{HeadSha: "previous-head"}}
		}, sibling, false},
		{"lone fork on another base", func(p *gtapi.PullRequestInfo) { p.StackNumber, p.BaseRefName = 0, "main" }, sibling, false},
		{"wrong native parent PR", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber = 99 }, nil, false},
		{"closed PR", func(p *gtapi.PullRequestInfo) { p.State = gtapi.PRClosed }, nil, false},
		{"bottom of a fork stack on the parent", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber, p.CliTrunk = 0, "base" }, nil, true},
		{"bottom of a stack on another base", func(p *gtapi.PullRequestInfo) { p.DependentPRNumber, p.CliTrunk = 0, "main" }, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := matching
			tt.edit(&p)
			if got := nativeStackTracksBranch(p, branch, "main", parent, gtapi.PullRequestInfo{PRNumber: 41, HeadRefName: "base", StackNumber: 50, OpenAbove: tt.above}); got != tt.want {
				t.Fatalf("tracks branch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGTDropUnchangedResubmitsANativePullRequestOutsideItsParentsStack(t *testing.T) {
	branch := gtSubmitBranch{name: "upper", head: "upper-head", base: "lower", baseSha: "lower-head", pr: 2}
	parent := gtapi.PullRequestInfo{PRNumber: 4, HeadRefName: "lower", BaseRefName: "main", State: gtapi.PROpen, StackNumber: 5}
	last := map[string]gtmeta.Version{"upper": {HeadSha: "upper-head", BaseSha: "lower-head", BaseName: "lower"}}
	for _, tt := range []struct {
		name      string
		edit      func(*gtapi.PullRequestInfo)
		orphaned  bool
		above     bool
		graphite  bool
		unchanged bool
	}{
		{name: "linked right above its parent", edit: func(p *gtapi.PullRequestInfo) { p.StackNumber, p.DependentPRNumber = 5, 4 }, unchanged: true},
		{name: "in no native stack", edit: func(*gtapi.PullRequestInfo) {}},
		{name: "in no native stack beside an open pull request above its parent", edit: func(*gtapi.PullRequestInfo) {}, above: true, unchanged: true},
		{name: "above another pull request", edit: func(p *gtapi.PullRequestInfo) { p.StackNumber, p.DependentPRNumber = 5, 1 }},
		{name: "bottom of a stack on its parent's branch", edit: func(p *gtapi.PullRequestInfo) { p.StackNumber, p.CliTrunk = 7, "lower" }, unchanged: true},
		{name: "in a stack under a parent the run cannot read", edit: func(p *gtapi.PullRequestInfo) { p.StackNumber, p.DependentPRNumber = 5, 9 }, orphaned: true, unchanged: true},
		{name: "on a Graphite lane", edit: func(*gtapi.PullRequestInfo) {}, graphite: true, unchanged: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pr := gtapi.PullRequestInfo{PRNumber: 2, HeadRefName: "upper", BaseRefName: "lower", State: gtapi.PROpen, Versions: []gtapi.PRVersion{{HeadSha: "upper-head", BaseSha: "lower-head", BaseName: "lower"}}}
			tt.edit(&pr)
			lower := parent
			if tt.above {
				lower.OpenAbove = []gtapi.StackPull{{Number: 3, Head: "sibling"}}
			}
			known := map[string]gtapi.PullRequestInfo{"upper": pr, "lower": lower}
			if tt.orphaned {
				delete(known, "lower")
			}
			_, unchanged := gtDropUnchanged([]gtSubmitBranch{branch}, last, known, nil, "", "main", func(string) *bool { return nil }, !tt.graphite)
			if got := len(unchanged) == 1; got != tt.unchanged {
				t.Fatalf("unchanged = %v, want upper left out %v", unchanged, tt.unchanged)
			}
		})
	}
}

func TestNativeTrackingAllowsAnOrdinaryRootPR(t *testing.T) {
	branch := &stackRebaseBranch{Name: "feature", Parent: "main", NewHead: "current-head"}
	info := gtapi.PullRequestInfo{PRNumber: 42, HeadRefName: "feature", BaseRefName: "main", State: gtapi.PROpen, Versions: []gtapi.PRVersion{{HeadSha: "current-head"}}}
	if !nativeStackTracksBranch(info, branch, "main", nil, gtapi.PullRequestInfo{}) {
		t.Fatal("ordinary root PR should not require a multibranch native stack")
	}
}

func TestNativeTrackingSettlesALoneForkOffALowerStackEntry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entries := `{"nodes":[` +
			`{"position":1,"pullRequest":{"number":14,"state":"OPEN","headRefName":"base"}},` +
			`{"position":2,"pullRequest":{"number":15,"state":"OPEN","headRefName":"card"}},` +
			`{"position":3,"pullRequest":{"number":27,"state":"OPEN","headRefName":"claim"}}` +
			`],"pageInfo":{"hasNextPage":false}}`
		_, _ = fmt.Fprintf(w, `{"data":{"repository":{`+
			`"p0":{"number":14,"state":"OPEN","headRefName":"base","headRefOid":"base-head","baseRefName":"main","stackEntry":{"position":1},"stack":{"number":16,"baseRefName":"main","entries":%[1]s}},`+
			`"p1":{"number":15,"state":"OPEN","headRefName":"card","headRefOid":"card-head","baseRefName":"base","stackEntry":{"position":2},"stack":{"number":16,"baseRefName":"main","entries":%[1]s}},`+
			`"p2":{"number":34,"state":"OPEN","headRefName":"thread","headRefOid":"thread-head","baseRefName":"card"}`+
			`}}}`, entries)
	}))
	defer server.Close()
	run := &stackRebaseRun{Trunk: "main", Branches: []stackRebaseBranch{
		{Name: "base", Parent: "main", NewHead: "base-head", Pinned: true},
		{Name: "card", Parent: "base", NewHead: "card-head", Pinned: true},
		{Name: "thread", Parent: "card", NewHead: "thread-head"},
	}}
	prs := map[string]*stackPR{"base": {Number: 14, State: "OPEN"}, "card": {Number: 15, State: "OPEN"}, "thread": {Number: 34, State: "OPEN"}}
	ctx := withGTAPI(context.Background(), gtapi.NewNativeWithToken(server.URL, "stub"))
	untracked, err := nativeReadTracking(ctx, lane{native: true, repo: &vcs.Repo{NameWithOwner: "org/repo"}}, run, []string{"base", "card", "thread"}, prs)
	if err != nil {
		t.Fatal(err)
	}
	if len(untracked) != 0 {
		t.Fatalf("untracked = %+v, want #34 settled on #15 beside #27", untracked)
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
	for _, tt := range []struct {
		name, queue, want string
		reads             int
	}{
		{"unqueued", `{"mergeQueueEntry":null}`, "", 1},
		{"queued", `{"mergeQueueEntry":{"state":"AWAITING_CHECKS"}}`, "dequeue", 1},
		{"missing PR", `null`, "read the github queue state of #42: no record after a second read", 2},
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
			if calls != tt.reads {
				t.Fatalf("queue reads=%d, want %d", calls, tt.reads)
			}
			plan[0].head = "published"
			if err := nativeRefuseQueuedChanges(context.Background(), client, "org", "repo", "ship", plan, known); err != nil {
				t.Fatal(err)
			}
			if calls != tt.reads {
				t.Fatalf("unchanged published head polled queue: %d", calls)
			}
		})
	}
}

func TestShipPushesAPullRequestStatusReadsAsNotQueuedWhenTrunkHasNoRecordOfIt(t *testing.T) {
	t.Setenv("TRUNK_MERGE_API_KEY", "trunk-fixture-key")
	for _, tt := range []struct {
		name, pulls string
		status      int
		refusal     string
	}{
		{"no record", `{"data":[],"hasMore":false,"nextCursor":null}`, http.StatusOK, ""},
		{"unreadable", `{"message":"forbidden"}`, http.StatusForbidden, "read the trunk queue state of #41009"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/repositories":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"repo-1"}],"hasMore":false,"nextCursor":null}`)
				case "/v2/pull-requests":
					w.WriteHeader(tt.status)
					_, _ = fmt.Fprint(w, tt.pulls)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			}))
			defer server.Close()
			client := gtapi.NewNativeWithToken(server.URL, "stub").WithQueueProvider("trunk")
			info := gtapi.PullRequestInfo{PRNumber: 41009, HeadRefName: "yasyf/iris-dom-extract", BaseRefName: "dev", State: gtapi.PROpen, Versions: []gtapi.PRVersion{{HeadSha: "published"}}}
			states, readErr := prstate.QueueStates(context.Background(), client, "Forge-AI", "monorepo", []gtapi.PullRequestInfo{info}, []int{41009}, io.Discard)
			plan := []gtSubmitBranch{{name: info.HeadRefName, head: "new-head", base: "dev"}}
			err := nativeRefuseQueuedChanges(context.Background(), client, "Forge-AI", "monorepo", "ship", plan, map[string]gtapi.PullRequestInfo{info.HeadRefName: info})
			if tt.refusal == "" {
				if readErr != nil || prQueueOf(info, prstate.PR{Mergeability: states[41009]}, "").Queue != prQueueNotQueued {
					t.Fatalf("status read states=%v err=%v, want #41009 not queued", states, readErr)
				}
				if err != nil {
					t.Fatalf("ship refused a pull request status reads as not queued: %v", err)
				}
				return
			}
			if readErr == nil || err == nil || !strings.Contains(err.Error(), tt.refusal) {
				t.Fatalf("status read err=%v, ship err=%v; want both to fail and ship to name %q", readErr, err, tt.refusal)
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
	if native, err := gtmeta.NativeMembership(t.Context(), common, "legacy-child"); err != nil || native {
		t.Fatalf("legacy child adopted as native: %v %v", native, err)
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "legacy-parent")
	nativeChild := filepath.Join(t.TempDir(), "native-child")
	if _, _, err := runStackCmd(t, f, "new", "native-child", "--parent", "native-current", "--full-history", "--path", nativeChild); err != nil {
		t.Fatal(err)
	}
	if native, err := gtmeta.NativeMembership(t.Context(), common, "native-child"); err != nil || !native {
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
	native, err := gtmeta.NativeMembership(t.Context(), filepath.Join(f.Dir, ".git"), "native-first")
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

func TestStackNewDeepensAShallowCheckoutToReachAParentOnlyOriginHolds(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	stubStackPRs(t, f, nil)
	fork := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	shipGTUntracked(t, f, "theirs")
	theirs := gitAt(t, f.Env(), f.Dir, "rev-parse", "theirs")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	for _, name := range []string{"one", "two"} {
		writeShipFile(t, f.Dir, name+".txt", name+"\n")
		mustRun(t, f.Env(), f.Dir, "git", "add", name+".txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "main", "theirs")
	shallow := filepath.Join(t.TempDir(), "shallow")
	mustRun(t, f.Env(), filepath.Dir(shallow), "git", "clone", "-q", "--no-local", "--depth", "1", "--branch", "main", f.RemoteDir, shallow)
	mustRun(t, f.Env(), shallow, "git", "config", "ccx.stack", "github")
	seedLaneRecords(f.ContextIn(shallow), t, shallow, laneSeed{})
	child := filepath.Join(t.TempDir(), "child")

	out, errOut, err := runStackCmdIn(t, f, shallow, "new", "child", "--full-history", "--parent", "theirs", "--path", child)
	if err != nil {
		t.Fatalf("stack new --parent theirs in a shallow clone: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, "tracked theirs onto main"+shipSep+"cut child onto theirs") {
		t.Errorf("stack new = %q, want theirs tracked onto main before child is cut onto it", out)
	}
	state, err := gtStateQuery(f.ContextIn(shallow), render.Dir(shallow), "test")
	if err != nil {
		t.Fatal(err)
	}
	for branch, want := range map[string]gtRef{"theirs": {Ref: "main", SHA: fork}, "child": {Ref: "theirs", SHA: theirs}} {
		if s := state[branch]; len(s.Parents) != 1 || s.Parents[0] != want {
			t.Errorf("%s = %+v, want it recorded on %s at %.12s", branch, s, want.Ref, want.SHA)
		}
	}
}
