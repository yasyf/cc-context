package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestLookupRepoInvalidatesEffectiveIncludedConfig(t *testing.T) {
	for _, scope := range []string{"command", "nested", "global", "local", "gitdir", "onbranch", "static-shadow"} {
		t.Run(scope, func(t *testing.T) {
			f := vcstest.Repo(t)
			f.Isolate(t)
			log := ghReplay(t, f, map[string][]string{
				ghRepoVerb:    {"repo-api-foreign", "repo-api-own"},
				"api graphql": {"viewer-graphql-foreign"},
				"api user":    {"user-api-own"},
			})
			dir := t.TempDir()
			include := filepath.Join(dir, "routing.gitconfig")
			switch scope {
			case "command":
				f.Setenv("GIT_CONFIG_COUNT", "1")
				f.Setenv("GIT_CONFIG_KEY_0", "include.path")
				f.Setenv("GIT_CONFIG_VALUE_0", include)
			case "nested", "global":
				outer := filepath.Join(dir, "outer.gitconfig")
				mustWriteFile(t, outer, "[include]\n\tpath = routing.gitconfig\n")
				if scope == "global" {
					f.Setenv("GIT_CONFIG_GLOBAL", outer)
				} else {
					f.Setenv("GIT_CONFIG_COUNT", "1")
					f.Setenv("GIT_CONFIG_KEY_0", "include.path")
					f.Setenv("GIT_CONFIG_VALUE_0", outer)
				}
			case "local", "static-shadow":
				f.Out(t, "git", "config", "include.path", include)
				if scope == "static-shadow" {
					selected := filepath.Join(dir, "static.gitconfig")
					mustWriteFile(t, selected, "[ccx]\n\tmarker = private-config-marker\n")
					f.Setenv("GIT_CONFIG", selected)
					f.Setenv("CCX_EXPECT_GIT_CONFIG", selected)
					replay := render.LookPath(f.Context(), "gh")
					bin := t.TempDir()
					execstub.Write(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+
						`test "$GIT_CONFIG" = "$CCX_EXPECT_GIT_CONFIG" || exit 2`+"\n"+
						`exec `+shQuote(replay)+` "$@"`+"\n")
					f.PrependPATH(bin)
				}
			case "gitdir", "onbranch":
				condition := "gitdir:**"
				if scope == "onbranch" {
					condition = "onbranch:main"
				}
				f.Setenv("GIT_CONFIG_COUNT", "1")
				f.Setenv("GIT_CONFIG_KEY_0", "includeIf."+condition+".path")
				f.Setenv("GIT_CONFIG_VALUE_0", include)
			}
			ctx := f.Context()
			var first Repo
			for i, owner := range []string{"cli/cli", "yasyf/cc-context"} {
				remote := "https://github.com/" + owner + ".git"
				mustWriteFile(t, include, "[remote \"origin\"]\n\turl = "+remote+"\n")
				effective, err := render.RunCLI(ctx, render.Dir(f.Dir), "git", []string{"remote", "get-url", "origin"})
				if err != nil || strings.TrimSpace(effective) != remote {
					t.Fatalf("effective remote = %q, %v; want %q", effective, err, remote)
				}
				repo, err := LookupRepo(ctx, render.Dir(f.Dir), false)
				if err != nil || repo.NameWithOwner != owner {
					t.Fatalf("lookup %d = %+v, %v; want %q", i, repo, err, owner)
				}
				if i == 0 {
					first = repo
				}
				cached, err := LookupRepo(ctx, render.Dir(f.Dir), false)
				if err != nil || withoutFetchTime(cached) != withoutFetchTime(repo) || !cached.FetchedAt.Equal(repo.FetchedAt) {
					t.Fatalf("warm lookup %d = %+v, %v; want %+v", i, cached, err, repo)
				}
			}
			if first.NameWithOwner != "cli/cli" || ghVerbCount(t, log, ghRepoVerb) != 2 {
				t.Fatalf("first=%+v, repository reads=%d", first, ghVerbCount(t, log, ghRepoVerb))
			}
		})
	}
}

func TestRepoCachePathEffectiveGitSelection(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	for _, key := range []string{"remote.upstream.url", "remote.origin.pushurl", "remote.origin.gh-resolved", "url.https://github.com/.insteadOf"} {
		t.Run(key, func(t *testing.T) {
			f.Out(t, "git", "config", key, "first")
			first, err := RepoCachePath(f.Context(), f.Dir)
			if err != nil {
				t.Fatal(err)
			}
			f.Out(t, "git", "config", key, "second")
			second, err := RepoCachePath(f.Context(), f.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Fatal("effective selection change reused the old path")
			}
		})
	}
}

func TestRepoCachePathInactiveAndMissingIncludes(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	include := filepath.Join(t.TempDir(), "routing.gitconfig")
	ctx := render.WithEnv(f.Context(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=includeIf.onbranch:never-selected.path", "GIT_CONFIG_VALUE_0="+include)
	first, err := RepoCachePath(ctx, f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, include, "[invalid secret-config-marker\n")
	second, err := RepoCachePath(ctx, f.Dir)
	if err != nil || first != second {
		t.Fatalf("inactive include changed path or failed: %v", err)
	}
	if err := os.Remove(include); err != nil {
		t.Fatal(err)
	}
	active := render.WithEnv(f.Context(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=include.path", "GIT_CONFIG_VALUE_0="+include)
	missing, err := RepoCachePath(active, f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, include, "[remote \"origin\"]\n\turl = https://github.com/cli/cli.git\n")
	present, err := RepoCachePath(active, f.Dir)
	if err != nil || missing == present {
		t.Fatalf("newly present active include reused the missing path: %v", err)
	}
}

func TestRepoCachePathDoesNotExposeConfig(t *testing.T) {
	f := vcstest.Repo(t)
	f.Isolate(t)
	const secret = "private-config-marker"
	f.Out(t, "git", "config", "credential.helper", secret)
	selected := filepath.Join(t.TempDir(), "static.gitconfig")
	mustWriteFile(t, selected, "[credential]\n\thelper = "+secret+"\n")
	f.Setenv("GIT_CONFIG", selected)
	path, err := RepoCachePath(f.Context(), f.Dir)
	if err != nil || strings.Contains(path, secret) {
		t.Fatalf("config fingerprint failed or exposed value: %v", err)
	}
	bin := t.TempDir()
	execstub.Write(t, filepath.Join(bin, "git"), "#!/bin/sh\n"+
		`test -z "${GIT_CONFIG+x}" || exit 8`+"\n"+
		"printf '%s' "+shQuote(secret)+"\nprintf '%s' "+shQuote(secret)+" >&2\nexit 7\n")
	ctx := render.WithEnv(f.Context(), "PATH="+bin)
	_, err = RepoCachePath(ctx, f.Dir)
	if !errors.Is(err, ErrNoGitHub) || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("config failure lost classification or exposed value: %v", err)
	}
}

func TestLookupRepoEffectiveConfigChildBudget(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Worktree("feat"))
	f.Isolate(t)
	log := ghReplay(t, f, map[string][]string{ghRepoVerb: {"repo-api-own"}, "api user": {"user-api-own"}})
	var first Repo
	for i, root := range []string{f.Dir, f.Dir, f.WorktreePath("feat")} {
		f.RotateLog(t)
		repo, err := LookupRepo(f.Context(), render.Dir(root), false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = repo
		} else if withoutFetchTime(repo) != withoutFetchTime(first) || !repo.FetchedAt.Equal(first.FetchedAt) {
			t.Fatal("warm lookup changed the entire repository record")
		}
		want := [][]string{{"git", "config", "--null", "--list", "--includes"}}
		if got := vcstest.Invocations(t, f.ArgvLog); !reflect.DeepEqual(got, want) {
			t.Fatalf("lookup %d Git children = %q, want %q", i, got, want)
		}
		if got := len(ghCalls(t, log)); got != 2 {
			t.Fatalf("lookup %d GH children = %d, want 2 total", i, got)
		}
	}
}
