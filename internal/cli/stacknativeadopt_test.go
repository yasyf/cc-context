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

func TestStackRebaseAdoptsAnUntrackedBranchOntoItsNativeStackBase(t *testing.T) {
	f := shipRepo(t, vcstest.Remote())
	f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
	mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
	for _, name := range []string{"base", "mid"} {
		shipGTUntracked(t, f, name)
	}
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid")
	if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("base", "mid")
	api.bases["base"], api.bases["mid"] = "main", "base"
	api.stacks["base"], api.stacks["mid"] = 46, 46
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)
	heads := map[string]string{}
	for _, name := range []string{"base", "mid"} {
		heads[name] = gitAt(t, f.Env(), f.Dir, "rev-parse", name)
	}
	recorded := func() gtState {
		t.Helper()
		state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--no-push")
	if err != nil {
		t.Fatalf("stack rebase --dry-run from an untracked mid: %v\n%s%s", err, plan, errOut)
	}
	if !strings.Contains(plan, "\nmid · ") || strings.Contains(plan, "mid · onto main") {
		t.Errorf("plan = %s, want mid planned on base, its native stack base", plan)
	}
	if _, tracked := recorded()["mid"]; tracked {
		t.Errorf("the dry run recorded mid; it must write nothing")
	}

	if out, errOut, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase from an untracked mid: %v\n%s%s", err, out, errOut)
	}
	state := recorded()
	for branch, parent := range map[string]string{"base": "main", "mid": "base"} {
		if s := state[branch]; len(s.Parents) != 1 || s.Parents[0].Ref != parent {
			t.Errorf("%s = %+v, want it recorded on %s", branch, s, parent)
		}
		if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != heads[branch] {
			t.Errorf("%s moved from %.12s to %.12s, want it left where its stack has it", branch, heads[branch], got)
		}
	}
}
