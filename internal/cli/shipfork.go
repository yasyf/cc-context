package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const forkRemote = "fork"

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
			if remote, err = shipAddForkRemote(ctx, dir, branch, repo); err != nil {
				return "", "", err
			}
		case 1:
			remote = forks[0]
		default:
			return "", "", refuse("ship: %s takes pull request branches from your fork, and %s all hold your forks — name the one to push to with git config remote.pushDefault <remote>", repo.NameWithOwner, strings.Join(forks, ", "))
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

func shipPushRepo(ctx context.Context, l lane, plan branchPlan, prRun bool, base string) (vcs.Repo, error) {
	if prRun {
		return shipPRRepo(ctx, l, plan, base)
	}
	if plan.trunk != "" && plan.name == plan.trunk {
		return vcs.Repo{}, nil
	}
	repo, err := vcs.LookupRepo(ctx, l.dir(), false)
	if errors.Is(err, vcs.ErrNoGitHub) {
		return vcs.Repo{}, nil
	}
	if err != nil {
		return vcs.Repo{}, fmt.Errorf("ship: %w", err)
	}
	return repo, nil
}

func shipAddForkRemote(ctx context.Context, dir render.Dir, branch string, repo vcs.Repo) (string, error) {
	out, err := ghAPI(ctx, dir, "-X", "POST", "repos/"+repo.NameWithOwner+"/forks", "--jq", `[.clone_url, .ssh_url] | @tsv`)
	if err != nil {
		return "", fmt.Errorf("ship: %s takes pull request branches from your fork, and forking it failed: %w", repo.NameWithOwner, err)
	}
	https, ssh, ok := strings.Cut(strings.TrimSpace(out), "\t")
	if !ok || https == "" || ssh == "" {
		return "", fmt.Errorf("ship: gh api repos/%s/forks answered %q, not a fork's clone URLs", repo.NameWithOwner, out)
	}
	upstream, err := vcs.GitRemoteFor(ctx, dir, branch)
	if err != nil {
		return "", fmt.Errorf("ship: %w", err)
	}
	upstreamURL, err := render.RunCLI(ctx, dir, "git", []string{"remote", "get-url", upstream})
	if err != nil {
		return "", fmt.Errorf("ship: git remote get-url %s: %w", upstream, err)
	}
	forkURL := ssh
	if strings.HasPrefix(strings.TrimSpace(upstreamURL), "https://") {
		forkURL = https
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"remote", "add", forkRemote, forkURL}); err != nil {
		return "", fmt.Errorf("ship: git remote add %s %s: %w", forkRemote, forkURL, err)
	}
	return forkRemote, nil
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
