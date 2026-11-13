package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackPlanReplaysAChildPastAParentRewrittenBeforeItLanded(t *testing.T) {
	tests := []struct {
		name     string
		declared bool
	}{
		{name: "GitHub retargeted it off the landed parent"},
		{name: "the parent is declared landed", declared: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			common := filepath.Join(f.Dir, ".git")
			trunk := shipHead(t, f)
			shipGTUntracked(t, f, "a")
			shipGTUntracked(t, f, "b")
			cut := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
			b := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "a", "b")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
			writeShipFile(t, f.Dir, "a.txt", "a, rebased\n")
			mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
			rewritten := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "a")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
			restackSquashRemote(t, f, "main", "a (#41)", "a")
			mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
			if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
				t.Fatal(err)
			}
			links := []struct{ branch, parent, fork, head string }{{"b", "main", trunk, b}}
			prs := map[string]*stackPR{"b": {Number: 42, Title: "b", State: "OPEN", Base: "main", Head: b, NativeStack: 46}}
			opts := stackRebaseOpts{tip: "b", submit: true, dryRun: true}
			if tt.declared {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "-f", "a", cut)
				links = append(links, struct{ branch, parent, fork, head string }{"a", "main", trunk, cut})
				prs["a"] = &stackPR{Number: 41, Title: "a", State: "MERGED", Landed: true, Head: rewritten}
				opts.landed = []string{"a"}
			} else {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "-D", "a")
				mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "--delete", "a")
				prs["b"].LandedBase, prs["b"].LandedHeads = "a", []string{cut, rewritten}
			}
			for _, link := range links {
				if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, link.fork, link.head); err != nil {
					t.Fatal(err)
				}
			}
			api := newGTAPIStub(t)
			api.openPRs("b")
			api.bases["b"], api.stacks["b"] = "main", 46
			f.Decorate(api.ctx)
			stubOpenPRs(t, f, prs)
			stubStackStanding(t, f, nil)
			l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
			if err != nil {
				t.Fatal(err)
			}

			run, err := stackPlan(f.Context(), l, common, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := run.branch("b").OldBase; got != cut {
				t.Errorf("b replays from %s, want a's head before its rewrite %s, past a's commits", shortOID(got), shortOID(cut))
			}
		})
	}
}

func TestStackRefuseDroppedCommitsCountsACommitTheLocalHeadHoldsAsCarried(t *testing.T) {
	rewrite := func(t *testing.T, f *vcstest.Fixture) {
		mustRun(t, f.Env(), f.Dir, "git", "rm", "-q", "c.txt", "d.txt")
		mustRun(t, f.Env(), f.Dir, "git", "commit", "-qm", "later work takes c and d out")
	}
	tests := []struct {
		name    string
		hold    func(t *testing.T, f *vcstest.Fixture, below, c, d string)
		dropped []string
	}{
		{
			name: "by sha, with a reworded copy of the next",
			hold: func(t *testing.T, f *vcstest.Fixture, _, c, d string) {
				mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", c)
				mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", d)
				mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "--amend", "-m", "d, reworded")
				rewrite(t, f)
			},
		},
		{
			name: "as rebased copies",
			hold: func(t *testing.T, f *vcstest.Fixture, below, c, d string) {
				mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", below)
				pushCommit(t, f, "x.txt", "x\n", "x")
				mustRun(t, f.Env(), f.Dir, "git", "cherry-pick", c, d)
				rewrite(t, f)
			},
		},
		{
			name: "by neither, after a reset",
			hold: func(t *testing.T, f *vcstest.Fixture, below, _, _ string) {
				mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", below)
				pushCommit(t, f, "x.txt", "x\n", "x")
			},
			dropped: []string{`"d"`, `"c"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := stackRebaseRepo(t, "feature")
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "feature")
			below := shipHead(t, f)
			c := pushCommit(t, f, "c.txt", "c\n", "c")
			remote := pushCommit(t, f, "d.txt", "d\n", "d")
			tt.hold(t, f, below, c, remote)
			dir := render.Dir(f.Dir)
			tr, err := gtTrunkRefOffline(f.Context(), dir, stackRebasePrefix, "main")
			if err != nil {
				t.Fatal(err)
			}
			local := shipHead(t, f)

			err = stackRefuseDroppedCommits(f.Context(), dir, tr, "feature", local, remote, "main", "")
			if len(tt.dropped) == 0 {
				if err != nil {
					t.Fatalf("stackRefuseDroppedCommits = %v, want the commits the local head holds counted as carried", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "drops 2 commit(s)") || !strings.Contains(err.Error(), strings.Join(tt.dropped, ", ")) {
				t.Fatalf("stackRefuseDroppedCommits = %v, want a refusal naming %v", err, tt.dropped)
			}
		})
	}
}

func TestStackGraphQLReadTakesTheLandedBaseFromThePullRequestsTimeline(t *testing.T) {
	payload := `{"number": 52772, "baseRefName": "dev", "headRefOid": "17454031", "state": "OPEN", "retargets": {"nodes": [
		{"__typename": "BaseRefChangedEvent"},
		{"__typename": "BaseRefForcePushedEvent", "beforeCommit": {"oid": "49797e37"}, "afterCommit": {"oid": "ada43255"}},
		{"__typename": "AutomaticBaseChangeSucceededEvent", "oldBase": "sandsql-joint-commit", "newBase": "dev"}]}}`
	var n stackPRNode
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		t.Fatal(err)
	}
	pr := stackGraphQLRead("sharding-table-map", n).pr
	if pr.LandedBase != "sandsql-joint-commit" || !slices.Equal(pr.LandedHeads, []string{"49797e37", "ada43255"}) {
		t.Errorf("landed base = %q %v, want sandsql-joint-commit [49797e37 ada43255]", pr.LandedBase, pr.LandedHeads)
	}
}

func TestStackLandedBaseKeepsOnlyTheBaseGitHubRetargetedOntoTheCurrentOne(t *testing.T) {
	push := func(before, after string) stackRetarget {
		return stackRetarget{Typename: "BaseRefForcePushedEvent", BeforeCommit: &ghCommitOID{before}, AfterCommit: &ghCommitOID{after}}
	}
	landed := func(old, base string) stackRetarget {
		return stackRetarget{Typename: "AutomaticBaseChangeSucceededEvent", OldBase: old, NewBase: base}
	}
	changed := stackRetarget{Typename: "BaseRefChangedEvent"}
	tests := []struct {
		name      string
		events    []stackRetarget
		base      string
		heads     []string
		baseHeads []string
	}{
		{name: "a base rewritten, then landed", events: []stackRetarget{push("old", "new"), landed("a", "main")}, base: "a", heads: []string{"old", "new"}},
		{name: "a push onto an earlier base", events: []stackRetarget{push("x1", "x2"), changed, push("old", "new"), landed("a", "main")}, base: "a", heads: []string{"old", "new"}},
		{name: "a base landed as it was", events: []stackRetarget{landed("a", "main")}, base: "a"},
		{name: "a push GitHub kept one side of", events: []stackRetarget{{Typename: "BaseRefForcePushedEvent", AfterCommit: &ghCommitOID{"new"}}, landed("a", "main")}, base: "a", heads: []string{"new"}},
		{name: "retargeted by hand since", events: []stackRetarget{push("old", "new"), landed("a", "main"), changed}},
		{name: "landed onto a base it has left", events: []stackRetarget{landed("a", "b")}},
		{name: "never retargeted"},
		{name: "a base rewritten in place", events: []stackRetarget{push("old", "new"), push("new", "newer")}, baseHeads: []string{"old", "new", "new", "newer"}},
		{name: "a base rewritten after a retarget", events: []stackRetarget{push("x1", "x2"), changed, push("old", "new")}, baseHeads: []string{"old", "new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, heads, baseHeads := stackLandedBase("main", tt.events)
			if base != tt.base || !slices.Equal(heads, tt.heads) || !slices.Equal(baseHeads, tt.baseHeads) {
				t.Errorf("stackLandedBase = %q %v %v, want %q %v %v", base, heads, baseHeads, tt.base, tt.heads, tt.baseHeads)
			}
		})
	}
}

func TestStackPlanReplaysAChildFromTheParentHeadGitHubRecordedBeforeItsRewrite(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	common := filepath.Join(f.Dir, ".git")
	trunk := shipHead(t, f)
	shipGTUntracked(t, f, "x")
	x := gitAt(t, f.Env(), f.Dir, "rev-parse", "x")
	shipGTUntracked(t, f, "a")
	cut := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	shipGTUntracked(t, f, "b")
	b := gitAt(t, f.Env(), f.Dir, "rev-parse", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "a")
	writeShipFile(t, f.Dir, "a.txt", "a, rebased\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-a", "--amend", "--no-edit")
	rewritten := gitAt(t, f.Env(), f.Dir, "rev-parse", "a")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "x", "a", "b")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "b")
	mustRun(t, f.Env(), f.Dir, "git", "reflog", "expire", "--expire=now", "--all")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ branch, parent, fork, head string }{{"x", "main", trunk, x}, {"a", "x", x, rewritten}, {"b", "a", x, b}} {
		if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, link.fork, link.head); err != nil {
			t.Fatal(err)
		}
	}
	api := newGTAPIStub(t)
	api.openPRs("x", "a", "b")
	api.bases["x"], api.bases["a"], api.bases["b"] = "main", "x", "a"
	api.stacks["x"], api.stacks["a"], api.stacks["b"] = 46, 46, 46
	f.Decorate(api.ctx)
	stubOpenPRs(t, f, map[string]*stackPR{
		"x": {Number: 40, Title: "x", State: "OPEN", Base: "main", Head: x, NativeStack: 46},
		"a": {Number: 41, Title: "a", State: "OPEN", Base: "x", Head: rewritten, NativeStack: 46},
		"b": {Number: 42, Title: "b", State: "OPEN", Base: "a", Head: b, NativeStack: 46, BaseHeads: []string{cut, rewritten}},
	})
	stubStackStanding(t, f, nil)
	l, err := resolveLane(f.Context(), "stack rebase", f.Dir, false)
	if err != nil {
		t.Fatal(err)
	}

	run, err := stackPlan(f.Context(), l, common, stackRebaseOpts{tip: "b", submit: true, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.branch("b").OldBase; got != cut {
		t.Errorf("b replays from %s, want a's head before its rewrite %s, which GitHub recorded on b's pull request", shortOID(got), shortOID(cut))
	}
}
