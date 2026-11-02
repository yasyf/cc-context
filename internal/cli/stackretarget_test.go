package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestStackRetargetMovesAStackOffABaseThatLanded(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	shipGTUntracked(t, f, "landed")
	shipGTUntracked(t, f, "child")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "landed", "child")
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	l := lane{kind: vcs.Git, root: f.Dir, checkout: checkout, gt: true, native: true, repo: &vcs.Repo{NameWithOwner: "org/repo"}}
	common := filepath.Join(f.Dir, ".git")
	landed := gitAt(t, f.Env(), f.Dir, "rev-parse", "landed")
	child := gitAt(t, f.Env(), f.Dir, "rev-parse", "child")
	catalog := fmt.Sprintf(`{"schemaVersion":1,"stacks":[{"trunk":{"branch":"landed","head":%q},"branches":[{"branch":"child","base":%q,"head":%q}]}]}`, landed, landed, child)
	if err := os.WriteFile(filepath.Join(common, "gh-stack"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.merged["landed"] = gtStubMerged{number: 87, head: landed}
	f.Decorate(api.ctx)

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
