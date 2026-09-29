package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/vcstest"
)

func stubStackStanding(t *testing.T, f *vcstest.Fixture, standings map[string]stackStanding) {
	t.Helper()
	query := func(_ context.Context, _ lane, branches []string) (map[string]stackStanding, error) {
		out := map[string]stackStanding{}
		for _, b := range branches {
			if s, ok := standings[b]; ok {
				out[b] = s
			}
		}
		return out, nil
	}
	f.Decorate(func(parent context.Context) context.Context { return withStackStanding(parent, query) })
}

// shipGTLandedBottom is join-scope's #27533: a, b, c published, a landed
// on trunk, and a ship from c that would replay b onto trunk for no change of
// its own.
func shipGTLandedBottom(t *testing.T, standing stackStanding) (*vcstest.Fixture, string) {
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
	stubStackStanding(t, f, map[string]stackStanding{"b": standing, "c": standing})
	restackSquashRemote(t, f, "main", "a (#100)", "a")
	mustRun(t, f.Env(), f.Dir, "git", "fetch", "-q", "origin")
	published := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b")
	shipGTReady(t, f)
	return f, published
}

func TestShipRefusesToRestackAGreenOrApprovedPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		standing stackStanding
		want     string
	}{
		{"green", stackStanding{Green: true}, "b #101 (green)"},
		{"approved", stackStanding{Approved: true}, "b #101 (approved)"},
		{"green and approved", stackStanding{Green: true, Approved: true}, "b #101 (green, approved)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, published := shipGTLandedBottom(t, tc.standing)

			_, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch")
			var refusal *shipRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("ship = %v, want a refusal", err)
			}
			for _, want := range []string{"shipping c would restack " + tc.want + " onto a new base", "--tip-only", "--restack"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "c #102") {
				t.Errorf("refusal = %q, names the tip it was asked to ship", err)
			}
			if refs := gtPushedRefs(shipGTInvocations(t, f)); len(refs) != 0 {
				t.Errorf("pushed %v, want nothing", refs)
			}
			if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got != published {
				t.Errorf("origin b moved to %s, want its published %s", got, published)
			}
		})
	}
}

func TestShipAfterAGreenRestackRefusal(t *testing.T) {
	for _, tc := range []struct {
		flag  string
		moved bool
		refs  []string
	}{
		{"--tip-only", false, []string{"c"}},
		{"--restack", true, []string{"b", "c"}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			f, published := shipGTLandedBottom(t, stackStanding{Green: true, Approved: true})
			if _, _, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err == nil {
				t.Fatal("ship: want the green b refused")
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

func TestShipRestacksAPullRequestWithNothingEarned(t *testing.T) {
	f, published := shipGTLandedBottom(t, stackStanding{})

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got == published {
		t.Error("origin b stayed on the landed a, want it restacked onto trunk")
	}
}

func TestShipMovesAGreenConflictingAncestor(t *testing.T) {
	f, _, base := stackMergeableBase(t, "CONFLICTING")
	stubStackStanding(t, f, map[string]stackStanding{"base": {Green: true, Approved: true}})

	if _, errStr, err := runShipCmdFull(f.Context(), t, "-m", "fix: frobnicate", "--no-watch"); err != nil {
		t.Fatalf("ship = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "base"); got == base || !stackOnto(t, f, "origin/main", got) {
		t.Errorf("origin base = %s, want the conflicting base moved off %s onto trunk", got, base)
	}
}

func TestStackSubmitWithPRMetaStillRestacksAGreenPullRequest(t *testing.T) {
	f, published := shipGTLandedBottom(t, stackStanding{Green: true, Approved: true})
	mustRun(t, f.Env(), f.Dir, "git", "checkout", "-q", "--", "f.txt")
	writeShipGH(t, f)
	seedPRViews(t, map[string]string{"c": `{"number":102,"url":"https://github.com/x/pull/102","body":""}`})

	if _, errStr, err := runStackCmd(t, f, "submit", "--pr-title", "c=c, retitled"); err != nil {
		t.Fatalf("stack submit --pr-title = %v (stderr=%q)", err, errStr)
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "b"); got == published {
		t.Error("origin b stayed on the landed a, want stack submit to restack it onto trunk")
	}
}

func TestStackRestacks(t *testing.T) {
	open := &stackPR{Number: 1, State: "OPEN", Mergeable: "MERGEABLE"}
	conflicting := &stackPR{Number: 2, State: "OPEN", Mergeable: "CONFLICTING"}
	published := func(name, parent, oldBase string, pr *stackPR) stackRebaseBranch {
		return stackRebaseBranch{Name: name, Parent: parent, WasParent: parent, OldBase: oldBase, Head: name + "-head", Remote: name + "-head", PR: pr}
	}
	for _, tc := range []struct {
		name     string
		branches []stackRebaseBranch
		want     []string
	}{
		{
			name:     "trunk moved under the bottom and everything above it",
			branches: []stackRebaseBranch{published("a", "main", "old", open), published("b", "a", "a-head", open), published("tip", "b", "b-head", open)},
			want:     []string{"a", "b"},
		},
		{
			name:     "a branch already on trunk stays, and so does its child",
			branches: []stackRebaseBranch{published("a", "main", "pin", open), published("b", "a", "a-head", open), published("tip", "b", "b-head", open)},
		},
		{
			name: "a kept ancestor holds its children",
			branches: []stackRebaseBranch{
				func() stackRebaseBranch { b := published("a", "main", "old", open); b.Kept = true; return b }(),
				published("b", "a", "a-head", open), published("tip", "b", "b-head", open),
			},
		},
		{
			name: "a landed parent moves its child onto trunk",
			branches: []stackRebaseBranch{
				{Name: "a", Landed: "#1 landed"},
				func() stackRebaseBranch { b := published("b", "main", "a-head", open); b.WasParent = "a"; return b }(),
				published("tip", "b", "b-head", open),
			},
			want: []string{"b"},
		},
		{
			name:     "a conflicting branch moves, and its green child is still guarded",
			branches: []stackRebaseBranch{published("a", "main", "old", conflicting), published("b", "a", "a-head", open), published("tip", "b", "b-head", open)},
			want:     []string{"b"},
		},
		{
			name: "an unpublished branch is the author's own work, and so is what it moves",
			branches: []stackRebaseBranch{
				func() stackRebaseBranch { b := published("a", "main", "old", open); b.Remote = ""; return b }(),
				published("b", "a", "a-head", open), published("tip", "b", "b-head", open),
			},
		},
		{
			name: "a held parent republished without work of its own still guards its children",
			branches: []stackRebaseBranch{
				func() stackRebaseBranch { b := published("a", "main", "old", open); b.Held = "frozen"; return b }(),
				published("b", "a", "a-old", open), published("tip", "b", "b-head", open),
			},
			want: []string{"b"},
		},
		{
			name: "a held parent carrying unpublished work moves its children as the author's change",
			branches: []stackRebaseBranch{
				func() stackRebaseBranch {
					b := published("a", "main", "old", open)
					b.Held, b.Remote = "frozen", ""
					return b
				}(),
				published("b", "a", "a-old", open), published("tip", "b", "b-head", open),
			},
		},
		{
			name: "a dropped middle hands its child the grandparent's own work",
			branches: []stackRebaseBranch{
				func() stackRebaseBranch { b := published("a", "main", "old", open); b.Remote = ""; return b }(),
				{Name: "b", Landed: "#2 closed"},
				func() stackRebaseBranch { b := published("c", "a", "b-head", open); b.WasParent = "b"; return b }(),
				published("tip", "c", "c-head", open),
			},
		},
		{
			name: "a branch without an open pull request earns nothing but still passes its churn up",
			branches: []stackRebaseBranch{
				published("a", "main", "old", nil),
				published("b", "a", "a-head", open), published("tip", "b", "b-head", open),
			},
			want: []string{"b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &stackRebaseRun{Trunk: "main", Pin: "pin", Tip: "tip", Branches: tc.branches}
			restacks, err := stackRestacks(t.Context(), "", run)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, b := range restacks {
				got = append(got, b.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("restacks = %v, want %v", got, tc.want)
			}
		})
	}
}
