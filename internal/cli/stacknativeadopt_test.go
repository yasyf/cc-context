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

// TestStackRebaseAdoptsAnUntrackedParentFromGitAncestry is cc-transcript's
// parallel-grep: its pull request sat on fix/discovery-index, which no native
// stack tracked, and the restack refused the stack as unresolvable.
func TestStackRebaseAdoptsAnUntrackedParentFromGitAncestry(t *testing.T) {
	for _, tt := range []struct {
		name       string
		remoteBase bool
	}{
		{name: "base is a local branch"},
		{name: "base is only on origin", remoteBase: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote())
			f.Decorate(func(ctx context.Context) context.Context { return withStackProvider(ctx, true) })
			mustRun(t, f.Env(), f.Dir, "git", "config", "ccx.stack", "github")
			trunk := gitAt(t, f.Env(), f.Dir, "rev-parse", "main")
			for _, name := range []string{"base", "mid"} {
				shipGTUntracked(t, f, name)
			}
			mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "base", "mid")
			heads := map[string]string{"main": trunk}
			for _, name := range []string{"base", "mid"} {
				heads[name] = gitAt(t, f.Env(), f.Dir, "rev-parse", name)
			}
			if tt.remoteBase {
				mustRun(t, f.Env(), f.Dir, "git", "branch", "-qD", "base")
			}
			if err := nativemeta.Init(f.Context(), filepath.Join(f.Dir, ".git"), "main"); err != nil {
				t.Fatal(err)
			}
			api := stubGTAPI(t)
			api.openPRs("base", "mid")
			api.bases["base"], api.bases["mid"] = "main", "base"
			f.Decorate(api.ctx)
			stubStackPRs(t, f, nil)
			stubStackStanding(t, f, nil)
			recorded := func() gtState {
				t.Helper()
				state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
				if err != nil {
					t.Fatal(err)
				}
				return state
			}

			if !tt.remoteBase {
				plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run", "--no-push")
				if err != nil {
					t.Fatalf("stack rebase --dry-run from mid on an untracked base: %v\n%s%s", err, plan, errOut)
				}
				if !strings.Contains(plan, "\nbase · ") || !strings.Contains(plan, "\nmid · ") || strings.Contains(plan, "mid · onto main") {
					t.Errorf("plan = %s, want base planned on main and mid on base", plan)
				}
				for _, name := range []string{"base", "mid"} {
					if _, tracked := recorded()[name]; tracked {
						t.Errorf("the dry run recorded %s; it must write nothing", name)
					}
				}
			}

			if out, errOut, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
				t.Fatalf("stack rebase from mid on an untracked base: %v\n%s%s", err, out, errOut)
			}
			state := recorded()
			for branch, parent := range map[string]string{"base": "main", "mid": "base"} {
				if s := state[branch]; len(s.Parents) != 1 || s.Parents[0] != (gtRef{Ref: parent, SHA: heads[parent]}) {
					t.Errorf("%s = %+v, want it recorded on %s at %.12s", branch, s, parent, heads[parent])
				}
				if got := gitAt(t, f.Env(), f.Dir, "rev-parse", branch); got != heads[branch] {
					t.Errorf("%s moved from %.12s to %.12s, want it left where it sits", branch, heads[branch], got)
				}
			}
		})
	}
}

func TestStackRebaseTakesTheNativeLaneOverAGraphiteRowItDoesNotTrack(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	seedLaneRecords(f.Context(), t, f.Dir, laneSeed{})
	f.Decorate(func(ctx context.Context) context.Context { return withCleanup(ctx, unsupportedCleanup{}) })
	shipGTUntracked(t, f, "mid")
	mustRun(t, f.Env(), f.Dir, "git", "push", "-q", "origin", "mid")
	common := filepath.Join(f.Dir, ".git")
	vcstest.WriteGraphiteMeta(t, common, `{"main":{"trunk":true},"mid":{}}`)
	if err := nativemeta.Init(f.Context(), common, "main"); err != nil {
		t.Fatal(err)
	}
	api := stubGTAPI(t)
	api.openPRs("mid")
	api.bases["mid"] = "main"
	f.Decorate(api.ctx)
	stubStackPRs(t, f, nil)
	stubStackStanding(t, f, nil)

	plan, errOut, err := runStackCmd(t, f, "rebase", "--dry-run")
	if err != nil {
		t.Fatalf("stack rebase --dry-run over a parentless Graphite row: %v\n%s%s", err, plan, errOut)
	}
	if !strings.Contains(plan, "\nmid · ") {
		t.Errorf("plan = %s, want mid planned on its pull request's base", plan)
	}

	if out, errOut, err := runStackCmd(t, f, "rebase", "--no-push"); err != nil {
		t.Fatalf("stack rebase over a parentless Graphite row: %v\n%s%s", err, out, errOut)
	}
	if native, err := nativemeta.Membership(t.Context(), common, "mid"); err != nil || !native {
		t.Errorf("mid native = %v, %v; want it recorded in the native stack", native, err)
	}
	state, err := gtStateQuery(f.Context(), render.Dir(f.Dir), "test")
	if err != nil {
		t.Fatal(err)
	}
	if s := state["mid"]; len(s.Parents) != 1 || s.Parents[0].Ref != "main" {
		t.Errorf("mid = %+v, want it recorded on main", s)
	}
}
