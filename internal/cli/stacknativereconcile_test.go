package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func stackNativeReconcileRepo(t *testing.T, names ...string) (*vcstest.Fixture, lane, *gtAPIStub) {
	t.Helper()
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	for _, name := range names {
		shipGTUntracked(t, f, name)
	}
	mustRun(t, f.Env(), f.Dir, "git", append([]string{"push", "-q", "origin"}, names...)...)
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	return f, lane{kind: vcs.Git, root: f.Dir, checkout: checkout, gt: true, native: true, repo: &vcs.Repo{NameWithOwner: "org/repo"}}, api
}

func stackNativeParents(t *testing.T, f *vcstest.Fixture) map[string]string {
	t.Helper()
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]string{}
	for name, s := range state {
		if len(s.Parents) > 0 {
			parents[name] = s.Parents[0].Ref
		}
	}
	return parents
}

func TestStackReconcileAdoptsABranchTheCatalogLostFromItsNativeStack(t *testing.T) {
	f, l, api := stackNativeReconcileRepo(t, "base", "mid", "child")
	if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
		t.Fatal(err)
	}
	api.openPRs("base", "mid", "child")
	api.bases["base"], api.bases["mid"], api.bases["child"] = "main", "base", "mid"
	api.stacks["base"], api.stacks["mid"], api.stacks["child"] = 46, 46, 46

	recorded, err := nativeAdoptFromGitHub(f.Context(), l, "stack rebase", "child")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"base on main", "mid on base", "child on mid"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("recorded = %q, want it to name %q", recorded, want)
		}
	}
	if got := stackNativeParents(t, f); got["base"] != "main" || got["mid"] != "base" || got["child"] != "mid" {
		t.Errorf("parents = %v, want child on mid on base on main", got)
	}
}

func TestStackReconcileLeavesABranchWithNoNativeStackForTheAncestryAdoption(t *testing.T) {
	f, l, api := stackNativeReconcileRepo(t, "fresh")
	if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
		t.Fatal(err)
	}
	api.openPRs("fresh")
	api.bases["fresh"] = "main"

	recorded, err := nativeAdoptFromGitHub(f.Context(), l, "stack submit", "fresh")
	if err != nil || recorded != "" {
		t.Fatalf("adopt = %q, %v; want nothing recorded and no refusal", recorded, err)
	}
}

func TestStackReconcileMovesAStackOffABaseThatLanded(t *testing.T) {
	f, l, api := stackNativeReconcileRepo(t, "landed", "child")
	common := filepath.Join(f.Dir, ".git")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "landed")
	child := gitAt(t, f.Env(), f.Dir, "rev-parse", "child")
	if err := nativemeta.Init(f.Context(), common, "landed"); err != nil {
		t.Fatal(err)
	}
	if err := nativemeta.AdoptRoot(f.Context(), common, "child", "landed", landed, child); err != nil {
		t.Fatal(err)
	}
	api.merged["landed"] = gtStubMerged{number: 87, head: landed}

	moved, err := nativeRetargetLandedRoot(f.Context(), l, "stack submit", "child")
	if err != nil {
		t.Fatal(err)
	}
	if want := "moved the native stack of child onto main: its base landed landed as #87"; moved != want {
		t.Errorf("moved = %q, want %q", moved, want)
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if s := state["child"]; len(s.Parents) != 1 || s.Parents[0].Ref != "main" || s.Parents[0].SHA != landed {
		t.Errorf("child parents = %+v, want main at the old fork %s", s.Parents, shortOID(landed))
	}
}
