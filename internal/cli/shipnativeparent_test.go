package cli

import (
	"context"
	"fmt"
	"os"
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
func shipNativeStaleParent(t *testing.T, trackBase, remoteBase bool) *vcstest.Fixture {
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
	if remoteBase {
		mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "base")
	}
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
	name       string
	trackBase  bool
	remoteBase bool
}{
	{name: "stale record", trackBase: true},
	{name: "base untracked here", trackBase: false},
	{name: "base only on origin", remoteBase: true},
}

// TestShipDryRunReadsTheNativeStackParent is a child of a branch whose pull
// request native stack #46 bases on base while this checkout records it on
// main. The dry run planned mid's restack onto main, "native stack base differs
// from its parent", and with it a rewrite of every file base changed.
func TestShipDryRunReadsTheNativeStackParent(t *testing.T) {
	for _, tt := range shipNativeStaleCases {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeStaleParent(t, tt.trackBase, tt.remoteBase)
			refs := dryRunRefs(t, f)

			report := dryRunReport(t, f, "--no-commit")
			if after := dryRunRefs(t, f); after != refs {
				t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
			}
			if tt.remoteBase {
				want := "ship: base, the base of mid, is no local branch, and a dry run fetches nothing — ship itself cuts it at origin's head; git fetch origin base:base previews past it"
				if refusals := dryRunValues(report, "refuses"); len(refusals) != 1 || refusals[0] != want {
					t.Errorf("refuses = %q, want %q", refusals, want)
				}
				return
			}
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
		})
	}
}

// TestShipRecordsTheNativeStackParentBeforeRestacking runs the ship the dry run
// above reports: mid is recorded on base and left where it is, never replayed
// onto main.
func TestShipRecordsTheNativeStackParentBeforeRestacking(t *testing.T) {
	for _, tt := range shipNativeStaleCases {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeStaleParent(t, tt.trackBase, tt.remoteBase)
			base := gitAt(t, f.Env(), f.Dir, "rev-parse", "origin/base")
			mid := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid")
			shipGTReady(t, f)

			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
				t.Fatalf("ship: %v", err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "mid"); got != mid {
				t.Errorf("mid moved from %s to %s, want it left on base", mid, got)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "base"); got != base {
				t.Errorf("base = %s, want it at origin's head %s", got, base)
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
			theirs := gitAt(t, f.Env(), f.Dir, "rev-parse", "theirs")
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
			if !tt.local {
				refs := dryRunRefs(t, f)
				report := dryRunReport(t, f, "-m", "fix: frobnicate", "--no-push")
				want := "ship: theirs, the base of mine, is no local branch, and a dry run fetches nothing — ship itself cuts it at origin's head; git fetch origin theirs:theirs previews past it"
				if refusals := dryRunValues(report, "refuses"); len(refusals) != 1 || refusals[0] != want {
					t.Errorf("dry run refuses = %q, want %q", refusals, want)
				}
				if after := dryRunRefs(t, f); after != refs {
					t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
				}
			}

			if _, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--no-push"); err != nil {
				t.Fatalf("ship: %v", err)
			}
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "theirs"); got != theirs {
				t.Errorf("theirs = %s, want it at origin's head %s", got, theirs)
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

// TestShipTracksABranchCutFromAnotherLanesPublishedHead ships mine, a new
// branch with no pull request cut from the published head of theirs, another
// lane's open pull request this checkout never tracked. Nothing named theirs,
// so ship recorded mine on main at the old merge base and every --tip-only ship
// planned a whole-stack rebase onto main.
func TestShipTracksABranchCutFromAnotherLanesPublishedHead(t *testing.T) {
	for _, tt := range []struct {
		name  string
		local bool
		flags []string
	}{
		{name: "ship", local: true},
		{name: "tip only", local: true, flags: []string{"--tip-only"}},
		{name: "theirs is only on origin", local: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			shipGTUntracked(t, f, "theirs")
			theirs := gitAt(t, f.Env(), f.Dir, "rev-parse", "theirs")
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
			api.openPRs("theirs")
			api.bases["theirs"], api.recorded["theirs"] = "main", "main"
			f.Decorate(api.ctx)
			stubStackPRs(t, f, nil)
			stubStackStanding(t, f, nil)
			shipGTReady(t, f)
			refs := dryRunRefs(t, f)
			for _, parent := range [][]string{nil, {"--parent", "theirs"}} {
				report := dryRunReport(t, f, append(append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.flags...), parent...)...)
				refusals := dryRunValues(report, "refuses")
				if !tt.local {
					if want := dryRunUnfetchedBase("theirs", "mine").Error(); len(refusals) != 1 || refusals[0] != want {
						t.Errorf("dry run %q refuses = %q, want %q", parent, refusals, want)
					}
					continue
				}
				if len(refusals) != 0 {
					t.Errorf("dry run %q refuses = %q, want none", parent, refusals)
				}
				if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "theirs"+shipSep) || !strings.HasSuffix(got[0], ", after ship tracks theirs onto main, the base of its open #9000") {
					t.Errorf("dry run %q parent = %q, want mine on theirs, after ship tracks theirs onto main", parent, got)
				}
			}
			if after := dryRunRefs(t, f); after != refs {
				t.Errorf("the dry run moved refs:\n%s\nwant\n%s", after, refs)
			}

			if _, err := runShipCmd(f.Context(), t, append([]string{"-m", "fix: frobnicate", "--no-push"}, tt.flags...)...); err != nil {
				t.Fatalf("ship: %v", err)
			}
			state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
			if err != nil {
				t.Fatal(err)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "theirs"); got != theirs {
				t.Errorf("theirs = %s, want it at origin's head %s", got, theirs)
			}
			if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "mine~1"); got != mine {
				t.Errorf("mine was replayed: its commit before the new one is %s, want %s", got, mine)
			}
			if s := state["mine"]; len(s.Parents) != 1 || s.Parents[0].Ref != "theirs" || s.NeedsRestack {
				t.Errorf("mine = %+v, want it recorded on theirs with nothing to restack", s)
			}
		})
	}
}

func TestShipTipOnlyParentReportsTheNativeStackRefusal(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	trunk := shipHead(t, f)
	mustRun(t, f.Env(), f.Dir, "git", "branch", "release")
	shipGTUntracked(t, f, "theirs")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "theirs", "release")
	shipGTUntracked(t, f, "mine")
	catalog := fmt.Sprintf(`{"schemaVersion":1,"stacks":[{"trunk":{"branch":"main"},"branches":[{"branch":"theirs","base":%q}]},{"trunk":{"branch":"release"},"branches":[]}]}`, trunk)
	if err := os.WriteFile(filepath.Join(f.Dir, ".git", "gh-stack"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("theirs")
	api.bases["theirs"], api.stacks["theirs"] = "main", 48
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)
	shipGTReady(t, f)

	_, err := runShipCmd(f.Context(), t, "-m", "fix: frobnicate", "--tip-only", "--parent", "theirs", "--no-push")
	if err == nil {
		t.Fatal("ship succeeded, want the native catalog's refusal to record mine")
	}
	if !strings.Contains(err.Error(), "ship: record mine in the native stack: ") || !strings.Contains(err.Error(), "decide whether native stack root release is a trunk") {
		t.Errorf("ship error = %q, want the native catalog's own reason", err)
	}
	if strings.Contains(err.Error(), "--no-gt") || strings.Contains(err.Error(), "gt track") {
		t.Errorf("ship error = %q, want no Graphite advice on the native lane", err)
	}
}

// TestShipParentOnANativeBranchShipsOnTheNativeLane moves child, tracked by
// Graphite on main, onto base, a native stack member, in a Graphite
// repository. The Graphite lane hides every native branch, so it refused
// --parent base as a branch graphite does not track while gt still listed it.
func TestShipParentOnANativeBranchShipsOnTheNativeLane(t *testing.T) {
	f := shipRepo(t, vcstest.GT(), vcstest.Remote(), vcstest.GTStack("child"))
	trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
	shipGTUntracked(t, f, "base")
	base := gitAt(t, f.Env(), f.Dir, "rev-parse", "base")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "child")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "base", "main", trunk, base); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("base")
	api.bases["base"] = "main"
	api.stacks["base"] = 46
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)

	report := dryRunReport(t, f, "--no-commit", "--parent", "base")

	if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
		t.Errorf("refuses = %q, want none", refusals)
	}
	if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "base") {
		t.Errorf("parent = %q, want child shipped onto base", got)
	}
}

// TestShipOnTrunkPastALoneNativeStackOnAPullRequestBranch is cci-who-fast: the
// one native stack in cc-inbox sat on store-bounded-open, a plain pull request
// branch, so ccx read it as the trunk, and a ship of a branch cut from main
// refused "gt state has no parent for main".
func TestShipOnTrunkPastALoneNativeStackOnAPullRequestBranch(t *testing.T) {
	for _, args := range [][]string{{"--parent", "main"}, nil} {
		t.Run(strings.Join(append([]string{"ship"}, args...), " "), func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			shipGTUntracked(t, f, "open")
			shipGTUntracked(t, f, "post")
			open := gitAt(t, f.Env(), f.Dir, "rev-parse", "open")
			post := gitAt(t, f.Env(), f.Dir, "rev-parse", "post")
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "open", "post")
			catalog := fmt.Sprintf(`{"schemaVersion":1,"stacks":[{"trunk":{"branch":"open","head":%q},"branches":[{"branch":"post","base":%q,"head":%q}]}]}`, open, open, post)
			if err := os.WriteFile(filepath.Join(f.Dir, ".git", "gh-stack"), []byte(catalog), 0o600); err != nil {
				t.Fatal(err)
			}
			mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "main")
			shipGTUntracked(t, f, "mine")
			api := stubGTAPI(t)
			f.Decorate(api.ctx)
			stubStackPRs(t, f, nil)
			stubStackStanding(t, f, nil)

			report := dryRunReport(t, f, append([]string{"--no-commit"}, args...)...)

			if refusals := dryRunValues(report, "refuses"); len(refusals) != 0 {
				t.Errorf("refuses = %q, want none", refusals)
			}
			if got := dryRunValues(report, "trunk"); len(got) != 1 || got[0] != "main" {
				t.Errorf("trunk = %q, want main, the default branch, not the base of the one native stack\n%s", got, report)
			}
			if got := dryRunValues(report, "parent"); len(got) != 1 || !strings.HasPrefix(got[0], "main"+shipSep) {
				t.Errorf("parent = %q, want mine shipped onto main\n%s", got, report)
			}
		})
	}
}

func TestShipParentRetargetsANativeBranchItsLaneRebuiltOffItsStackBase(t *testing.T) {
	f := shipNativeStaleParent(t, false, false)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "mid")
	mustRun(t, f.Env(), f.Dir, "git", "reset", "-q", "--hard", "main")
	writeShipFile(t, f.Dir, "rebuilt.txt", "rebuilt from main\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "rebuilt.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", "rebuilt")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "-f", "origin", "mid")
	shipGTReady(t, f)
	writeShipFile(t, f.Dir, "follow.txt", "follow-up\n")

	if out, err := runShipCmd(f.Context(), t, "-m", "fix: follow up", "--parent", "main", "--no-push"); err != nil {
		t.Fatalf("ship --parent main: %v\n%s", err, out)
	}
}

func shipNativeRestackedElsewhere(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	trunk := shipHead(t, f)
	shipGTUntracked(t, f, "images")
	shipGTUntracked(t, f, "plumbing")
	recorded := shipHead(t, f)
	writeShipFile(t, f.Dir, "plumbing.txt", "plumbing, rewritten elsewhere\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-a", "--amend", "-m", "plumbing", "-m", "why plumbing")
	shipGTUntracked(t, f, "sand")
	shipGTUntracked(t, f, "anti-bot")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "images", "plumbing", "sand", "anti-bot")
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("images", "plumbing", "sand", "anti-bot")
	prs := map[string]*stackPR{}
	for _, link := range []struct{ branch, parent string }{{"images", "main"}, {"plumbing", "images"}, {"sand", "plumbing"}, {"anti-bot", "sand"}} {
		head := gitAt(t, f.Env(), f.Dir, "rev-parse", link.branch)
		base := gitAt(t, f.Env(), f.Dir, "rev-parse", link.parent)
		switch link.branch {
		case "images":
			base = trunk
		case "sand":
			base = recorded
		}
		if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, base, head); err != nil {
			t.Fatal(err)
		}
		api.bases[link.branch], api.stacks[link.branch] = link.parent, 53276
		prs[link.branch] = &stackPR{Number: api.prs[link.branch], Title: link.branch, State: "OPEN", Base: link.parent, Head: head, Mergeable: "MERGEABLE"}
	}
	f.Decorate(api.ctx)
	stubStackPRs(t, f, prs)
	stubStackStanding(t, f, nil)
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "sand")
	mustRun(t, f.Env(), f.Dir, "git", "worktree", "add", "-q", f.WorktreePath("anti-bot"), "anti-bot")
	return f
}

func TestShipPlansOnlyTheMidStackBranchAnotherCheckoutRestacked(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags []string
		amend bool
	}{
		{name: "ship"},
		{name: "tip only", flags: []string{"--tip-only"}},
		{name: "amend", flags: []string{"--amend"}, amend: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeRestackedElsewhere(t)
			published := map[string]string{}
			for _, branch := range []string{"images", "plumbing", "sand", "anti-bot"} {
				published[branch] = gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch)
			}
			shipGTReady(t, f)
			args := append([]string{"-m", "fix: frobnicate", "--no-watch"}, tt.flags...)

			report := dryRunReport(t, f, args...)
			for _, label := range []string{"restack", "rewrites", "refuses"} {
				if got := dryRunValues(report, label); len(got) != 0 {
					t.Errorf("%s = %q, want none: sand sits on plumbing's head and images stays where its pull request merges\n%s", label, got, report)
				}
			}

			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship = %v (stderr=%q)", err, errStr)
			}
			for _, branch := range []string{"images", "plumbing", "anti-bot"} {
				if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", branch); got != published[branch] {
					t.Errorf("origin %s moved from %s to %s, want sand pushed alone", branch, published[branch], got)
				}
			}
			below := published["sand"]
			if tt.amend {
				below = published["plumbing"]
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "sand^"); got != below {
				t.Errorf("origin sand^ = %s, want %s: sand's own commit kept where it was, not replayed", got, below)
			}
		})
	}
}

func shipNativeRewrittenBelow(t *testing.T) *vcstest.Fixture {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	recorded := map[string]string{"main": shipHead(t, f)}
	stack := []struct{ branch, parent string }{{"bottom", "main"}, {"middle", "bottom"}, {"top", "middle"}}
	for _, link := range stack {
		shipGTUntracked(t, f, link.branch)
		recorded[link.branch] = shipHead(t, f)
	}
	common := filepath.Join(f.Dir, ".git")
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("bottom", "middle", "top")
	for _, link := range stack {
		if err := nativemeta.AdoptRoot(f.Context(), common, link.branch, link.parent, recorded[link.parent], recorded[link.branch]); err != nil {
			t.Fatal(err)
		}
		api.bases[link.branch], api.stacks[link.branch] = link.parent, 49
	}
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "bottom")
	writeShipFile(t, f.Dir, "bottom.txt", "bottom, rewritten outside ccx\n")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-a", "--amend", "-m", "bottom", "-m", "why bottom")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "bottom", recorded["bottom"], "middle")
	mustRun(t, f.Env(), f.Dir, "git", "rebase", "-q", "--onto", "middle", recorded["middle"], "top")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "bottom", "middle", "top")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "bottom")
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)
	return f
}

func TestShipReplaysOnlyTheOwnCommitsOfABranchRebasedOutsideCcx(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags []string
	}{
		{name: "append", flags: []string{"-m", "fix: follow up"}},
		{name: "amend", flags: []string{"--amend"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipNativeRewrittenBelow(t)
			bottom := gitAt(t, f.Env(), f.Dir, "rev-parse", "bottom")
			middle := gitAt(t, f.Env(), f.Dir, "rev-parse", "middle")
			shipGTReady(t, f)
			writeShipFile(t, f.Dir, "bottom.txt", "bottom, edited by ship\n")
			args := append([]string{"--no-push", "--no-watch"}, tt.flags...)

			report := dryRunReport(t, f, args...)
			want := "middle" + shipSep + shortSHA(middle) + " onto bottom" + shipSep + "replays " + shortSHA(bottom) + ".." + shortSHA(middle)
			if moves := dryRunValues(report, "restack"); len(moves) != 2 || !strings.HasPrefix(moves[0], want) {
				t.Errorf("restack = %q, want middle then top, middle replayed from bottom's head: %q\n%s", moves, want, report)
			}
			for _, label := range []string{"rewrites", "refuses"} {
				if got := dryRunValues(report, label); len(got) != 0 {
					t.Errorf("%s = %q, want none: middle changes no file bottom changed since it\n%s", label, got, report)
				}
			}

			if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err != nil {
				t.Fatalf("ship = %v (stderr=%q)", err, errStr)
			}
			for _, link := range [][2]string{{"bottom", "middle"}, {"middle", "top"}} {
				if got := gitAt(t, f.Env(), f.Dir, "rev-list", "--count", link[0]+".."+link[1]); got != "1" {
					t.Errorf("%s carries %s commits above %s, want only its own", link[1], got, link[0])
				}
			}
		})
	}
}

func TestShipRefusesABranchThatHoldsNoHeadOfItsParent(t *testing.T) {
	f := shipNativeRewrittenBelow(t)
	middle := gitAt(t, f.Env(), f.Dir, "rev-parse", "middle")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "--detach")
	mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "bottom")
	mustRun(t, f.Env(), f.Dir, "git", "update-ref", "-d", "refs/remotes/origin/bottom")
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-qc", "bottom", "main")
	writeShipFile(t, f.Dir, "bottom.txt", "bottom, cut again\n")
	mustRun(t, f.Env(), f.Dir, "git", "add", "bottom.txt")
	mustRun(t, f.Env(), f.Dir, "git", "commit", "-q", "-m", "bottom")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-qf", "origin", "bottom")
	shipGTReady(t, f)
	args := []string{"-m", "fix: follow up", "--no-push", "--no-watch"}
	want := "ship: middle holds neither "

	report := dryRunReport(t, f, args...)
	if refusals := dryRunValues(report, "refuses"); len(refusals) != 1 || !strings.HasPrefix(refusals[0], want) {
		t.Errorf("refuses = %q, want %q\n%s", refusals, want, report)
	}
	if _, errStr, err := runShipCmdFull(f.Context(), t, args...); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("ship = %v (stderr=%q), want the refusal %q", err, errStr, want)
	}
	if got := gitAt(t, f.Env(), f.Dir, "rev-parse", "middle"); got != middle {
		t.Errorf("middle moved from %s to %s, want it left where it is", middle, got)
	}
}

func TestShipNoPushReadsABranchHoldingItsParentHeadAsOnItsParent(t *testing.T) {
	f := shipNativeRewrittenBelow(t)
	mustRun(t, f.Env(), f.Dir, "git", "switch", "-q", "middle")
	bottom := gitAt(t, f.Env(), f.Dir, "rev-parse", "bottom")
	middle := gitAt(t, f.Env(), f.Dir, "rev-parse", "middle")
	shipGTReady(t, f)
	writeShipFile(t, f.Dir, "middle.txt", "middle, edited by ship\n")
	args := []string{"-m", "fix: follow up", "--no-push", "--no-watch"}

	report := dryRunReport(t, f, args...)
	for _, note := range dryRunValues(report, "note") {
		if strings.HasPrefix(note, "middle ") {
			t.Errorf("note = %q, want none on middle: it holds bottom's head though its stack records bottom's first one\n%s", note, report)
		}
	}

	got, errStr, err := runShipCmdFull(f.Context(), t, args...)
	if err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if strings.Contains(got, "kept middle") {
		t.Errorf("summary = %q, want no rebase named for middle: it already holds bottom's head", got)
	}
	if parent := gitAt(t, f.Env(), f.Dir, "rev-parse", "middle^"); parent != middle {
		t.Errorf("middle^ = %s, want middle's head %s: the commit lands on it", parent, middle)
	}
	if after := gitAt(t, f.Env(), f.Dir, "rev-parse", "bottom"); after != bottom {
		t.Errorf("bottom moved from %s to %s, want it left where it is", bottom, after)
	}
}
