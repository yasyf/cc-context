package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

// shipGTLandedBottom is join-scope's #27533 and the landed-parent tip ship: a, b, c
// published, a landed on trunk, and a ship from c that would replay b and c
// onto trunk for no change of their own.
func shipGTLandedBottom(t *testing.T) (*vcstest.Fixture, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b", "c")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["a"], api.prs["b"], api.prs["c"] = 100, 101, 102
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 100, Title: "a", State: "CLOSED", Base: "main", Landed: true},
		"b": {Number: 101, Title: "b", State: "OPEN", Base: "a", Mergeable: "MERGEABLE"},
		"c": {Number: 102, Title: "c", State: "OPEN", Base: "b", Mergeable: "MERGEABLE"},
	})
	restackSquashRemote(t, f, "main", "a (#100)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	shipGTReady(t, f)
	return f, published
}

func TestShipRefusesToReplayPublishedPullRequestsNoFlagAskedToRewrite(t *testing.T) {
	f, published := shipGTLandedBottom(t)
	tip := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	var refusal *shipRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("ship = %v, want a refusal", err)
	}
	for _, want := range []string{
		"shipping c would force-push b (#101) from its published head " + published[:12] + " to its 1 commit replayed onto main@",
		"c (#102) from its published head " + tip[:12] + " to its 2 commits replayed onto b's new head",
		"rerun with --restack to replay them, or with --tip-only to ship c alone",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}
	if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
		t.Errorf("pushed %v, want nothing", refs)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got != published {
		t.Errorf("origin b moved to %s, want its published %s", got, published)
	}
}

func TestShipAfterARewriteRefusal(t *testing.T) {
	for _, tc := range []struct {
		flag  string
		moved bool
		refs  []string
	}{
		{"--tip-only", false, []string{"c"}},
		{"--restack", true, []string{"b", "c"}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			f, published := shipGTLandedBottom(t)
			if _, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err == nil {
				t.Fatal("ship: want the replay of b refused")
			}
			shipResetLog(t, f)

			if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", tc.flag); err != nil {
				t.Fatalf("ship %s = %v (stderr=%q)", tc.flag, err, errStr)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); !slices.Equal(refs, tc.refs) {
				t.Errorf("pushed %v, want %v", refs, tc.refs)
			}
			b := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
			if moved := b != published; moved != tc.moved {
				t.Errorf("origin b moved = %t (%s from %s), want %t", moved, b, published, tc.moved)
			}
			if !stackOnto(t, f, b, gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "c")) {
				t.Error("origin c does not sit on origin b")
			}
		})
	}
}

// TestShipTipOnlyRefusesToReplayItsTipOntoTrunkPastALandedParent is the
// landed-parent tip ship: --tip-only shipped only the newest commit of a
// branch whose parent had landed, and the run replayed the branch's published
// commits onto trunk and force-pushed them. --tip-only leaves no ancestor to
// keep, so only --restack moves it.
func TestShipTipOnlyRefusesToReplayItsTipOntoTrunkPastALandedParent(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "a", "b")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["a"], api.prs["b"] = 100, 101
	stubStackPRs(t, f, map[string]*stackPR{
		"a": {Number: 100, Title: "a", State: "MERGED", Base: "main", Landed: true},
		"b": {Number: 101, Title: "b", State: "OPEN", Base: "main", Mergeable: "MERGEABLE"},
	})
	restackSquashRemote(t, f, "main", "a (#100)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	shipGTReady(t, f)

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "tip: doc 3", "--no-watch", "--tip-only")
	var refusal *shipRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("ship --tip-only = %v, want a refusal", err)
	}
	want := "shipping b would force-push b (#101) from its published head " + published[:12] + " to its 2 commits replayed onto main@"
	if !strings.Contains(err.Error(), want) || !strings.HasSuffix(err.Error(), "rerun with --restack to replay them") {
		t.Errorf("refusal = %q, want %q and only --restack to rerun with", err, want)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got != published {
		t.Errorf("origin b moved to %s, want its published %s", got, published)
	}
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "tip: doc 3", "--no-watch", "--tip-only", "--restack"); err != nil {
		t.Fatalf("ship --tip-only --restack = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got == published || !stackOnto(t, f, "origin/main", got) {
		t.Errorf("origin b = %s, want it replayed off %s onto trunk", got, published)
	}
}

func TestShipMovesAConflictingAncestorOnlyWhenAsked(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")

	_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
	if want := "would force-push base (#100) from its published head " + base[:12]; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ship = %v, want the refusal %q", err, want)
	}
	shipResetLog(t, f)

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch", "--restack"); err != nil {
		t.Fatalf("ship --restack = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got == base || !stackOnto(t, f, "origin/main", got) {
		t.Errorf("origin base = %s, want the conflicting base moved off %s onto trunk", got, base)
	}
}

func TestStackSubmitWithPRMetaStillRestacksAPublishedPullRequest(t *testing.T) {
	f, published := shipGTLandedBottom(t)
	mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", "--", "f.txt")
	writeShipGH(t, f)
	seedPRViews(t, map[string]string{"c": `{"number":102,"url":"https://github.com/x/pull/102","body":""}`})

	if _, errStr, err := runStackCmd(t, f, "submit", "--replace-pr-text", "--pr-title", "c=c, retitled"); err != nil {
		t.Fatalf("stack submit --pr-title = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got == published {
		t.Error("origin b stayed on the landed a, want stack submit to restack it onto trunk")
	}
}

func TestStackRewrites(t *testing.T) {
	open := &stackPR{Number: 1, State: "OPEN", Mergeable: "MERGEABLE"}
	published := func(name, parent, oldBase string) stackRebaseBranch {
		return stackRebaseBranch{Name: name, Parent: parent, WasParent: parent, OldBase: oldBase, Head: name + "-head", Remote: name + "-head", PR: open}
	}
	with := func(b stackRebaseBranch, edit func(*stackRebaseBranch)) stackRebaseBranch {
		edit(&b)
		return b
	}
	for _, tc := range []struct {
		name     string
		branches []stackRebaseBranch
		asked    []string
		want     []string
	}{
		{
			name:     "trunk moved under the bottom and everything above it, tip included",
			branches: []stackRebaseBranch{published("a", "main", "old"), published("b", "a", "a-head"), published("tip", "b", "b-head")},
			want:     []string{"a", "b", "tip"},
		},
		{
			name:     "a branch already on trunk stays, and so does its child",
			branches: []stackRebaseBranch{published("a", "main", "pin"), published("b", "a", "a-head"), published("tip", "b", "b-head")},
		},
		{
			name:     "a branch that merges clean stays, and so does its child",
			branches: []stackRebaseBranch{with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Stays = true }), published("b", "a", "a-head"), published("tip", "b", "b-head")},
		},
		{
			name:     "a kept ancestor holds its children",
			branches: []stackRebaseBranch{with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Kept = true }), published("b", "a", "a-head"), published("tip", "b", "b-head")},
		},
		{
			name: "a landed parent moves its child onto trunk",
			branches: []stackRebaseBranch{
				{Name: "a", Landed: "#1 landed"},
				with(published("tip", "main", "a-head"), func(b *stackRebaseBranch) { b.WasParent = "a" }),
			},
			want: []string{"tip"},
		},
		{
			name:     "an unpublished branch is the author's own work, and so is what it moves",
			branches: []stackRebaseBranch{with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Remote = "" }), published("b", "a", "a-head"), published("tip", "b", "b-head")},
		},
		{
			name: "a held parent republished without work of its own still guards its children",
			branches: []stackRebaseBranch{
				with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Held = "frozen" }),
				published("b", "a", "a-old"), published("tip", "b", "b-head"),
			},
			want: []string{"b", "tip"},
		},
		{
			name: "a held parent carrying unpublished work moves its children as the author's change",
			branches: []stackRebaseBranch{
				with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Held, b.Remote = "frozen", "" }),
				published("b", "a", "a-old"), published("tip", "b", "b-head"),
			},
		},
		{
			name: "a dropped middle hands its child the grandparent's own work",
			branches: []stackRebaseBranch{
				with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.Remote = "" }),
				{Name: "b", Landed: "#2 closed"},
				with(published("tip", "a", "b-head"), func(b *stackRebaseBranch) { b.WasParent = "b" }),
			},
		},
		{
			name:     "a local branch the run never pushes is no rewrite",
			branches: []stackRebaseBranch{with(published("a", "main", "old"), func(b *stackRebaseBranch) { b.LocalOnly = true }), published("b", "a", "a-head"), published("tip", "b", "b-head")},
			want:     []string{"b", "tip"},
		},
		{
			name:     "a flag that asked for the tip's rewrite covers the tip and nothing below it",
			branches: []stackRebaseBranch{published("a", "main", "old"), published("tip", "a", "a-head")},
			asked:    []string{"tip"},
			want:     []string{"a"},
		},
		{
			name:     "an included branch's replay is asked for, and so is what it moves",
			branches: []stackRebaseBranch{published("a", "main", "old"), published("b", "a", "a-head"), published("tip", "b", "b-head")},
			asked:    []string{"a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &stackRebaseRun{Trunk: "main", Pin: "pin", Tip: "tip", Branches: tc.branches}
			asked := map[string]bool{}
			for _, name := range tc.asked {
				asked[name] = true
			}
			rewrites, err := stackRewrites(t.Context(), "", run, asked)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rewrites {
				got = append(got, r.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("rewrites = %v, want %v", got, tc.want)
			}
		})
	}
}
