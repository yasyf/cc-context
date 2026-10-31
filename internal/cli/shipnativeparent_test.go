package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

// shipNativeStaleParent is native stack #46, base then mid, whose local record
// still names main as mid's parent, with child cut off mid and left untracked:
// the record a pull request retargeted on GitHub leaves behind. trackBase also
// records base, which otherwise only GitHub knows as a stack member.
func shipNativeStaleParent(t *testing.T, trackBase bool) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	trunk := shipHead(t, f)
	for _, name := range []string{"base", "mid", "child"} {
		shipGTUntracked(t, f, name)
	}
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	mid := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if trackBase {
		if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, base); err != nil {
			t.Fatal(err)
		}
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "mid", "main", base, mid); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("base", "mid")
	api.bases["base"], api.bases["mid"] = "main", "base"
	api.stacks["base"], api.stacks["mid"] = 46, 46
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)
	return f
}

var shipNativeStaleCases = []struct {
	name      string
	trackBase bool
}{
	{name: "stale record", trackBase: true},
	{name: "base untracked here", trackBase: false},
}

// TestShipDryRunReadsTheNativeStackParent is a child of a branch whose pull
// request native stack #46 bases on base while this checkout records it on
// main. The dry run planned mid's restack onto main, "native stack base differs
// from its parent", and with it a rewrite of every file base changed.
func TestShipDryRunReadsTheNativeStackParent(t *testing.T) {
	for _, tt := range shipNativeStaleCases {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeStaleParent(t, tt.trackBase)
			refs := dryRunRefs(t, f)

			report := dryRunReport(t, f, "--no-commit")
			if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "mid"+shipSep) {
				t.Errorf("parent = %q, want child adopted onto mid", got)
			}
			if moves := dryRunValues(report, "restack"); len(moves) != 0 {
				t.Errorf("restack = %q, want nothing moved: mid already sits on base", moves)
			}
			if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
				t.Errorf("refuses = %q, want none", refusals)
			}
			want := "ship records mid on base, the base of #9001 in native stack #46, where this checkout recorded main"
			if notes := dryRunValues(report, "note"); !strings.Contains(strings.Join(notes, "\n"), want) {
				t.Errorf("note = %q, want %q", notes, want)
			}
			if after := dryRunRefs(t, f); after != refs {
				t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
			}
		})
	}
}

// TestShipRecordsTheNativeStackParentBeforeRestacking runs the ship the dry run
// above reports: mid is recorded on base and left where it is, never replayed
// onto main.
func TestShipRecordsTheNativeStackParentBeforeRestacking(t *testing.T) {
	for _, tt := range shipNativeStaleCases {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeStaleParent(t, tt.trackBase)
			mid := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid")
			shipGTReady(t, f)

			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
				t.Fatalf("ship: %v", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid"); got != mid {
				t.Errorf("mid moved from %s to %s, want it left on base", mid, got)
			}
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			for branch, parent := range map[string]string{"base": "main", "mid": "base", "child": "mid"} {
				if s := state[branch]; len(s.Parents) != 1 || s.Parents[0].Ref != parent || s.NeedsRestack {
					t.Errorf("%s = %+v, want it recorded on %s with nothing to restack", branch, s, parent)
				}
			}
		})
	}
}

// TestShipNewBranchStacksOnTheBranchItWasCutFrom cuts child with --new-branch
// off parent, a branch with an open pull request on main, tracked here or
// checked out from another lane's push. The native create recorded child on
// main at its merge base, so the restack and submit carried parent's commit
// into child's pull request against main.
func TestShipNewBranchStacksOnTheBranchItWasCutFrom(t *testing.T) {
	for _, tt := range []struct {
		name    string
		tracked bool
	}{
		{name: "parent tracked here", tracked: true},
		{name: "parent cut by another lane", tracked: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			trunk := shipHead(t, f)
			shipGTUntracked(t, f, "parent")
			parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "parent")
			common := filepath.Join(f.Dir, ".git")
			if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
				t.Fatal(err)
			}
			if tt.tracked {
				if err := nativemeta.AdoptRoot(f.Context(), common, "parent", "main", trunk, parent); err != nil {
					t.Fatal(err)
				}
			}
			api := stubGTAPI(t)
			api.openPRs("parent")
			api.bases["parent"], api.stacks["parent"] = "main", 47
			f.Decorate(api.ctx)
			stubStackPRs(t, f, nil)
			stubStackStanding(t, f, nil)
			shipGTReady(t, f)

			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--new-branch=child", "--no-push"); err != nil {
				t.Fatalf("ship: %v", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "parent"); got != parent {
				t.Errorf("parent moved from %s to %s", parent, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", "parent..child"); got != "1" {
				t.Errorf("child carries %s commits above parent, want only its own", got)
			}
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			for branch, want := range map[string]string{"parent": "main", "child": "parent"} {
				if s := state[branch]; len(s.Parents) != 1 || s.Parents[0].Ref != want || s.NeedsRestack {
					t.Errorf("%s = %+v, want it recorded on %s with nothing to restack", branch, s, want)
				}
			}
		})
	}
}

// TestShipTracksABranchCutFromAnotherLaneOnItsPullRequestBase ships mine, cut
// from theirs, another lane's pushed branch that this checkout never tracked,
// with mine's pull request open on theirs outside any native stack. No commit
// above trunk named a tracked branch, so ship recorded mine on main and the
// restack and submit replayed theirs into it and retargeted it onto main.
func TestShipTracksABranchCutFromAnotherLaneOnItsPullRequestBase(t *testing.T) {
	for _, tt := range []struct {
		name  string
		local bool
	}{
		{name: "theirs is a local branch", local: true},
		{name: "theirs is only on origin", local: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			shipGTUntracked(t, f, "theirs")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "theirs")
			shipGTUntracked(t, f, "mine")
			mine := gitAt(t, f.Env(), f.Dir, "rev-parse", "mine")
			if !tt.local {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "theirs")
			}
			if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
				t.Fatal(err)
			}
			api := stubGTAPI(t)
			api.openPRs("theirs", "mine")
			api.bases["theirs"], api.bases["mine"] = "main", "theirs"
			api.recorded["theirs"], api.recorded["mine"] = "main", "theirs"
			f.Decorate(api.ctx)
			stubStackPRs(t, f, nil)
			stubStackStanding(t, f, nil)
			shipGTReady(t, f)

			_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push")
			state, stateErr := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			if !tt.local {
				if err == nil || !strings.Contains(err.Error(), "theirs, the base of mine's open #") {
					t.Fatalf("ship error = %v, want a refusal naming theirs as mine's base to fetch", err)
				}
				if s, tracked := state["mine"]; tracked {
					t.Errorf("mine = %+v, want it left untracked rather than recorded on main", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("ship: %v", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "mine~1"); got != mine {
				t.Errorf("mine was replayed: its commit before the new one is %s, want %s", got, mine)
			}
			for branch, parent := range map[string]string{"theirs": "main", "mine": "theirs"} {
				if s := state[branch]; len(s.Parents) != 1 || s.Parents[0].Ref != parent || s.NeedsRestack {
					t.Errorf("%s = %+v, want it recorded on %s with nothing to restack", branch, s, parent)
				}
			}
		})
	}
}
