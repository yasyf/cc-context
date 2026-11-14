package vcs

import (
	"slices"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGitPushRemote(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config [][]string
		want   string
	}{
		{name: "none set"},
		{name: "push default", config: [][]string{{"remote.pushDefault", "fork"}}, want: "fork"},
		{name: "branch push remote", config: [][]string{{"branch.feature.pushRemote", "mine"}}, want: "mine"},
		{name: "branch push remote beats push default", config: [][]string{{"remote.pushDefault", "fork"}, {"branch.feature.pushRemote", "mine"}}, want: "mine"},
		{name: "another branch's push remote is not this one's", config: [][]string{{"branch.other.pushRemote", "mine"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Remote())
			for _, kv := range tt.config {
				runGit(t, f, f.Dir, "config", kv[0], kv[1])
			}
			got, err := GitPushRemote(f.Context(), render.Dir(f.Dir), "feature")
			if err != nil {
				t.Fatalf("GitPushRemote: %v", err)
			}
			if got != tt.want {
				t.Errorf("GitPushRemote = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRemotesOwnedBy(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	runGit(t, f, f.Dir, "remote", "add", "upstream", "https://github.com/maximhq/bifrost.git")
	runGit(t, f, f.Dir, "remote", "add", "fork", "git@github.com:Yasyf/bifrost.git")
	runGit(t, f, f.Dir, "remote", "add", "mirror", "https://github.com/yasyf/bifrost-mirror")

	got, err := RemotesOwnedBy(f.Context(), render.Dir(f.Dir), "yasyf")
	if err != nil {
		t.Fatalf("RemotesOwnedBy: %v", err)
	}
	slices.Sort(got)
	if want := []string{"fork", "mirror"}; !slices.Equal(got, want) {
		t.Errorf("RemotesOwnedBy = %v, want %v", got, want)
	}
}

func TestRepoPushDenied(t *testing.T) {
	for _, tt := range []struct {
		repo Repo
		want bool
	}{
		{repo: Repo{ViewerPermission: "READ"}, want: true},
		{repo: Repo{ViewerPermission: "TRIAGE"}, want: true},
		{repo: Repo{ViewerPermission: "WRITE"}},
		{repo: Repo{ViewerPermission: "MAINTAIN"}},
		{repo: Repo{ViewerPermission: "ADMIN"}},
		{repo: Repo{}},
		{repo: Repo{ViewerPermission: "READ", Installation: true}},
	} {
		if got := tt.repo.PushDenied(); got != tt.want {
			t.Errorf("%+v.PushDenied() = %v, want %v", tt.repo, got, tt.want)
		}
	}
}
