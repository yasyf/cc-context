package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func shipForkRemote(ctx context.Context, dir render.Dir, branch string, repo vcs.Repo) (remote, owner string, err error) {
	remote, err = vcs.GitPushRemote(ctx, dir, branch)
	if err != nil {
		return "", "", fmt.Errorf("ship: %w", err)
	}
	if remote == "" {
		forks, err := vcs.RemotesOwnedBy(ctx, dir, repo.ViewerLogin)
		if err != nil {
			return "", "", fmt.Errorf("ship: %w", err)
		}
		switch len(forks) {
		case 0:
			return "", "", refuse("ship: you have %s access to %s and cannot push to it, and no remote of yours holds a fork — run gh repo fork --remote --remote-name fork, then ship again", strings.ToLower(repo.ViewerPermission), repo.NameWithOwner)
		case 1:
			remote = forks[0]
		default:
			return "", "", refuse("ship: you have %s access to %s and cannot push to it, and %s all hold your forks — name the one to push to with git config remote.pushDefault <remote>", strings.ToLower(repo.ViewerPermission), repo.NameWithOwner, strings.Join(forks, ", "))
		}
	}
	owner, ok, err := vcs.RemoteOwner(ctx, dir, remote)
	if err != nil {
		return "", "", fmt.Errorf("ship: %w", err)
	}
	if !ok {
		return "", "", refuse("ship: %s cannot take a push for a pull request on %s: its URL names no GitHub owner", remote, repo.NameWithOwner)
	}
	return remote, owner, nil
}

func githubDefaultBranch(ctx context.Context, dir render.Dir) (string, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", "remote.origin.url"})
	if err != nil {
		return "", fmt.Errorf("ship: git config remote.origin.url: %w", err)
	}
	switch code {
	case 0:
	case 1:
		return "", nil
	default:
		return "", fmt.Errorf("ship: git config remote.origin.url: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	repo, err := vcs.LookupRepo(ctx, dir, false)
	if errors.Is(err, vcs.ErrNoGitHub) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ship: %w", err)
	}
	return repo.DefaultBranch, nil
}
