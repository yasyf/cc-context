package cli

import (
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestGitTrunkBranchFromGitHubDefault(t *testing.T) {
	for _, tt := range []struct {
		name          string
		defaultBranch string
	}{
		{name: "github names the default branch", defaultBranch: "trunk"},
		{name: "github cannot name it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote(), vcstest.NoOriginHead())
			seedLaneRecords(f.Context(), t, f.Dir, laneSeed{defaultBranch: tt.defaultBranch})

			got, err := gitTrunkBranch(f.Context(), render.Dir(f.Dir))
			if err != nil {
				t.Fatalf("gitTrunkBranch: %v", err)
			}
			if got != tt.defaultBranch {
				t.Errorf("gitTrunkBranch = %q, want %q", got, tt.defaultBranch)
			}
		})
	}
}

func TestShipForkRemote(t *testing.T) {
	repo := vcs.Repo{NameWithOwner: "maximhq/bifrost", ViewerLogin: "yasyf", ViewerPermission: "READ"}
	for _, tt := range []struct {
		name       string
		remotes    map[string]string
		config     [][]string
		fork       string
		wantRemote string
		wantOwner  string
		wantURL    string
		wantErr    string
	}{
		{
			name:       "the one remote of the viewer's",
			remotes:    map[string]string{"upstream": "https://github.com/maximhq/bifrost.git", "fork": "git@github.com:yasyf/bifrost.git"},
			wantRemote: "fork",
			wantOwner:  "yasyf",
		},
		{
			name:       "push default wins over the scan",
			remotes:    map[string]string{"fork": "git@github.com:yasyf/bifrost.git", "mine": "git@github.com:yasyf/other.git"},
			config:     [][]string{{"remote.pushDefault", "mine"}},
			wantRemote: "mine",
			wantOwner:  "yasyf",
		},
		{
			name:       "branch push remote wins over push default",
			remotes:    map[string]string{"fork": "git@github.com:yasyf/bifrost.git", "mine": "git@github.com:yasyf/other.git"},
			config:     [][]string{{"remote.pushDefault", "mine"}, {"branch.feature.pushRemote", "fork"}},
			wantRemote: "fork",
			wantOwner:  "yasyf",
		},
		{
			name:       "no fork remote adds one over ssh",
			remotes:    map[string]string{"upstream": "https://github.com/maximhq/bifrost.git"},
			fork:       "rest-fork-existing",
			wantRemote: "fork",
			wantOwner:  "yasyf",
			wantURL:    "git@github.com:yasyf/bifrost.git",
		},
		{
			name:       "no fork remote adds one over the upstream's https",
			remotes:    map[string]string{"upstream": "https://github.com/maximhq/bifrost.git"},
			config:     [][]string{{"branch.feature.remote", "upstream"}},
			fork:       "rest-fork-existing",
			wantRemote: "fork",
			wantOwner:  "yasyf",
			wantURL:    "https://github.com/yasyf/bifrost.git",
		},
		{
			name:    "fork answer names no clone URLs",
			remotes: map[string]string{"upstream": "https://github.com/maximhq/bifrost.git"},
			fork:    "run-list-none",
			wantErr: "not a fork's clone URLs",
		},
		{
			name:    "several forks",
			remotes: map[string]string{"a": "git@github.com:yasyf/bifrost.git", "b": "git@github.com:yasyf/other.git"},
			wantErr: "remote.pushDefault",
		},
		{
			name:    "configured remote with no owner",
			config:  [][]string{{"remote.pushDefault", "origin"}},
			wantErr: "names no GitHub owner",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := shipRepo(t, vcstest.Remote(), vcstest.Branch("feature"))
			for name, url := range tt.remotes {
				mustRun(t, f.Env(), f.Dir, "git", "remote", "add", name, url)
			}
			for _, kv := range tt.config {
				mustRun(t, f.Env(), f.Dir, "git", "config", kv[0], kv[1])
			}
			if tt.fork != "" {
				writeShipGH(t, f)
				f.Setenv("GH_FORK_URLS", ghStdout(t, tt.fork))
			}

			remote, owner, err := shipForkRemote(f.Context(), render.Dir(f.Dir), "feature", repo)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("shipForkRemote error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("shipForkRemote: %v", err)
			}
			if remote != tt.wantRemote || owner != tt.wantOwner {
				t.Errorf("shipForkRemote = %q, %q; want %q, %q", remote, owner, tt.wantRemote, tt.wantOwner)
			}
			if tt.wantURL != "" {
				if got := gitAt(t, f.Env(), f.Dir, "remote", "get-url", tt.wantRemote); got != tt.wantURL {
					t.Errorf("remote %s URL = %q, want %q", tt.wantRemote, got, tt.wantURL)
				}
			}
		})
	}
}

func TestShipPRCrossRepo(t *testing.T) {
	f := shipPRFixture(t, vcstest.Branch("feature"))
	created := shipPRCreated(t)
	l, err := resolveLane(f.Context(), "ship", f.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	shipResetLog(t, f)

	seg, err := shipPR(f.Context(), io.Discard, l, fakePRRepo, "viewer", "fork", "feature", "main", "fix: frobnicate", nil, nil)
	if err != nil {
		t.Fatalf("shipPR: %v", err)
	}
	assertInvocations(t, vcstest.Invocations(t, f.ArgvLog), [][]string{
		append([]string{"gh"}, ghPullsByHeadArgv(fakePRRepo, "viewer:feature", "open")...),
		append([]string{"gh"}, prCreateArgv(fakePRRepo, "viewer:feature", "main", "fix: frobnicate", "", false)...),
	})
	if want := "opened PR #" + strconv.Itoa(created.Number) + " " + created.URL; seg != want {
		t.Errorf("segment = %q, want %q", seg, want)
	}
}

func TestGHPullsByHeadArgvNamesAForkOwner(t *testing.T) {
	for head, want := range map[string]string{"feature": "head=yasyf:feature", "viewer:feature": "head=viewer:feature"} {
		argv := ghPullsByHeadArgv("yasyf/cc-context", head, "open")
		if !slices.Contains(argv, want) {
			t.Errorf("ghPullsByHeadArgv(%q) = %v, want %s", head, argv, want)
		}
	}
}
