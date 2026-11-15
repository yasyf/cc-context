package cli

import (
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestInfoGitHubReusesFreshLaneRepository(t *testing.T) {
	f := vcstest.Repo(t)
	f.RotateLog(t)
	ctx := render.WithEnv(f.Context(), "PATH="+t.TempDir())
	repo := vcs.Repo{NameWithOwner: "yasyf/cc-context", Owner: "yasyf", ViewerLogin: "yasyf", ViewerPermission: "ADMIN", Affiliated: true}
	var info vcsInfo
	err := infoGitHub(ctx, lane{root: f.Dir, repo: &repo}, vcsInfoOpts{}, &info)
	if err != nil || info.GitHub != &repo || info.GitHubError != "" {
		t.Fatalf("fresh lane repository was not reused: %+v, %v", info, err)
	}
	if calls := vcstest.Invocations(t, f.ArgvLog); len(calls) != 0 {
		t.Fatalf("fresh lane reuse spawned children: %q", calls)
	}
}
