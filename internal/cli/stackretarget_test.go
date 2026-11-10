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

	moved, err := nativeRetargetRoot(f.Context(), l, "stack submit", "child")
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

func TestStackRetargetTracksAnOpenRootTheCatalogCalledTrunk(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	fork := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
	shipGTUntracked(t, f, "runner")
	shipGTUntracked(t, f, "child")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "runner", "child")
	mustRun(t, f.Env(), f.Dir, "git", "remote", "set-head", "origin", "--delete")
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	l := lane{kind: vcs.Git, root: f.Dir, checkout: checkout, gt: true, native: true, repo: &vcs.Repo{NameWithOwner: "org/repo"}}
	common := filepath.Join(f.Dir, ".git")
	runner := gitAt(t, f.Env(), f.Dir, "rev-parse", "runner")
	child := gitAt(t, f.Env(), f.Dir, "rev-parse", "child")
	catalog := fmt.Sprintf(`{"schemaVersion":1,"stacks":[{"number":4,"trunk":{"branch":"runner","head":%q},"branches":[{"branch":"child","base":%q,"head":%q}]}]}`, runner, runner, child)
	if err := os.WriteFile(filepath.Join(common, "gh-stack"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("runner")
	api.bases["runner"] = "main"
	api.stacks["runner"] = 4
	f.Decorate(api.ctx)

	moved, err := nativeRetargetRoot(f.Context(), l, "stack rebase", "child")
	if err != nil {
		t.Fatal(err)
	}
	if want := "tracked runner onto main: the native stack of child sat on it as its trunk, and its pull request #9000 is open on main"; moved != want {
		t.Errorf("moved = %q, want %q", moved, want)
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if s := state["runner"]; s.Trunk || len(s.Parents) != 1 || s.Parents[0].Ref != "main" || s.Parents[0].SHA != fork {
		t.Errorf("runner = %+v, want a branch on main at its fork %s", s, shortOID(fork))
	}
	if s := state["child"]; len(s.Parents) != 1 || s.Parents[0].Ref != "runner" {
		t.Errorf("child parents = %+v, want runner", s.Parents)
	}
	if s := state["main"]; !s.Trunk {
		t.Errorf("main = %+v, want the trunk", s)
	}
}
